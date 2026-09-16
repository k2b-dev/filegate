package domain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

func (r *Root) versions(id string) ([]Version, error) {
	vs := []Version{}
	e := r.State.Scan("v/"+id+"/", func(_ string, b []byte) error {
		var v Version
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		vs = append(vs, v)
		return nil
	})
	sort.Slice(vs, func(i, j int) bool { return vs[i].Created.After(vs[j].Created) })
	return vs, e
}
func (r *Root) Versions(p string) ([]Version, error) {
	if !r.Config.Versioning.Enabled {
		return nil, ErrDisabled
	}
	p, e := validWrite(p)
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, e := r.node(p, true)
	if e != nil {
		return nil, e
	}
	return r.versions(n.ID)
}
func (r *Root) snapshot(n Node, pinned bool, metadata Metadata, force bool) (*Version, error) {
	if !r.Config.Versioning.Enabled {
		return nil, ErrDisabled
	}
	if n.Directory {
		return nil, ErrInvalid
	}
	vs, e := r.versions(n.ID)
	if e != nil {
		return nil, e
	}
	if !force && len(vs) > 0 && r.now().Sub(vs[0].Created) < r.Config.Versioning.Cooldown {
		return nil, nil
	}
	if metadata == nil {
		var c revision
		e = r.State.Get("current/"+n.ID, &c)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		metadata = c.Metadata
	}
	v := Version{ID: newID(), FileID: n.ID, Created: r.now().UTC(), Size: n.Size, Pinned: pinned, Metadata: metadata, CopyMode: "copy"}
	blob := ".filegate/versions/" + v.ID
	src, e := r.Files.Open(n.Path, os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	defer src.Close()
	dst, e := r.Files.Open(blob, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		dst.Close()
		if !success {
			r.Files.Remove(blob, false)
		}
	}()
	reflink, e := r.Files.Clone(src, dst)
	if e != nil {
		return nil, e
	}
	if reflink {
		v.CopyMode = "reflink"
	}
	if e = dst.Sync(); e != nil {
		return nil, e
	}
	if e = r.Files.Sync(".filegate/versions"); e != nil {
		return nil, e
	}
	success = true
	if e = r.State.Put("v/"+n.ID+"/"+v.ID, v); e != nil {
		return nil, e
	}
	success = true
	return &v, nil
}
func (r *Root) Snapshot(p string, pinned bool, m Metadata) (Version, error) {
	if e := ValidateMetadata(m); e != nil {
		return Version{}, e
	}
	p, e := validWrite(p)
	if e != nil {
		return Version{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, e := r.node(p, true)
	if e != nil {
		return Version{}, e
	}
	v, e := r.snapshot(n, pinned, m, true)
	if e != nil {
		return Version{}, e
	}
	return *v, nil
}
func (r *Root) versionFor(p, id string) (Node, Version, error) {
	n, e := r.node(p, true)
	if e != nil {
		return n, Version{}, e
	}
	var v Version
	e = r.State.Get("v/"+n.ID+"/"+id, &v)
	return n, v, e
}
func (r *Root) UpdateVersion(p, id string, pinned bool, m Metadata) (Version, error) {
	if !r.Config.Versioning.Enabled {
		return Version{}, ErrDisabled
	}
	if e := ValidateMetadata(m); e != nil {
		return Version{}, e
	}
	p, e := validWrite(p)
	if e != nil {
		return Version{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, e := r.versionFor(p, id)
	if e != nil {
		return v, e
	}
	v.Pinned = pinned
	v.Metadata = m
	return v, r.State.Put("v/"+v.FileID+"/"+v.ID, v)
}
func (r *Root) DeleteVersion(p, id string) error {
	if !r.Config.Versioning.Enabled {
		return ErrDisabled
	}
	p, e := validWrite(p)
	if e != nil {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, e := r.versionFor(p, id)
	if e != nil {
		return e
	}
	return r.removeVersion(v)
}
func (r *Root) removeVersion(v Version) error {
	e := r.Files.Remove(".filegate/versions/"+v.ID, false)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return r.State.Delete("v/" + v.FileID + "/" + v.ID)
}
func (r *Root) deleteHistory(id string) error {
	if id == "" {
		return nil
	}
	vs, e := r.versions(id)
	if e != nil {
		return e
	}
	for _, v := range vs {
		if e = r.removeVersion(v); e != nil {
			return e
		}
	}
	return r.State.Delete("current/" + id)
}
func (r *Root) OpenVersion(p, id string) (*os.File, error) {
	if !r.Config.Versioning.Enabled {
		return nil, ErrDisabled
	}
	p, e := validWrite(p)
	if e != nil {
		return nil, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, e := r.versionFor(p, id)
	if e != nil {
		return nil, e
	}
	return r.Files.Open(".filegate/versions/"+v.ID, os.O_RDONLY, 0)
}
func (r *Root) Restore(p, id string) (Node, error) {
	if !r.Config.Versioning.Enabled {
		return Node{}, ErrDisabled
	}
	p, e := validWrite(p)
	if e != nil {
		return Node{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, v, e := r.versionFor(p, id)
	if e != nil {
		return Node{}, e
	}
	src, e := r.Files.Open(".filegate/versions/"+v.ID, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, e
	}
	defer src.Close()
	temp := ".filegate/staging/" + newID()
	dst, e := r.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return Node{}, e
	}
	defer func() { dst.Close(); r.Files.Remove(temp, false) }()
	if _, e = r.Files.Clone(src, dst); e != nil {
		return Node{}, e
	}
	return r.publish(p, temp, dst, WriteOptions{OnConflict: "overwrite", Metadata: v.Metadata}, true, "")
}

// Retained returns the union of newest calendar buckets, recent versions and pins.
func Retained(vs []Version, keep Keep, now time.Time) map[string]bool {
	out := map[string]bool{}
	copyVS := append([]Version(nil), vs...)
	sort.Slice(copyVS, func(i, j int) bool {
		if copyVS[i].Created.Equal(copyVS[j].Created) {
			return copyVS[i].ID > copyVS[j].ID
		}
		return copyVS[i].Created.After(copyVS[j].Created)
	})
	last := 0
	seen := map[string]bool{}
	start := func(t time.Time, unit string) time.Time {
		t = t.UTC()
		switch unit {
		case "h":
			return t.Truncate(time.Hour)
		case "d":
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		case "w":
			d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			return d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
		default:
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	}
	for _, v := range copyVS {
		if v.Pinned {
			out[v.ID] = true
			continue
		}
		if last < keep.Last {
			out[v.ID] = true
			last++
		}
		for _, tier := range []struct {
			unit string
			n    int
		}{{"h", keep.Hourly}, {"d", keep.Daily}, {"w", keep.Weekly}, {"m", keep.Monthly}} {
			if tier.n <= 0 {
				continue
			}
			bucket := start(v.Created, tier.unit)
			cut := start(now, tier.unit)
			switch tier.unit {
			case "h":
				cut = cut.Add(-time.Duration(tier.n-1) * time.Hour)
			case "d":
				cut = cut.AddDate(0, 0, -tier.n+1)
			case "w":
				cut = cut.AddDate(0, 0, -7*(tier.n-1))
			case "m":
				cut = cut.AddDate(0, -tier.n+1, 0)
			}
			key := tier.unit + bucket.String()
			if !bucket.Before(cut) && !bucket.After(now) && !seen[key] {
				out[v.ID] = true
				seen[key] = true
			}
		}
	}
	return out
}
func (r *Root) Prune(ctx context.Context) (int, error) {
	if !r.Config.Versioning.Enabled {
		return 0, ErrDisabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return 0, e
	}
	count := 0
	var group []Version
	fileID := ""
	flush := func() error {
		retained := Retained(group, r.Config.Versioning.Keep, r.now())
		for _, v := range group {
			if e := ctx.Err(); e != nil {
				return e
			}
			if !retained[v.ID] {
				if e := r.removeVersion(v); e != nil {
					return e
				}
				count++
			}
		}
		group = nil
		return nil
	}
	e := r.State.Scan("v/", func(_ string, b []byte) error {
		var v Version
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		if v.FileID != fileID {
			if e := flush(); e != nil {
				return e
			}
			fileID = v.FileID
		}
		group = append(group, v)
		return nil
	})
	if e == nil {
		e = flush()
	}
	return count, e
}
