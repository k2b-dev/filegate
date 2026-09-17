package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type mutation struct {
	Kind   string
	From   string
	To     string
	ID     string
	Device uint64
	Inode  uint64
}

func (r *Root) Remove(p string, recursive bool) error {
	p, e := validWrite(p)
	if e != nil {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remove(p, recursive)
}
func (r *Root) remove(p string, recursive bool) error {
	n, e := r.node(p, true)
	if e != nil {
		return e
	}
	if n.Directory && !recursive {
		d, e := r.Files.Open(p, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		entries, e := d.Readdirnames(1)
		d.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(entries) > 0 {
			return ErrConflict
		}
	}
	st, e := r.Files.Stat(p)
	if e != nil {
		return e
	}
	dev, ino, _, _, _ := r.Files.Identity(st)
	m := mutation{Kind: "delete", From: p, To: ".filegate/staging/delete-" + newID(), ID: n.ID, Device: dev, Inode: ino}
	key := "mutation/" + newID()
	if e = r.State.Put(key, m); e != nil {
		return e
	}
	r.needsRecovery = true
	if e = r.Files.Rename(p, m.To, false); e != nil {
		return e
	}
	if e = r.finishMutation(key, m); e != nil {
		return e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return nil
}
func (r *Root) Move(p, to string) (Node, error) {
	p, e := validWrite(p)
	if e != nil {
		return Node{}, e
	}
	to, e = validWrite(to)
	if e != nil || p == to || strings.HasPrefix(to, p+"/") {
		return Node{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, e := r.node(p, true)
	if e != nil {
		return Node{}, e
	}
	if e = r.parents(to, nil); e != nil {
		return Node{}, e
	}
	st, e := r.Files.Stat(p)
	if e != nil {
		return Node{}, e
	}
	dev, ino, _, _, _ := r.Files.Identity(st)
	m := mutation{Kind: "move", From: p, To: to, ID: n.ID, Device: dev, Inode: ino}
	key := "mutation/" + newID()
	if e = r.State.Put(key, m); e != nil {
		return Node{}, e
	}
	r.needsRecovery = true
	if e = r.Files.Rename(p, to, false); e != nil {
		return Node{}, e
	}
	r.recovering = true
	e = r.finishMutation(key, m)
	r.recovering = false
	if e != nil {
		return Node{}, e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return r.node(to, true)
}
func (r *Root) finishMutation(key string, m mutation) error {
	if r.Config.Index {
		cs := []Change{}
		flush := func() error { e := r.State.Batch(cs); cs = nil; return e }
		e := r.State.Scan("i/"+r.generation+"/", func(k string, b []byte) error {
			var n Node
			if e := json.Unmarshal(b, &n); e != nil {
				return e
			}
			if n.Path != m.From && !strings.HasPrefix(n.Path, m.From+"/") {
				return nil
			}
			if m.Kind == "move" {
				p := m.To + strings.TrimPrefix(n.Path, m.From)
				fresh, e := r.node(p, true)
				if e != nil {
					return e
				}
				c, e := encoded("i/"+r.generation+"/"+p, fresh)
				if e != nil {
					return e
				}
				cs = append(cs, c)
			} else {
				if e := r.deleteHistory(n.ID); e != nil {
					return e
				}
			}
			cs = append(cs, Change{Key: k, Delete: true})
			if len(cs) >= 256 {
				return flush()
			}
			return nil
		})
		if e != nil {
			return e
		}
		if len(cs) > 0 {
			if e = flush(); e != nil {
				return e
			}
		}
		if m.Kind == "move" {
			n, e := r.node(m.To, true)
			if e != nil {
				return e
			}
			if e = r.indexNode(n); e != nil {
				return e
			}
		} else {
			if e = r.deleteHistory(m.ID); e != nil {
				return e
			}
		}
	}
	if m.Kind == "delete" {
		e := r.Files.Remove(m.To, true)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return r.State.Batch([]Change{{Key: key, Delete: true}, {Key: "index/stats", Delete: true}})
}
func (r *Root) recoverMutations() error {
	was := r.recovering
	r.recovering = true
	defer func() { r.recovering = was }()
	return r.State.Scan("mutation/", func(k string, b []byte) error {
		var m mutation
		if e := json.Unmarshal(b, &m); e != nil {
			return e
		}
		st, e := r.Files.Stat(m.To)
		if e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == m.Device && ino == m.Inode {
				if e = r.Files.Sync(path.Dir(m.From)); e != nil {
					return e
				}
				if e = r.Files.Sync(path.Dir(m.To)); e != nil {
					return e
				}
				return r.finishMutation(k, m)
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		st, e = r.Files.Stat(m.From)
		if e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == m.Device && ino == m.Inode {
				return r.State.Delete(k)
			}
		}
		if m.Kind == "delete" && errors.Is(e, os.ErrNotExist) {
			if e = r.Files.Sync(path.Dir(m.From)); e != nil {
				return e
			}
			if e = r.Files.Sync(path.Dir(m.To)); e != nil {
				return e
			}
			return r.finishMutation(k, m)
		}
		return fmt.Errorf("unfinished %s requires recovery; source or target changed externally", m.Kind)
	})
}

// Cross-root transfer holds both roots in name order through source removal.
// A failed transfer may leave copied destination files, but never removes newer source bytes.
func Transfer(ctx context.Context, src *Root, p string, dst *Root, to string, move bool, o WriteOptions) (Node, error) {
	p, e := validWrite(p)
	if e != nil {
		return Node{}, e
	}
	to, e = validWrite(to)
	if e != nil {
		return Node{}, e
	}
	if e = ValidateOptions(o); e != nil {
		return Node{}, e
	}
	if src == dst && move {
		return src.Move(p, to)
	}
	if src == dst && (p == to || strings.HasPrefix(to, p+"/")) {
		return Node{}, ErrInvalid
	}
	first, second := src, dst
	if first.Config.Name > second.Config.Name {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if second != first {
		second.mu.Lock()
		defer second.mu.Unlock()
	}
	if e = src.guard(); e != nil {
		return Node{}, e
	}
	if e = dst.guard(); e != nil {
		return Node{}, e
	}
	var copyOne func(string, string) (Node, error)
	copyOne = func(a, b string) (Node, error) {
		if e := ctx.Err(); e != nil {
			return Node{}, e
		}
		n, e := src.node(a, true)
		if e != nil {
			return Node{}, e
		}
		if n.Directory {
			if e = dst.parents(b, o.Ownership); e != nil {
				return Node{}, e
			}
			if e = dst.makeDirectory(b, o.Ownership); e != nil {
				return Node{}, e
			}
			out, e := dst.node(b, true)
			if e != nil {
				return Node{}, e
			}
			if e = dst.indexNode(out); e != nil {
				return Node{}, e
			}
			f, e := src.Files.Open(a, os.O_RDONLY, 0)
			if e != nil {
				return Node{}, e
			}
			defer f.Close()
			for {
				entries, e := f.ReadDir(256)
				for _, entry := range entries {
					child := path.Join(a, entry.Name())
					if _, ce := CleanPath(child); ce != nil || entry.Type()&os.ModeSymlink != 0 {
						return Node{}, fmt.Errorf("%w: transfer contains unsupported path %s", ErrInvalid, child)
					}
					if _, e = copyOne(child, path.Join(b, entry.Name())); e != nil {
						return Node{}, e
					}
				}
				if errors.Is(e, io.EOF) {
					break
				}
				if e != nil {
					return Node{}, e
				}
			}
			return out, nil
		}
		f, e := src.Files.Open(a, os.O_RDONLY, 0)
		if e != nil {
			return Node{}, e
		}
		defer f.Close()
		temp := ".filegate/staging/" + newID()
		out, e := dst.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return Node{}, e
		}
		defer func() { out.Close(); dst.Files.Remove(temp, false) }()
		if _, e = copyStream(out, &contextReader{ctx, f}, dst.MaxBytes); e != nil {
			return Node{}, e
		}
		return dst.publish(b, temp, out, o, false, "")
	}
	n, e := copyOne(p, to)
	dst.invalidateStats()
	if e != nil {
		return n, e
	}
	if move {
		e = src.remove(p, true)
	}
	return n, e
}
func (r *Root) cleanArtifacts() error {
	referenced := map[string]bool{}
	if e := r.State.Scan("session/", func(_ string, b []byte) error {
		var s Session
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		for i := range s.Segments {
			referenced[path.Base(segmentPath(s.ID, i))] = true
		}
		return nil
	}); e != nil {
		return e
	}
	if e := r.State.Scan("v/", func(_ string, b []byte) error {
		var v Version
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		referenced[v.ID] = true
		return nil
	}); e != nil {
		return e
	}
	for _, dir := range []string{".filegate/staging", ".filegate/versions"} {
		f, e := r.Files.Open(dir, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		for {
			names, e := f.Readdirnames(256)
			for _, n := range names {
				if !referenced[n] {
					if e := r.Files.Remove(path.Join(dir, n), true); e != nil {
						f.Close()
						return e
					}
				}
			}
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				f.Close()
				return e
			}
		}
		f.Close()
	}
	return nil
}
