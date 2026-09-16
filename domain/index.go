package domain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"sort"
	"strings"
)

func (r *Root) Rebuild(ctx context.Context) (err error) {
	if !r.Config.Index {
		return ErrDisabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return e
	}
	started := r.now()
	r.statusMu.Lock()
	r.status.Rebuilding = true
	r.status.Scanned = 0
	r.status.Error = ""
	r.statusMu.Unlock()
	defer func() {
		r.statusMu.Lock()
		r.status.Rebuilding = false
		r.status.DurationMS = r.now().Sub(started).Milliseconds()
		if err != nil {
			r.status.Error = err.Error()
		}
		r.statusMu.Unlock()
	}()
	generation := newID()
	prefix := "i/" + generation + "/"
	cs := make([]Change, 0, 256)
	stats := Stats{Source: "index", Updated: r.now()}
	flush := func() error {
		if len(cs) == 0 {
			return nil
		}
		e := r.State.Batch(cs)
		cs = cs[:0]
		return e
	}
	err = r.walk(ctx, func(n Node) error {
		if n.Directory {
			stats.Directories++
		} else {
			stats.Files++
			stats.Bytes += n.Size
		}
		c, e := encoded(prefix+n.Path, n)
		if e != nil {
			return e
		}
		cs = append(cs, c)
		r.statusMu.Lock()
		r.status.Scanned++
		r.statusMu.Unlock()
		if len(cs) == 256 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	built, _ := encoded("index/built", r.now())
	gen, _ := encoded("index/generation", generation)
	st, _ := encoded("index/stats", stats)
	if err = r.State.Batch([]Change{gen, st, built}); err != nil {
		return err
	}
	r.generation = generation
	r.statusMu.Lock()
	r.stats = &stats
	t := r.now()
	r.status.LastBuilt = &t
	r.statusMu.Unlock()
	// Only derived rows are collected. Identities, sessions and versions survive.
	cs = cs[:0]
	err = r.State.Scan("i/", func(k string, _ []byte) error {
		if !strings.HasPrefix(k, prefix) {
			cs = append(cs, Change{Key: k, Delete: true})
		}
		if len(cs) == 256 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}
func (r *Root) RefreshStats(ctx context.Context, maxEntries int) (Stats, error) {
	if maxEntries < 1 || maxEntries > 10000000 {
		return Stats{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Stats{Source: "filesystem", Updated: r.now()}
	e := r.walk(ctx, func(n Node) error {
		if s.Files+s.Directories >= int64(maxEntries) {
			return ErrLimit
		}
		if n.Directory {
			s.Directories++
		} else {
			s.Files++
			s.Bytes += n.Size
		}
		return nil
	})
	if e != nil {
		return Stats{}, e
	}
	if e = r.State.Put("index/stats", s); e != nil {
		return Stats{}, e
	}
	r.statusMu.Lock()
	r.stats = &s
	r.statusMu.Unlock()
	return s, nil
}
func (r *Root) Search(ctx context.Context, query, base, after string, limit, maxEntries int) (Page, error) {
	base, e := CleanPath(base)
	if e != nil || limit < 1 || limit > 1000 || maxEntries < 1 || maxEntries > 10000000 {
		return Page{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Page{Items: []Node{}}
	matches := func(n Node) bool {
		return n.Path > after && (base == "." || n.Path == base || strings.HasPrefix(n.Path, base+"/")) && strings.Contains(strings.ToLower(path.Base(n.Path)), strings.ToLower(query))
	}
	if r.Config.Index {
		sentinel := errors.New("page complete")
		e = r.State.Scan("i/"+r.generation+"/", func(_ string, b []byte) error {
			var n Node
			if e := json.Unmarshal(b, &n); e != nil {
				return e
			}
			if matches(n) {
				if len(out.Items) == limit {
					out.Next = out.Items[len(out.Items)-1].Path
					return sentinel
				}
				out.Items = append(out.Items, n)
			}
			return nil
		})
		if errors.Is(e, sentinel) {
			e = nil
		}
		return out, e
	}
	count := 0
	e = r.walkFrom(ctx, base, func(n Node) error {
		count++
		if count > maxEntries {
			return ErrLimit
		}
		if matches(n) {
			out.Items = append(out.Items, n)
			sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Path < out.Items[j].Path })
			if len(out.Items) > limit+1 {
				out.Items = out.Items[:limit+1]
			}
		}
		return nil
	})
	if e != nil {
		return Page{}, e
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].Path
	}
	return out, nil
}
func (r *Root) Resolve(id string) (Node, error) {
	if !r.Config.Index {
		return Node{}, ErrDisabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var c claim
	if e := r.State.Get("identity/"+id, &c); e != nil {
		return Node{}, e
	}
	n, e := r.node(c.Path, false)
	if e == nil && n.ID != id {
		e = os.ErrNotExist
	}
	return n, e
}
func (r *Root) Info() (RootInfo, error) {
	r.statusMu.RLock()
	info := RootInfo{Name: r.Config.Name, Index: r.status, Versioning: r.Config.Versioning, Cooldown: r.Config.Versioning.Cooldown.String()}
	if r.stats != nil {
		s := *r.stats
		info.Stats = &s
	}
	r.statusMu.RUnlock()
	var e error
	info.Filesystem, info.Capacity, info.Available, e = r.Files.Capacity()
	if e != nil {
		return info, e
	}
	e = r.State.Scan("v/", func(_ string, b []byte) error {
		var v Version
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		info.Versions++
		info.VersionBytes += v.Size
		return nil
	})
	if e != nil {
		return info, e
	}
	e = r.State.Scan("session/", func(_ string, b []byte) error {
		var s Session
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		var result Node
		doneErr := r.State.Get("done/"+s.ID, &result)
		if doneErr != nil && !errors.Is(doneErr, os.ErrNotExist) {
			return doneErr
		}
		if s.Expires.After(r.now()) && errors.Is(doneErr, os.ErrNotExist) {
			info.ActiveUploads++
			info.StagingBytes += s.Received
		}
		return nil
	})
	return info, e
}
