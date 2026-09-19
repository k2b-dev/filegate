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
	"strings"
	"sync"
	"time"
)

type Root struct {
	*rootShared
	Config    RootConfig
	Files     Files
	State     State
	MaxBytes  int64
	control   *Root
	execution *ExecutionIdentity
}

// Every request view shares coordination and recovery with its service root.
type rootShared struct {
	needsRecovery   bool
	recovering      bool
	mu              sync.RWMutex
	statusMu        sync.RWMutex
	status          IndexStatus
	stats           *Stats
	listings        map[string]*listingSnapshot
	listingBytes    int64
	listingEpoch    uint64
	listingInstance string
	generation      string
	now             func() time.Time
}
type claim struct {
	Device uint64
	Inode  uint64
	Path   string
}
type revision struct{ Metadata Metadata }
type publication struct {
	TransferID      string
	Tree            bool
	WriteGeneration string
	Path            string
	Temp            string
	Node            Node
	Claim           claim
	Metadata        Metadata
	ResultKey       string
	Receipt         *sessionReceipt
}

func NewRoot(cfg RootConfig, f Files, s State, maxBytes int64) (*Root, error) {
	r := &Root{rootShared: &rootShared{now: time.Now}, Config: cfg, Files: f, State: s, MaxBytes: maxBytes}
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
	if e := r.ensureVersionOrder(); e != nil {
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
	if e := r.initializeManagedSetting(); e != nil {
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
	var indexFormat int
	if err := s.Get("index/format", &indexFormat); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if cfg.Index && (r.generation == "" || indexFormat != currentIndexFormat) {
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
	if o.AccessACL != nil {
		if _, e := NormalizeACL(*o.AccessACL); e != nil {
			return e
		}
	}
	if e := validatePrecondition(o); e != nil {
		return e
	}
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
	f, e := r.openMetadata(p)
	if e != nil {
		return Node{}, e
	}
	defer f.Close()
	return r.nodeFile(p, f, assign)
}

func (r *Root) nodeFile(p string, f *os.File, assign bool) (Node, error) {
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
		return r.withRevision(n, f, assign)
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
			return r.withRevision(n, f, assign)
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
	return r.withRevision(n, f, assign)
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

// OpenWithNode returns metadata and content for the same opened inode. Managed
// callers can use Revision as the strong HTTP ETag without a Stat/Open race.
func (r *Root) OpenWithNode(p string) (*os.File, Node, error) {
	p, err := validWrite(p)
	if err != nil {
		return nil, Node{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return nil, Node{}, err
	}
	f, err := r.Files.Open(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, Node{}, err
	}
	n, err := r.nodeFile(p, f, true)
	if err != nil {
		f.Close()
		return nil, Node{}, err
	}
	if n.Directory {
		f.Close()
		return nil, Node{}, ErrInvalid
	}
	return f, n, nil
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
	var previous Node
	var old *Node
	if err := r.State.Get("i/"+r.generation+"/"+n.Path, &previous); err == nil {
		old = &previous
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changes, err := r.indexChanges(n, old)
	if err != nil {
		return err
	}
	if err := r.State.Batch(changes); err != nil {
		return err
	}
	r.invalidateListings()
	return nil
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
	return r.publishTransfer(p, temp, f, o, force, resultKey, "")
}
func (r *Root) publishTransfer(p, temp string, f *os.File, o WriteOptions, force bool, resultKey, transferID string) (Node, error) {
	if err := r.guard(); err != nil {
		return Node{}, err
	}
	if e := r.checkPrecondition(p, o.Precondition); e != nil {
		return Node{}, e
	}
	if e := r.parents(p, o.Ownership); e != nil {
		return Node{}, e
	}
	requested := p
	p, exists, e := r.chooseTarget(requested, false, o.OnConflict)
	if e != nil {
		return Node{}, e
	}
	var old Node
	if exists {
		old, e = r.node(p, true)
		if e != nil {
			return Node{}, e
		}
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
	if e = r.preparePublication(p, f, exists, o.Ownership, o.AccessACL); e != nil {
		return Node{}, e
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
	if r.Config.Managed {
		n.Revision = newID()
	}
	rec := publication{TransferID: transferID, Path: p, Temp: temp, Node: n, Claim: claim{dev, ino, p}, Metadata: o.Metadata, ResultKey: resultKey}
	if r.Config.Managed {
		rec.WriteGeneration = newID()
	}
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
	if e = r.renamePublication(key, &rec, exists, requested, o.OnConflict); e != nil {
		if o.Precondition != nil && o.Precondition.IfNoneMatch && errors.Is(e, os.ErrExist) {
			return Node{}, fmt.Errorf("%w: target appeared before publication", ErrPrecondition)
		}
		return Node{}, e
	}
	if e = r.finishPublication(key, rec); e != nil {
		return Node{}, e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return rec.Node, nil
}
func (r *Root) finishPublication(key string, p publication) error {
	if p.Tree {
		if err := r.finishTreePublication(p); err != nil {
			return err
		}
	}
	cs := []Change{{Key: key, Delete: true}, {Key: "index/stats", Delete: true}}
	if p.WriteGeneration != "" {
		cs = append(cs, generationChange(p.WriteGeneration))
	}
	add := func(k string, v any) error { c, e := encoded(k, v); cs = append(cs, c); return e }
	if change, err := r.publicationRevision(p); err != nil {
		return err
	} else if change != nil {
		cs = append(cs, *change)
	}
	if p.Node.ID != "" {
		if e := add("identity/"+p.Node.ID, p.Claim); e != nil {
			return e
		}
		if e := add("current/"+p.Node.ID, revision{p.Metadata}); e != nil {
			return e
		}
		var previous Node
		var old *Node
		if err := r.State.Get("i/"+r.generation+"/"+p.Path, &previous); err == nil {
			old = &previous
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		changes, err := r.indexChanges(p.Node, old)
		if err != nil {
			return err
		}
		cs = append(cs, changes...)
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
	transferChanges, err := r.transferPublicationChanges(p)
	if err != nil {
		return err
	}
	cs = append(cs, transferChanges...)
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
		if e := r.Files.Remove(p.Temp, p.Tree); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if p.Tree {
			if e := r.deleteTreeManifest(p.Temp); e != nil {
				return e
			}
		}
		return r.State.Delete(k)
	})
}
func (r *Root) invalidateStats() {
	r.invalidateListings()
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
	if r.control != nil {
		return r.control.guard()
	}
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
