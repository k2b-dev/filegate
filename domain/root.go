package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

type Root struct {
	needsRecovery bool
	recovering    bool
	Config        RootConfig
	Files         Files
	State         State
	MaxBytes      int64
	mu            sync.RWMutex
	statusMu      sync.RWMutex
	status        IndexStatus
	stats         *Stats
	generation    string
	now           func() time.Time
}
type claim struct {
	Device uint64
	Inode  uint64
	Path   string
}
type revision struct{ Metadata Metadata }
type publication struct {
	Path      string
	Temp      string
	Node      Node
	Claim     claim
	Metadata  Metadata
	ResultKey string
	Receipt   *sessionReceipt
}

func NewRoot(cfg RootConfig, f Files, s State, maxBytes int64) (*Root, error) {
	r := &Root{Config: cfg, Files: f, State: s, MaxBytes: maxBytes, now: time.Now}
	if cfg.Versioning.Enabled && !cfg.Index || maxBytes <= 0 {
		return nil, ErrInvalid
	}
	r.status.Enabled = cfg.Index
	var bound string
	if e := s.Get("root/path", &bound); e == nil && bound != cfg.Path {
		return nil, fmt.Errorf("root name is already bound to another path")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if bound == "" {
		if e := s.Put("root/path", cfg.Path); e != nil {
			return nil, e
		}
	}
	for _, p := range []string{".filegate", ".filegate/staging", ".filegate/versions"} {
		if e := f.Mkdir(p, 0700); e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		if p == ".filegate" {
			if e := f.SecurePrivate(); e != nil {
				return nil, e
			}
		}
		st, e := f.Stat(p)
		if e != nil || !st.IsDir() {
			return nil, fmt.Errorf("private directory %s is unavailable", p)
		}
		_, _, uid, _, _ := f.Identity(st)
		if uid != uint32(os.Geteuid()) || st.Mode().Perm() != 0700 {
			return nil, fmt.Errorf("private directory %s must be owned by daemon with mode 0700", p)
		}
		if e := f.Sync(p); e != nil {
			return nil, e
		}
		if e := f.Sync(path.Dir(p)); e != nil {
			return nil, e
		}
	}
	if e := r.bindState(); e != nil {
		return nil, e
	}
	e := s.Get("index/generation", &r.generation)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if e := r.recover(); e != nil {
		return nil, e
	}
	if e := r.recoverMutations(); e != nil {
		return nil, e
	}
	if e := r.cleanArtifacts(); e != nil {
		return nil, e
	}
	var stats Stats
	if e := s.Get("index/stats", &stats); e == nil {
		r.stats = &stats

	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	var built time.Time
	if e := s.Get("index/built", &built); e == nil {
		r.status.LastBuilt = &built
	}
	if cfg.Index && r.generation == "" {
		if e := r.Rebuild(context.Background()); e != nil {
			return nil, e
		}
	}
	return r, nil
}
func newID() string { return uuid.Must(uuid.NewV7()).String() }
func CleanPath(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.IndexByte(p, 0) >= 0 {
		return "", ErrInvalid
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." || c == ".filegate" || strings.HasPrefix(c, ".fg-") {
			return "", ErrInvalid
		}
	}
	return path.Clean(p), nil
}
func validWrite(p string) (string, error) {
	p, e := CleanPath(p)
	if e == nil && p == "." {
		e = ErrInvalid
	}
	return p, e
}
func ValidateOptions(o WriteOptions) error {
	if o.OnConflict != "" && o.OnConflict != "error" && o.OnConflict != "overwrite" && o.OnConflict != "rename" {
		return ErrInvalid
	}
	if e := ValidateMetadata(o.Metadata); e != nil {
		return e
	}
	return validateOwnership(o.Ownership)
}
func (r *Root) node(p string, assign bool) (Node, error) {
	if e := r.guard(); e != nil {
		return Node{}, e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return Node{}, e
	}
	dev, ino, uid, gid, links := r.Files.Identity(st)
	n := Node{Root: r.Config.Name, Path: p, Directory: st.IsDir(), Size: st.Size(), Modified: st.ModTime().UTC(), Mode: fmt.Sprintf("%04o", UnixMode(st.Mode())), UID: uid, GID: gid}
	if st.IsDir() {
		n.Size = 0
	}
	if !r.Config.Index {
		return n, nil
	}
	if !st.IsDir() && links != 1 {
		return Node{}, fmt.Errorf("%w: indexed files must not have hard links", ErrInvalid)
	}
	id, e := r.Files.ID(f)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return n, e
	}
	var c claim
	if e == nil {
		ce := r.State.Get("identity/"+id, &c)
		if ce != nil && !errors.Is(ce, os.ErrNotExist) {
			return n, ce
		}
		if ce == nil && (c.Device != dev || c.Inode != ino) {
			id = ""
		}
	}
	if id == "" {
		if !assign {
			return n, nil
		}
		id = newID()
		if e := r.Files.SetID(f, id); e != nil {
			return n, e
		}
		if e := f.Sync(); e != nil {
			return n, e
		}
	}
	n.ID = id
	if assign && (c.Device != dev || c.Inode != ino || c.Path != p) {
		if e := r.State.Put("identity/"+id, claim{dev, ino, p}); e != nil {
			return n, e
		}
	}
	return n, nil
}
func (r *Root) Stat(p string) (Node, error) {
	p, e := CleanPath(p)
	if e != nil {
		return Node{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node(p, true)
}
func (r *Root) Open(p string) (*os.File, error) {
	p, e := validWrite(p)
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return nil, e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	s, e := f.Stat()
	if e != nil || s.IsDir() {
		f.Close()
		return nil, ErrInvalid
	}
	return f, nil
}
func (r *Root) List(p, after string, limit int) (Page, error) {
	p, e := CleanPath(p)
	if e != nil {
		return Page{}, e
	}
	if limit < 1 || limit > 1000 {
		return Page{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return Page{}, e
	}
	defer d.Close()
	entries, e := d.ReadDir(-1)
	if e != nil {
		return Page{}, e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := Page{Items: []Node{}}
	for _, entry := range entries {
		if entry.Name() <= after || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		q := path.Join(p, entry.Name())
		if _, e := CleanPath(q); e != nil {
			continue
		}
		if len(out.Items) == limit {
			out.Next = path.Base(out.Items[len(out.Items)-1].Path)
			break
		}
		n, e := r.node(q, true)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, n)
	}
	return out, nil
}
func (r *Root) parents(p string, o *Ownership) error {
	dir := path.Dir(p)
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		q := strings.Join(parts[:i+1], "/")
		e := r.makeDirectory(q, o)
		if errors.Is(e, os.ErrExist) {
			continue
		}
		if e != nil {
			return e
		}
		if r.Config.Index {
			n, e := r.node(q, true)
			if e != nil {
				return e
			}
			if e = r.indexNode(n); e != nil {
				return e
			}
		}
	}
	return nil
}
func (r *Root) indexNode(n Node) error {
	if !r.Config.Index {
		return nil
	}
	return r.State.Put("i/"+r.generation+"/"+n.Path, n)
}

// Put streams outside the root lock; only publication waits for a rebuild.
func (r *Root) Put(ctx context.Context, p string, body io.Reader, o WriteOptions) (Node, error) {
	p, e := validWrite(p)
	if e != nil {
		return Node{}, e
	}
	if e = ValidateOptions(o); e != nil {
		return Node{}, e
	}
	temp := ".filegate/staging/" + newID()
	f, e := r.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return Node{}, e
	}
	defer func() { f.Close(); r.Files.Remove(temp, false) }()
	if _, e = copyStream(f, &contextReader{ctx, body}, r.MaxBytes); e != nil {
		return Node{}, e
	}
	if e = f.Sync(); e != nil {
		return Node{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publish(p, temp, f, o, false, "")
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(b)
}
func (r *Root) publish(p, temp string, f *os.File, o WriteOptions, force bool, resultKey string) (Node, error) {
	if e := r.parents(p, o.Ownership); e != nil {
		return Node{}, e
	}
	old, e := r.node(p, true)
	exists := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return Node{}, e
	}
	if exists && old.Directory {
		return Node{}, ErrConflict
	}
	if exists && o.OnConflict != "overwrite" {
		if o.OnConflict != "rename" {
			return Node{}, ErrConflict
		}
		base, ext := strings.TrimSuffix(p, path.Ext(p)), path.Ext(p)
		for i := 1; i <= 10000; i++ {
			candidate := fmt.Sprintf("%s-%02d%s", base, i, ext)
			_, e := r.Files.Stat(candidate)
			if errors.Is(e, os.ErrNotExist) {
				p = candidate
				exists = false
				break
			}
			if e != nil {
				return Node{}, e
			}
			if i == 10000 {
				return Node{}, ErrLimit
			}
		}
	}
	if e = r.preparePublication(p, f, exists, o.Ownership); e != nil {
		return Node{}, e
	}
	id := ""
	if r.Config.Index {
		id = old.ID
		if !exists || id == "" {
			id = newID()
		}
		if e = r.Files.SetID(f, id); e != nil {
			return Node{}, e
		}
	}
	if e = f.Sync(); e != nil {
		return Node{}, e
	}
	if exists && r.Config.Versioning.Enabled {
		if _, e = r.snapshot(old, false, nil, force); e != nil {
			return Node{}, e
		}
	}
	st, e := f.Stat()
	if e != nil {
		return Node{}, e
	}
	dev, ino, uid, gid, _ := r.Files.Identity(st)
	n := Node{Root: r.Config.Name, Path: p, ID: id, Size: st.Size(), Modified: st.ModTime().UTC(), Mode: fmt.Sprintf("%04o", UnixMode(st.Mode())), UID: uid, GID: gid}
	rec := publication{Path: p, Temp: temp, Node: n, Claim: claim{dev, ino, p}, Metadata: o.Metadata, ResultKey: resultKey}
	if resultKey != "" {
		var session Session
		if e := r.State.Get("session/"+strings.TrimPrefix(resultKey, "done/"), &session); e != nil {
			return Node{}, e
		}
		receipt := sessionReceipt{Session: terminalSession(session, SessionCommitted, r.now(), &n), CleanupPending: true}
		rec.Receipt = &receipt
	}
	key := "pending/" + newID()
	if e = r.State.Put(key, rec); e != nil {
		return Node{}, e
	}
	r.needsRecovery = true
	if e = r.Files.Rename(temp, p, exists); e != nil {
		return Node{}, e
	}
	if e = r.finishPublication(key, rec); e != nil {
		return Node{}, e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return n, nil
}
func (r *Root) finishPublication(key string, p publication) error {
	cs := []Change{{Key: key, Delete: true}, {Key: "index/stats", Delete: true}}
	add := func(k string, v any) error { c, e := encoded(k, v); cs = append(cs, c); return e }
	if p.Node.ID != "" {
		if e := add("identity/"+p.Node.ID, p.Claim); e != nil {
			return e
		}
		if e := add("current/"+p.Node.ID, revision{p.Metadata}); e != nil {
			return e
		}
		if e := add("i/"+r.generation+"/"+p.Path, p.Node); e != nil {
			return e
		}
	}
	if p.ResultKey != "" {
		if p.Receipt == nil {
			return errors.New("publication missing session receipt")
		}
		cs = append(cs, Change{Key: "session/" + p.Receipt.ID, Delete: true})
		if e := add(p.ResultKey, p.Receipt); e != nil {
			return e
		}
	}
	return r.State.Batch(cs)
}
func (r *Root) recover() error {
	return r.State.Scan("pending/", func(k string, b []byte) error {
		var p publication
		if e := json.Unmarshal(b, &p); e != nil {
			return e
		}
		st, e := r.Files.Stat(p.Path)
		if e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == p.Claim.Device && ino == p.Claim.Inode {
				if e = r.Files.Sync(path.Dir(p.Path)); e != nil {
					return e
				}
				if e = r.Files.Sync(path.Dir(p.Temp)); e != nil {
					return e
				}
				return r.finishPublication(k, p)
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		_ = r.Files.Remove(p.Temp, false)
		return r.State.Delete(k)
	})
}
func (r *Root) invalidateStats() {
	r.statusMu.Lock()
	r.stats = nil
	r.statusMu.Unlock()
	_ = r.State.Delete("index/stats")
}
func (r *Root) walk(ctx context.Context, fn func(Node) error) error { return r.walkFrom(ctx, ".", fn) }
func (r *Root) walkFrom(ctx context.Context, base string, fn func(Node) error) error {
	var visit func(string) error
	visit = func(p string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, e := r.node(p, true)
		if e != nil {
			return e
		}
		if e = fn(n); e != nil {
			return e
		}
		if !n.Directory {
			return nil
		}
		d, e := r.Files.Open(p, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		defer d.Close()
		for {
			entries, e := d.ReadDir(256)
			for _, entry := range entries {
				q := path.Join(p, entry.Name())
				if _, ce := CleanPath(q); ce != nil || entry.Type()&fs.ModeSymlink != 0 {
					continue
				}
				if e := visit(q); e != nil {
					return e
				}
			}
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return e
			}
		}
	}
	return visit(base)
}

func (r *Root) guard() error {
	if !r.needsRecovery || r.recovering {
		return nil
	}
	r.recovering = true
	defer func() { r.recovering = false }()
	if e := r.recover(); e != nil {
		return e
	}
	if e := r.recoverMutations(); e != nil {
		return e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return nil
}
