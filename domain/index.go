package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
)

func (r *Root) Rebuild(ctx context.Context) (err error) {
	if r.execution != nil {
		return ErrInvalid
	}
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
	cs := make([]Change, 0, 256)
	stats := Stats{Path: ".", Source: "index", Started: started, Freshness: "unknown"}
	flush := func() error {
		if len(cs) == 0 {
			return nil
		}
		e := r.State.Batch(cs)
		cs = cs[:0]
		return e
	}
	err = r.walk(ctx, func(n Node) error {
		if err := addStatsNode(&stats, n); err != nil {
			return err
		}
		changes, e := indexChangesFor(generation, n, nil)
		if e != nil {
			return e
		}
		cs = append(cs, changes...)
		r.statusMu.Lock()
		r.status.Scanned++
		r.statusMu.Unlock()
		if len(cs) >= 256 {
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
	stats.Completed = r.now()
	stats.Updated = stats.Completed
	stats.Complete = true
	stats.IndexBuilt = &stats.Completed
	built, _ := encoded("index/built", stats.Completed)
	gen, _ := encoded("index/generation", generation)
	st, _ := encoded("index/stats", stats)
	format, _ := encoded("index/format", currentIndexFormat)
	if err = r.State.Batch([]Change{gen, st, built, format}); err != nil {
		return err
	}
	r.generation = generation
	r.invalidateListings()
	r.statusMu.Lock()
	r.stats = &stats
	t := r.now()
	r.status.LastBuilt = &t
	r.statusMu.Unlock()
	// Only derived rows are collected. Identities, sessions and versions survive.
	cs = cs[:0]
	for _, family := range []string{"i/", "q/"} {
		err = r.State.Scan(family, func(k string, _ []byte) error {
			if !strings.HasPrefix(k, family+generation+"/") {
				cs = append(cs, Change{Key: k, Delete: true})
			}
			if len(cs) >= 256 {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return flush()
}
func (r *Root) RefreshStats(ctx context.Context, maxEntries int) (Stats, error) {
	if r.execution != nil {
		return Stats{}, ErrInvalid
	}
	if maxEntries < 1 || maxEntries > 10000000 {
		return Stats{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, e := r.recursiveStats(ctx, ".", maxEntries)
	if e != nil {
		return Stats{}, e
	}
	if !s.Complete {
		return Stats{}, ErrLimit
	}
	if e = r.State.Put("index/stats", s); e != nil {
		return Stats{}, e
	}
	r.statusMu.Lock()
	r.stats = &s
	r.statusMu.Unlock()
	return s, nil
}

// RecursiveStats observes one subtree in one bounded filesystem traversal.
// Complete means every encountered entry was visited during the scan interval;
// it does not promise an atomic filesystem snapshot or a storage quota.
func (r *Root) RecursiveStats(ctx context.Context, p string, maxEntries int) (Stats, error) {
	if maxEntries < 1 || maxEntries > 100000 {
		return Stats{}, ErrInvalid
	}
	p, err := CleanPath(p)
	if err != nil {
		return Stats{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return Stats{}, err
	}
	return r.recursiveStats(ctx, p, maxEntries)
}

func (r *Root) recursiveStats(ctx context.Context, p string, maxEntries int) (Stats, error) {
	if err := r.guard(); err != nil {
		return Stats{}, err
	}
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	stats := Stats{Path: p, Source: "filesystem", Started: r.now(), Freshness: "observed"}
	count := func(n Node) error { return addStatsNode(&stats, n) }
	n, err := r.node(p, true)
	if err != nil {
		return stats, err
	}
	if err := count(n); err != nil {
		return stats, err
	}
	if n.Directory {
		err = r.scanDirectory(ctx, p, true, maxEntries-1, count)
	}
	stats.Completed = r.now()
	stats.Updated = stats.Completed
	stats.Complete = err == nil
	if errors.Is(err, ErrLimit) {
		return stats, nil
	}
	return stats, err
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
	if r.execution != nil {
		return RootInfo{}, ErrInvalid
	}
	r.statusMu.RLock()
	info := RootInfo{Managed: r.Config.Managed, Execution: r.Config.Execution, Name: r.Config.Name, Index: r.status, Versioning: r.Config.Versioning, Cooldown: r.Config.Versioning.Cooldown.String()}
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
		if s.State == SessionOpen && s.Expires.After(r.now()) {
			info.ActiveUploads++
			info.StagingBytes += s.Received
		}
		return nil
	})
	return info, e
}

func addStatsNode(stats *Stats, n Node) error {
	if n.Directory {
		stats.Directories++
		return nil
	}
	if n.Size < 0 || n.Size > math.MaxInt64-stats.Bytes {
		return ErrLimit
	}
	stats.Files++
	stats.Bytes += n.Size
	return nil
}
