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
	Kind            string
	From            string
	To              string
	ID              string
	ReplacedID      string
	Device          uint64
	Inode           uint64
	Applied         bool
	CompletionKey   string
	WriteGeneration string
}

func (r *Root) Remove(p string, recursive bool) error {
	p, err := validWrite(p)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remove(p, recursive)
}
func (r *Root) remove(p string, recursive bool) error {
	return r.removeWithReceipt(p, recursive, "")
}

// removeWithReceipt is called while holding the root lock. Namespace removal and
// its durable acknowledgement share the same recovery intent.
func (r *Root) removeWithReceipt(p string, recursive bool, completionKey string) error {
	n, err := r.node(p, true)
	if err != nil {
		return err
	}
	if n.Directory && !recursive {
		d, err := r.Files.Open(p, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		entries, err := d.Readdirnames(1)
		d.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(entries) > 0 {
			return ErrConflict
		}
	}
	st, err := r.Files.Stat(p)
	if err != nil {
		return err
	}
	dev, ino, _, _, _ := r.Files.Identity(st)
	m := mutation{Kind: "delete", From: p, To: ".filegate/staging/delete-" + newID(), ID: n.ID, Device: dev, Inode: ino, CompletionKey: completionKey}
	if r.Config.Managed {
		m.WriteGeneration = newID()
	}
	key := "mutation/" + newID()
	if err = r.State.Put(key, m); err != nil {
		return err
	}
	r.needsRecovery = true
	if err = r.Files.Rename(p, m.To, false); err != nil {
		return err
	}
	m.Applied = true
	if err = r.State.Put(key, m); err != nil {
		return err
	}
	if err = r.finishMutation(key, m); err != nil {
		return err
	}
	r.needsRecovery = false
	r.invalidateStats()
	return nil
}

func (r *Root) Move(p, to string) (Node, error) { return r.MoveWithOptions(p, to, WriteOptions{}) }
func (r *Root) MoveWithOptions(p, to string, o WriteOptions) (Node, error) {
	var err error
	if p, err = validWrite(p); err != nil {
		return Node{}, err
	}
	if to, err = validWrite(to); err != nil {
		return Node{}, err
	}
	if err = ValidateOptions(o); err != nil {
		return Node{}, err
	}
	// A native move preserves inode metadata and identity. Provisioning belongs to
	// copy/publication; silently ignoring those instructions would be surprising.
	if o.Ownership != nil || o.AccessACL != nil || len(o.Metadata) > 0 || o.Precondition != nil {
		return Node{}, fmt.Errorf("%w: native moves accept only onConflict", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.node(p, true)
	if err != nil {
		return Node{}, err
	}
	if n.Directory && strings.HasPrefix(to, p+"/") {
		return Node{}, ErrInvalid
	}
	if p == to && o.OnConflict != "rename" {
		return Node{}, ErrConflict
	}
	requested := to
	to, _, err = r.chooseTarget(to, n.Directory, o.OnConflict)
	if err != nil {
		return Node{}, err
	}
	if err = r.parents(to, nil); err != nil {
		return Node{}, err
	}
	st, err := r.Files.Stat(p)
	if err != nil {
		return Node{}, err
	}
	dev, ino, _, _, _ := r.Files.Identity(st)
	m := mutation{Kind: "move", From: p, To: to, ID: n.ID, Device: dev, Inode: ino}
	if r.Config.Managed {
		m.WriteGeneration = newID()
	}
	replace := o.OnConflict == "overwrite"
	if replace {
		old, e := r.node(to, false)
		if e == nil {
			m.ReplacedID = old.ID
		} else if !errors.Is(e, os.ErrNotExist) {
			return Node{}, e
		}
	}
	key := "mutation/" + newID()
	for attempt := 0; attempt < 8; attempt++ {
		m.To = to
		if err = r.State.Put(key, m); err != nil {
			return Node{}, err
		}
		r.needsRecovery = true
		err = r.Files.Rename(p, to, replace)
		if !errors.Is(err, os.ErrExist) || o.OnConflict != "rename" {
			break
		}
		// An NFS retransmission can report EEXIST after applying the rename.
		// Keep the original intent unless both inodes prove a real collision.
		if st, e := r.Files.Stat(to); e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == m.Device && ino == m.Inode {
				if e = r.Files.Sync(path.Dir(p)); e != nil {
					return Node{}, e
				}
				if e = r.Files.Sync(path.Dir(to)); e != nil {
					return Node{}, e
				}
				err = nil
				break
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return Node{}, e
		}
		st, e := r.Files.Stat(p)
		if e != nil {
			return Node{}, err
		}
		dev, ino, _, _, _ := r.Files.Identity(st)
		if dev != m.Device || ino != m.Inode {
			return Node{}, ErrConflict
		}
		if attempt == 7 {
			return Node{}, ErrConflict
		}
		to, _, err = r.chooseTarget(requested, n.Directory, "rename")
		if err != nil {
			return Node{}, err
		}
	}
	if err != nil {
		return Node{}, err
	}
	m.Applied = true
	if err = r.State.Put(key, m); err != nil {
		return Node{}, err
	}
	r.recovering = true
	err = r.finishMutation(key, m)
	r.recovering = false
	if err != nil {
		return Node{}, err
	}
	r.needsRecovery = false
	r.invalidateStats()
	return r.node(m.To, true)
}

func (r *Root) finishMutation(key string, m mutation) error {
	service := r
	if r.control != nil {
		service = r.control
	}
	// Recovery work only reconciles an already-authorized rename. It must not add
	// descendant read requirements to native directory moves or quarantined deletes.
	if m.ReplacedID != "" && m.ReplacedID != m.ID {
		if err := service.deleteHistory(m.ReplacedID); err != nil {
			return err
		}
	}
	if r.Config.Managed {
		if err := service.finishRevisionMutation(m.From, m.To, m.Kind == "delete"); err != nil {
			return err
		}
	}
	if r.Config.Index {
		var changes []Change
		flush := func() error {
			if len(changes) == 0 {
				return nil
			}
			err := r.State.Batch(changes)
			changes = nil
			return err
		}
		apply := func(_ string, b []byte) error {
			var n Node
			if err := json.Unmarshal(b, &n); err != nil {
				return err
			}
			changes = append(changes, r.removeIndexChanges(n)...)
			if m.Kind == "move" {
				n.Path = m.To + strings.TrimPrefix(n.Path, m.From)
				var previous Node
				err := r.State.Get("i/"+r.generation+"/"+n.Path, &previous)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				var prior *Node
				if err == nil {
					prior = &previous
				}
				cs, err := r.indexChanges(n, prior)
				if err != nil {
					return err
				}
				changes = append(changes, cs...)
				if n.ID != "" {
					var c claim
					if err = r.State.Get("identity/"+n.ID, &c); err == nil {
						c.Path = n.Path
						v, e := encoded("identity/"+n.ID, c)
						if e != nil {
							return e
						}
						changes = append(changes, v)
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
			} else {
				if err := service.deleteHistory(n.ID); err != nil {
					return err
				}
				if n.ID != "" {
					changes = append(changes, Change{Key: "identity/" + n.ID, Delete: true})
				}
			}
			if len(changes) >= 256 {
				return flush()
			}
			return nil
		}
		prefix := "i/" + r.generation + "/"
		var top Node
		if err := r.State.Get(prefix+m.From, &top); err == nil {
			b, _ := json.Marshal(top)
			if err = apply(prefix+m.From, b); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := r.State.Scan(prefix+m.From+"/", apply); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		if m.Kind == "move" {
			n, err := service.node(m.To, true)
			if err != nil {
				return err
			}
			if err = service.indexNode(n); err != nil {
				return err
			}
		} else if err := service.deleteHistory(m.ID); err != nil {
			return err
		}
	}
	if m.Kind == "delete" {
		if err := service.Files.Remove(m.To, true); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cs := []Change{{Key: key, Delete: true}, {Key: "index/stats", Delete: true}}
	if m.ReplacedID != "" && m.ReplacedID != m.ID {
		cs = append(cs, Change{Key: "identity/" + m.ReplacedID, Delete: true})
	}
	if m.CompletionKey != "" {
		cs = append(cs, Change{Key: m.CompletionKey, Value: []byte("true")})
	}
	if m.WriteGeneration != "" {
		cs = append(cs, generationChange(m.WriteGeneration))
	}
	return r.State.Batch(cs)
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
		if m.Kind == "delete" && m.Applied {
			return r.finishMutation(k, m)
		}
		st, e := r.Files.Stat(m.To)
		if e == nil {
			dev, ino, _, _, _ := r.Files.Identity(st)
			if dev == m.Device && ino == m.Inode {
				m.Applied = true
				if e = r.State.Put(k, m); e != nil {
					return e
				}
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

// lockRoots gives every operation the same lock order. Root names are unique in
// the server registry; scoped execution views share their control root's lock.
func lockRoots(a, b *Root) func() {
	if a.rootShared == b.rootShared {
		a.mu.Lock()
		return func() { a.mu.Unlock() }
	}
	if a.Config.Name > b.Config.Name {
		a, b = b, a
	}
	a.mu.Lock()
	b.mu.Lock()
	return func() { b.mu.Unlock(); a.mu.Unlock() }
}

// Transfer copies a file/tree or performs a same-root native move. Cross-root
// moves require TransferMove's caller-chosen recovery ID.
func Transfer(ctx context.Context, src *Root, p string, dst *Root, to string, move bool, o WriteOptions) (Node, error) {
	var err error
	if p, err = validWrite(p); err != nil {
		return Node{}, err
	}
	if to, err = validWrite(to); err != nil {
		return Node{}, err
	}
	if err = ValidateOptions(o); err != nil {
		return Node{}, err
	}
	if move {
		if src.rootShared != dst.rootShared {
			return Node{}, fmt.Errorf("%w: cross-root moves require a transfer ID", ErrInvalid)
		}
		if !sameExecution(src.execution, dst.execution) {
			return Node{}, ErrInvalid
		}
		return src.MoveWithOptions(p, to, o)
	}
	unlock := lockRoots(src, dst)
	defer unlock()
	if err = src.guard(); err != nil {
		return Node{}, err
	}
	if err = dst.guard(); err != nil {
		return Node{}, err
	}
	return copyLocked(ctx, src, p, dst, to, o, "")
}
func (r *Root) cleanArtifacts() error {
	if err := r.cleanupTreeManifests(); err != nil {
		return err
	}
	if e := r.cleanupSessions(context.Background()); e != nil {
		return e
	}
	referenced := map[string]bool{}
	if e := r.State.Scan("session/", func(_ string, b []byte) error {
		var s Session
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		for i := 0; int64(i) < (s.Size+s.ChunkSize-1)/s.ChunkSize; i++ {
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
