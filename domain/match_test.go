package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNameQueryMatch(t *testing.T) {
	for _, tc := range []struct {
		label, query, name         string
		fuzzy                      bool
		tier, rank, distance, span int
		word, accent               bool
	}{
		{label: "equal", query: "TEST", name: "test", tier: 1, word: true},
		{label: "prefix", query: "test", name: "testtest.ods", tier: 1, rank: exactPrefix, word: true},
		{label: "word", query: "test", name: "my_test.pdf", tier: 1, rank: exactWord, word: true},
		{label: "substring", query: "bericht", name: "Quartalsbericht.pdf", tier: 1, rank: exactSubstring},
		{label: "later word", query: "test", name: "mytest_test.pdf", tier: 1, rank: exactWord, word: true},
		{label: "order", query: "2026 bericht", name: "Quartalsbericht 2026.pdf", tier: 1, rank: exactWord, word: true},
		{label: "all required", query: "2026 missing", name: "Quartalsbericht 2026.pdf"},
		{label: "unicode whitespace", query: "\t2026\u2003bericht\n", name: "Quartalsbericht_2026.pdf", tier: 1, rank: exactWord, word: true},
		{label: "NFC", query: "Ä", name: "A\u0308pfel.txt", tier: 1, rank: exactPrefix, word: true},
		{label: "decomposed query", query: "A\u0308", name: "Äpfel.txt", tier: 1, rank: exactPrefix, word: true},
		{label: "full fold", query: "strasse", name: "Straße.pdf", tier: 1, rank: exactPrefix, word: true},
		{label: "unicode case", query: "ДОКУМЕНТ", name: "Документ.pdf", tier: 1, rank: exactPrefix, word: true},
		{label: "accent", query: "muller", name: "Müller.docx", tier: 1, rank: exactPrefix, word: true, accent: true},
		{label: "accent query", query: "müller", name: "Muller.docx", tier: 1, rank: exactPrefix, word: true, accent: true},
		{label: "same accent", query: "müller", name: "Müller.docx", tier: 1, rank: exactPrefix, word: true},
		{label: "prefix outranks unaccented word", query: "muller", name: "Müller_muller.docx", tier: 1, rank: exactPrefix, word: true, accent: true},
		{label: "unaccented same rank", query: "muller", name: "x_Müller_muller.docx", tier: 1, rank: exactWord, word: true},
		{label: "gapless", query: "quartalsbericht", name: "Quartals_Bericht.xlsx", tier: 1, rank: exactPrefix, word: true},
		{label: "all gaps", query: "quartalsbericht", name: "Quartals - .\t\u2003Bericht.xlsx", tier: 1, rank: exactPrefix, word: true},
		{label: "query separator", query: "quartals-bericht", name: "Quartals_Bericht.xlsx", tier: 1, rank: exactPrefix, word: true},
		{label: "separator token", query: "--", name: "x---", tier: 1, rank: exactWord, word: true},
		{label: "gapless word rank", query: "testtest", name: "my_test-test.pdf", tier: 1, rank: exactWord, word: true},
		{label: "gapless typo", query: "taxfrom", name: "tax-form.pdf", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "gapless typo word rank", query: "taxfrom", name: "my_tax-form.pdf", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "gapless loose word rank", query: "ab", name: "q_a---x---b", fuzzy: true, tier: 3, span: 3, word: true},
		{label: "folded gapless word rank", query: "testtest", name: "q_tést-test.pdf", tier: 1, rank: exactWord, word: true, accent: true},
		{label: "gapless query typo", query: "tax-frm", name: "taxform.pdf", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "gapless loose", query: "ab", name: "a---x---b", fuzzy: true, tier: 3, span: 3, word: true},
		{label: "folded gapless typo", query: "taxfrom", name: "täx-form.pdf", fuzzy: true, tier: 2, distance: 1, word: true, accent: true},
		{label: "gapless query loose", query: "ab-cd", name: "a1b2c3d", fuzzy: true, tier: 3, span: 7, word: true},
		{label: "exact no typo", query: "tsettest", name: "testtest.ods"},
		{label: "transpose first", query: "tsettest", name: "testtest.ods", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "transpose last", query: "testtets", name: "testtest.ods", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "insertion", query: "testest", name: "testtest.ods", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "delete first", query: "xabcd", name: "abcd.txt", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "delete last", query: "abcdx", name: "abcd", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "substitution", query: "abcd", name: "abxd.txt", fuzzy: true, tier: 2, distance: 1, word: true},
		{label: "two edits eight runes", query: "abcdefgh", name: "bacdefhg", fuzzy: true, tier: 2, distance: 2, word: true},
		{label: "two substitutions eight", query: "abcdefgh", name: "abxdefxh", fuzzy: true, tier: 2, distance: 2, word: true},
		{label: "two edits seven runes", query: "abcdefg", name: "abxdefx", fuzzy: true},
		{label: "three edits eight runes", query: "abcdefgh", name: "abxdyfxh", fuzzy: true},
		{label: "short no substitution", query: "abc", name: "abd", fuzzy: true},
		{label: "short no transpose", query: "abc", name: "acb", fuzzy: true},
		{label: "short no insertion", query: "abc", name: "axbc", fuzzy: true, tier: 3, span: 4, word: true},
		{label: "typo example tete", query: "tete", name: "testtest.ods", fuzzy: true, tier: 2, distance: 1},
		{label: "loose example tst", query: "tst", name: "testtest.ods", fuzzy: true, tier: 3, span: 4, word: true},
		{label: "loose one rune exact only", query: "a", name: "b", fuzzy: true},
		{label: "loose two runes", query: "ab", name: "axxxxb", fuzzy: true, tier: 3, span: 6, word: true},
		{label: "loose span limit", query: "ab", name: "axxxxxb", fuzzy: true},
		{label: "loose word preferred", query: "ab", name: "xaxb_aqqb", fuzzy: true, tier: 3, span: 4, word: true},
		{label: "loose best word span", query: "ab", name: "xaxb_aqqb_axb", fuzzy: true, tier: 3, span: 3, word: true},
		{label: "loose smaller interior span", query: "ab", name: "xaxxxbxaxb", fuzzy: true, tier: 3, span: 3},
		{label: "empty query", name: "anything", tier: 1},
		{label: "empty name", query: "abcd", fuzzy: true},
		{label: "long typo", query: strings.Repeat("ä", 64) + "b", name: strings.Repeat("Ä", 64) + "c", fuzzy: true, tier: 2, distance: 1, word: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			q, err := compileNameQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			q.fuzzy = tc.fuzzy
			got, score := q.match(tc.name)
			want := nameTokenMatch{tier: tc.tier, exactRank: tc.rank, distance: tc.distance, span: tc.span, wordStart: tc.word, accentFolded: tc.accent}
			if got != (tc.tier != 0) || score.tier != tc.tier || len(q.tokens) > 0 && score.tokens[0] != want {
				t.Fatalf("match(%q, %q) = %v, %+v; want tier=%d token=%+v", tc.query, tc.name, got, score, tc.tier, want)
			}
		})
	}
}

func TestNameQueryWorstTier(t *testing.T) {
	q, _ := compileNameQuery("2026 tst")
	q.fuzzy = true
	ok, score := q.match("testtest_2026.ods")
	if !ok || score.tier != nameLoose || score.tokens[0].tier != nameExact || score.tokens[1].tier != nameLoose {
		t.Fatalf("%v %+v", ok, score)
	}
}

func TestNameQueryTokenLimit(t *testing.T) {
	for _, count := range []int{0, 8, 9, 100} {
		q, err := compileNameQuery(strings.Repeat("word\u2003", count))
		if count > nameQueryTokenLimit {
			if q != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("%d: %v %v", count, q, err)
			}
		} else if err != nil || len(q.tokens) != count {
			t.Fatalf("%d: %v %v", count, q, err)
		}
	}
}

func TestNameQueryBufferReuse(t *testing.T) {
	for _, query := range []string{"tset 2026", "muller", "tst"} {
		q, _ := compileNameQuery(query)
		q.fuzzy = true
		for _, name := range []string{"long_test_2026.pdf", "Müller.docx", "x", "test_2026.pdf", "", "TEST_2026.PDF"} {
			fresh, _ := compileNameQuery(query)
			fresh.fuzzy = true
			got, score := q.match(name)
			want, wantScore := fresh.match(name)
			if got != want || score != wantScore {
				t.Fatalf("reused %q: %v %+v; want %v %+v", name, got, score, want, wantScore)
			}
		}
		for _, name := range []string{"test_2026.pdf", "Müller.docx", "Mu\u0308ller.docx"} {
			q.match(name)
			if allocations := testing.AllocsPerRun(100, func() { q.match(name) }); allocations != 0 {
				t.Fatalf("warm match for %q allocated %g", name, allocations)
			}
		}
	}
}

// Compare both edit budgets to full OSA over every contiguous fragment. This
// bypasses tier 1/3 and normalization so they cannot conceal a typo error.
func TestNameQueryAgainstOSA(t *testing.T) {
	names := []string{""}
	level := []string{""}
	for length := 1; length <= 5; length++ {
		var next []string
		for _, prefix := range level {
			for _, suffix := range []string{"a", "b", "_"} {
				next = append(next, prefix+suffix)
			}
		}
		names = append(names, next...)
		level = next
	}
	for length := 2; length <= 5; length++ {
		for bits := 0; bits < 1<<length; bits++ {
			token := make([]rune, length)
			for i := range token {
				token[i] = rune('a' + (bits>>i)&1)
			}
			for _, name := range names {
				runes := []rune(name)
				best := length
				for start := 0; start < len(runes); start++ {
					for end := start + 1; end <= len(runes); end++ {
						best = min(best, nameQueryOSADistance(token, runes[start:end]))
					}
				}
				for _, budget := range []int{1, 2} {
					score := typoNameToken(runes, token, budget, nil)
					// An empty fragment is irrelevant for real tokens (length >= 4, budget <= 2).
					want := best <= budget && len(runes) > 0
					if (score.tier != 0) != want || want && score.distance != best {
						t.Fatalf("budget=%d query=%q name=%q: %+v; want hit=%v distance=%d", budget, string(token), name, score, want, best)
					}
				}
			}
		}
	}
}

func nameQueryOSADistance(a, b []rune) int {
	var d [12][12]int
	for i := 0; i <= len(a); i++ {
		d[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

var nameQueryBenchmarkScore nameMatch

func BenchmarkNameQueryTiers(b *testing.B) {
	for _, tc := range []struct {
		label, query, name string
		tier               int
	}{
		{"exact", "bericht 2026", "Quartalsbericht_0123_2026.pdf", 1},
		{"accent", "muller", "Müller.docx", 1},
		{"typo1", "quartalsberihct", "Quartalsbericht_0123_2026.pdf", 2},
		{"typo2", "quatralsberihct", "Quartalsbericht_0123_2026.pdf", 2},
		{"loose", "tata", "t1a2t3a.ods", 3},
		{"loose-short", "tst", "testtest.ods", 3},
		{"miss", "inventarliste", "Quartalsbericht_0123_2026.pdf", 0},
	} {
		b.Run(tc.label, func(b *testing.B) {
			q, _ := compileNameQuery(tc.query)
			q.fuzzy = true
			_, score := q.match(tc.name)
			if score.tier != tc.tier {
				b.Fatalf("tier=%d want=%d", score.tier, tc.tier)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, nameQueryBenchmarkScore = q.match(tc.name)
			}
		})
	}
}
