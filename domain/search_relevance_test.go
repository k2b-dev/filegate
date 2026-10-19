package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSearchRelevanceRanking(t *testing.T) {
	for _, tc := range []struct {
		label, query string
		names        []string
	}{
		{"exact ranks", "report", []string{"report", "report.pdf", "annual_report.pdf", "annualreport.pdf"}},
		{"tiers", "report", []string{"report.pdf", "reprot.pdf", "r1e2p3o4r5t.pdf"}},
		{"typo distance", "abcdefgh", []string{"abxdefgh.txt", "abxdefxh.txt"}},
		{"loose starts then spans", "ab", []string{"axb.txt", "aqqb.txt", "xaxb.txt"}},
		{"accent before length", "muller", []string{"Muller.docx", "Müller.pdf"}},
		{"shorter name", "report", []string{"report.txt", "report-annual.txt"}},
		{"segments then path bytes", "report", []string{"report", "a/report", "z/report", "a/b/report"}},
		{"summed exact subranks", "annual report", []string{"annual-report", "report-annual.txt", "xannualreport.txt"}},
		{"exact plus typo before two typos", "annual report", []string{"annual-reprot", "annula-reprot"}},
		{"worst tier before summed distances", "annual abcdefgh", []string{"annula-abxdefxh", "annual-axbxcxdxexfxgxh"}},
		{"only worst tier contributes subranks", "annual report", []string{"xannualx-reprot", "annula-reportxxx", "annula-reprot"}},
		{"one loose token before two", "ab cd", []string{"axb-cd", "axb-cxd"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var nodes []Node
			for i := len(tc.names) - 1; i >= 0; i-- {
				nodes = append(nodes, Node{Path: tc.names[i]})
			}
			r := indexedListingRoot(t, nodes)
			for _, order := range []string{"asc", "desc"} {
				got := collectSearch(t, r, tc.query, ".", ListingOptions{Sort: "relevance", Match: "fuzzy", Order: order, Limit: 1})
				var names []string
				for _, n := range got {
					names = append(names, n.Path)
				}
				if !reflect.DeepEqual(names, tc.names) {
					t.Fatalf("order=%s got=%v want=%v", order, names, tc.names)
				}
			}
		})
	}
}
func TestSearchRelevanceRequiresQuery(t *testing.T) {
	r := indexedListingRoot(t, nil)
	for _, q := range []string{"", " \t\n"} {
		if _, err := r.Search(context.Background(), q, ".", ListingOptions{Sort: "relevance"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q: %v", q, err)
		}
	}
	if _, err := r.List(context.Background(), ".", ListingOptions{Sort: "relevance"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list: %v", err)
	}
	if r.State.(*listingState).scans != 0 {
		t.Fatal("invalid relevance scanned")
	}
}
func TestSearchRelevanceTopKPages(t *testing.T) {
	var nodes []Node
	for i := 0; i < 50; i++ {
		nodes = append(nodes, Node{Path: fmt.Sprintf("folder/report-%02d.pdf", i)})
	}
	r := indexedListingRoot(t, nodes)
	r.SearchTopK = 3
	opts := ListingOptions{Sort: "relevance", Limit: 1, MaxEntries: 3}
	for i := 0; i < 3; i++ {
		page, err := r.Search(context.Background(), "report", ".", opts)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total == nil || *page.Total != 50 || page.Truncated == nil || !*page.Truncated || len(page.Items) != 1 || page.Items[0].Path != fmt.Sprintf("folder/report-%02d.pdf", i) || page.Matches == nil || !reflect.DeepEqual(*page.Matches, [][]MatchRange{{{0, 6}}}) {
			t.Fatalf("page %d: %+v", i, page)
		}
		if (page.Next != "") != (i < 2) {
			t.Fatalf("continuation %d: %q", i, page.Next)
		}
		for _, s := range r.listings {
			if len(s.hits) != 3 || len(s.entries) != 0 || s.bytes > 1024 {
				t.Fatalf("not compact: %+v", s)
			}
		}
		opts.After = page.Next
		opts.Order = "desc" // ignored, including cursor fingerprint
	}
	if r.State.(*listingState).scans != 1 {
		t.Fatal("rescanned relevance pages")
	}
}
func TestSearchPageFields(t *testing.T) {
	for _, tc := range []struct {
		label, query, base string
		opts               ListingOptions
		total              bool
		count              int
	}{
		{"keyset", "report", ".", ListingOptions{}, false, 1},
		{"snapshot exact", "report", "docs", ListingOptions{Sort: "name"}, true, 1},
		{"snapshot fuzzy", "report", ".", ListingOptions{Match: "fuzzy"}, true, 1},
		{"empty relevance", "missing", ".", ListingOptions{Sort: "relevance"}, true, 0},
		{"no query snapshot", "", "docs", ListingOptions{Sort: "name"}, true, 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "docs/report.pdf"}})
			p, err := r.Search(context.Background(), tc.query, tc.base, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(p)
			var wire map[string]json.RawMessage
			json.Unmarshal(b, &wire)
			if (p.Total != nil) != tc.total || (p.Truncated != nil) != tc.total || (wire["matches"] != nil) != (tc.query != "") || len(p.Items) != tc.count {
				t.Fatalf("%s", b)
			}
			if tc.total && (*p.Total != tc.count || *p.Truncated) {
				t.Fatalf("%s", b)
			}
		})
	}
	r, _, _ := liveListingRoot(t)
	p, err := r.List(context.Background(), ".", ListingOptions{})
	if err != nil || p.Total != nil || p.Truncated != nil || p.Matches != nil {
		t.Fatalf("listing changed: %+v %v", p, err)
	}
}
func TestSearchHighlightRanges(t *testing.T) {
	for _, tc := range []struct {
		query, name string
		want        []MatchRange
	}{
		{"report", "Annual_Report.pdf", []MatchRange{{7, 13}}},
		{"quartalsbericht", "Quartals_Bericht.xlsx", []MatchRange{{0, 8}, {9, 16}}},
		{"tsettest", "testtest.ods", []MatchRange{{0, 8}}},
		{"tete", "testtest.ods", []MatchRange{{3, 6}}},
		{"abcdx", "abcd", []MatchRange{{0, 4}}},
		{"tst", "testtest.ods", []MatchRange{{0, 1}, {2, 4}}},
		{"muller", "Müller.docx", []MatchRange{{0, 6}}},
		{"müller", "Muller.docx", []MatchRange{{0, 6}}},
		{"strasse", "Straße.pdf", []MatchRange{{0, 6}}},
		{"ss", "Straße.pdf", []MatchRange{{4, 5}}},
		{"ffi", "ﬃ.txt", []MatchRange{{0, 1}}},
		{"report", "📄_Report.pdf", []MatchRange{{2, 8}}},
		{"café", "Cafe\u0301.txt", []MatchRange{{0, 5}}},
		{"cafe", "Cafe\u0301.txt", []MatchRange{{0, 5}}},
		{"e", "é.txt", []MatchRange{{0, 1}}},
		{"report port", "report.pdf", []MatchRange{{0, 6}}},
		{"2026 bericht", "Quartalsbericht 2026.pdf", []MatchRange{{8, 15}, {16, 20}}},
		{"ab", "a---x---b", []MatchRange{{0, 1}, {8, 9}}},
	} {
		t.Run(tc.query+"/"+tc.name, func(t *testing.T) {
			q, err := compileNameQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			q.fuzzy = true
			got := q.highlights(tc.name)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%q/%q: %v want %v", tc.query, tc.name, got, tc.want)
			}
		})
	}
}
func TestSearchRelevanceExecutionAuthorization(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "unfiltered-report"}})
	f := &searchAccessFiles{listingFiles: &listingFiles{base: t.TempDir()}, checked: map[string]int{}}
	r.Files = f
	r.execution = &ExecutionIdentity{UID: 1001, GID: 100}
	r.now = func() time.Time { return time.Unix(123, 0) }
	for _, name := range []string{"report", "report.pdf"} {
		if err := os.WriteFile(filepath.Join(f.base, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		value, _ := json.Marshal(claim{Device: 1, Inode: 1, Path: name})
		if err := r.State.Batch([]Change{{Key: "identity/" + name, Value: value}}); err != nil {
			t.Fatal(err)
		}
	}
	opts := ListingOptions{Sort: "relevance", Limit: 1}
	// Stat is called only by the final access recheck, after live collection
	// and page materialization. A failure must not admit the observation.
	f.denied = "report"
	p, err := r.Search(context.Background(), "report", ".", opts)
	if !errors.Is(err, os.ErrPermission) || len(p.Items) != 0 || len(r.listings) != 0 || r.listingBytes != 0 || len(r.searches) != 0 {
		t.Fatalf("failed authorization cached first page: %+v %v cache=%d", p, err, len(r.listings))
	}
	f.denied = ""
	p, err = r.Search(context.Background(), "report", ".", opts)
	if err != nil || len(p.Items) != 1 || p.Items[0].Path != "report" || p.Next == "" || f.checked["report"] == 0 {
		t.Fatalf("first %+v %v", p, err)
	}
	if r.State.(*listingState).scans != 0 {
		t.Fatal("execution used index")
	}
	opts.After = p.Next
	if err := os.WriteFile(filepath.Join(f.base, "report.pdf"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	f.denied = "report.pdf"
	p, err = r.Search(context.Background(), "report", ".", opts)
	if !errors.Is(err, os.ErrPermission) || len(p.Items) != 0 {
		t.Fatalf("revoked %+v %v", p, err)
	}
	f.denied = ""
	p, err = r.Search(context.Background(), "report", ".", opts)
	if err != nil || len(p.Items) != 1 || p.Items[0].Path != "report.pdf" || p.Items[0].Size != 7 || p.Next != "" {
		t.Fatalf("retry %+v %v", p, err)
	}
}
func TestSearchRelevanceKeepsLooseHits(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "tata"}, {Path: "taxa"}, {Path: "t1a2t3a"}})
	got := collectSearch(t, r, "tata", ".", ListingOptions{Sort: "relevance", Match: "fuzzy"})
	var paths []string
	for _, n := range got {
		paths = append(paths, n.Path)
	}
	if strings.Join(paths, "|") != "tata|taxa|t1a2t3a" {
		t.Fatal(paths)
	}
}

func TestSearchRelevanceRequestReaderRelease(t *testing.T) {
	for _, reason := range []string{"single page", "invalidation", "expiry", "eviction", "cancellation", "memory limit"} {
		t.Run(reason, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "report-a"}, {Path: "report-b"}})
			state := r.State.(*listingState)
			now := time.Unix(1000, 0)
			r.now = func() time.Time { return now }
			opts := ListingOptions{Sort: "relevance", Limit: 1}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if reason == "single page" {
				opts.Limit = 2
			}
			if reason == "cancellation" {
				state.onScan = cancel
			}
			if reason == "memory limit" {
				previous := listingMemoryLimit
				listingMemoryLimit = 1
				t.Cleanup(func() { listingMemoryLimit = previous })
			}
			page, err := r.Search(ctx, "report", ".", opts)
			if reason == "cancellation" || reason == "memory limit" {
				want := ErrLimit
				if reason == "cancellation" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || state.closed != 1 || len(r.listings) != 0 {
					t.Fatalf("failure leaked reader: %+v %v closed=%d", page, err, state.closed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if reason == "single page" {
				if state.closed != 1 {
					t.Fatal("single-page reader retained")
				}
				return
			}
			if state.closed != 1 {
				t.Fatal("request reader retained in cache")
			}
			switch reason {
			case "invalidation":
				r.invalidateListings()
			case "expiry":
				now = now.Add(listingLifetime)
				opts.After = page.Next
				if _, err := r.Search(ctx, "report", ".", opts); !errors.Is(err, ErrCursorInvalid) {
					t.Fatal(err)
				}
			case "eviction":
				r.evictListing()
			}
			if state.closed != 1 || len(r.listings) != 0 || r.listingBytes != 0 {
				t.Fatalf("%s leaked reader: closed=%d cache=%d bytes=%d", reason, state.closed, len(r.listings), r.listingBytes)
			}
		})
	}
}

type relevanceExecutionProvider struct{ *searchAccessFiles }

func (f *relevanceExecutionProvider) WithExecution(context.Context, uint32, uint32, []uint32) (Files, func(), error) {
	return f, func() {}, nil
}
func (f *relevanceExecutionProvider) Chmod(*os.File, os.FileMode) error { return nil }
func (f *relevanceExecutionProvider) Chown(*os.File, int, int) error    { return nil }
func TestSearchRelevanceExecutionKeepsTopK(t *testing.T) {
	r := indexedListingRoot(t, nil)
	r.Config.Execution = true
	r.SearchTopK = 7
	r.Files = &relevanceExecutionProvider{searchAccessFiles: &searchAccessFiles{}}
	view, close, err := r.WithExecution(context.Background(), &ExecutionIdentity{UID: 1001, GID: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if view.SearchTopK != 7 {
		t.Fatalf("execution topK=%d", view.SearchTopK)
	}
}

func TestSearchRelevanceRetainedBudget(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a-unrelated"}, {Path: "b-unrelated"}, {Path: "report-a"}, {Path: "report-b"}})
	r.SearchTopK = 2
	opts := ListingOptions{Sort: "relevance", MaxEntries: 1}
	if _, err := r.Search(context.Background(), "report", ".", opts); !errors.Is(err, ErrLimit) {
		t.Fatalf("retained hit budget: %v", err)
	}
	r.SearchTopK = 1
	p, err := r.Search(context.Background(), "report", ".", opts)
	if err != nil || len(p.Items) != 1 || *p.Total != 2 || !*p.Truncated {
		t.Fatalf("scan keys consumed retention budget: %+v %v", p, err)
	}
}

func TestSearchRelevanceHeapReplacesWorst(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "a-r1e2p3o4r5t"}, {Path: "b-reprot"}, {Path: "report"}, {Path: "z-report"}})
	r.SearchTopK = 2
	p, err := r.Search(context.Background(), "report", ".", ListingOptions{Sort: "relevance", Match: "fuzzy"})
	if err != nil || len(p.Items) != 2 || p.Items[0].Path != "report" || p.Items[1].Path != "z-report" || *p.Total != 4 || !*p.Truncated {
		t.Fatalf("heap failed to replace weaker early hits: %+v %v", p, err)
	}
}

func TestSearchRelevanceDefaultTopK(t *testing.T) {
	nodes := make([]Node, DefaultSearchTopK+2)
	for i := range nodes {
		nodes[i].Path = fmt.Sprintf("report-%04d", i)
	}
	r := indexedListingRoot(t, nodes)
	p, err := r.Search(context.Background(), "report", ".", ListingOptions{Sort: "relevance", Limit: 1000})
	if err != nil || len(p.Items) != DefaultSearchTopK || p.Total == nil || *p.Total != DefaultSearchTopK+2 || p.Truncated == nil || !*p.Truncated || p.Next != "" {
		t.Fatalf("default top-K: items=%d total=%v truncated=%v next=%q err=%v", len(p.Items), p.Total, p.Truncated, p.Next, err)
	}
}

func TestSearchRelevanceFirstPageFailureDoesNotCache(t *testing.T) {
	for _, failure := range []string{"read", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "report-a"}, {Path: "report-b"}})
			state := r.State.(*listingState)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("injected primary read failure")
			state.onGet = func(key string) error {
				if key != "i/"+r.generation+"/report-a" {
					t.Fatalf("unexpected materialization key: %s", key)
				}
				if failure == "cancellation" {
					cancel()
					return nil
				}
				return want
			}
			if failure == "cancellation" {
				want = context.Canceled
			}
			page, err := r.Search(ctx, "report", ".", ListingOptions{Sort: "relevance", Limit: 1})
			if !errors.Is(err, want) || len(page.Items) != 0 || page.Next != "" || len(r.listings) != 0 || r.listingBytes != 0 || state.closed != 1 || len(r.searches) != 0 {
				t.Fatalf("failed first page retained state: page=%+v err=%v cache=%d bytes=%d closed=%d slots=%d", page, err, len(r.listings), r.listingBytes, state.closed, len(r.searches))
			}
			state.onGet = nil
			page, err = r.Search(context.Background(), "report", ".", ListingOptions{Sort: "relevance", Limit: 1})
			if err != nil || page.Next == "" || len(r.listings) != 1 {
				t.Fatalf("retry failed: page=%+v err=%v cache=%d", page, err, len(r.listings))
			}
		})
	}
}

func TestSearchRelevanceContinuationInvalidatesChangedPrimaryRow(t *testing.T) {
	for _, filter := range []string{"all", "files", "directories"} {
		for _, change := range []string{"missing", "kind"} {
			t.Run(filter+"/"+change, func(t *testing.T) {
				directory := filter == "directories"
				r := indexedListingRoot(t, []Node{{Path: "report-a", Directory: directory}, {Path: "report-b", Directory: directory}})
				opts := ListingOptions{Sort: "relevance", Type: filter, Limit: 1}
				page, err := r.Search(context.Background(), "report", ".", opts)
				if err != nil || page.Next == "" {
					t.Fatalf("first: %+v %v", page, err)
				}
				// Simulate an inconsistent index without an epoch change. Never skip a hit.
				state := r.State.(*listingState)
				key := "i/" + r.generation + "/report-b"
				if change == "missing" {
					delete(state.rows, key)
				} else {
					state.rows[key], err = json.Marshal(Node{Path: "report-b", Directory: !directory})
					if err != nil {
						t.Fatal(err)
					}
				}
				opts.After = page.Next
				page, err = r.Search(context.Background(), "report", ".", opts)
				if !errors.Is(err, ErrCursorInvalid) || len(page.Items) != 0 || page.Next != "" || state.closed != 1 || state.scans != 1 {
					t.Fatalf("changed row did not require restart: %+v %v closed=%d scans=%d", page, err, state.closed, state.scans)
				}
			})
		}
	}
}

func TestSearchRelevanceLiveContinuationInvalidatesChangedHit(t *testing.T) {
	for _, execution := range []bool{false, true} {
		for _, filter := range []string{"all", "files", "directories"} {
			for _, change := range []string{"delete", "rename", "kind"} {
				t.Run(fmt.Sprintf("execution=%v/%s/%s", execution, filter, change), func(t *testing.T) {
					r := indexedListingRoot(t, nil)
					f := &searchAccessFiles{listingFiles: &listingFiles{base: t.TempDir()}, checked: map[string]int{}}
					r.Files = f
					r.Config.Index = execution
					if execution {
						r.execution = &ExecutionIdentity{UID: 1001, GID: 100}
					}
					directory := filter == "directories"
					for _, name := range []string{"report-a", "report-b"} {
						var err error
						if directory {
							err = os.Mkdir(filepath.Join(f.base, name), 0700)
						} else {
							err = os.WriteFile(filepath.Join(f.base, name), nil, 0600)
						}
						if err != nil {
							t.Fatal(err)
						}
						value, _ := json.Marshal(claim{Device: 1, Inode: 1, Path: name})
						if err := r.State.Batch([]Change{{Key: "identity/" + name, Value: value}}); err != nil {
							t.Fatal(err)
						}
					}
					opts := ListingOptions{Sort: "relevance", Type: filter, Limit: 1}
					page, err := r.Search(context.Background(), "report", ".", opts)
					if err != nil || page.Next == "" || len(page.Items) != 1 || page.Items[0].Path != "report-a" {
						t.Fatalf("first: %+v %v", page, err)
					}
					p := filepath.Join(f.base, "report-b")
					if change == "rename" {
						err = os.Rename(p, filepath.Join(f.base, "renamed"))
					} else {
						err = os.Remove(p)
					}
					if err != nil {
						t.Fatal(err)
					}
					if change == "kind" {
						if directory {
							err = os.WriteFile(p, nil, 0600)
						} else {
							err = os.Mkdir(p, 0700)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					opts.After = page.Next
					page, err = r.Search(context.Background(), "report", ".", opts)
					if !errors.Is(err, ErrCursorInvalid) || len(page.Items) != 0 || page.Next != "" {
						t.Fatalf("changed live hit did not require restart: %+v %v", page, err)
					}
				})
			}
		}
	}
}

func TestSearchRelevanceCursorBindsEffectiveTopK(t *testing.T) {
	for _, tc := range []struct {
		label, sort   string
		before, after int
		invalid       bool
	}{
		{"changed relevance K", "relevance", 3, 1, true},
		{"same effective relevance K", "relevance", 0, DefaultSearchTopK, false},
		{"other sort ignores K", "name", 3, 1, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "docs/report-a"}, {Path: "docs/report-b"}, {Path: "docs/report-c"}})
			r.SearchTopK = tc.before
			opts := ListingOptions{Sort: tc.sort, Limit: 1}
			page, err := r.Search(context.Background(), "report", "docs", opts)
			if err != nil || page.Next == "" {
				t.Fatalf("first: %+v %v", page, err)
			}
			r.SearchTopK = tc.after
			opts.After = page.Next
			page, err = r.Search(context.Background(), "report", "docs", opts)
			if tc.invalid {
				if !errors.Is(err, ErrCursorInvalid) || len(page.Items) != 0 {
					t.Fatalf("cursor survived changed K: %+v %v", page, err)
				}
			} else if err != nil || len(page.Items) != 1 || page.Items[0].Path != "docs/report-b" {
				t.Fatalf("unchanged selection invalidated: %+v %v", page, err)
			}
		})
	}
}

func TestSearchRelevanceScanBudget(t *testing.T) {
	for _, reason := range []string{"server budget", "request deadline", "request cancellation", "server budget then request cancellation", "server budget then request deadline"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := indexedListingRoot(t, []Node{{Path: "report-a"}, {Path: "report-b"}})
				r.searchScanTimeout = time.Second
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := ErrLimit
				if reason == "request deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, r.searchScanTimeout/2)
					defer stop()
					want = context.DeadlineExceeded
				} else if reason == "request cancellation" {
					want = context.Canceled
				} else if reason == "server budget then request deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 3*r.searchScanTimeout/2)
					defer stop()
				}
				state := r.State.(*listingState)
				scanning, release := make(chan struct{}), make(chan struct{})
				state.onScan = func() { close(scanning); <-release }
				done := make(chan error, 1)
				go func() {
					page, err := r.Search(ctx, "report", ".", ListingOptions{Sort: "relevance", Limit: 1})
					if len(page.Items) != 0 || page.Next != "" {
						t.Errorf("expired search returned a partial page: %+v", page)
					}
					done <- err
				}()
				<-scanning
				if reason == "request cancellation" {
					cancel()
				} else {
					// Virtual time advances only while the scan is blocked by channels.
					<-time.After(2 * r.searchScanTimeout)
					if reason == "server budget then request cancellation" {
						cancel()
					}
				}
				close(release)
				if err := <-done; !errors.Is(err, want) {
					t.Fatalf("search error=%v want=%v", err, want)
				}
				if state.closed != 1 || len(r.listings) != 0 || r.listingBytes != 0 || len(r.searches) != 0 {
					t.Fatalf("expired search retained state: closed=%d cache=%d bytes=%d slots=%d", state.closed, len(r.listings), r.listingBytes, len(r.searches))
				}
				// Both admission slots remain usable after the failed scan.
				for range 2 {
					if err := r.admitSearch(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				<-r.searches
				<-r.searches
				state.onScan = nil
				if _, err := r.Search(context.Background(), "report", ".", ListingOptions{Sort: "relevance"}); err != nil {
					t.Fatalf("retry failed: %v", err)
				}
			})
		})
	}
}
