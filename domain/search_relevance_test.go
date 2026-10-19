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
	} {
		t.Run(tc.label, func(t *testing.T) {
			var nodes []Node
			for i := len(tc.names) - 1; i >= 0; i-- {
				nodes = append(nodes, Node{Path: tc.names[i]})
			}
			r := indexedListingRoot(t, nodes)
			t.Cleanup(r.CloseListings)
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
	t.Cleanup(r.CloseListings)
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
			if len(s.hits) != 3 || len(s.entries) != 0 || s.reader == nil || s.bytes > 1024 {
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
			t.Cleanup(r.CloseListings)
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
	p, err := r.Search(context.Background(), "report", ".", opts)
	if err != nil || len(p.Items) != 1 || p.Items[0].Path != "report" || p.Next == "" || f.checked["report"] == 0 {
		t.Fatalf("first %+v %v", p, err)
	}
	if r.State.(*listingState).scans != 0 {
		t.Fatal("execution used index")
	}
	opts.After = p.Next
	f.denied = "report.pdf"
	p, err = r.Search(context.Background(), "report", ".", opts)
	if !errors.Is(err, os.ErrPermission) || len(p.Items) != 0 {
		t.Fatalf("revoked %+v %v", p, err)
	}
	f.denied = ""
	p, err = r.Search(context.Background(), "report", ".", opts)
	if err != nil || len(p.Items) != 1 || p.Items[0].Path != "report.pdf" || p.Next != "" {
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

func TestSearchRelevanceReaderRelease(t *testing.T) {
	for _, reason := range []string{"single page", "invalidation", "expiry", "eviction", "shutdown", "cancellation", "memory limit"} {
		t.Run(reason, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "report-a"}, {Path: "report-b"}})
			t.Cleanup(r.CloseListings)
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
			if state.closed != 0 {
				t.Fatal("cached reader closed")
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
			case "shutdown":
				r.CloseListings()
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

func TestSearchRelevanceIdleExpiry(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "report-a"}, {Path: "report-b"}})
	t.Cleanup(r.CloseListings)
	p, err := r.Search(context.Background(), "report", ".", ListingOptions{Sort: "relevance", Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("first: %+v %v", p, err)
	}
	closed := make(chan struct{})
	state := r.State.(*listingState)
	r.mu.Lock()
	state.onClose = func() { close(closed) }
	for _, s := range r.listings {
		s.timer.Reset(0)
	}
	r.mu.Unlock()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("idle observation did not release its reader")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if state.closed != 1 || len(r.listings) != 0 || r.listingBytes != 0 {
		t.Fatalf("idle reader retained: closed=%d cache=%d bytes=%d", state.closed, len(r.listings), r.listingBytes)
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
