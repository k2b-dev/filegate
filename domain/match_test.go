package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNameQueryMatch(t *testing.T) {
	for _, tc := range []struct {
		label, query, name string
		matched            bool
		distance           int
		boundary           bool
	}{
		{"exact", "testtest", "testtest.ods", true, 0, true},
		{"partial", "testt", "testtest.ods", true, 0, true},
		{"case", "TESTTEST", "testtest.ods", true, 0, true},
		{"insertion", "testest", "testtest.ods", true, 1, true},
		{"transpose first", "tsettest", "testtest.ods", true, 1, true},
		{"transpose last", "testtets", "testtest.ods", true, 1, true},
		{"terms", "bericht 2026", "Quartalsbericht 2026.pdf", true, 0, true},
		{"reordered terms", "2026 bericht", "Quartalsbericht 2026.pdf", true, 0, true},
		{"unicode whitespace", "\tbericht\u2003\n2026\u00a0", "Quartalsbericht 2026.pdf", true, 0, true},
		{"all terms required", "bericht 2027 missing", "Quartalsbericht 2026.pdf", false, 0, false},
		{"sum distances", "tsettest 2027", "testtest_2026.pdf", true, 2, true},
		{"composed query", "Ä", "A\u0308pfel.txt", true, 0, true},
		{"decomposed query", "A\u0308", "äpfel.txt", true, 0, true},
		{"unicode lowercase", "ДОКУМЕНТ", "Документ.pdf", true, 0, true},
		{"simple lowercase", "strasse", "Straße.pdf", false, 0, false},
		{"short exact", "abc", "abc.txt", true, 0, true},
		{"short substitution", "abc", "abd", false, 0, false},
		{"short transposition", "abc", "acb", false, 0, false},
		{"short runes not bytes", "äöü", "äöx", false, 0, false},
		{"unicode fuzzy", "äöüß", "äöxß.txt", true, 1, true},
		{"substitution", "abcd", "abxd.txt", true, 1, true},
		{"deletion", "abcde", "abde.txt", true, 1, true},
		{"delete first", "xabcd", "abcd.txt", true, 1, true},
		{"delete last", "abcdx", "abcd", true, 1, true},
		{"insert first", "abcd", "xabcd", true, 0, false},
		{"insert inside", "abcd", "abxcd.txt", true, 1, true},
		{"two substitutions", "abcd", "axyd", false, 0, false},
		{"two transpositions", "abcdefgh", "bacdefhg", false, 0, false},
		{"noncontiguous", "abcd", "a---b---c---d", false, 0, false},
		{"empty query", "", "anything", true, 0, false},
		{"whitespace query", "\u2003 \t\n", "anything", true, 0, false},
		{"empty both", "", "", true, 0, false},
		{"empty name", "abcd", "", false, 0, false},
		{"too short name", "abcd", "ab", false, 0, false},
		{"interior", "bericht", "Quartalsbericht.pdf", true, 0, false},
		{"space boundary", "test", "my test.pdf", true, 0, true},
		{"hyphen boundary", "test", "my-test.pdf", true, 0, true},
		{"underscore boundary", "test", "my_test.pdf", true, 0, true},
		{"dot boundary", "test", "my.test.pdf", true, 0, true},
		{"unicode boundary", "test", "my\u2003test.pdf", true, 0, true},
		{"camel case optional", "test", "myTest.pdf", true, 0, false},
		{"later exact boundary", "test", "mytest-test.pdf", true, 0, true},
		{"overlapping exact boundary", "--", "x---", true, 0, true},
		{"fuzzy boundary", "tset", "my_test.pdf", true, 1, true},
		{"fuzzy interior", "tset", "mytest.pdf", true, 1, false},
		{"later fuzzy boundary", "tset", "mytest_test.pdf", true, 1, true},
		{"exact takes precedence", "test", "mytest_tset.pdf", true, 0, false},
		{"64 rune fuzzy", strings.Repeat("ä", 63) + "b", strings.Repeat("ä", 63) + "c", true, 1, true},
		{"long exact", strings.Repeat("ä", 65), strings.Repeat("Ä", 65), true, 0, true},
		{"long substring only", strings.Repeat("ä", 64) + "b", strings.Repeat("ä", 64) + "c", false, 0, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			q, err := compileNameQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			matched, score := q.match(tc.name)
			if matched != tc.matched || score != (nameMatch{distance: tc.distance, boundary: tc.boundary}) {
				t.Fatalf("match(%q, %q) = %v, %+v; want %v, distance=%d boundary=%v", tc.query, tc.name, matched, score, tc.matched, tc.distance, tc.boundary)
			}
		})
	}
}

func TestNameQueryTokenLimit(t *testing.T) {
	for _, count := range []int{0, 8, 9, 100} {
		q, err := compileNameQuery(strings.Repeat("word\u2003", count))
		if count > nameQueryTokenLimit {
			if q != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("%d tokens: query=%v error=%v; want nil, ErrInvalid", count, q, err)
			}
		} else if err != nil || len(q.tokens) != count {
			t.Fatalf("%d tokens: query=%v error=%v", count, q, err)
		}
	}
}

func TestNameQueryBufferReuse(t *testing.T) {
	q, err := compileNameQuery("tset 2026")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"long_test_2026.pdf", "x", "test_2026.pdf", "", "TEST_2026.PDF"} {
		fresh, err := compileNameQuery("tset 2026")
		if err != nil {
			t.Fatal(err)
		}
		got, score := q.match(name)
		want, wantScore := fresh.match(name)
		if got != want || score != wantScore {
			t.Fatalf("reused buffers for %q: %v, %+v; want %v, %+v", name, got, score, want, wantScore)
		}
	}
	if allocations := testing.AllocsPerRun(100, func() { q.match("test_2026.pdf") }); allocations != 0 {
		t.Fatalf("warm ASCII fuzzy match allocated %g times", allocations)
	}
}

// Compare the optimized single-edit check against full OSA distance on every
// contiguous fragment. Exhaustive small strings exercise repeated characters,
// alignments at either end, and all four edits without sharing matcher logic.
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
	for length := 4; length <= 5; length++ {
		for bits := 0; bits < 1<<length; bits++ {
			token := make([]rune, length)
			for i := range token {
				token[i] = rune('a' + (bits>>i)&1)
			}
			q, err := compileNameQuery(string(token))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				runes := []rune(name)
				best, boundary := 2, false
				for start := 0; start < len(runes); start++ {
					for end := start + 1; end <= len(runes); end++ {
						distance := nameQueryOSADistance(token, runes[start:end])
						atBoundary := start == 0 || runes[start-1] == '_'
						if distance < best {
							best, boundary = distance, atBoundary
						} else if distance == best {
							boundary = boundary || atBoundary
						}
					}
				}
				matched, score := q.match(name)
				want := nameMatch{}
				if best <= 1 {
					want = nameMatch{distance: best, boundary: boundary}
				}
				if matched != (best <= 1) || score != want {
					t.Fatalf("query=%q name=%q: got %v, %+v; want %v, %+v", string(token), name, matched, score, best <= 1, want)
				}
			}
		}
	}
}

func nameQueryOSADistance(a, b []rune) int {
	var d [6][6]int
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

var nameQueryBenchmarkMatches int

func BenchmarkNameQuery(b *testing.B) {
	names := make([]string, 3000)
	for i := range names {
		stem := []string{"Quartalsbericht", "Quartalsberihct", "Inventarliste"}[i%3]
		names[i] = fmt.Sprintf("%s_%04d_2026.pdf", stem, i)
	}
	q, err := compileNameQuery("bericht 2026")
	if err != nil {
		b.Fatal(err)
	}
	for _, name := range names {
		q.match(name)
	}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	matches := 0
	for i := 0; i < b.N; i++ {
		for _, name := range names {
			if matched, _ := q.match(name); matched {
				matches++
			}
		}
	}
	b.ReportMetric(float64(time.Since(start).Nanoseconds())/float64(b.N*len(names)), "ns/name")
	nameQueryBenchmarkMatches = matches
}
