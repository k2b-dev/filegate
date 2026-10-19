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

func TestSearchPathMatching(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		nodes := []Node{{Path: "docs/Rechnungen/2026/scan01.pdf"}, {Path: "docs/other/2026/scan02.pdf"}}
		var r *Root
		if indexed {
			r = indexedListingRoot(t, nodes)
		} else {
			files := &listingFiles{base: t.TempDir()}
			r = &Root{Config: RootConfig{Name: "test"}, Files: files, rootShared: &rootShared{now: func() time.Time { return time.Unix(100000, 0) }}}
			for _, n := range nodes {
				target := filepath.Join(files.base, n.Path)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, sortBy := range []string{"path", "name", "size", "modified", "relevance"} {
			for _, tc := range []struct {
				query, in string
				count     int
			}{
				{"rechnung 2026", "path", 1}, {"scan01 rechnung", "path", 1},
				{"2026rechnungen", "path", 0}, {"rechnungen2026", "path", 1},
				{"rechnungen/2026", "path", 1}, {"docs scan01", "path", 0},
				{"rechnung 2026", "name", 0}, {"rechnung 2026", "", 0},
			} {
				got := collectSearch(t, r, tc.query, "docs", ListingOptions{In: tc.in, Sort: sortBy, Type: "files", Limit: 1})
				if len(got) != tc.count || len(got) > 0 && got[0].Path != nodes[0].Path {
					t.Fatalf("indexed=%v sort=%s q=%q in=%q: %+v", indexed, sortBy, tc.query, tc.in, got)
				}
			}
		}
	}
}

func TestSearchPathRanking(t *testing.T) {
	for _, tc := range []struct{ query, a, b string }{
		// Basename wins even though it needs accent folding.
		{"muller", "x/Müller-long.pdf", "x/muller/a"},
		{"rechnung", "Archiv/Alte Rechnung.pdf", "Rechnungen/x.pdf"},
		{"report", "x/report.pdf", "report/z/myreport.pdf"},
		// Tier still precedes the basename preference.
		{"report", "report/notes.pdf", "notes/reprot.pdf"},
		// Same typo distance or loose sub-rank: basename wins before name length/path ties.
		{"tsettest", "x/testtest-long.pdf", "x/testtest/a"},
		{"tst", "x/testtest-long.pdf", "x/testtest/a"},
		// Count parent-only tokens across all tiers, not just the node's worst tier.
		{"2026 tst", "x/2026-testtest-long.pdf", "2026/x/testtest-a"},
	} {
		r := indexedListingRoot(t, []Node{{Path: tc.b}, {Path: tc.a}})
		got := collectSearch(t, r, tc.query, ".", ListingOptions{In: "path", Match: "fuzzy", Sort: "relevance", Limit: 1})
		if len(got) != 2 || got[0].Path != tc.a {
			t.Fatalf("q=%s: %+v", tc.query, got)
		}
	}
}

func TestSearchPathBasenamePrecedesSubrank(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "report/z/notes.txt"}, {Path: "a/report.pdf"}})
	got := collectSearch(t, r, "report", ".", ListingOptions{In: "path", Sort: "relevance", Type: "files"})
	if len(got) != 2 || got[0].Path != "a/report.pdf" {
		t.Fatalf("basename must precede whole-path prefix: %+v", got)
	}
}

func TestSearchPathBasenameScore(t *testing.T) {
	for _, tc := range []struct {
		query, candidate string
		rank             int
		accent           bool
	}{
		{"report", "report/z/myreport.pdf", exactSubstring, false},
		{"muller", "muller/Müller.pdf", exactPrefix, true},
	} {
		q, err := compileSearchQuery(tc.query, true)
		if err != nil {
			t.Fatal(err)
		}
		ok, m := q.match(tc.candidate)
		if !ok || m.parentOnly != 0 || m.tokens[0].exactRank != tc.rank || m.tokens[0].accentFolded != tc.accent {
			t.Fatalf("basename score for %q: %+v", tc.candidate, m)
		}
	}
}

func TestSearchPathHighlights(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "relevance"} {
		for _, tc := range []struct {
			query, path string
			want        []MatchRange
		}{
			{"muller 2026", "Müller/2026/scan.pdf", []MatchRange{{0, 6}, {7, 11}}},
			{"cafe strasse", "📄/Cafe\u0301/Straße.pdf", []MatchRange{{2, 7}, {8, 14}}},
			{"quartalsbericht", "Quartals/Bericht.pdf", []MatchRange{{0, 8}, {9, 16}}},
			{"tsettest", "x/testtest.pdf", []MatchRange{{2, 10}}},
			{"tst", "x/testtest.pdf", []MatchRange{{2, 3}, {4, 6}}},
			{"report", "report/z/myreport.pdf", []MatchRange{{11, 17}}},
			{"rechnung", "Archiv/Alte Rechnung.pdf", []MatchRange{{12, 20}}},
			{"muller", "muller/Müller.pdf", []MatchRange{{7, 13}}},
			{"tsettest", "testtest/é/testtest.pdf", []MatchRange{{11, 19}}},
			{"tst", "testtest/é/testtest.pdf", []MatchRange{{11, 12}, {13, 15}}},
			{"tsettest", "é/testtest/scan.pdf", []MatchRange{{2, 10}}},
			{"tst", "é/testtest/scan.pdf", []MatchRange{{2, 3}, {4, 6}}},
		} {
			r := indexedListingRoot(t, []Node{{Path: "docs/" + tc.path}})
			page, err := r.Search(context.Background(), tc.query, "docs", ListingOptions{In: "path", Match: "fuzzy", Sort: sortBy})
			if err != nil || len(page.Items) != 1 || page.Matches == nil || !reflect.DeepEqual(*page.Matches, [][]MatchRange{tc.want}) {
				t.Fatalf("sort=%s q=%s: %+v ranges=%v err=%v", sortBy, tc.query, page, page.Matches, err)
			}
		}
	}
}

func TestNameQueryPathSeparatorsStayOptIn(t *testing.T) {
	for _, inPath := range []bool{false, true} {
		q, err := compileSearchQuery("scan/01", inPath)
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := q.match("scan01.pdf"); ok != inPath {
			t.Fatalf("path=%v matched=%v", inPath, ok)
		}
	}
}

func TestSearchPathCursorAndValidation(t *testing.T) {
	defaults, _ := normalizeListing(ListingOptions{})
	explicit, _ := normalizeListing(ListingOptions{In: "name"})
	if queryFingerprint(".", "test", true, defaults, nil) != queryFingerprint(".", "test", true, explicit, nil) {
		t.Fatal("default differs from explicit name")
	}
	for _, in := range []string{"PATH", "unknown"} {
		r := indexedListingRoot(t, nil)
		if _, err := r.Search(context.Background(), "test", ".", ListingOptions{In: in}); !errors.Is(err, ErrInvalid) || r.State.(*listingState).scans != 0 {
			t.Fatalf("in=%s: %v", in, err)
		}
	}
	for _, sortBy := range []string{"path", "name", "relevance"} {
		for _, in := range []string{"name", "path"} {
			r := indexedListingRoot(t, []Node{{Path: "docs/test-a"}, {Path: "docs/test-b"}})
			opts := ListingOptions{In: in, Sort: sortBy, Limit: 1}
			first, err := r.Search(context.Background(), "test", "docs", opts)
			if err != nil || first.Next == "" {
				t.Fatalf("first: %+v %v", first, err)
			}
			opts.After = first.Next
			if _, err := r.Search(context.Background(), "test", "docs", opts); err != nil {
				t.Fatal(err)
			}
			if in == "name" {
				opts.In = "path"
			} else {
				opts.In = "name"
			}
			if _, err := r.Search(context.Background(), "test", "docs", opts); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("changed in accepted: %v", err)
			}
		}
	}
}

func TestSearchPathLongBaseAllocations(t *testing.T) {
	q, err := compileSearchQuery("test", true)
	if err != nil {
		t.Fatal(err)
	}
	q.base = strings.Repeat("base/", 100) + "scope"
	candidate := q.base + "/test.pdf"
	if got := q.subject(candidate); got != "test.pdf" {
		t.Fatal(got)
	}
	if allocations := testing.AllocsPerRun(10, func() { q.match(q.subject(candidate)) }); allocations != 0 {
		t.Fatalf("per-node scope allocations: %v", allocations)
	}
}

func TestSearchPathFuzzySegments(t *testing.T) {
	for _, tc := range []struct {
		query, candidate string
		tier             int
	}{
		{"taxform", "tax/form.pdf", nameExact},
		{"taxfrom", "tax/form.pdf", 0},
		{"tst", "t1/s2/t.pdf", 0},
		{"quartalsberihct", "Quartals_Bericht/scan.pdf", nameTypo},
		{"tsettest", strings.Repeat("x/", 1900) + "testtest.pdf", nameTypo},
		{"tst", strings.Repeat("é/", 1000) + "testtest.pdf", nameLoose},
		{"tsettest", strings.Repeat("x/", 1800) + strings.Repeat("é", 120) + "testtest.pdf", nameTypo},
		{"tst", strings.Repeat("x/", 1800) + strings.Repeat("a", 243) + "testtest.pdf", nameLoose},
		{"tsettest", "testtest/" + strings.Repeat("folder/", 50) + "scan.pdf", nameTypo},
		{"tst", "testtest/" + strings.Repeat("folder/", 50) + "scan.pdf", nameLoose},
	} {
		q, err := compileSearchQuery(tc.query, true)
		if err != nil {
			t.Fatal(err)
		}
		q.fuzzy = true
		ok, m := q.match(tc.candidate)
		if ok != (tc.tier != 0) || m.tier != tc.tier {
			t.Fatalf("query=%q candidate=%q: ok=%v score=%+v", tc.query, tc.candidate, ok, m)
		}
		if allocations := testing.AllocsPerRun(10, func() { q.match(tc.candidate) }); allocations != 0 {
			t.Fatalf("per-path allocations: %v", allocations)
		}
	}
}

func TestSearchPathFuzzyLongPathCoverage(t *testing.T) {
	for _, query := range []string{"testtest", "tst"} {
		candidate := strings.Repeat("folder/", 50) + "tsettest.pdf"
		r := indexedListingRoot(t, []Node{{Path: candidate}})
		for _, in := range []string{"name", "path"} {
			page, err := r.Search(context.Background(), query, ".", ListingOptions{In: in, Match: "fuzzy", Sort: "relevance"})
			if err != nil || len(page.Items) != 1 || page.Matches == nil || len((*page.Matches)[0]) == 0 {
				t.Fatalf("in=%s query=%q: %+v err=%v", in, query, page, err)
			}
			offset := 0
			if in == "path" {
				offset = len(strings.Repeat("folder/", 50))
			}
			for _, r := range (*page.Matches)[0] {
				if r[0] < offset || r[1] > offset+len("tsettest.pdf") {
					t.Fatalf("range outside basename: %v", r)
				}
			}
		}
	}
}

func TestSearchPathFiltersAndLooseSelection(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "tata/ignored.txt"}, {Path: "x/t1a2t3a.pdf"}})
	got := collectSearch(t, r, "tata", ".", ListingOptions{In: "path", Match: "fuzzy", Ext: []string{"pdf"}})
	if len(got) != 1 || got[0].Path != "x/t1a2t3a.pdf" {
		t.Fatalf("filters must precede tier selection: %+v", got)
	}
	r = indexedListingRoot(t, []Node{{Path: "tata/included.pdf"}, {Path: "x/t1a2t3a.pdf"}})
	got = collectSearch(t, r, "tata", ".", ListingOptions{In: "path", Match: "fuzzy"})
	if len(got) != 1 || got[0].Path != "tata/included.pdf" {
		t.Fatalf("strong parent hit must suppress loose: %+v", got)
	}
}

func TestSearchPathExecutionStaysLive(t *testing.T) {
	r := indexedListingRoot(t, []Node{{Path: "docs/parent/stale.pdf"}})
	f := &searchAccessFiles{listingFiles: &listingFiles{base: t.TempDir()}, checked: map[string]int{}}
	r.Files = f
	r.execution = &ExecutionIdentity{UID: 1001, GID: 100}
	if err := os.MkdirAll(filepath.Join(f.base, "docs/parent"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"docs", "docs/parent", "docs/parent/a.pdf", "docs/parent/b.pdf"} {
		if strings.HasSuffix(p, ".pdf") {
			if err := os.WriteFile(filepath.Join(f.base, p), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		value, _ := json.Marshal(claim{Device: 1, Inode: 1, Path: p})
		if err := r.State.Batch([]Change{{Key: "identity/" + filepath.Base(p), Value: value}}); err != nil {
			t.Fatal(err)
		}
	}
	opts := ListingOptions{In: "path", Sort: "relevance", Type: "files", Limit: 1}
	first, err := r.Search(context.Background(), "parent", "docs", opts)
	if err != nil || len(first.Items) != 1 || first.Items[0].Path != "docs/parent/a.pdf" || first.Next == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	if r.State.(*listingState).scans != 0 || f.checked[first.Items[0].Path] == 0 {
		t.Fatal("used index or skipped authorization")
	}
	opts.After = first.Next
	f.denied = "docs/parent/b.pdf"
	if page, err := r.Search(context.Background(), "parent", "docs", opts); !errors.Is(err, os.ErrPermission) || len(page.Items) != 0 {
		t.Fatalf("revoked: %+v %v", page, err)
	}
}

func BenchmarkSearchPathLong(b *testing.B) {
	candidate := strings.Repeat("folder/", 580) + "scan01.pdf"
	for _, query := range []string{"folder scan01", "tsettest", "tst"} {
		b.Run(query, func(b *testing.B) {
			q, err := compileSearchQuery(query, true)
			if err != nil {
				b.Fatal(err)
			}
			q.fuzzy = true
			q.match(candidate)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				q.match(candidate)
			}
		})
	}
}

func TestNameQueryExactUnicodeCursor(t *testing.T) {
	for _, tc := range []struct {
		query, name string
		rank        int
	}{
		{"ää", "xääxää_ää.pdf", exactWord},
		{"aa", "éaaaéaaa_aa.pdf", exactWord},
		{"ää", "xääääxää.pdf", exactSubstring},
	} {
		q, err := compileNameQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		ok, m := q.match(tc.name)
		if !ok || m.tokens[0].exactRank != tc.rank {
			t.Fatalf("query=%q name=%q: %+v", tc.query, tc.name, m)
		}
		// Check the compact form directly so the plain form cannot mask a
		// byte/rune cursor error at overlapping non-ASCII occurrences.
		compact := exactNameToken(q.compact, q.tokens[0].compact, q.compactStarts)
		if compact.tier != nameExact || compact.exactRank != tc.rank {
			t.Fatalf("compact query=%q name=%q: %+v", tc.query, tc.name, compact)
		}
	}
}

func BenchmarkSearchPath(b *testing.B) {
	for _, tc := range []struct{ label, query, candidate string }{
		{"typical-exact", "rechnung 2026", "Rechnungen/2026/scan01.pdf"},
		{"typical-typo", "tsettest", "reports/2026/testtest.pdf"},
		{"typical-loose", "tst", "reports/2026/testtest.pdf"},
		{"long-repeated-substring", "aa", strings.TrimSuffix(strings.Repeat("x"+strings.Repeat("a", 254)+"/", 16), "/")},
	} {
		b.Run(tc.label, func(b *testing.B) {
			q, err := compileSearchQuery(tc.query, true)
			if err != nil {
				b.Fatal(err)
			}
			q.fuzzy = true
			if ok, _ := q.match(tc.candidate); !ok {
				b.Fatal("candidate did not match")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, nameQueryBenchmarkScore = q.match(tc.candidate)
			}
		})
	}
}
