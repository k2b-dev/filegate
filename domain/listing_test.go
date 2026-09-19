package domain

import (
	"context"
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
	rows   map[string][]byte
	keys   []string
	visits int
	scans  int
}

func (s *listingState) Get(key string, out any) error {
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
