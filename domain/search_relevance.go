package domain

import (
	"cmp"
	"container/heap"
	"context"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// DefaultSearchTopK bounds retained relevance hits unless the server overrides it.
const DefaultSearchTopK = 1000

// Token sub-ranks are summed only within the node's worst tier. Loose scores
// first count non-word-start tokens, then sum spans. Path searches next prefer
// fewer tokens without a basename hit at their selected tier or better.
// Any accent folding loses
// an otherwise equal comparison, followed by basename length and path ties.
type relevanceScore struct {
	tier, rank, span, parentOnly int
	accent                       bool
	nameLength, segments         int
}
type searchHit struct {
	path      string
	directory bool
	score     relevanceScore
	ranges    []MatchRange
}

func scoreName(p string, m nameMatch) relevanceScore {
	s := relevanceScore{tier: m.tier, parentOnly: m.parentOnly, nameLength: utf8.RuneCountInString(path.Base(p)), segments: strings.Count(p, "/")}
	for _, t := range m.tokens {
		s.accent = s.accent || t.accentFolded
		if t.tier != m.tier {
			continue
		}
		switch t.tier {
		case nameExact:
			s.rank += t.exactRank
		case nameTypo:
			s.rank += t.distance
		case nameLoose:
			if !t.wordStart {
				s.rank++
			}
			s.span += t.span
		}
	}
	return s
}
func compareSearchHit(a, b searchHit) int {
	for _, c := range []int{cmp.Compare(a.score.tier, b.score.tier), cmp.Compare(a.score.rank, b.score.rank), cmp.Compare(a.score.span, b.score.span), cmp.Compare(a.score.parentOnly, b.score.parentOnly)} {
		if c != 0 {
			return c
		}
	}
	if a.score.accent != b.score.accent {
		if a.score.accent {
			return 1
		}
		return -1
	}
	for _, c := range []int{cmp.Compare(a.score.nameLength, b.score.nameLength), cmp.Compare(a.score.segments, b.score.segments)} {
		if c != 0 {
			return c
		}
	}
	return strings.Compare(a.path, b.path)
}

// The heap's root is the worst retained hit; replacing it keeps O(K) storage.
type searchHeap []searchHit

func (h searchHeap) Len() int           { return len(h) }
func (h searchHeap) Less(i, j int) bool { return compareSearchHit(h[i], h[j]) > 0 }
func (h searchHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *searchHeap) Push(x any)        { *h = append(*h, x.(searchHit)) }
func (h *searchHeap) Pop() any          { a := *h; x := a[len(a)-1]; *h = a[:len(a)-1]; return x }
func hitBytes(h searchHit) int64        { return int64(len(h.path) + 104 + len(h.ranges)*16) }
func collectRelevance(ctx context.Context, o ListingOptions, query string, matches func(Node) nameMatch, scan func(func(Node) error) error, matcher *nameQuery) (*listingSnapshot, error) {
	s := &listingSnapshot{query: query, hits: []searchHit{}}
	h := searchHeap{}
	err := scan(func(n Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := matches(n)
		if m.tier == 0 {
			return nil
		}
		s.total++
		candidate := searchHit{path: n.Path, directory: n.Directory, score: scoreName(n.Path, m)}
		if len(h) == o.topK && compareSearchHit(candidate, h[0]) >= 0 {
			return nil
		}
		// Indexed relevance scans bound retained hits rather than scanned keys.
		// A tighter request budget still fails instead of silently reducing K.
		if len(h) < o.topK && len(h) >= o.MaxEntries {
			return ErrLimit
		}
		candidate.ranges = matcher.highlights(matcher.subject(n.Path))
		if len(h) == o.topK {
			s.bytes -= hitBytes(h[0])
			h[0] = candidate
			heap.Fix(&h, 0)
		} else {
			heap.Push(&h, candidate)
		}
		s.bytes += hitBytes(candidate)
		if s.bytes > listingMemoryLimit {
			return ErrLimit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(h, compareSearchHit)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.hits = slices.Clone([]searchHit(h))
	return s, nil
}
