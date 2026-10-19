package domain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func filterSize(v int64) *int64         { return &v }
func filterTime(v time.Time) *time.Time { return &v }

func TestSearchFiltersNormalize(t *testing.T) {
	at := time.Date(2026, 1, 1, 1, 0, 0, 123, time.FixedZone("offset", 3600))
	original := []string{"PDF", "gz", "pdf", "DOCX"}
	opts, err := normalizeListing(ListingOptions{Ext: original, MinSize: filterSize(0), MaxSize: filterSize(0), ModifiedAfter: &at, ModifiedBefore: &at})
	if err != nil || !reflect.DeepEqual(opts.Ext, []string{"docx", "gz", "pdf"}) || opts.ModifiedAfter.Location() != time.UTC || opts.ModifiedBefore.Location() != time.UTC || !opts.ModifiedAfter.Equal(at) {
		t.Fatalf("normalization: %+v %v", opts, err)
	}
	if !reflect.DeepEqual(original, []string{"PDF", "gz", "pdf", "DOCX"}) {
		t.Fatal("mutated caller extensions")
	}
	equivalent, err := normalizeListing(ListingOptions{Ext: []string{"gz", "docx", "pdf"}, MinSize: filterSize(0), MaxSize: filterSize(0), ModifiedAfter: filterTime(at.UTC()), ModifiedBefore: filterTime(at.UTC())})
	if err != nil || queryFingerprint(".", "", true, opts, nil) != queryFingerprint(".", "", true, equivalent, nil) {
		t.Fatal("equivalent filters have different fingerprints")
	}
	for _, invalid := range []ListingOptions{
		{Ext: []string{""}}, {Ext: []string{".pdf"}}, {Ext: []string{"pdf", ""}},
		{MinSize: filterSize(-1)}, {MaxSize: filterSize(-1)}, {MinSize: filterSize(2), MaxSize: filterSize(1)},
	} {
		if _, err := normalizeListing(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %+v: %v", invalid, err)
		}
	}
}

func TestSearchFiltersPredicate(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		opts ListingOptions
		node Node
		want bool
	}{
		{"case insensitive", ListingOptions{Ext: []string{"PDF"}}, Node{Path: "reports/a.PdF"}, true},
		{"multi dot final", ListingOptions{Ext: []string{"gz"}}, Node{Path: "a.tar.gz"}, true},
		{"multi dot earlier", ListingOptions{Ext: []string{"tar"}}, Node{Path: "a.tar.gz"}, false},
		{"no extension", ListingOptions{Ext: []string{"pdf"}}, Node{Path: "pdf"}, false},
		{"parent extension", ListingOptions{Ext: []string{"pdf"}}, Node{Path: "parent.pdf/file"}, false},
		{"dotfile", ListingOptions{Ext: []string{"bashrc"}}, Node{Path: ".bashrc"}, false},
		{"dotfile extension", ListingOptions{Ext: []string{"pdf"}}, Node{Path: ".hidden.pdf"}, true},
		{"trailing dot", ListingOptions{Ext: []string{"pdf"}}, Node{Path: "pdf."}, false},
		{"directory ext", ListingOptions{Ext: []string{"pdf"}}, Node{Path: "a.pdf", Directory: true}, false},
		{"directory min", ListingOptions{MinSize: filterSize(0)}, Node{Directory: true}, false},
		{"directory max", ListingOptions{MaxSize: filterSize(100)}, Node{Directory: true}, false},
		{"min inclusive", ListingOptions{MinSize: filterSize(10)}, Node{Size: 10}, true},
		{"below min", ListingOptions{MinSize: filterSize(10)}, Node{Size: 9}, false},
		{"max inclusive", ListingOptions{MaxSize: filterSize(10)}, Node{Size: 10}, true},
		{"above max", ListingOptions{MaxSize: filterSize(10)}, Node{Size: 11}, false},
		{"zero max", ListingOptions{MaxSize: filterSize(0)}, Node{Size: 1}, false},
		{"after inclusive", ListingOptions{ModifiedAfter: &at}, Node{Modified: at}, true},
		{"before after", ListingOptions{ModifiedAfter: &at}, Node{Modified: at.Add(-time.Nanosecond)}, false},
		{"before exclusive", ListingOptions{ModifiedBefore: &at}, Node{Modified: at}, false},
		{"below before", ListingOptions{ModifiedBefore: &at}, Node{Modified: at.Add(-time.Nanosecond)}, true},
		{"directory time", ListingOptions{ModifiedAfter: &at}, Node{Directory: true, Modified: at}, true},
		{"directory outside time", ListingOptions{ModifiedBefore: &at}, Node{Directory: true, Modified: at}, false},
		{"empty time interval", ListingOptions{ModifiedAfter: &at, ModifiedBefore: &at}, Node{Modified: at}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := normalizeListing(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := opts.matchesSearchFilters(tc.node); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSearchFiltersIndexed(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nodes := []Node{
		{Path: "docs/a.PDF", Size: 10, Modified: at},
		{Path: "docs/b.pdf", Size: 20, Modified: at.Add(time.Hour)},
		{Path: "docs/c.pdf", Size: 21, Modified: at},
		{Path: "docs/d.txt", Size: 10, Modified: at},
		{Path: "docs/e.pdf", Size: 10, Modified: at.Add(2 * time.Hour)},
		{Path: "docs/f.pdf", Directory: true, Size: 10, Modified: at},
	}
	for _, base := range []string{".", "docs"} {
		for _, sortBy := range []string{"path", "name", "size", "modified"} {
			for _, mode := range []string{"exact", "fuzzy"} {
				for _, query := range []string{"", "pdf"} {
					t.Run(strings.Join([]string{base, sortBy, mode, query}, "/"), func(t *testing.T) {
						r := indexedListingRoot(t, nodes)
						r.now = func() time.Time { return at }
						opts := ListingOptions{Ext: []string{"PDF"}, MinSize: filterSize(10), MaxSize: filterSize(20), ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(2 * time.Hour)), Sort: sortBy, Match: mode, Limit: 1}
						got := collectSearch(t, r, query, base, opts)
						if len(got) != 2 || got[0].Path != "docs/a.PDF" || got[1].Path != "docs/b.pdf" {
							t.Fatalf("filtered: %+v", got)
						}
						opts.Type = "directories"
						if got := collectSearch(t, r, query, base, opts); len(got) != 0 {
							t.Fatalf("directories matched file filters: %+v", got)
						}
						opts = ListingOptions{Type: "directories", ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(time.Hour))}
						if got := collectSearch(t, r, "", base, opts); len(got) != 1 || got[0].Path != "docs/f.pdf" {
							t.Fatalf("directory time filter: %+v", got)
						}
					})
				}
			}
		}
	}
}

func TestSearchFiltersCursor(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"exact", "fuzzy"} {
		r := indexedListingRoot(t, []Node{{Path: "a.pdf", Size: 10, Modified: at}, {Path: "b.pdf", Size: 10, Modified: at}})
		r.now = func() time.Time { return at }
		opts := ListingOptions{Ext: []string{"PDF", "pdf"}, MinSize: filterSize(0), MaxSize: filterSize(20), ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(time.Hour)), Limit: 1, Match: mode}
		page, err := r.Search(context.Background(), "", ".", opts)
		if err != nil || page.Next == "" {
			t.Fatalf("first page: %+v %v", page, err)
		}
		opts.After = page.Next
		opts.Ext = []string{"pdf"}
		opts.ModifiedAfter = filterTime(at.In(time.FixedZone("offset", 3600)))
		if _, err := r.Search(context.Background(), "", ".", opts); err != nil {
			t.Fatalf("equivalent filters rejected: %v", err)
		}
		changes := []func(*ListingOptions){
			func(o *ListingOptions) { o.Ext = []string{"txt"} },
			func(o *ListingOptions) { o.MinSize = filterSize(1) },
			func(o *ListingOptions) { o.MinSize = nil },
			func(o *ListingOptions) { o.MaxSize = filterSize(19) },
			func(o *ListingOptions) { o.ModifiedAfter = filterTime(at.Add(-time.Hour)) },
			func(o *ListingOptions) { o.ModifiedBefore = filterTime(at.Add(2 * time.Hour)) },
		}
		for i, change := range changes {
			changed := opts
			change(&changed)
			if _, err := r.Search(context.Background(), "", ".", changed); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("%s change %d accepted: %v", mode, i, err)
			}
		}
	}
}

func TestSearchFiltersLiveAndExecution(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, execution := range []bool{false, true} {
		r, files, _ := liveListingRoot(t)
		if err := os.WriteFile(filepath.Join(files.base, "dir", "report.PDF"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := filepath.WalkDir(files.base, func(p string, _ os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(p, at, at)
		}); err != nil {
			t.Fatal(err)
		}
		if execution {
			r.Config.Index = true
			r.execution = &ExecutionIdentity{UID: 1001, GID: 100}
			r.Files = &searchAccessFiles{listingFiles: files, checked: map[string]int{}}
			state := &listingState{}
			r.State = state
			// Live metadata still reads identity claims. Seed them so this fixture
			// can reject any index scan without exercising xattr assignment.
			if err := filepath.WalkDir(files.base, func(p string, _ os.DirEntry, err error) error {
				if err != nil || p == files.base {
					return err
				}
				rel, err := filepath.Rel(files.base, p)
				if err != nil {
					return err
				}
				value, err := json.Marshal(claim{Device: 1, Inode: 1, Path: rel})
				if err != nil {
					return err
				}
				return state.Batch([]Change{{Key: "identity/" + filepath.Base(p), Value: value}})
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, query := range []string{"", "report"} {
			opts := ListingOptions{Ext: []string{"pdf"}, MinSize: filterSize(4), MaxSize: filterSize(4), ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(time.Second))}
			got := collectSearch(t, r, query, ".", opts)
			if len(got) != 1 || got[0].Path != "dir/report.PDF" {
				t.Fatalf("execution=%v query=%q: %+v", execution, query, got)
			}
			opts.ModifiedBefore = &at
			if got := collectSearch(t, r, query, ".", opts); len(got) != 0 {
				t.Fatalf("time bound ignored: %+v", got)
			}
		}
		if execution && r.State.(*listingState).scans != 0 {
			t.Fatal("execution query scanned index")
		}
		page, err := r.List(context.Background(), ".", ListingOptions{Ext: []string{".invalid"}, MinSize: filterSize(-1)})
		if err != nil || len(page.Items) != 4 {
			t.Fatalf("search filters changed directory listing: %+v %v", page, err)
		}
	}
}

func TestSearchFiltersBeforeFuzzyTierSelection(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "tst.txt"}, {Path: "testtest.pdf"}})
	// A filtered-out exact hit must not suppress a surviving loose hit.
	got := collectSearch(t, r, "tst", ".", ListingOptions{Match: "fuzzy", Ext: []string{"pdf"}})
	if len(got) != 1 || got[0].Path != "testtest.pdf" {
		t.Fatalf("filtered exact hit suppressed loose hit: %+v", got)
	}
}

func TestSearchFiltersTimestampRange(t *testing.T) {
	for _, raw := range []string{"9999-12-31T23:59:59-01:00", "0000-01-01T00:00:00+01:00"} {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(at.UTC()); err == nil {
			t.Fatal("fixture must exceed JSON timestamp range after UTC normalization")
		}
		for _, opts := range []ListingOptions{{ModifiedAfter: &at}, {ModifiedBefore: &at}} {
			if _, err := normalizeListing(opts); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unencodable filter accepted: %s %+v %v", raw, opts, err)
			}
		}
	}
	// Keep the representable RFC 3339 boundary years valid.
	for _, raw := range []string{"0000-01-01T00:00:00Z", "9999-12-31T23:59:59Z"} {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeListing(ListingOptions{ModifiedAfter: &at, ModifiedBefore: &at}); err != nil {
			t.Fatalf("valid boundary rejected: %s %v", raw, err)
		}
	}
}
