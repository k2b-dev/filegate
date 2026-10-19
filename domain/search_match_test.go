package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchMatchTierSelection(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "size", "modified"} {
		for _, order := range []string{"asc", "desc"} {
			for _, strong := range []string{"", "tata", "taxa"} {
				t.Run(sortBy+"/"+order+"/"+strong, func(t *testing.T) {
					nodes := []Node{{Path: "scope/a-t1a2t3a.ods", Size: 1}, {Path: "scope/z-t1a2t3a.ods", Size: 2}, {Path: "outside/tata"}}
					want := []string{"scope/a-t1a2t3a.ods", "scope/z-t1a2t3a.ods"}
					if strong != "" {
						nodes = append(nodes, Node{Path: "scope/m-" + strong})
						want = []string{"scope/m-" + strong}
					}
					if order == "desc" && len(want) == 2 {
						want[0], want[1] = want[1], want[0]
					}
					r := indexedListingRoot(t, nodes)
					got := collectSearch(t, r, "tata", "scope", ListingOptions{Match: "fuzzy", Sort: sortBy, Order: order, Limit: 1})
					var paths []string
					for _, n := range got {
						paths = append(paths, n.Path)
					}
					if strings.Join(paths, "|") != strings.Join(want, "|") {
						t.Fatalf("%v want %v", paths, want)
					}
					if state := r.State.(*listingState); state.scans != 1 {
						t.Fatalf("rescanned snapshot: %d", state.scans)
					}
				})
			}
		}
	}
}

func TestSearchMatchTierSelectionUsesTypeFilter(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a-t1a2t3a.ods"}, {Path: "tata", Directory: true}})
	got := collectSearch(t, r, "tata", ".", ListingOptions{Match: "fuzzy", Type: "files"})
	if len(got) != 1 || got[0].Path != "a-t1a2t3a.ods" {
		t.Fatalf("filtered strong hit suppressed loose files: %+v", got)
	}
}

func TestSearchMatchSnapshotMemoryLimit(t *testing.T) {
	previousLimit := listingMemoryLimit
	listingMemoryLimit = 1024
	t.Cleanup(func() { listingMemoryLimit = previousLimit })
	loose := []Node{{Path: "a-t1a2t3a"}, {Path: "b-t1a2t3a"}, {Path: "c-t1a2t3a"}, {Path: "d-t1a2t3a"}, {Path: "e-t1a2t3a"}}
	for _, tc := range []struct {
		label string
		nodes []Node
		want  string
		limit bool
	}{
		{label: "overflow before late exact", nodes: append(append([]Node{}, loose...), Node{Path: "z-tata"}), want: "z-tata"},
		{label: "overflow before late typo", nodes: append(append([]Node{}, loose...), Node{Path: "z-taxa"}), want: "z-taxa"},
		{label: "overflow without strong", nodes: append(append([]Node{}, loose...), Node{Path: "z-unrelated"}), limit: true},
		{label: "loose after strong", nodes: append([]Node{{Path: "0-tata"}}, loose...), want: "0-tata"},
		{label: "strong still bounded", nodes: []Node{{Path: "a-tata"}, {Path: "b-tata"}, {Path: "c-tata"}, {Path: "d-tata"}, {Path: "e-tata"}}, limit: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			r := indexedListingRoot(t, tc.nodes)
			page, err := r.Search(context.Background(), "tata", ".", ListingOptions{Match: "fuzzy"})
			if tc.limit {
				if !errors.Is(err, ErrLimit) || len(page.Items) != 0 {
					t.Fatalf("overflow: %+v %v", page, err)
				}
			} else if err != nil || len(page.Items) != 1 || page.Items[0].Path != tc.want || page.Next != "" {
				t.Fatalf("selection: %+v %v; want %q", page, err, tc.want)
			}
			if tc.label == "overflow without strong" && r.State.(*listingState).visits != len(tc.nodes) {
				t.Fatal("loose overflow stopped scanning before checking for strong hits")
			}
			if r.listingBytes != 0 || len(r.listings) != 0 {
				t.Fatal("discarded snapshot retained in cache")
			}
		})
	}
}

func TestSearchMatchRootSnapshotAndBudget(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a-t1a2t3a.ods"}, {Path: "z-taxa"}})
	page, err := r.Search(context.Background(), "tata", ".", ListingOptions{Match: "fuzzy", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "z-taxa" || page.Next != "" {
		t.Fatalf("root selection: %+v %v", page, err)
	}
	if _, err := r.Search(context.Background(), "tata", ".", ListingOptions{Match: "fuzzy", MaxEntries: 1}); !errors.Is(err, ErrLimit) {
		t.Fatalf("budget: %v", err)
	}
}

func TestSearchMatchEvictsOnlyForFinalSnapshot(t *testing.T) {
	previousLimit := listingMemoryLimit
	listingMemoryLimit = 1024
	t.Cleanup(func() { listingMemoryLimit = previousLimit })
	for _, tc := range []struct {
		label  string
		strong int
		evict  bool
	}{
		{label: "loose overflow without strong"},
		{label: "single page", strong: 1},
		{label: "final snapshot fits", strong: 2},
		{label: "final snapshot needs eviction", strong: 3, evict: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			nodes := []Node{{Path: "cached-a"}, {Path: "cached-b"}, {Path: "a-t1a2t3a"}, {Path: "b-t1a2t3a"}, {Path: "c-t1a2t3a"}, {Path: "d-t1a2t3a"}, {Path: "e-t1a2t3a"}}
			for i := 0; i < tc.strong; i++ {
				nodes = append(nodes, Node{Path: "z-tata" + string(rune('a'+i))})
			}
			r := indexedListingRoot(t, nodes)
			r.now = func() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) }
			opts := ListingOptions{Match: "fuzzy", Limit: 1}
			cached, err := r.Search(context.Background(), "cached", ".", opts)
			if err != nil || cached.Next == "" {
				t.Fatalf("cached query: %+v %v", cached, err)
			}
			page, err := r.Search(context.Background(), "tata", ".", opts)
			if tc.strong == 0 {
				if !errors.Is(err, ErrLimit) {
					t.Fatalf("expected loose overflow, got %v", err)
				}
			} else if err != nil || len(page.Items) != 1 || (page.Next != "") != (tc.strong > 1) {
				t.Fatalf("final selection: %+v %v", page, err)
			}
			if r.listingBytes > listingMemoryLimit {
				t.Fatalf("cache exceeds memory limit: %d", r.listingBytes)
			}
			opts.After = cached.Next
			page, err = r.Search(context.Background(), "cached", ".", opts)
			if tc.evict {
				if !errors.Is(err, ErrCursorInvalid) {
					t.Fatalf("expected capacity eviction, got %+v %v", page, err)
				}
			} else if err != nil || len(page.Items) != 1 || page.Items[0].Path != "cached-b" {
				t.Fatalf("provisional hits evicted cached cursor: %+v %v", page, err)
			}
		})
	}
}

func TestSearchMatchCursorFingerprint(t *testing.T) {
	for _, sortBy := range []string{"path", "name"} {
		for _, mode := range []string{"exact", "fuzzy"} {
			r := indexedListingRoot(t, []Node{{Path: "fresh/test-a"}, {Path: "fresh/test-b"}})
			opts := ListingOptions{Match: mode, Sort: sortBy, Limit: 1}
			page, err := r.Search(context.Background(), "test", "fresh", opts)
			if err != nil || page.Next == "" {
				t.Fatalf("first: %+v %v", page, err)
			}
			opts.After = page.Next
			if mode == "exact" {
				opts.Match = "fuzzy"
			} else {
				opts.Match = "exact"
			}
			if _, err := r.Search(context.Background(), "test", "fresh", opts); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("changed match accepted: %v", err)
			}
		}
	}
	defaults, _ := normalizeListing(ListingOptions{})
	explicit, _ := normalizeListing(ListingOptions{Match: "exact"})
	if queryFingerprint(".", "q", true, defaults, nil) != queryFingerprint(".", "q", true, explicit, nil) {
		t.Fatal("default and explicit exact differ")
	}
}

func TestSearchMatchOptionsAndTokenLimit(t *testing.T) {
	for _, mode := range []string{"unknown", "EXACT", "Fuzzy"} {
		if _, err := normalizeListing(ListingOptions{Match: mode}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %q: %v", mode, err)
		}
	}
	r := indexedListingRoot(t, nil)
	if _, err := r.Search(context.Background(), strings.Repeat("word ", 9), ".", ListingOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token limit: %v", err)
	}
	if r.State.(*listingState).scans != 0 {
		t.Fatal("scanned invalid query")
	}
}

func TestSearchQueryByteLimit(t *testing.T) {
	for _, mode := range []string{"exact", "fuzzy"} {
		for _, tc := range []struct {
			label, query string
			invalid      bool
		}{
			{label: "below limit", query: strings.Repeat("a", 8191)},
			{label: "at limit", query: strings.Repeat("a", 8192)},
			{label: "over limit", query: strings.Repeat("a", 8193), invalid: true},
			{label: "multibyte at limit", query: strings.Repeat("é", 4096)},
			{label: "multibyte over limit", query: strings.Repeat("é", 4096) + "a", invalid: true},
			{label: "whitespace at limit", query: strings.Repeat(" ", 8192)},
			{label: "whitespace over limit", query: strings.Repeat(" ", 8193), invalid: true},
		} {
			t.Run(mode+"/"+tc.label, func(t *testing.T) {
				r := indexedListingRoot(t, nil)
				_, err := r.Search(context.Background(), tc.query, ".", ListingOptions{Match: mode})
				if tc.invalid {
					if !errors.Is(err, ErrInvalid) || r.State.(*listingState).scans != 0 {
						t.Fatalf("oversized raw query: err=%v scans=%d", err, r.State.(*listingState).scans)
					}
				} else if err != nil {
					t.Fatalf("valid query rejected: %v", err)
				}
			})
		}
	}
}

func TestSearchMatchIndexedAndIndexFree(t *testing.T) {
	nodes := []Node{{Path: "Quartals_Bericht_2026.xlsx"}, {Path: "Müller.docx"}, {Path: "testtest.ods"}}
	for _, indexed := range []bool{false, true} {
		var r *Root
		if indexed {
			r = indexedListingRoot(t, nodes)
		} else {
			files := &listingFiles{base: t.TempDir()}
			r = &Root{Config: RootConfig{Name: "test"}, Files: files, rootShared: &rootShared{now: func() time.Time { return time.Unix(100000, 0) }}}
			// listingFiles has no xattr or state implementation: an accidental index
			// dependency panics rather than quietly passing on a capable filesystem.
			for _, n := range nodes {
				if err := os.WriteFile(filepath.Join(files.base, n.Path), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, tc := range []struct{ query, mode, want string }{
			{"2026 bericht", "", "Quartals_Bericht_2026.xlsx"},
			{"quartalsbericht", "exact", "Quartals_Bericht_2026.xlsx"},
			{"muller", "", "Müller.docx"},
			{"tsettest", "fuzzy", "testtest.ods"},
			{"tete", "fuzzy", "testtest.ods"},
			{"tst", "fuzzy", "testtest.ods"},
		} {
			got := collectSearch(t, r, tc.query, ".", ListingOptions{Match: tc.mode})
			if len(got) != 1 || got[0].Path != tc.want {
				t.Fatalf("indexed=%v query=%q: %+v", indexed, tc.query, got)
			}
		}
	}
}

type searchAccessFiles struct {
	*listingFiles
	checked map[string]int
	denied  string
}

func (f *searchAccessFiles) MetadataOpen(p string) (*os.File, error) {
	return f.Open(p, os.O_RDONLY, 0)
}
func (f *searchAccessFiles) ID(file *os.File) (string, error) { return filepath.Base(file.Name()), nil }
func (f *searchAccessFiles) Stat(p string) (os.FileInfo, error) {
	f.checked[p]++
	if p == f.denied {
		return nil, os.ErrPermission
	}
	return f.listingFiles.Stat(p)
}

func TestSearchMatchExecutionAuthorizesEveryNode(t *testing.T) {
	// The indexed root contains a stale extra hit. A scoped query must collect
	// the live files, then reauthorize every returned node on every page.
	r := indexedListingRoot(t, []Node{{Path: "unfiltered-tata"}})
	f := &searchAccessFiles{listingFiles: &listingFiles{base: t.TempDir()}, checked: map[string]int{}}
	r.Files = f
	r.execution = &ExecutionIdentity{UID: 1001, GID: 100}
	for _, name := range []string{"a-tata", "b-taxa", "c-t1a2t3a"} {
		if err := os.WriteFile(filepath.Join(f.base, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		// Existing claims avoid assigning IDs in this authorization-only fixture.
		value, _ := json.Marshal(claim{Device: 1, Inode: 1, Path: name})
		if err := r.State.Batch([]Change{{Key: "identity/" + name, Value: value}}); err != nil {
			t.Fatal(err)
		}
	}
	opts := ListingOptions{Match: "fuzzy", Limit: 1}
	page, err := r.Search(context.Background(), "tata", ".", opts)
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "a-tata" || page.Next == "" {
		t.Fatalf("first: %+v %v", page, err)
	}
	if f.checked["a-tata"] == 0 || r.State.(*listingState).scans != 0 {
		t.Fatalf("authorization=%v scans=%d", f.checked, r.State.(*listingState).scans)
	}
	opts.After = page.Next
	f.denied = "b-taxa"
	page, err = r.Search(context.Background(), "tata", ".", opts)
	if !errors.Is(err, os.ErrPermission) || len(page.Items) != 0 || f.checked["b-taxa"] == 0 {
		t.Fatalf("revoked node: %+v %v checks=%v", page, err, f.checked)
	}
	f.denied = ""
	page, err = r.Search(context.Background(), "tata", ".", opts)
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "b-taxa" || page.Next != "" {
		t.Fatalf("retry: %+v %v", page, err)
	}
}

func TestSearchMatchListKeepsImmediateChildPaging(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a", Directory: true}, {Path: "a/nested"}, {Path: "a/nested/another"}, {Path: "z"}})
	r.Files = &listingFiles{base: t.TempDir()}
	page, err := r.List(context.Background(), ".", ListingOptions{Match: "fuzzy", Limit: 1, MaxEntries: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "a" || page.Next == "" {
		t.Fatalf("list descended into subtree: %+v %v", page, err)
	}
	var cursor listingCursor
	raw, err := base64.RawURLEncoding.DecodeString(page.Next)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.Snapshot != "" || cursor.Last == "" {
		t.Fatalf("list lost indexed continuation: %+v", cursor)
	}
}

func TestSearchMatchFirstPageHighlightCancellationDoesNotCache(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "docs/report-a"}, {Path: "docs/report-b"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Unix(1000, 0)
	clockReads := 0
	r.now = func() time.Time {
		clockReads++
		if clockReads == 2 {
			// The observation is complete and starts its lifetime, before the
			// first page's highlights. Collection itself has already succeeded.
			cancel()
		}
		return now
	}
	page, err := r.Search(ctx, "report", "docs", ListingOptions{Sort: "name", Match: "fuzzy", Limit: 1})
	state := r.State.(*listingState)
	if !errors.Is(err, context.Canceled) || len(page.Items) != 0 || page.Next != "" || len(r.listings) != 0 || r.listingBytes != 0 || len(r.searches) != 0 || state.closed != 1 {
		t.Fatalf("failed highlights retained state: page=%+v err=%v cache=%d bytes=%d slots=%d closed=%d", page, err, len(r.listings), r.listingBytes, len(r.searches), state.closed)
	}
}
