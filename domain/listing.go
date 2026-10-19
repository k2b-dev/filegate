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
	"slices"
	"sort"
	"strings"
	"time"
)

const currentIndexFormat = 2
const listingLifetime = time.Minute
const listingSnapshotLimit = 16
const searchAdmissionWait = 200 * time.Millisecond
const searchScanBudget = 10 * time.Second

var listingMemoryLimit int64 = 32 << 20

var ErrCursorInvalid = errors.New("listing cursor is invalid or expired; restart the query")
var ErrSearchBusy = errors.New("root search capacity exhausted; retry the query")
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
	Match      string `json:"match,omitempty"`
	// In selects search basenames (name, default) or scope-relative paths (path).
	In     string `json:"in,omitempty"`
	topK   int
	search bool
	// Search-only metadata filters. Pointers distinguish an omitted bound from zero.
	Ext            []string   `json:"ext,omitempty"`
	MinSize        *int64     `json:"minSize,omitempty"`
	MaxSize        *int64     `json:"maxSize,omitempty"`
	ModifiedAfter  *time.Time `json:"modifiedAfter,omitempty"`
	ModifiedBefore *time.Time `json:"modifiedBefore,omitempty"`
}

type listingSnapshot struct {
	query   string
	created time.Time
	entries []Node
	bytes   int64
	hits    []searchHit
	total   int
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
	if o.Match == "" {
		o.Match = "exact"
	}
	if o.Match != "exact" && o.Match != "fuzzy" {
		return o, ErrInvalid
	}
	if o.In == "" {
		o.In = "name"
	}
	if o.In != "name" && o.In != "path" {
		return o, ErrInvalid
	}
	if o.Type == "" {
		o.Type = "all"
	}
	if o.Limit < 1 || o.Limit > 1000 || o.MaxEntries < 1 || o.MaxEntries > 100000 || len(o.After) > 16384 {
		return o, ErrInvalid
	}
	if o.Sort != "path" && o.Sort != "name" && o.Sort != "size" && o.Sort != "modified" && o.Sort != "relevance" {
		return o, ErrInvalid
	}
	if o.Order != "asc" && o.Order != "desc" {
		return o, ErrInvalid
	}
	if o.Type != "all" && o.Type != "files" && o.Type != "directories" {
		return o, ErrInvalid
	}
	if o.Sort == "relevance" {
		o.Order = "asc"
	}
	return normalizeSearchFilters(o)
}

func (r *Root) invalidateListings() {
	r.listingEpoch++
	r.listings = nil
	r.listingBytes = 0
}

func queryFingerprint(p, query string, recursive bool, o ListingOptions, actor *ExecutionIdentity) string {
	o.After = ""
	topK := 0
	if o.Sort == "relevance" {
		topK = o.topK
	}
	b, _ := json.Marshal(struct {
		Path, Query string
		Recursive   bool
		Options     ListingOptions
		Execution   *ExecutionIdentity
		TopK        int
	}{p, query, recursive, o, actor, topK})
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
	o.Ext, o.MinSize, o.MaxSize, o.ModifiedAfter, o.ModifiedBefore = nil, nil, nil, nil, nil
	return r.queryListing(ctx, p, "", false, o)
}

// Search selects descendants of base using normalized basename matching,
// or scope-relative path matching with In: "path".
// The raw query must be at most 8 KiB and contain at most eight whitespace tokens.
// Indexed subtree searches with non-path ordering sort a bounded snapshot.
// Execution views scan the live filesystem and bind snapshots to their identity.
func (r *Root) Search(ctx context.Context, query, base string, o ListingOptions) (Page, error) {
	return r.queryListing(ctx, base, query, true, o)
}

// Admission is independent of mu so a live walk holding the root lock cannot
// prevent excess requests from observing the bounded capacity wait. Request
// views share these slots with their service root.
func (r *Root) admitSearch(ctx context.Context) error {
	return r.admitSearchWithin(ctx, searchAdmissionWait)
}

func (r *Root) admitSearchWithin(ctx context.Context, wait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.searchOnce.Do(func() { r.searches = make(chan struct{}, 2) })
	select {
	case r.searches <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r.searches <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrSearchBusy
	}
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
	matcher, err := compileSearchQuery(query, o.In == "path")
	if err != nil {
		return Page{}, err
	}
	matcher.base = p
	o.search = recursive
	matcher.fuzzy = o.Match == "fuzzy"
	if o.Sort == "relevance" && (!recursive || len(matcher.tokens) == 0) {
		return Page{}, ErrInvalid
	}
	o.topK = r.SearchTopK
	if o.topK == 0 {
		o.topK = DefaultSearchTopK
	}
	if o.topK < 1 {
		return Page{}, ErrInvalid
	}
	if recursive {
		if err := r.admitSearch(ctx); err != nil {
			return Page{}, err
		}
		defer func() { <-r.searches }()
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
		if e != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != fingerprint || !r.listingCurrent(cursor) {
			return Page{}, ErrCursorInvalid
		}
	}
	// Live selections check traversal and base directory read permission on every
	// page. Execution views also authorize every returned node and parent below.
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
	matches := func(n Node) nameMatch {
		if n.Path == p {
			return nameMatch{}
		}
		if recursive {
			if p != "." && !strings.HasPrefix(n.Path, p+"/") {
				return nameMatch{}
			}
		} else if path.Dir(n.Path) != p {
			return nameMatch{}
		}
		if o.Type == "files" && n.Directory || o.Type == "directories" && !n.Directory {
			return nameMatch{}
		}
		if recursive && !o.matchesSearchFilters(n) {
			return nameMatch{}
		}
		matched, score := matcher.match(matcher.subject(n.Path))
		if !matched {
			return nameMatch{}
		}
		return score
	}
	var out Page
	if r.Config.Index && r.execution == nil {
		out, err = r.indexedListing(ctx, p, recursive, o, cursor, matches, matcher)
	} else {
		out, err = r.liveListing(ctx, p, recursive, o, cursor, matches, matcher)
	}
	if err != nil {
		return Page{}, err
	}
	if recursive && len(matcher.tokens) > 0 && out.Matches == nil {
		if err := listingHighlights(ctx, &out, matcher); err != nil {
			return Page{}, err
		}
	}
	return out, nil
}

func listingHighlights(ctx context.Context, out *Page, matcher *nameQuery) error {
	ranges := make([][]MatchRange, 0, len(out.Items))
	for _, n := range out.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		ranges = append(ranges, matcher.highlights(matcher.subject(n.Path)))
	}
	out.Matches = &ranges
	return nil
}

func (r *Root) listingCurrent(c listingCursor) bool {
	return c.Instance == r.listingInstance && c.Epoch == r.listingEpoch && c.Generation == r.generation
}

// scanListing releases the lock only for read-only indexed work. The caller
// owns the lock on entry and return. Initial pages keep their captured
// observation; continuation pages must still match the current root state.
func (r *Root) scanListing(ctx context.Context, c listingCursor, continuation bool, scan func() error) error {
	err := func() error {
		r.mu.Unlock()
		defer r.mu.Lock()
		return scan()
	}()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.guard(); err != nil {
		return err
	}
	if continuation && !r.listingCurrent(c) {
		return ErrCursorInvalid
	}
	return nil
}

func directoryIndexPrefix(generation, p, sortBy string) string {
	if sortBy == "name" {
		sortBy = "path"
	}
	return "q/" + generation + "/d/" + hex.EncodeToString([]byte(p)) + "/" + sortBy + "/"
}
func (r *Root) indexedListing(ctx context.Context, p string, recursive bool, o ListingOptions, c listingCursor, matches func(Node) nameMatch, matcher *nameQuery) (out Page, err error) {
	if o.Sort == "relevance" && c.Snapshot != "" {
		return r.snapshotListing(ctx, p, o, c, matches, nil, matcher, nil)
	}
	if o.Sort == "relevance" {
		budget := r.searchScanTimeout
		if budget == 0 {
			budget = searchScanBudget
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, budget, ErrLimit)
		defer cancel()
		defer func() {
			if errors.Is(context.Cause(ctx), ErrLimit) && errors.Is(err, context.DeadlineExceeded) {
				err = ErrLimit
			}
		}()
	}
	state, err := r.State.Snapshot()
	if err != nil {
		return Page{}, err
	}
	defer state.Close()
	if recursive && (o.Sort == "relevance" || o.Match == "fuzzy" || p != "." && o.Sort != "path") {
		return r.snapshotListing(ctx, p, o, c, matches, func(visit func(Node) error) error {
			scanned := 0
			prefix := "i/" + c.Generation + "/"
			if p != "." {
				prefix += p + "/"
			}
			return state.ScanAfter(prefix, "", func(_ string, b []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				scanned++
				if o.Sort != "relevance" && scanned > o.MaxEntries {
					return ErrLimit
				}
				var n Node
				if err := json.Unmarshal(b, &n); err != nil {
					return err
				}
				return visit(n)
			})
		}, matcher, state)
	}
	prefix := "i/" + c.Generation + "/"
	primary := recursive && o.Sort == "path"
	if !recursive {
		prefix = directoryIndexPrefix(c.Generation, p, o.Sort)
	} else if !primary {
		prefix = "q/" + c.Generation + "/a/" + o.Sort + "/"
	} else if p != "." {
		prefix += p + "/"
	}
	if c.Snapshot != "" || c.Offset != 0 || c.Last != "" && !strings.HasPrefix(c.Last, prefix) {
		return Page{}, ErrCursorInvalid
	}
	out = Page{Items: []Node{}}
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
			if err := state.Get("i/"+c.Generation+"/"+p, &n); err != nil {
				return err
			}
		}
		if matches(n).tier != 0 {
			out.Items = append(out.Items, n)
		}
		return nil
	}
	err = r.scanListing(ctx, c, o.After != "", func() error {
		var err error
		if o.Order == "desc" {
			err = state.ScanBefore(prefix, c.Last, visit)
		} else {
			err = state.ScanAfter(prefix, c.Last, visit)
		}
		if errors.Is(err, errListingStop) {
			c.Last = last
			out.Next = encodeListingCursor(c)
			return nil
		}
		return err
	})
	if err != nil {
		return Page{}, err
	}
	return out, nil
}

func (r *Root) liveListing(ctx context.Context, p string, recursive bool, o ListingOptions, c listingCursor, matches func(Node) nameMatch, matcher *nameQuery) (Page, error) {
	return r.snapshotListing(ctx, p, o, c, matches, func(visit func(Node) error) error {
		return r.scanDirectory(ctx, p, recursive, o.MaxEntries, visit)
	}, matcher, nil)
}

func (r *Root) snapshotListing(ctx context.Context, p string, o ListingOptions, c listingCursor, matches func(Node) nameMatch, scan func(func(Node) error) error, matcher *nameQuery, reader StateSnapshot) (Page, error) {
	now := r.now()
	for id, s := range r.listings {
		if !now.Before(s.created.Add(listingLifetime)) {
			r.removeListing(id)
		}
	}
	var snapshot *listingSnapshot
	if c.Snapshot != "" {
		snapshot = r.listings[c.Snapshot]
		if snapshot == nil || snapshot.query != c.Query || c.Last != "" || c.Offset < 0 || c.Offset > snapshot.len() {
			return Page{}, ErrCursorInvalid
		}
	} else {
		if c.Last != "" || c.Offset != 0 || o.After != "" {
			return Page{}, ErrCursorInvalid
		}
		collect := func() (err error) {
			if o.Sort == "relevance" {
				snapshot, err = collectRelevance(ctx, o, c.Query, matches, scan, matcher)
			} else {
				snapshot, err = collectListing(ctx, o, c.Query, matches, scan)
			}
			return err
		}
		var err error
		if r.Config.Index && r.execution == nil {
			err = r.scanListing(ctx, c, false, collect)
		} else {
			// Live metadata collection can assign identities and revisions. Keep
			// it serialized with publication and execution permission rechecks.
			err = collect()
		}
		if err != nil {
			return Page{}, err
		}
		// The lease starts after collection and sorting; slow filesystems must
		// not consume the usable lifetime before the first page is returned.
		snapshot.created = r.now()
	}
	end := min(c.Offset+o.Limit, snapshot.len())
	out := Page{Items: []Node{}}
	if o.Sort == "relevance" {
		ranges := make([][]MatchRange, 0, end-c.Offset)
		for _, h := range snapshot.hits[c.Offset:end] {
			if err := ctx.Err(); err != nil {
				return Page{}, err
			}
			var n Node
			var err error
			if reader != nil {
				err = reader.Get("i/"+c.Generation+"/"+h.path, &n)
			} else if r.Config.Index && r.execution == nil {
				// Reload retained hits from the current primary rows.
				err = r.State.Get("i/"+c.Generation+"/"+h.path, &n)
			} else {
				n, err = r.node(h.path, true)
			}
			if errors.Is(err, os.ErrNotExist) {
				return Page{}, ErrCursorInvalid
			}
			if err != nil {
				return Page{}, err
			}
			if n.Directory != h.directory || o.Type == "files" && n.Directory || o.Type == "directories" && !n.Directory || !o.matchesSearchFilters(n) {
				return Page{}, ErrCursorInvalid
			}
			out.Items = append(out.Items, n)
			ranges = append(ranges, slices.Clone(h.ranges))
		}
		out.Matches = &ranges
	} else {
		out.Items = append(out.Items, snapshot.entries[c.Offset:end]...)
	}
	// Directories retain their existing envelope; only search observations have totals.
	if o.search {
		total := snapshot.total
		truncated := total > snapshot.len()
		out.Total = &total
		out.Truncated = &truncated
	}
	if r.execution != nil {
		if err := r.checkListingAccess(ctx, p, out.Items); err != nil {
			if o.Sort == "relevance" && errors.Is(err, os.ErrNotExist) {
				return Page{}, ErrCursorInvalid
			}
			return Page{}, err
		}
	}
	if o.search && len(matcher.tokens) > 0 && out.Matches == nil {
		if err := listingHighlights(ctx, &out, matcher); err != nil {
			return Page{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	// Admit only a successful first page, using the final retained size after
	// collection and only if the captured observation is still current.
	if c.Snapshot == "" && snapshot.len() > o.Limit {
		c.Snapshot = newID()
		if r.listingCurrent(c) {
			if r.listings == nil {
				r.listings = make(map[string]*listingSnapshot)
			}
			for len(r.listings) >= listingSnapshotLimit || r.listingBytes+snapshot.bytes > listingMemoryLimit {
				r.evictListing()
			}
			r.listings[c.Snapshot] = snapshot
			r.listingBytes += snapshot.bytes
		}
	}
	if end < snapshot.len() {
		c.Offset = end
		out.Next = encodeListingCursor(c)
	}
	return out, nil
}

// Collection and sorting touch request-local data only. The per-root cache is
// updated after collection, under mu, so concurrent scans cannot race eviction.
// Admission bounds indexed listing data per root to the 32 MiB cache plus two
// in-flight collections of at most 32 MiB each, including provisional loose hits.
func collectListing(ctx context.Context, o ListingOptions, query string, matches func(Node) nameMatch, scan func(func(Node) error) error) (*listingSnapshot, error) {
	snapshot := &listingSnapshot{query: query, entries: []Node{}}
	strong := false
	looseOverflow := false
	err := scan(func(n Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tier := matches(n).tier
		if tier == 0 || tier == nameLoose && (strong || looseOverflow) {
			return nil
		}
		// Once any exact/typo hit exists, loose hits cannot enter this
		// selection. Drop earlier loose hits before sorting or pagination.
		if !strong && tier < nameLoose {
			strong = true
			snapshot.entries = nil // Release discarded nodes and their backing array.
			snapshot.bytes = 0
		}
		encoded, e := json.Marshal(n)
		if e != nil {
			return e
		}
		snapshot.bytes += int64(len(encoded) + 128)
		if snapshot.bytes > listingMemoryLimit {
			if strong {
				return ErrLimit
			}
			// Loose hits are provisional: a later exact/typo hit can
			// suppress them all. Release storage but finish the scan.
			looseOverflow = true
			snapshot.entries = nil
			snapshot.bytes = 0
			return nil
		}
		snapshot.entries = append(snapshot.entries, n)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if looseOverflow && !strong {
		return nil, ErrLimit
	}
	sort.Slice(snapshot.entries, func(i, j int) bool {
		a, b := snapshot.entries[i], snapshot.entries[j]
		comparison := compareListing(a, b, o.Sort)
		if o.Order == "desc" {
			return comparison > 0
		}
		return comparison < 0
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot.total = len(snapshot.entries)
	return snapshot, nil
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
				if err := ctx.Err(); err != nil {
					return err
				}
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
		r.removeListing(oldest)
	}
}

// Recheck only returned nodes and their ancestor directories, without scanning
// their children again. Stat enforces search permission on every ancestor;
// opening each distinct parent additionally enforces directory read permission.
// Leaf read permission is intentionally separate: listing names and metadata
// does not authorize reading the file's bytes.
func (r *Root) checkListingAccess(ctx context.Context, base string, nodes []Node) error {
	checked := map[string]bool{base: true}
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := r.Files.Stat(n.Path); err != nil {
			return err
		}
		for parent := path.Dir(n.Path); !checked[parent]; parent = path.Dir(parent) {
			if err := ctx.Err(); err != nil {
				return err
			}
			d, err := r.Files.Open(parent, os.O_RDONLY, 0)
			if err != nil {
				return err
			}
			st, err := d.Stat()
			d.Close()
			if err != nil {
				return err
			}
			if !st.IsDir() {
				return ErrConflict
			}
			checked[parent] = true
			if parent == "." {
				break
			}
		}
	}
	return nil
}

func (s *listingSnapshot) len() int {
	if s.hits != nil {
		return len(s.hits)
	}
	return len(s.entries)
}
func (r *Root) removeListing(id string) {
	r.listingBytes -= r.listings[id].bytes
	delete(r.listings, id)
}
