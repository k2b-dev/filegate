package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type listingState struct {
	State
	rows    map[string][]byte
	keys    []string
	visits  int
	scans   int
	closed  int
	onGet   func(string) error
	onScan  func()
	onClose func()
}

// These fixtures only mutate rows between requests. Production snapshot
// isolation and concurrent mutations are covered with Pebble in search tests.
type listingReadSnapshot struct{ *listingState }

func (s *listingState) Snapshot() (StateSnapshot, error) {
	return &listingReadSnapshot{s}, nil
}
func (s *listingReadSnapshot) Close() error {
	if s.onClose != nil {
		s.onClose()
	}
	s.closed++
	return nil
}

func (s *listingState) Get(key string, out any) error {
	if s.onGet != nil {
		if err := s.onGet(key); err != nil {
			return err
		}
	}
	b, ok := s.rows[key]
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal(b, out)
}
func (s *listingState) Batch(cs []Change) error {
	if s.rows == nil {
		s.rows = map[string][]byte{}
	}
	for _, c := range cs {
		if c.Delete {
			delete(s.rows, c.Key)
		} else {
			s.rows[c.Key] = append([]byte{}, c.Value...)
		}
	}
	s.keys = s.keys[:0]
	for key := range s.rows {
		s.keys = append(s.keys, key)
	}
	sort.Strings(s.keys)
	return nil
}
func (s *listingState) ScanAfter(prefix, after string, fn func(string, []byte) error) error {
	if s.onScan != nil {
		s.onScan()
	}
	s.scans++
	start := sort.SearchStrings(s.keys, prefix)
	if after != "" {
		start = sort.Search(len(s.keys), func(i int) bool { return s.keys[i] > after })
	}
	for i := start; i < len(s.keys) && strings.HasPrefix(s.keys[i], prefix); i++ {
		s.visits++
		if err := fn(s.keys[i], s.rows[s.keys[i]]); err != nil {
			return err
		}
	}
	return nil
}
func (s *listingState) ScanBefore(prefix, before string, fn func(string, []byte) error) error {
	s.scans++
	end := sort.SearchStrings(s.keys, prefix+"\xff") - 1
	if before != "" {
		end = sort.SearchStrings(s.keys, before) - 1
	}
	for i := end; i >= 0 && strings.HasPrefix(s.keys[i], prefix); i-- {
		s.visits++
		if err := fn(s.keys[i], s.rows[s.keys[i]]); err != nil {
			return err
		}
	}
	return nil
}
func indexedListingRoot(t *testing.T, nodes []Node) *Root {
	t.Helper()
	state := &listingState{}
	root := &Root{Config: RootConfig{Name: "test", Index: true}, State: state, rootShared: &rootShared{generation: "generation", now: time.Now}}
	var changes []Change
	for _, node := range nodes {
		cs, err := root.indexChanges(node, nil)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, cs...)
	}
	if err := state.Batch(changes); err != nil {
		t.Fatal(err)
	}
	return root
}
func collectSearch(t *testing.T, r *Root, query, base string, opts ListingOptions) []Node {
	t.Helper()
	var result []Node
	for pages := 0; pages < 100000; pages++ {
		page, err := r.Search(context.Background(), query, base, opts)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, page.Items...)
		if page.Next == "" {
			return result
		}
		opts.After = page.Next
	}
	t.Fatal("pagination did not terminate")
	return nil
}
func TestIndexedOrderingFilteringAndContinuation(t *testing.T) {
	now := time.Now().UTC()
	nodes := []Node{
		{Path: "one/a", Size: 2, Modified: now},
		{Path: "two/a", Size: 2, Modified: now},
		{Path: "one/z", Size: 1, Modified: now.Add(-time.Second)},
		{Path: "one/sub", Directory: true, Modified: now},
		{Path: "outside", Size: 0, Modified: now},
	}
	r := indexedListingRoot(t, nodes)
	for _, tc := range []struct {
		sort, order, base, kind string
		want                    []string
	}{
		{"size", "asc", ".", "files", []string{"outside", "one/z", "one/a", "two/a"}},
		{"size", "desc", ".", "files", []string{"two/a", "one/a", "one/z", "outside"}},
		{"name", "asc", ".", "files", []string{"one/a", "two/a", "outside", "one/z"}},
		{"modified", "asc", ".", "files", []string{"one/z", "one/a", "outside", "two/a"}},
		{"path", "asc", "one", "all", []string{"one/a", "one/sub", "one/z"}},
		{"path", "desc", "one", "directories", []string{"one/sub"}},
	} {
		got := collectSearch(t, r, "", tc.base, ListingOptions{Sort: tc.sort, Order: tc.order, Type: tc.kind, Limit: 1, MaxEntries: 1})
		paths := []string{}
		for _, n := range got {
			paths = append(paths, n.Path)
		}
		if strings.Join(paths, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%+v: %v", tc, paths)
		}
	}
	// Sparse filters may produce an empty page with a continuation. Every scanned
	// row advances the cursor, so completing a sparse query never starts over.
	state := r.State.(*listingState)
	state.visits = 0
	got := collectSearch(t, r, "z", ".", ListingOptions{Limit: 1, MaxEntries: 1})
	if len(got) != 1 || got[0].Path != "one/z" || state.visits > 2*len(nodes) {
		t.Fatalf("sparse result=%v visits=%d", got, state.visits)
	}
}

func TestIndexedSubtreeSearchFindsFirstPage(t *testing.T) {
	nodes := []Node{{Path: "fresh", Directory: true}, {Path: "fresh/needle.txt"}}
	for i := 0; i < 129; i++ {
		nodes = append(nodes, Node{Path: fmt.Sprintf("outside/a-%03d", i)})
	}
	r := indexedListingRoot(t, nodes)
	page, err := r.Search(context.Background(), "needle", "fresh", ListingOptions{Sort: "name", MaxEntries: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "fresh/needle.txt" || page.Next != "" {
		t.Fatalf("subtree match missing from first page: %+v", page)
	}
	if state := r.State.(*listingState); state.visits != 1 {
		t.Fatalf("scanned outside the subtree: %d visits", state.visits)
	}
}

func TestIndexedSubtreeSearchSortsOnce(t *testing.T) {
	now := time.Unix(100000, 0)
	nodes := []Node{
		{Path: "fresh/a", Size: 2, Modified: now},
		{Path: "fresh/sub/a", Size: 2, Modified: now},
		{Path: "fresh/z", Size: 1, Modified: now.Add(-time.Second)},
		{Path: "fresh/sub", Directory: true, Modified: now},
		{Path: "fresh-other/a"},
	}
	for _, tc := range []struct {
		sort, order string
		want        string
	}{
		{"name", "asc", "fresh/a|fresh/sub/a|fresh/z"},
		{"name", "desc", "fresh/z|fresh/sub/a|fresh/a"},
		{"size", "asc", "fresh/z|fresh/a|fresh/sub/a"},
		{"size", "desc", "fresh/sub/a|fresh/a|fresh/z"},
		{"modified", "asc", "fresh/z|fresh/a|fresh/sub/a"},
		{"modified", "desc", "fresh/sub/a|fresh/a|fresh/z"},
	} {
		t.Run(tc.sort+"/"+tc.order, func(t *testing.T) {
			r := indexedListingRoot(t, nodes)
			opts := ListingOptions{Sort: tc.sort, Order: tc.order, Type: "files", Limit: 1, MaxEntries: 4}
			var paths []string
			for i := 0; i < 3; i++ {
				page, err := r.Search(context.Background(), "", "fresh", opts)
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("page %d: %+v %v", i, page, err)
				}
				paths = append(paths, page.Items[0].Path)
				if (page.Next != "") != (i < 2) {
					t.Fatalf("page %d continuation: %+v", i, page)
				}
				opts.After = page.Next
			}
			if strings.Join(paths, "|") != tc.want {
				t.Fatalf("got %v, want %s", paths, tc.want)
			}
			if state := r.State.(*listingState); state.scans != 1 || state.visits != 4 {
				t.Fatalf("rescanned or left subtree: scans=%d visits=%d", state.scans, state.visits)
			}
		})
	}
}

func TestIndexedSubtreeSearchRespectsRetainedBudget(t *testing.T) {
	r := indexedListingRoot(t, []Node{
		{Path: "fresh/a", Directory: true},
		{Path: "fresh/needle"},
		{Path: "fresh/z"},
	})
	opts := ListingOptions{Sort: "name", Type: "files", Limit: 1, MaxEntries: 2}
	page, err := r.Search(context.Background(), "needle", "fresh", opts)
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "fresh/needle" {
		t.Fatalf("nonmatching rows consumed retained budget: %+v %v", page, err)
	}
	if _, err := r.Search(context.Background(), "", "fresh", ListingOptions{Sort: "name", MaxEntries: 1}); !errors.Is(err, ErrLimit) {
		t.Fatalf("retained budget not enforced: %v", err)
	}
	page, err = r.Search(context.Background(), "missing", "fresh", opts)
	if err != nil || len(page.Items) != 0 || page.Next != "" {
		t.Fatalf("empty selection has continuation: %+v %v", page, err)
	}
}

func TestIndexedSubtreeSearchCursorInvalidation(t *testing.T) {
	for _, change := range []string{"query", "options", "generation", "mutation", "expiry", "eviction"} {
		t.Run(change, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "fresh/a"}, {Path: "fresh/b"}})
			now := time.Unix(100000, 0)
			r.now = func() time.Time { return now }
			opts := ListingOptions{Sort: "name", Limit: 1, MaxEntries: 2}
			page, err := r.Search(context.Background(), "", "fresh", opts)
			if err != nil || page.Next == "" {
				t.Fatalf("first page: %+v %v", page, err)
			}
			opts.After = page.Next
			query := ""
			switch change {
			case "query":
				query = "b"
			case "options":
				opts.Order = "desc"
			case "generation":
				r.generation = "rebuilt"
			case "mutation":
				r.invalidateListings()
			case "expiry":
				now = now.Add(listingLifetime)
			case "eviction":
				r.evictListing()
			}
			if _, err := r.Search(context.Background(), query, "fresh", opts); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("cursor accepted %s: %v", change, err)
			}
		})
	}
}

func TestIndexedSubtreeSearchDoesNotCacheStaleObservation(t *testing.T) {
	for _, route := range []struct{ mode, base, sort string }{{"exact", "fresh", "name"}, {"fuzzy", "fresh", "name"}, {"fuzzy", ".", "path"}} {
		mode := route.mode
		for _, change := range []string{"mutation", "generation", "instance"} {
			t.Run(mode+"/"+route.base+"/"+change, func(t *testing.T) {
				r := indexedListingRoot(t, []Node{{Path: "fresh/a"}, {Path: "fresh/b"}})
				r.listingInstance = "instance"
				opts, err := normalizeListing(ListingOptions{Match: mode, Sort: route.sort, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				captured := listingCursor{
					Query:    queryFingerprint(route.base, "", true, opts, nil),
					Instance: r.listingInstance, Epoch: r.listingEpoch, Generation: r.generation,
				}
				r.State.(*listingState).onScan = func() {
					r.mu.Lock()
					defer r.mu.Unlock()
					switch change {
					case "mutation":
						r.invalidateListings()
					case "generation":
						r.generation = "rebuilt"
					case "instance":
						r.listingInstance = "restarted"
					}
				}
				page, err := r.Search(context.Background(), "", route.base, opts)
				if err != nil || len(page.Items) != 1 || page.Items[0].Path != "fresh/a" || page.Next == "" {
					t.Fatalf("initial snapshot page: %+v %v", page, err)
				}
				if len(r.listings) != 0 || r.listingBytes != 0 {
					t.Fatalf("stale observation cached: entries=%d bytes=%d", len(r.listings), r.listingBytes)
				}
				raw, err := base64.RawURLEncoding.DecodeString(page.Next)
				var cursor listingCursor
				if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Query != captured.Query || cursor.Instance != captured.Instance || cursor.Epoch != captured.Epoch || cursor.Generation != captured.Generation || cursor.Snapshot == "" {
					t.Fatalf("cursor lost captured binding: %+v %v", cursor, err)
				}
				opts.After = page.Next
				if _, err := r.Search(context.Background(), "", route.base, opts); !errors.Is(err, ErrCursorInvalid) {
					t.Fatalf("stale cursor accepted: %v", err)
				}
			})
		}
	}
}

func TestIndexSortReferencesReplacedAtomically(t *testing.T) {
	old := Node{Path: "a", Size: 99, Modified: time.Unix(0, 0)}
	r := indexedListingRoot(t, []Node{old, {Path: "b", Size: 10, Modified: time.Unix(1, 0)}})
	fresh := old
	fresh.Size = 1
	fresh.Modified = time.Unix(2, 0)
	changes, err := r.indexChanges(fresh, &old)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 14 {
		t.Fatalf("index overwrite writes unbounded/redundant rows: %d", len(changes))
	}
	if err := r.State.Batch(changes); err != nil {
		t.Fatal(err)
	}
	got := collectSearch(t, r, "", ".", ListingOptions{Sort: "size", Limit: 1})
	if len(got) != 2 || got[0].Size != 1 || got[0].Path != "a" {
		t.Fatal(got)
	}
	if err := r.State.Batch(r.removeIndexChanges(fresh)); err != nil {
		t.Fatal(err)
	}
	got = collectSearch(t, r, "", ".", ListingOptions{Sort: "modified", Limit: 1})
	if len(got) != 1 || got[0].Path != "b" {
		t.Fatal(got)
	}
}
func TestIndexedQueryCursorRejectsChanges(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a"}, {Path: "b"}})
	page, err := r.Search(context.Background(), "", ".", ListingOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Search(context.Background(), "b", ".", ListingOptions{Limit: 1, After: page.Next}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("cursor accepted changed query: %v", err)
	}
	r.invalidateListings()
	if _, err = r.Search(context.Background(), "", ".", ListingOptions{Limit: 1, After: page.Next}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("cursor accepted mutation: %v", err)
	}
}
func TestIndexedPaginationLinearAtScale(t *testing.T) {
	for _, size := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			state := &listingState{}
			r := &Root{Config: RootConfig{Index: true}, State: state, rootShared: &rootShared{generation: "g", now: time.Now}}
			changes := make([]Change, 0, size)
			for i := 0; i < size; i++ {
				n := Node{Path: fmt.Sprintf("file-%08d", i)}
				c, _ := encoded("i/g/"+n.Path, n)
				changes = append(changes, c)
			}
			if err := state.Batch(changes); err != nil {
				t.Fatal(err)
			}
			got := collectSearch(t, r, "", ".", ListingOptions{Limit: 100})
			if len(got) != size || state.visits > size+size/100 {
				t.Fatalf("rows=%d results=%d visits=%d", size, len(got), state.visits)
			}
			t.Logf("rows=%d visited=%d pages=%d", size, state.visits, state.scans)
		})
	}
}

type listingFiles struct {
	Files
	base  string
	opens int
}

func (f *listingFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	f.opens++
	return os.OpenFile(filepath.Join(f.base, p), flags, mode)
}
func (f *listingFiles) Stat(p string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(f.base, p))
}
func (f *listingFiles) Identity(os.FileInfo) (uint64, uint64, uint32, uint32, uint64) {
	return 1, 1, 1, 1, 1
}
func liveListingRoot(t *testing.T) (*Root, *listingFiles, *time.Time) {
	t.Helper()
	files := &listingFiles{base: t.TempDir()}
	now := time.Unix(100000, 0)
	r := &Root{Config: RootConfig{Name: "test"}, Files: files, rootShared: &rootShared{now: func() time.Time { return now }}}
	for name, content := range map[string]string{"a": "xx", "m": "xx", "z": "x"} {
		if err := os.WriteFile(filepath.Join(files.base, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(files.base, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files.base, "dir", "nested"), []byte("deep"), 0600); err != nil {
		t.Fatal(err)
	}
	return r, files, &now
}
func TestLiveListingSortsOnceAndBindsCursor(t *testing.T) {
	r, files, now := liveListingRoot(t)
	opts := ListingOptions{Sort: "size", Type: "files", Limit: 1}
	page, err := r.List(context.Background(), ".", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "z" {
		t.Fatal(page)
	}
	firstOpens := files.opens
	opts.After = page.Next
	second, err := r.List(context.Background(), ".", opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Items[0].Path != "a" || files.opens-firstOpens != 1 {
		t.Fatalf("rescanned live directory: page=%+v extra opens=%d", second, files.opens-firstOpens)
	}
	changed := opts
	changed.Type = "directories"
	if _, err := r.List(context.Background(), ".", changed); !errors.Is(err, ErrCursorInvalid) {
		t.Fatal(err)
	}
	*now = now.Add(listingLifetime)
	if _, err := r.List(context.Background(), ".", opts); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("expired cursor accepted: %v", err)
	}
}
func TestLiveSearchAndStatisticsRespectBounds(t *testing.T) {
	r, _, _ := liveListingRoot(t)
	if _, err := r.Search(context.Background(), "", ".", ListingOptions{MaxEntries: 1}); !errors.Is(err, ErrLimit) {
		t.Fatalf("partial sorted result returned: %v", err)
	}
	result := collectSearch(t, r, "nested", ".", ListingOptions{Limit: 1})
	if len(result) != 1 || result[0].Path != "dir/nested" {
		t.Fatal(result)
	}
	partial, err := r.RecursiveStats(context.Background(), "dir", 1)
	if err != nil || partial.Complete || partial.Source != "filesystem" || partial.Path != "dir" {
		t.Fatalf("partial statistics: %+v %v", partial, err)
	}
	complete, err := r.RecursiveStats(context.Background(), "dir", 100)
	if err != nil || !complete.Complete || complete.Files != 1 || complete.Directories != 1 || complete.Bytes != 4 {
		t.Fatalf("statistics: %+v %v", complete, err)
	}
}

func TestLiveSnapshotBudgetAndExecutionBinding(t *testing.T) {
	r, _, _ := liveListingRoot(t)
	// Each query is distinct but selects the same files; cached selections never
	// grow beyond the configured root budget.
	first, err := r.List(context.Background(), ".", ListingOptions{Limit: 1, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < listingSnapshotLimit; i++ {
		if _, err := r.List(context.Background(), ".", ListingOptions{Limit: 1, MaxEntries: 101 + i}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.listings) > listingSnapshotLimit || r.listingBytes > listingMemoryLimit {
		t.Fatalf("snapshot budgets exceeded: %d %d", len(r.listings), r.listingBytes)
	}
	if _, err := r.List(context.Background(), ".", ListingOptions{Limit: 1, MaxEntries: 100, After: first.Next}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("evicted cursor accepted: %v", err)
	}
	actor := &Root{Config: r.Config, Files: r.Files, rootShared: r.rootShared, execution: &ExecutionIdentity{UID: 1001, GID: 100}}
	page, err := actor.List(context.Background(), ".", ListingOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	other := &Root{Config: r.Config, Files: r.Files, rootShared: r.rootShared, execution: &ExecutionIdentity{UID: 1002, GID: 100}}
	if _, err := other.List(context.Background(), ".", ListingOptions{Limit: 1, After: page.Next}); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("cross-identity cursor accepted: %v", err)
	}
}

func TestIndexedSearchMatchesKeysBeforeLoadingNodes(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "size", "modified", "relevance"} {
		for _, match := range []string{"exact", "fuzzy"} {
			t.Run(sortBy+"/"+match, func(t *testing.T) {
				r := indexedListingRoot(t, []Node{{Path: "a-unrelated.txt"}, {Path: "b-needle.pdf"}, {Path: "c-needle.pdf", Directory: true}})
				state := r.State.(*listingState)
				// The unrelated value must not be decoded or fetched. Extension and name
				// matching both come from keys, including keys in every ordered family.
				state.rows["i/"+r.generation+"/a-unrelated.txt"] = []byte("invalid JSON")
				state.onGet = func(k string) error {
					if strings.HasSuffix(k, "a-unrelated.txt") {
						t.Fatal("loaded noncandidate")
					}
					return nil
				}
				page, err := r.Search(context.Background(), "needle", ".", ListingOptions{Sort: sortBy, Match: match, Ext: []string{"pdf"}, Limit: 1})
				if err != nil || len(page.Items) != 1 || page.Items[0].Path != "b-needle.pdf" {
					t.Fatalf("page=%+v err=%v", page, err)
				}
			})
		}
	}
}

func TestIndexedCachedPageDoesNotOpenSnapshot(t *testing.T) {
	for _, sortBy := range []string{"name", "relevance"} {
		t.Run(sortBy, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "needle-a"}, {Path: "needle-b"}})
			opts := ListingOptions{Sort: sortBy, Match: "fuzzy", Limit: 1}
			first, err := r.Search(context.Background(), "needle", ".", opts)
			if err != nil || first.Next == "" {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			state := r.State.(*listingState)
			closed := state.closed
			opts.After = first.Next
			page, err := r.Search(context.Background(), "needle", ".", opts)
			if err != nil || len(page.Items) != 1 || state.closed != closed {
				t.Fatalf("cached=%+v err=%v snapshots=%d", page, err, state.closed-closed)
			}
		})
	}
}

func TestIndexedStreamingSearchRetainedBudget(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "size", "modified"} {
		t.Run(sortBy, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "a-miss", Size: 1}, {Path: "b-needle", Size: 2}, {Path: "c-needle", Size: 3}})
			opts := ListingOptions{Sort: sortBy, MaxEntries: 1, Limit: 100}
			page, err := r.Search(context.Background(), "needle", ".", opts)
			if err != nil || len(page.Items) != 1 || page.Items[0].Path != "b-needle" || page.Next == "" {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			opts.After = page.Next
			page, err = r.Search(context.Background(), "needle", ".", opts)
			if err != nil || len(page.Items) != 1 || page.Items[0].Path != "c-needle" || page.Next != "" {
				t.Fatalf("continuation=%+v err=%v", page, err)
			}
		})
	}
}
