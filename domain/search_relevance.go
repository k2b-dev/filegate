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

// Path searches prefer fewer parent-only tokens directly after the worst tier.
// Token sub-ranks are summed within that tier; loose scores count non-word starts
// before summing spans. Accent folding, basename length and path break ties.
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
	for _, c := range []int{cmp.Compare(a.score.tier, b.score.tier), cmp.Compare(a.score.parentOnly, b.score.parentOnly), cmp.Compare(a.score.rank, b.score.rank), cmp.Compare(a.score.span, b.score.span)} {
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
func collectRelevance(ctx context.Context, o ListingOptions, query string, scan func(func(listingCandidate) error) error, matcher *nameQuery) (*listingSnapshot, error) {
	s := &listingSnapshot{query: query, hits: []searchHit{}}
	h := searchHeap{}
	err := scan(func(candidate listingCandidate) error {
		n := candidate.node
		if err := ctx.Err(); err != nil {
			return err
		}
		m := candidate.match
		if m.tier == 0 {
			return nil
		}
		s.total++
		hit := searchHit{path: n.Path, directory: n.Directory, score: scoreName(n.Path, m)}
		if len(h) == o.topK && compareSearchHit(hit, h[0]) >= 0 {
			return nil
		}
		// Indexed relevance scans bound retained hits rather than scanned keys.
		// A tighter request budget still fails instead of silently reducing K.
		if len(h) < o.topK && len(h) >= o.MaxEntries {
			return ErrLimit
		}
		if len(h) == o.topK {
			s.bytes -= hitBytes(h[0])
			h[0] = hit
			heap.Fix(&h, 0)
		} else {
			heap.Push(&h, hit)
		}
		s.bytes += hitBytes(hit)
		if s.bytes > listingMemoryLimit {
			return ErrLimit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(h, compareSearchHit)
	// Recover highlights only for the final K, after heap replacements finish.
	// Highlight normalization is more expensive than scoring and discarded hits
	// never contribute ranges to a response or continuation.
	for i := range h {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h[i].ranges = matcher.highlights(matcher.subject(h[i].path))
		s.bytes += int64(len(h[i].ranges) * 16)
		if s.bytes > listingMemoryLimit {
			return nil, ErrLimit
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.hits = slices.Clone([]searchHit(h))
	return s, nil
}
