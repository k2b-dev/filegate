package domain

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const currentIndexFormat = 2
const listingLifetime = time.Minute
const listingSnapshotLimit = 16
const listingMemoryLimit int64 = 32 << 20

var ErrCursorInvalid = errors.New("listing cursor is invalid or expired; restart the query")
var errListingStop = errors.New("listing complete")

// ListingOptions applies ordering and filtering to the complete selection before
// pagination. After is an opaque cursor bound to every normalized query option.
type ListingOptions struct {
	After      string `json:"after,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	MaxEntries int    `json:"maxEntries,omitempty"`
	Sort       string `json:"sort,omitempty"`
	Order      string `json:"order,omitempty"`
	Type       string `json:"type,omitempty"`
}

type listingSnapshot struct {
	query   string
	created time.Time
	entries []Node
	bytes   int64
}
type listingCursor struct {
	Query      string `json:"q"`
	Instance   string `json:"i"`
	Epoch      uint64 `json:"e"`
	Generation string `json:"g,omitempty"`
	Last       string `json:"k,omitempty"`
	Snapshot   string `json:"s,omitempty"`
	Offset     int    `json:"o,omitempty"`
}

func normalizeListing(o ListingOptions) (ListingOptions, error) {
	if o.Limit == 0 {
		o.Limit = 100
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = 100000
	}
	if o.Sort == "" {
		o.Sort = "path"
	}
	if o.Order == "" {
		o.Order = "asc"
	}
	if o.Type == "" {
		o.Type = "all"
	}
	if o.Limit < 1 || o.Limit > 1000 || o.MaxEntries < 1 || o.MaxEntries > 100000 || len(o.After) > 16384 {
		return o, ErrInvalid
	}
	if o.Sort != "path" && o.Sort != "name" && o.Sort != "size" && o.Sort != "modified" {
		return o, ErrInvalid
	}
	if o.Order != "asc" && o.Order != "desc" {
		return o, ErrInvalid
	}
	if o.Type != "all" && o.Type != "files" && o.Type != "directories" {
		return o, ErrInvalid
	}
	return o, nil
}

func (r *Root) invalidateListings() {
	r.listingEpoch++
	r.listings = nil
	r.listingBytes = 0
}

func queryFingerprint(p, query string, recursive bool, o ListingOptions, actor *ExecutionIdentity) string {
	o.After = ""
	b, _ := json.Marshal(struct {
		Path, Query string
		Recursive   bool
		Options     ListingOptions
		Execution   *ExecutionIdentity
	}{p, query, recursive, o, actor})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func encodeListingCursor(c listingCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// List selects only the immediate children of p. Actor views use a live
// filesystem snapshot so indexed metadata never bypasses directory permissions.
func (r *Root) List(ctx context.Context, p string, o ListingOptions) (Page, error) {
	return r.queryListing(ctx, p, "", false, o)
}

// Search selects descendants of base, matching the case-insensitive basename.
// Indexed scans seek to their cursor; live queries scan and sort once per cursor.
func (r *Root) Search(ctx context.Context, query, base string, o ListingOptions) (Page, error) {
	if r.execution != nil {
		return Page{}, ErrInvalid
	}
	return r.queryListing(ctx, base, strings.ToLower(query), true, o)
}

func (r *Root) queryListing(ctx context.Context, p, query string, recursive bool, o ListingOptions) (Page, error) {
	p, err := CleanPath(p)
	if err != nil {
		return Page{}, err
	}
	o, err = normalizeListing(o)
	if err != nil {
		return Page{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if r.listingInstance == "" {
		r.listingInstance = newID()
	}
	fingerprint := queryFingerprint(p, query, recursive, o, r.Execution())
	cursor := listingCursor{Query: fingerprint, Instance: r.listingInstance, Epoch: r.listingEpoch, Generation: r.generation}
	if o.After != "" {
		raw, e := base64.RawURLEncoding.DecodeString(o.After)
		if e != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != fingerprint || cursor.Instance != r.listingInstance || cursor.Epoch != r.listingEpoch || cursor.Generation != r.generation {
			return Page{}, ErrCursorInvalid
		}
	}
	// Search remains an administrative index operation; live/actor directory
	// listings always check current traversal and directory read permissions.
	if !r.Config.Index || r.execution != nil || !recursive {
		d, e := r.Files.Open(p, os.O_RDONLY, 0)
		if e != nil {
			return Page{}, e
		}
		st, e := d.Stat()
		d.Close()
		if e != nil {
			return Page{}, e
		}
		if !st.IsDir() {
			return Page{}, ErrInvalid
		}
	}
	matches := func(n Node) bool {
		if n.Path == p {
			return false
		}
		if recursive {
			if p != "." && !strings.HasPrefix(n.Path, p+"/") {
				return false
			}
		} else if path.Dir(n.Path) != p {
			return false
		}
		if o.Type == "files" && n.Directory || o.Type == "directories" && !n.Directory {
			return false
		}
		return strings.Contains(strings.ToLower(path.Base(n.Path)), query)
	}
	if r.Config.Index && r.execution == nil {
		return r.indexedListing(ctx, p, recursive, o, cursor, matches)
	}
	return r.liveListing(ctx, p, recursive, o, cursor, matches)
}

func directoryIndexPrefix(generation, p, sortBy string) string {
	if sortBy == "name" {
		sortBy = "path"
	}
	return "q/" + generation + "/d/" + hex.EncodeToString([]byte(p)) + "/" + sortBy + "/"
}
func (r *Root) indexedListing(ctx context.Context, p string, recursive bool, o ListingOptions, c listingCursor, matches func(Node) bool) (Page, error) {
	prefix := "i/" + r.generation + "/"
	primary := recursive && o.Sort == "path"
	if !recursive {
		prefix = directoryIndexPrefix(r.generation, p, o.Sort)
	} else if !primary {
		prefix = "q/" + r.generation + "/a/" + o.Sort + "/"
	} else if p != "." {
		prefix += p + "/"
	}
	if c.Snapshot != "" || c.Offset != 0 || c.Last != "" && !strings.HasPrefix(c.Last, prefix) {
		return Page{}, ErrCursorInvalid
	}
	out := Page{Items: []Node{}}
	scanned := 0
	last := c.Last
	visit := func(k string, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scanned >= o.MaxEntries || len(out.Items) >= o.Limit {
			return errListingStop
		}
		scanned++
		last = k
		var n Node
		if primary {
			if err := json.Unmarshal(b, &n); err != nil {
				return err
			}
		} else {
			var p string
			if err := json.Unmarshal(b, &p); err != nil {
				return err
			}
			if err := r.State.Get("i/"+r.generation+"/"+p, &n); err != nil {
				return err
			}
		}
		if matches(n) {
			out.Items = append(out.Items, n)
		}
		return nil
	}
	var err error
	if o.Order == "desc" {
		err = r.State.ScanBefore(prefix, c.Last, visit)
	} else {
		err = r.State.ScanAfter(prefix, c.Last, visit)
	}
	if errors.Is(err, errListingStop) {
		c.Last = last
		out.Next = encodeListingCursor(c)
		err = nil
	}
	return out, err
}

func (r *Root) liveListing(ctx context.Context, p string, recursive bool, o ListingOptions, c listingCursor, matches func(Node) bool) (Page, error) {
	now := r.now()
	for id, s := range r.listings {
		if !now.Before(s.created.Add(listingLifetime)) {
			delete(r.listings, id)
			r.listingBytes -= s.bytes
		}
	}
	var snapshot *listingSnapshot
	if c.Snapshot != "" {
		snapshot = r.listings[c.Snapshot]
		if snapshot == nil || snapshot.query != c.Query || c.Last != "" || c.Offset < 0 || c.Offset > len(snapshot.entries) {
			return Page{}, ErrCursorInvalid
		}
	} else {
		if c.Last != "" || c.Offset != 0 || o.After != "" {
			return Page{}, ErrCursorInvalid
		}
		snapshot = &listingSnapshot{query: c.Query, entries: []Node{}}
		err := r.scanDirectory(ctx, p, recursive, o.MaxEntries, func(n Node) error {
			if !matches(n) {
				return nil
			}
			encoded, e := json.Marshal(n)
			if e != nil {
				return e
			}
			snapshot.bytes += int64(len(encoded) + 128)
			if snapshot.bytes > listingMemoryLimit {
				return ErrLimit
			}
			for r.listingBytes+snapshot.bytes > listingMemoryLimit {
				r.evictListing()
			}
			snapshot.entries = append(snapshot.entries, n)
			return nil
		})
		if err != nil {
			return Page{}, err
		}
		sort.Slice(snapshot.entries, func(i, j int) bool {
			a, b := snapshot.entries[i], snapshot.entries[j]
			comparison := compareListing(a, b, o.Sort)
			if o.Order == "desc" {
				return comparison > 0
			}
			return comparison < 0
		})
		// The lease starts after collection and sorting; slow filesystems must
		// not consume the usable lifetime before the first page is returned.
		snapshot.created = r.now()
		if len(snapshot.entries) > o.Limit {
			if r.listings == nil {
				r.listings = make(map[string]*listingSnapshot)
			}
			for len(r.listings) >= listingSnapshotLimit {
				r.evictListing()
			}
			c.Snapshot = newID()
			r.listings[c.Snapshot] = snapshot
			r.listingBytes += snapshot.bytes
		}
	}
	end := min(c.Offset+o.Limit, len(snapshot.entries))
	out := Page{Items: append([]Node{}, snapshot.entries[c.Offset:end]...)}
	if end < len(snapshot.entries) {
		c.Offset = end
		out.Next = encodeListingCursor(c)
	}
	return out, nil
}

func compareListing(a, b Node, sortBy string) int {
	switch sortBy {
	case "name":
		if c := strings.Compare(path.Base(a.Path), path.Base(b.Path)); c != 0 {
			return c
		}
	case "size":
		if a.Size < b.Size {
			return -1
		}
		if a.Size > b.Size {
			return 1
		}
	case "modified":
		if a.Modified.Before(b.Modified) {
			return -1
		}
		if a.Modified.After(b.Modified) {
			return 1
		}
	}
	return strings.Compare(a.Path, b.Path)
}

func orderedNodeKey(n Node, sortBy string) string {
	switch sortBy {
	case "name":
		return path.Base(n.Path) + "\x00" + n.Path
	case "size":
		return fmt.Sprintf("%016x", uint64(n.Size)^(uint64(1)<<63)) + "/" + n.Path
	case "modified":
		return fmt.Sprintf("%016x%08x", uint64(n.Modified.Unix())^(uint64(1)<<63), uint32(n.Modified.Nanosecond())) + "/" + n.Path
	default:
		return n.Path
	}
}
func nodeIndexKeys(generation string, n Node) []string {
	keys := []string{"i/" + generation + "/" + n.Path}
	if n.Path == "." {
		return keys
	}
	for _, sortBy := range []string{"name", "size", "modified"} {
		keys = append(keys, "q/"+generation+"/a/"+sortBy+"/"+orderedNodeKey(n, sortBy))
	}
	for _, sortBy := range []string{"path", "size", "modified"} {
		keys = append(keys, directoryIndexPrefix(generation, path.Dir(n.Path), sortBy)+orderedNodeKey(n, sortBy))
	}
	return keys
}
func indexChangesFor(generation string, n Node, previous *Node) ([]Change, error) {
	cs := []Change{}
	if previous != nil {
		for _, key := range nodeIndexKeys(generation, *previous) {
			cs = append(cs, Change{Key: key, Delete: true})
		}
	}
	for i, key := range nodeIndexKeys(generation, n) {
		var value any = n.Path
		if i == 0 {
			value = n
		}
		change, err := encoded(key, value)
		if err != nil {
			return nil, err
		}
		cs = append(cs, change)
	}
	return cs, nil
}
func (r *Root) indexChanges(n Node, previous *Node) ([]Change, error) {
	return indexChangesFor(r.generation, n, previous)
}
func (r *Root) removeIndexChanges(n Node) []Change {
	cs := []Change{}
	for _, key := range nodeIndexKeys(r.generation, n) {
		cs = append(cs, Change{Key: key, Delete: true})
	}
	return cs
}

// scanDirectory bounds work before opening a child. Symlinks and private names
// consume the scan budget but are never followed or exposed.
func (r *Root) scanDirectory(ctx context.Context, p string, recursive bool, maxEntries int, visit func(Node) error) error {
	scanned := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > 128 {
			return ErrLimit
		}
		d, err := r.Files.Open(dir, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer d.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := d.ReadDir(256)
			for _, entry := range entries {
				scanned++
				if scanned > maxEntries {
					return ErrLimit
				}
				child := path.Join(dir, entry.Name())
				if _, e := CleanPath(child); e != nil || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				n, e := r.node(child, true)
				if e != nil {
					return e
				}
				if e := visit(n); e != nil {
					return e
				}
				if recursive && n.Directory {
					if e := walk(child, depth+1); e != nil {
						return e
					}
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	return walk(p, 0)
}

func (r *Root) evictListing() {
	oldest := ""
	for id, s := range r.listings {
		if oldest == "" || s.created.Before(r.listings[oldest].created) || s.created.Equal(r.listings[oldest].created) && id < oldest {
			oldest = id
		}
	}
	if oldest != "" {
		r.listingBytes -= r.listings[oldest].bytes
		delete(r.listings, oldest)
	}
}
