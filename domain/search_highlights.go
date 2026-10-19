package domain

import (
	"bytes"
	"slices"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Normalization can compose several original runes or expand one (ß → ss).
// Each normalized rune maps to its original NFC segment's half-open rune range.
// Build this map only for retained/page hits, keeping the scoring hot path lean.
type highlightForm struct {
	text    []byte
	origins []MatchRange
	starts  []bool
}

func (q *nameQuery) highlightForms(name string) [4]highlightForm {
	var forms [4]highlightForm
	var it norm.Iter
	it.InitString(norm.NFC, name)
	folder := cases.Fold()
	original := 0
	word := true
	foldedWord := true
	for !it.Done() {
		begin := it.Pos()
		segment := it.Next()
		end := it.Pos()
		count := utf8.RuneCountInString(name[begin:end])
		origin := MatchRange{original, original + count}
		original += count
		cased := norm.NFC.String(folder.String(string(segment)))
		for _, r := range cased {
			gap := q.boundary(r)
			if gap {
				r = ' '
			}
			forms[0].text = utf8.AppendRune(forms[0].text, r)
			forms[0].origins = append(forms[0].origins, origin)
			if !gap {
				forms[1].text = utf8.AppendRune(forms[1].text, r)
				forms[1].origins = append(forms[1].origins, origin)
				forms[1].starts = append(forms[1].starts, word)
			}
			for _, fr := range norm.NFD.String(string(r)) {
				if unicode.Is(unicode.M, fr) {
					continue
				}
				forms[2].text = utf8.AppendRune(forms[2].text, fr)
				forms[2].origins = append(forms[2].origins, origin)
				if !gap {
					forms[3].text = utf8.AppendRune(forms[3].text, fr)
					forms[3].origins = append(forms[3].origins, origin)
					forms[3].starts = append(forms[3].starts, foldedWord)
				}
				foldedWord = gap
			}
			word = gap
		}
	}
	return forms
}
func (q *nameQuery) highlights(name string) []MatchRange {
	ok, m := q.match(name)
	ranges := []MatchRange{}
	if !ok {
		return ranges
	}
	forms := q.highlightForms(name)
	for i, t := range q.tokens {
		tokens := [4][]byte{t.text, t.compact, t.folded, t.foldedCompact}
		target := m.tokens[i]
		for f, form := range forms {
			if target.accentFolded != (f >= 2) {
				continue
			}
			token := tokens[f]
			var score nameTokenMatch
			if target.tier == nameExact {
				score = exactNameToken(form.text, token, form.starts)
			} else {
				score = q.fuzzyToken(form.text, []rune(string(token)), form.starts)
			}
			score.accentFolded = f >= 2
			if score != target {
				continue
			}
			positions := highlightPositions(form, token, target)
			for _, j := range positions {
				ranges = append(ranges, form.origins[j])
			}
			break
		}
	}
	slices.SortFunc(ranges, func(a, b MatchRange) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	merged := ranges[:0]
	for _, r := range ranges {
		if len(merged) > 0 && r[0] <= merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	return slices.Clone(merged)
}
func highlightPositions(form highlightForm, token []byte, target nameTokenMatch) []int {
	name := []rune(string(form.text))
	query := []rune(string(token))
	contiguous := func(start, end int) []int {
		out := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			out = append(out, i)
		}
		return out
	}
	switch target.tier {
	case nameExact:
		for offset := 0; offset < len(form.text); {
			index := bytes.Index(form.text[offset:], token)
			if index < 0 {
				break
			}
			index += offset
			start := utf8.RuneCount(form.text[:index])
			end := start + len(query)
			rank := exactSubstring
			if start == 0 {
				rank = exactPrefix
				if end == len(name) {
					rank = exactEqual
				}
			} else if nameRuneStart(name, form.starts, start) {
				rank = exactWord
			}
			if rank == target.exactRank {
				return contiguous(start, end)
			}
			offset = index + 1
		}
	case nameTypo:
		// Recover the consumed prefix for the same first best-distance start used
		// by the scorer. The bounded edit recursion follows osaPrefix's choices.
		for start := 0; start < len(name); start++ {
			if osaPrefix(name[start:], query, target.distance) == target.distance {
				end := osaPrefixEnd(name[start:], query, target.distance)
				return contiguous(start, start+end)
			}
		}
	case nameLoose:
		for start, r := range name {
			if r != query[0] || nameRuneStart(name, form.starts, start) != target.wordStart {
				continue
			}
			indices := []int{start}
			j := 1
			for end := start + 1; end < len(name) && end-start < 3*len(query); end++ {
				if name[end] != query[j] {
					continue
				}
				indices = append(indices, end)
				j++
				if j == len(query) {
					if end-start+1 == target.span {
						return indices
					}
					break
				}
			}
		}
	}
	return nil
}
func osaPrefixEnd(name, token []rune, budget int) int {
	consumed := 0
	for len(name) > 0 && len(token) > 0 && name[0] == token[0] {
		name, token = name[1:], token[1:]
		consumed++
	}
	if len(token) == 0 || len(name) == 0 {
		return consumed
	}
	distance := osaPrefix(name, token, budget)
	if len(name) > 1 && len(token) > 1 && name[0] == token[1] && name[1] == token[0] && 1+osaPrefix(name[2:], token[2:], budget-1) == distance {
		return consumed + 2 + osaPrefixEnd(name[2:], token[2:], budget-1)
	}
	if 1+osaPrefix(name, token[1:], budget-1) == distance {
		return consumed + osaPrefixEnd(name, token[1:], budget-1)
	}
	if 1+osaPrefix(name[1:], token, budget-1) == distance {
		return consumed + 1 + osaPrefixEnd(name[1:], token, budget-1)
	}
	return consumed + 1 + osaPrefixEnd(name[1:], token[1:], budget-1)
}
