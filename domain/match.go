package domain

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const nameQueryTokenLimit = 8
const nameQueryFuzzyLimit = 64

type nameToken struct {
	text  string
	runes []rune // Only populated for fuzzy-eligible tokens (4–64 runes).
}

// nameQuery is compiled once and reused for basenames. It owns normalization and
// rune buffers, so it is not safe for concurrent use. Buffers grow as needed;
// matching reuses their capacity rather than allocating for every name.
type nameQuery struct {
	tokens     []nameToken
	normalized []byte
	lower      []byte
	runes      []rune
}

// nameMatch scores the best distance for each token. Distance is their sum
// (0–8); boundary means any token has a best-distance match at the name's start
// or after Unicode whitespace, '-', '_' or '.'. A failed match has a zero score.
type nameMatch struct {
	distance int
	boundary bool
}

// compileNameQuery rejects more than eight Unicode-whitespace-separated tokens
// with ErrInvalid, rather than silently broadening the query by dropping terms.
// Empty queries match every name with a zero score. Tokens longer than 64 runes
// remain exact-substring-only to bound fuzzy work per name.
func compileNameQuery(query string) (*nameQuery, error) {
	q := &nameQuery{}
	for field := range strings.FieldsSeq(query) {
		if len(q.tokens) == nameQueryTokenLimit {
			return nil, fmt.Errorf("%w: name query must have at most %d tokens", ErrInvalid, nameQueryTokenLimit)
		}
		text := string(q.normalize(field))
		token := nameToken{text: text}
		if n := utf8.RuneCountInString(text); n >= 4 && n <= nameQueryFuzzyLimit {
			token.runes = []rune(text)
		}
		q.tokens = append(q.tokens, token)
	}
	return q, nil
}

// normalize uses simple Unicode lowercase, preserving the existing search's
// case semantics (e.g. sharp s is not expanded to "ss"). NFC before and after
// lowercasing keeps canonically equivalent input identical and the result NFC.
func (q *nameQuery) normalize(name string) []byte {
	q.normalized = norm.NFC.AppendString(q.normalized[:0], name)
	q.lower = q.lower[:0]
	for _, r := range string(q.normalized) {
		q.lower = utf8.AppendRune(q.lower, unicode.ToLower(r))
	}
	q.normalized = norm.NFC.Append(q.normalized[:0], q.lower...)
	return q.normalized
}

func (q *nameQuery) match(name string) (bool, nameMatch) {
	if len(q.tokens) == 0 {
		return true, nameMatch{}
	}
	nameBytes := q.normalize(name)
	q.runes = q.runes[:0]
	score := nameMatch{}
	for _, token := range q.tokens {
		found, boundary := exactNameToken(nameBytes, token.text)
		if !found {
			if len(token.runes) == 0 {
				return false, nameMatch{}
			}
			if len(q.runes) == 0 {
				for _, r := range string(nameBytes) {
					q.runes = append(q.runes, r)
				}
			}
			found, boundary = fuzzyNameToken(q.runes, token.runes)
			if !found {
				return false, nameMatch{}
			}
			score.distance++
		}
		score.boundary = score.boundary || boundary
	}
	return true, score
}

func nameBoundary(previous rune) bool {
	return unicode.IsSpace(previous) || previous == '-' || previous == '_' || previous == '.'
}

func exactNameToken(name []byte, token string) (bool, bool) {
	found := false
	for offset := 0; offset < len(name); {
		i := bytes.Index(name[offset:], []byte(token))
		if i < 0 {
			break
		}
		i += offset
		if i == 0 {
			return true, true
		}
		previous, _ := utf8.DecodeLastRune(name[:i])
		if nameBoundary(previous) {
			return true, true
		}
		found = true
		// Advance one byte to retain overlapping occurrences; UTF-8 token starts
		// cannot match continuation bytes in a normalized name.
		offset = i + 1
	}
	return found, false
}

// fuzzyNameToken tests contiguous fragments in O(m*n), with m bounded to 64.
// After the first mismatch, the remaining suffix must match exactly under one
// deletion, insertion, substitution or adjacent transposition (OSA distance 1).
// The caller has already ruled out exact matches, so every hit costs one edit.
func fuzzyNameToken(name, token []rune) (bool, bool) {
	found := false
	for start := 0; start < len(name); start++ {
		i, j := start, 0
		for i < len(name) && j < len(token) && name[i] == token[j] {
			i++
			j++
		}
		matched := j == len(token) || runePrefix(name[i:], token[j+1:]) // Delete a query rune.
		if !matched && i < len(name) {
			matched = runePrefix(name[i+1:], token[j:]) || // Insert a name rune.
				runePrefix(name[i+1:], token[j+1:]) // Substitute a rune.
			if !matched && i+1 < len(name) && j+1 < len(token) && name[i] == token[j+1] && name[i+1] == token[j] {
				matched = runePrefix(name[i+2:], token[j+2:])
			}
		}
		if matched {
			if start == 0 || nameBoundary(name[start-1]) {
				return true, true
			}
			found = true
		}
	}
	return found, false
}

func runePrefix(name, prefix []rune) bool {
	if len(prefix) > len(name) {
		return false
	}
	for i, r := range prefix {
		if name[i] != r {
			return false
		}
	}
	return true
}
