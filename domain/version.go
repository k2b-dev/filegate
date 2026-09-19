package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// versionHead is derived from immutable version identity and creation time.
// Metadata edits do not rewrite the chronological index or cooldown pointer.
type versionHead struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

const versionOrderFormat = "version-order/format"

var stopVersionOrder = errors.New("version order entry found")

func versionOrderKey(fileID string, head versionHead) string {
	return "vo/" + fileID + "/" + head.Created.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + head.ID
}
func newerVersion(a, b versionHead) bool {
	return a.Created.After(b.Created) || a.Created.Equal(b.Created) && a.ID > b.ID
}
func (r *Root) latestVersion(fileID string) (versionHead, error) {
	var head versionHead
	err := r.State.Get("vl/"+fileID, &head)
	if errors.Is(err, os.ErrNotExist) {
		return versionHead{}, nil
	}
	return head, err
}
func (r *Root) recordVersion(v Version, latest versionHead) error {
	head := versionHead{ID: v.ID, Created: v.Created}
	record, err := encoded("v/"+v.FileID+"/"+v.ID, v)
	if err != nil {
		return err
	}
	ordered, err := encoded(versionOrderKey(v.FileID, head), head)
	if err != nil {
		return err
	}
	changes := []Change{record, ordered}
	if latest.ID == "" || newerVersion(head, latest) {
		pointer, err := encoded("vl/"+v.FileID, head)
		if err != nil {
			return err
		}
		changes = append(changes, pointer)
	}
	return r.State.Batch(changes)
}
func (r *Root) removeVersionRecord(v Version) error {
	head := versionHead{ID: v.ID, Created: v.Created}
	latest, err := r.latestVersion(v.FileID)
	if err != nil {
		return err
	}
	changes := []Change{{Key: "v/" + v.FileID + "/" + v.ID, Delete: true}, {Key: versionOrderKey(v.FileID, head), Delete: true}}
	if latest.ID == v.ID {
		var previous versionHead
		err = r.State.ScanBefore("vo/"+v.FileID+"/", versionOrderKey(v.FileID, head), func(_ string, b []byte) error {
			if err := json.Unmarshal(b, &previous); err != nil {
				return err
			}
			return stopVersionOrder
		})
		if err != nil && !errors.Is(err, stopVersionOrder) {
			return err
		}
		if previous.ID == "" {
			changes = append(changes, Change{Key: "vl/" + v.FileID, Delete: true})
		} else {
			pointer, err := encoded("vl/"+v.FileID, previous)
			if err != nil {
				return err
			}
			changes = append(changes, pointer)
		}
	}
	return r.State.Batch(changes)
}

// ensureVersionOrder builds derived order records once, before startup recovery.
// Batches remain bounded and a crash before the format marker simply restarts
// the idempotent scan. Durable version IDs, metadata and blobs are untouched.
func (r *Root) ensureVersionOrder() error {
	var format int
	err := r.State.Get(versionOrderFormat, &format)
	if err == nil {
		if format != 1 {
			return fmt.Errorf("unsupported version order format %d", format)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changes := make([]Change, 0, 512)
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		if err := r.State.Batch(changes); err != nil {
			return err
		}
		changes = changes[:0]
		return nil
	}
	appendChange := func(c Change) error {
		changes = append(changes, c)
		if len(changes) >= 512 {
			return flush()
		}
		return nil
	}
	fileID := ""
	var latest versionHead
	finishFile := func() error {
		if fileID == "" {
			return nil
		}
		pointer, err := encoded("vl/"+fileID, latest)
		if err != nil {
			return err
		}
		return appendChange(pointer)
	}
	err = r.State.Scan("v/", func(_ string, b []byte) error {
		var v Version
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if v.FileID == "" || v.ID == "" || v.Created.IsZero() {
			return fmt.Errorf("invalid persisted version: %w", ErrInvalid)
		}
		if fileID != v.FileID {
			if err := finishFile(); err != nil {
				return err
			}
			fileID = v.FileID
			latest = versionHead{}
		}
		head := versionHead{ID: v.ID, Created: v.Created}
		if latest.ID == "" || newerVersion(head, latest) {
			latest = head
		}
		ordered, err := encoded(versionOrderKey(fileID, head), head)
		if err != nil {
			return err
		}
		return appendChange(ordered)
	})
	if err != nil {
		return err
	}
	if err = finishFile(); err != nil {
		return err
	}
	marker, err := encoded(versionOrderFormat, 1)
	if err != nil {
		return err
	}
	changes = append(changes, marker)
	return flush()
}

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
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Created.Equal(vs[j].Created) {
			return vs[i].ID > vs[j].ID
		}
		return vs[i].Created.After(vs[j].Created)
	})
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
	if err := r.guard(); err != nil {
		return nil, err
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	n, e := r.nodeFile(p, f, true)
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
	latest, e := r.latestVersion(n.ID)
	if e != nil {
		return nil, e
	}
	now := r.now().UTC()
	if !force && latest.ID != "" && now.Sub(latest.Created) < r.Config.Versioning.Cooldown {
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
	v := Version{ID: newID(), FileID: n.ID, Created: now, Size: n.Size, Pinned: pinned, Metadata: metadata, CopyMode: "copy"}
	blob := ".filegate/versions/" + v.ID
	src, e := r.Files.Open(n.Path, os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	defer src.Close()
	current, e := r.nodeFile(n.Path, src, false)
	if e != nil {
		return nil, e
	}
	if current.ID != n.ID || current.Directory {
		return nil, ErrConflict
	}
	v.Size = current.Size
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
	if e = r.recordVersion(v, latest); e != nil {
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
	if err := r.guard(); err != nil {
		return Node{}, Version{}, err
	}
	// Current-file read permission authorizes historical bytes. Derive the file
	// identity from this exact actor-opened descriptor, never a privileged reopen.
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, Version{}, e
	}
	defer f.Close()
	n, e := r.nodeFile(p, f, true)
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
	// Commit reference removal before deleting immutable content. A failed state
	// commit preserves readable history; a failed unlink leaves an unreferenced
	// blob that the existing startup artifact cleanup can safely remove.
	if err := r.removeVersionRecord(v); err != nil {
		return err
	}
	err := r.Files.Remove(".filegate/versions/"+v.ID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
	if r.execution != nil {
		return 0, ErrInvalid
	}
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
