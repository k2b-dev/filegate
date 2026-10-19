package domain

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const nameQueryTokenLimit = 8
const nameQueryByteLimit = 8 << 10
const nameTypoRuneLimit = 32

// Bound both typo recursion and loose subsequence work on path candidates.
const pathFuzzyRuneLimit = 256
const (
	nameExact = 1
	nameTypo  = 2
	nameLoose = 3
)
const (
	exactEqual = iota
	exactPrefix
	exactWord
	exactSubstring
)

type nameToken struct {
	text, compact, folded, foldedCompact                 []byte
	runes, compactRunes, foldedRunes, foldedCompactRunes []rune
}

// nameQuery is compiled once per selection. Its scratch buffers are reused per
// candidate; a query must not be shared by concurrent scans.
type nameQuery struct {
	pathSearch                            bool
	base                                  string
	tokens                                []nameToken
	fuzzy                                 bool
	folder                                cases.Caser
	nfc, cased                            []byte
	normalizer                            norm.Iter
	plain, compact, folded, foldedCompact []byte
	runes                                 []rune
	compactStarts, foldedCompactStarts    []bool
}

// Scores retain each token's best tier and sub-rank for relevance sorting.
// Exact sub-ranks and typo distances sort ascending. Loose word starts precede
// smaller spans; avoiding accent folding wins otherwise equal scores. The node's
// tier is the worst token tier. Ranges are recovered only for retained or
// returned hits.
type nameTokenMatch struct {
	tier, exactRank, distance, span int
	wordStart, accentFolded         bool
}
type nameMatch struct {
	tier       int
	parentOnly int
	tokens     [nameQueryTokenLimit]nameTokenMatch
}

func compileNameQuery(query string) (*nameQuery, error) {
	return compileSearchQuery(query, false)
}

func compileSearchQuery(query string, pathSearch bool) (*nameQuery, error) {
	if len(query) > nameQueryByteLimit {
		return nil, fmt.Errorf("%w: name query must be at most %d bytes", ErrInvalid, nameQueryByteLimit)
	}
	q := &nameQuery{folder: cases.Fold(), pathSearch: pathSearch}
	for field := range strings.FieldsSeq(query) {
		if len(q.tokens) == nameQueryTokenLimit {
			return nil, fmt.Errorf("%w: name query must have at most %d tokens", ErrInvalid, nameQueryTokenLimit)
		}
		q.normalize(field)
		token := nameToken{text: bytes.Clone(q.plain), compact: bytes.Clone(q.compact), folded: bytes.Clone(q.folded), foldedCompact: bytes.Clone(q.foldedCompact)}
		token.runes = []rune(string(token.text))
		token.compactRunes = []rune(string(token.compact))
		token.foldedRunes = []rune(string(token.folded))
		token.foldedCompactRunes = []rune(string(token.foldedCompact))
		q.tokens = append(q.tokens, token)
	}
	return q, nil
}

func nameBoundary(r rune) bool {
	return unicode.IsSpace(r) || r == '-' || r == '_' || r == '.'
}

func (q *nameQuery) boundary(r rune) bool {
	return nameBoundary(r) || q.pathSearch && r == '/'
}

// subject uses the normalized search base, without a leading slash. Node paths
// remain root-relative on the wire; only matching and highlight offsets change.
func (q *nameQuery) subject(p string) string {
	if !q.pathSearch {
		return path.Base(p)
	}
	if q.base != "." {
		// Callers have already checked that p is inside this base.
		return p[len(q.base)+1:]
	}
	return p
}

func (q *nameQuery) normalize(name string) {
	q.plain, q.compact = q.plain[:0], q.compact[:0]
	q.compactStarts = q.compactStarts[:0]
	wordStart := true
	// Most names are ASCII: full folding and NFC are equivalent to this loop.
	ascii := true
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		for i := 0; i < len(name); i++ {
			c := name[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if q.boundary(rune(c)) {
				q.plain = append(q.plain, ' ')
				wordStart = true
			} else {
				q.plain = append(q.plain, c)
				q.compact = append(q.compact, c)
				q.compactStarts = append(q.compactStarts, wordStart)
				wordStart = false
			}
		}
		q.folded = append(q.folded[:0], q.plain...)
		q.foldedCompact = append(q.foldedCompact[:0], q.compact...)
		q.foldedCompactStarts = append(q.foldedCompactStarts[:0], q.compactStarts...)
		return
	}
	q.normalizer.InitString(norm.NFC, name)
	q.nfc = q.nfc[:0]
	for !q.normalizer.Done() {
		q.nfc = append(q.nfc, q.normalizer.Next()...)
	}
	// Fold accepts all input; Append grows and reuses cased when folding expands.
	q.cased, _, _ = transform.Append(&q.folder, q.cased[:0], q.nfc)
	q.normalizer.Init(norm.NFC, q.cased)
	q.nfc = q.nfc[:0]
	for !q.normalizer.Done() {
		q.nfc = append(q.nfc, q.normalizer.Next()...)
	}
	for remaining := q.nfc; len(remaining) > 0; {
		r, size := utf8.DecodeRune(remaining)
		remaining = remaining[size:]
		if q.boundary(r) {
			q.plain = append(q.plain, ' ')
			wordStart = true
		} else {
			q.plain = utf8.AppendRune(q.plain, r)
			q.compact = utf8.AppendRune(q.compact, r)
			q.compactStarts = append(q.compactStarts, wordStart)
			wordStart = false
		}
	}
	q.normalizer.Init(norm.NFD, q.plain)
	q.folded, q.foldedCompact = q.folded[:0], q.foldedCompact[:0]
	q.foldedCompactStarts = q.foldedCompactStarts[:0]
	wordStart = true
	for !q.normalizer.Done() {
		for _, r := range string(q.normalizer.Next()) {
			if unicode.Is(unicode.M, r) {
				continue
			}
			q.folded = utf8.AppendRune(q.folded, r)
			if r != ' ' {
				q.foldedCompact = utf8.AppendRune(q.foldedCompact, r)
				q.foldedCompactStarts = append(q.foldedCompactStarts, wordStart)
				wordStart = false
			} else {
				wordStart = true
			}
		}
	}
}

func (q *nameQuery) match(name string) (bool, nameMatch) {
	score := nameMatch{tier: nameExact}
	if len(q.tokens) == 0 {
		return true, score
	}
	q.normalize(name)
	for i, token := range q.tokens {
		best := q.matchToken(token)
		if best.tier == 0 {
			return false, nameMatch{}
		}
		score.tokens[i] = best
		score.tier = max(score.tier, best.tier)
	}
	if q.pathSearch {
		q.normalize(path.Base(name))
		for i, token := range q.tokens {
			// A weaker basename tier cannot promote a stronger parent hit.
			basename := q.matchToken(token)
			if basename.tier == 0 || basename.tier > score.tokens[i].tier {
				score.parentOnly++
			}
		}
	}
	return true, score
}

func (q *nameQuery) matchToken(token nameToken) nameTokenMatch {
	best := exactNameToken(q.plain, token.text, nil)
	best = betterNameMatch(best, exactNameToken(q.compact, token.compact, q.compactStarts))
	accent := !bytes.Equal(q.plain, q.folded) || !bytes.Equal(token.text, token.folded)
	if accent {
		folded := betterNameMatch(exactNameToken(q.folded, token.folded, nil), exactNameToken(q.foldedCompact, token.foldedCompact, q.foldedCompactStarts))
		folded.accentFolded = true
		best = betterNameMatch(best, folded)
	}
	if best.tier == 0 && q.fuzzy {
		best = q.fuzzyToken(q.plain, token.runes, nil)
		// Exact matching failed for every form. A distance-one typo
		// without accent folding cannot be improved by another form.
		if (best.tier != nameTypo || best.distance > 1) && (len(q.compact) != len(q.plain) || !bytes.Equal(token.compact, token.text)) {
			best = betterNameMatch(best, q.fuzzyToken(q.compact, token.compactRunes, q.compactStarts))
		}
		if accent && (best.tier != nameTypo || best.distance > 1) {
			folded := q.fuzzyToken(q.folded, token.foldedRunes, nil)
			if (folded.tier != nameTypo || folded.distance > 1) && (len(q.foldedCompact) != len(q.folded) || !bytes.Equal(token.foldedCompact, token.folded)) {
				folded = betterNameMatch(folded, q.fuzzyToken(q.foldedCompact, token.foldedCompactRunes, q.foldedCompactStarts))
			}
			folded.accentFolded = true
			best = betterNameMatch(best, folded)
		}
	}
	return best
}

func betterNameMatch(a, b nameTokenMatch) nameTokenMatch {
	if b.tier == 0 {
		return a
	}
	if a.tier == 0 {
		return b
	}
	if a.tier != b.tier {
		if b.tier < a.tier {
			return b
		}
		return a
	}
	switch a.tier {
	case nameExact:
		if a.exactRank != b.exactRank {
			if b.exactRank < a.exactRank {
				return b
			}
			return a
		}
	case nameTypo:
		if a.distance != b.distance {
			if b.distance < a.distance {
				return b
			}
			return a
		}
	case nameLoose:
		if a.wordStart != b.wordStart {
			if b.wordStart {
				return b
			}
			return a
		}
		if a.span != b.span {
			if b.span < a.span {
				return b
			}
			return a
		}
	}
	if a.accentFolded && !b.accentFolded {
		return b
	}
	return a
}

// Count directly: utf8.RuneCount's byte-to-string slow path allocates on
// long non-ASCII inputs with the supported Go toolchain.
func nameRuneCount(text []byte) int {
	count := 0
	for len(text) > 0 {
		_, size := utf8.DecodeRune(text)
		text = text[size:]
		count++
	}
	return count
}

func exactNameToken(name, token []byte, starts []bool) nameTokenMatch {
	if len(token) == 0 {
		return nameTokenMatch{}
	}
	if bytes.Equal(name, token) {
		return nameTokenMatch{tier: nameExact, exactRank: exactEqual, wordStart: true}
	}
	if bytes.HasPrefix(name, token) {
		return nameTokenMatch{tier: nameExact, exactRank: exactPrefix, wordStart: true}
	}
	best := nameTokenMatch{}
	for offset := 0; offset < len(name); {
		i := bytes.Index(name[offset:], token)
		if i < 0 {
			break
		}
		i += offset
		previous, _ := utf8.DecodeLastRune(name[:i])
		wordStart := nameBoundary(previous)
		if starts != nil {
			wordStart = starts[nameRuneCount(name[:i])]
		}
		if wordStart {
			return nameTokenMatch{tier: nameExact, exactRank: exactWord, wordStart: true}
		}
		best = nameTokenMatch{tier: nameExact, exactRank: exactSubstring}
		offset = i + 1
	}
	return best
}

func (q *nameQuery) fuzzyToken(name []byte, token []rune, starts []bool) nameTokenMatch {
	// Exact matching still scans the entire path. Each normalized form exceeding
	// this limit skips fuzzy matching before growing scratch or edit recursion.
	if q.pathSearch && nameRuneCount(name) > pathFuzzyRuneLimit {
		return nameTokenMatch{}
	}
	q.runes = q.runes[:0]
	var present uint64
	for len(name) > 0 {
		r, size := utf8.DecodeRune(name)
		name = name[size:]
		q.runes = append(q.runes, r)
		present |= 1 << (uint32(r) % 64)
	}
	budget := 0
	if len(token) <= nameTypoRuneLimit {
		if len(token) >= 8 {
			budget = 2
		} else if len(token) >= 4 {
			budget = 1
		}
	}
	if len(token) > len(q.runes)+budget {
		return nameTokenMatch{}
	}
	// Missing-rune count is a safe lower bound on edits. Hash collisions only
	// admit extra work; they never reject a possible match.
	missing := 0
	for _, r := range token {
		if present&(1<<(uint32(r)%64)) == 0 {
			missing++
		}
		if missing > budget {
			return nameTokenMatch{}
		}
	}
	if budget > 0 {
		if score := typoNameToken(q.runes, token, budget, 1, starts); score.tier != 0 {
			return score
		}
	}
	if missing != 0 {
		return nameTokenMatch{}
	}
	return looseNameToken(q.runes, token, starts)
}

// Each start explores at most four edit choices at each of at most two edits.
// Matching prefixes are consumed without branching. O(n*m*4^k), k <= 2, constant
// stack space, no matrix or per-name allocations. The caller limits typo tokens
// to nameTypoRuneLimit runes; longer tokens still use exact and loose matching.
// minimumDistance is one after all exact forms miss, or zero for a general scan.
func typoNameToken(name, token []rune, budget, minimumDistance int, starts []bool) nameTokenMatch {
	best := nameTokenMatch{}
	for start := 0; start < len(name); start++ {
		if len(name)-start < len(token)-budget {
			break
		}
		distance := osaPrefix(name[start:], token, budget)
		if distance <= budget {
			candidate := nameTokenMatch{tier: nameTypo, distance: distance, wordStart: nameRuneStart(name, starts, start)}
			best = betterNameMatch(best, candidate)
			if distance == minimumDistance {
				break
			}
		}
	}
	return best
}

// osaPrefix computes the distance to any prefix of name. Consuming both runes
// on transposition prevents editing them again (optimal string alignment).
func osaPrefix(name, token []rune, budget int) int {
	for len(name) > 0 && len(token) > 0 && name[0] == token[0] {
		name, token = name[1:], token[1:]
	}
	if len(token) == 0 {
		return 0
	}
	if len(name) == 0 {
		return len(token)
	}
	if budget == 0 || len(token) > len(name)+budget {
		return budget + 1
	}
	best := budget + 1
	if len(name) > 1 && len(token) > 1 && name[0] == token[1] && name[1] == token[0] {
		best = 1 + osaPrefix(name[2:], token[2:], budget-1)
		if best == 1 {
			return best
		}
	}
	best = min(best, 1+osaPrefix(name, token[1:], budget-1)) // Delete query rune.
	if best == 1 {
		return best
	}
	best = min(best, 1+osaPrefix(name[1:], token, budget-1)) // Insert name rune.
	if best == 1 {
		return best
	}
	best = min(best, 1+osaPrefix(name[1:], token[1:], budget-1))
	return best
}

// Greedy completion gives the smallest span for each start. The span cap bounds
// each attempt to 3*m runes; a short name or missing rune fails without branching.
func looseNameToken(name, token []rune, starts []bool) nameTokenMatch {
	if len(token) < 2 || len(token) > len(name) {
		return nameTokenMatch{}
	}
	best := nameTokenMatch{}
	for start, r := range name {
		if r != token[0] {
			continue
		}
		j := 1
		for end := start + 1; end < len(name) && end-start < 3*len(token); end++ {
			if name[end] != token[j] {
				continue
			}
			j++
			if j == len(token) {
				best = betterNameMatch(best, nameTokenMatch{tier: nameLoose, wordStart: nameRuneStart(name, starts, start), span: end - start + 1})
				break
			}
		}
	}
	return best
}

// Compact forms retain the original word starts even after removing gaps.
func nameRuneStart(name []rune, starts []bool, start int) bool {
	if starts != nil {
		return starts[start]
	}
	return start == 0 || nameBoundary(name[start-1])
}
