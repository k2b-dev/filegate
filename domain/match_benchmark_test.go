package domain

import (
	"strings"
	"testing"
)

// sellersOSADistance is the conventional three-row approximate-substring OSA
// comparison. Row zero is free (a match can start anywhere). Saturating cells
// beyond k cuts off impossible prefixes; reaching minimumDistance ends the scan.
// Production typo tokens are bounded to 32 runes.
func sellersOSADistance(name, token []rune, budget, minimumDistance int) int {
	var rows [3][nameTypoRuneLimit + 1]int
	previous, current, older := &rows[0], &rows[1], &rows[2]
	for j := range len(token) + 1 {
		previous[j] = min(j, budget+1)
		older[j] = budget + 1
	}
	best := budget + 1
	for i, r := range name {
		current[0] = 0
		for j := 1; j <= len(token); j++ {
			cost := 1
			if r == token[j-1] {
				cost = 0
			}
			v := budget + 1
			if previous[j-1] <= budget || previous[j] <= budget || current[j-1] <= budget {
				v = min(v, previous[j-1]+cost, previous[j]+1, current[j-1]+1)
			}
			if i > 0 && j > 1 && r == token[j-2] && name[i-1] == token[j-1] {
				v = min(v, older[j-2]+1)
			}
			current[j] = v
		}
		best = min(best, current[len(token)])
		if best == minimumDistance {
			return best
		}
		older, previous, current = previous, current, older
	}
	return best
}

var benchmarkTypoDistance int

func BenchmarkTypoMatcher(b *testing.B) {
	for _, tc := range []struct{ label, query, name string }{
		{"typo1", "quartalsberihct", "quartalsbericht 0123 2026 pdf"},
		{"typo2", "quatralsberihct", "quartalsbericht 0123 2026 pdf"},
		{"miss", "inventarliste", "quartalsbericht 0123 2026 pdf"},
		{"invoice", "invocie", "invoice contract 2026 0012345 pdf"},
		{"path-typo", "tsettest", "testtest pdf"},
		{"adversarial", "abcdefghik", "abcdefghij"},
		{"repeated-prefix", strings.Repeat("a", 31) + "b", strings.Repeat("a", 255)},
	} {
		name, token := []rune(tc.name), []rune(tc.query)
		budget := nameTypoBudget(len(token))
		for _, algorithm := range []string{"prefix", "sellers"} {
			b.Run(tc.label+"/"+algorithm, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if algorithm == "prefix" {
						m := typoNameToken(name, token, budget, 1, nil)
						benchmarkTypoDistance = budget + 1
						if m.tier != 0 {
							benchmarkTypoDistance = m.distance
						}
					} else {
						benchmarkTypoDistance = sellersOSADistance(name, token, budget, 1)
					}
				}
			})
		}
	}
}
