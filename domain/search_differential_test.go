package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIndexedSearchDecodeFirstDifferential(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 123, time.UTC)
	r := indexedListingRoot(t, searchDifferentialNodes(at))
	r.now = func() time.Time { return at }
	r.SearchTopK = 11 // Exercise truncation and several relevance cursor pages.
	for _, tc := range []struct {
		name, query, base string
		opts              ListingOptions
	}{
		{"all", "", ".", ListingOptions{}},
		{"accents", "resume", ".", ListingOptions{}},
		{"typo", "resuem", ".", ListingOptions{Match: "fuzzy"}},
		{"sharp-s", "strasse", ".", ListingOptions{}},
		{"gaps", "quartalsbericht", ".", ListingOptions{}},
		{"path-tokens", "muller resume", ".", ListingOptions{Match: "fuzzy", In: "path"}},
		{"scoped-path", "resume", "docs/Müller", ListingOptions{In: "path"}},
		{"scoped-sibling", "resume", "docs", ListingOptions{In: "path"}},
		{"base-excluded", "docs", "docs", ListingOptions{In: "path"}},
		{"extensions", "", ".", ListingOptions{Ext: []string{"PDF", "gz", "SS"}}},
		{"metadata", "resume", ".", ListingOptions{Match: "fuzzy", Type: "files", Ext: []string{"pdf", "gz"}, MinSize: filterSize(128), MaxSize: filterSize(2048), ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(4 * time.Hour))}},
		{"directories", "", ".", ListingOptions{Type: "directories", ModifiedAfter: &at, ModifiedBefore: filterTime(at.Add(4 * time.Hour))}},
		{"strong-suppresses-loose", "tata", "tiers", ListingOptions{Match: "fuzzy"}},
		{"filtered-strong", "tata", "tiers", ListingOptions{Match: "fuzzy", Ext: []string{"pdf"}}},
		{"loose-only", "tst", "tiers", ListingOptions{Match: "fuzzy", Ext: []string{"pdf"}}},
		{"no-hits", "unfindable", ".", ListingOptions{}},
	} {
		for _, sortBy := range []string{"name", "path", "size", "modified", "relevance"} {
			if sortBy == "relevance" && tc.query == "" {
				continue
			}
			for _, order := range []string{"asc", "desc"} {
				t.Run(tc.name+"/"+sortBy+"/"+order, func(t *testing.T) {
					opts := tc.opts
					opts.Sort, opts.Order, opts.Limit = sortBy, order, 7
					want := decodeFirstSearchReference(t, r, tc.query, tc.base, opts)
					snapshot := sortBy == "relevance" || opts.Match == "fuzzy" || tc.base != "." && sortBy != "path"
					offset := 0
					seen := map[string]bool{}
					for pageNumber := 0; ; pageNumber++ {
						if pageNumber > len(want.Items)+1 {
							t.Fatal("cursor did not terminate")
						}
						got, err := r.Search(context.Background(), tc.query, tc.base, opts)
						if err != nil {
							t.Fatal(err)
						}
						end := min(offset+opts.Limit, len(want.Items))
						if !reflect.DeepEqual(got.Items, want.Items[offset:end]) {
							t.Fatalf("page %d items: got %+v, want %+v", pageNumber, got.Items, want.Items[offset:end])
						}
						var ranges *[][]MatchRange
						if want.Matches != nil {
							pageRanges := (*want.Matches)[offset:end]
							ranges = &pageRanges
						}
						if !reflect.DeepEqual(got.Matches, ranges) {
							t.Fatalf("page %d matches: got %v, want %v", pageNumber, got.Matches, ranges)
						}
						if snapshot {
							if !reflect.DeepEqual(got.Total, want.Total) || !reflect.DeepEqual(got.Truncated, want.Truncated) {
								t.Fatalf("page %d totals: got %+v, want %+v", pageNumber, got, want)
							}
						} else if got.Total != nil || got.Truncated != nil {
							t.Fatalf("seek page unexpectedly has totals: %+v", got)
						}
						offset = end
						if got.Next == "" {
							if offset != len(want.Items) {
								t.Fatalf("cursor ended at %d of %d hits", offset, len(want.Items))
							}
							break
						}
						if seen[got.Next] || len(got.Items) == 0 || snapshot && offset == len(want.Items) {
							t.Fatalf("invalid continuation on page %d: %+v", pageNumber, got)
						}
						seen[got.Next] = true
						opts.After = got.Next
					}
				})
			}
		}
	}
}

func searchDifferentialNodes(at time.Time) []Node {
	parents := []string{"docs", "docs/Müller", "docs/Straße", "archive", "archive/2026", "Résumé.pdf", "tiers", "docs.txt"}
	var nodes []Node
	for i, parent := range parents {
		nodes = append(nodes, Node{Root: "test", Path: parent, Directory: true, Modified: at.Add(time.Duration(i) * time.Hour), Mode: "drwxr-x---"})
	}
	names := []string{"Résumé", "Re\u0301sume\u0301", "Müller", "Straße", "Quartals_Bericht", "Quartals-Bericht", "report.notes", "budget plan", "reprot", "r1e2p3o4r5t", "README", ".hidden"}
	extensions := []string{".PDF", ".tar.gz", ".txt", ".ß", ".", ""}
	for i := 0; i < 288; i++ {
		name := fmt.Sprintf("%s_%02d%s", names[i%len(names)], i/len(names), extensions[(i/len(names))%len(extensions)])
		nodes = append(nodes, Node{Root: "test", Path: path.Join(parents[i%6], name), Size: int64(i%17) * 128, Modified: at.Add(time.Duration(i%7) * time.Hour), Mode: "-rw-r-----", UID: 1000, GID: 100})
	}
	for _, name := range []string{"a-tata.txt", "b-taxa.txt", "c-t1a2t3a.pdf", "d-testtest.pdf", "e-tst.txt"} {
		nodes = append(nodes, Node{Root: "test", Path: "tiers/" + name, Size: 128, Modified: at})
	}
	nodes = append(nodes, Node{Root: "test", Path: "docs.txt/resume-sibling.pdf", Modified: at})
	return nodes
}

func TestIndexedSearchNonUTF8Key(t *testing.T) {
	const rawPath = "docs/needle-\xff.pdf"
	for _, match := range []string{"exact", "fuzzy"} {
		for _, metadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/metadata=%t", match, metadata), func(t *testing.T) {
				r := indexedListingRoot(t, []Node{{Path: rawPath, Size: 128}})
				state := r.State.(*listingState)
				gets := 0
				state.onGet = func(key string) error {
					gets++
					if key != "i/"+r.generation+"/"+rawPath {
						t.Fatalf("materialization used %q instead of the raw primary key", key)
					}
					return nil
				}
				opts := ListingOptions{Sort: "relevance", Match: match}
				if metadata {
					opts.MinSize = filterSize(128)
				}
				page, err := r.Search(context.Background(), "needle", "docs", opts)
				if err != nil {
					t.Fatal(err)
				}
				// JSON metadata replaces invalid UTF-8. Selection and final lookup
				// must still use the original key; the returned Node keeps JSON's form.
				if gets == 0 || len(page.Items) != 1 || page.Items[0].Path != "docs/needle-\ufffd.pdf" || page.Total == nil || *page.Total != 1 {
					t.Fatalf("gets=%d page=%+v", gets, page)
				}
				want := [][]MatchRange{{{0, 6}}}
				if page.Matches == nil || !reflect.DeepEqual(*page.Matches, want) {
					t.Fatalf("matches=%v, want %v", page.Matches, want)
				}
			})
		}
	}
}

// This intentionally decodes every primary row, including out-of-scope rows,
// before any selection. It shares the matcher and comparison rules, but no key
// parsing, early metadata rejection, heap, deferred highlight or cursor code.
func decodeFirstSearchReference(t *testing.T, r *Root, query, base string, opts ListingOptions) Page {
	t.Helper()
	opts, err := normalizeListing(opts)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := compileSearchQuery(query, opts.In == "path")
	if err != nil {
		t.Fatal(err)
	}
	matcher.base, matcher.fuzzy = base, opts.Match == "fuzzy"
	type hit struct {
		node   Node
		match  nameMatch
		ranges []MatchRange
	}
	var hits []hit
	strong := false
	state := r.State.(*listingState)
	for _, key := range state.keys {
		if !strings.HasPrefix(key, "i/"+r.generation+"/") {
			continue
		}
		var n Node
		if err := json.Unmarshal(state.rows[key], &n); err != nil {
			t.Fatal(err)
		}
		if n.Path == base || base != "." && !strings.HasPrefix(n.Path, base+"/") ||
			opts.Type == "files" && n.Directory || opts.Type == "directories" && !n.Directory || !opts.matchesSearchFilters(n) {
			continue
		}
		matched, score := matcher.match(matcher.subject(n.Path))
		if matched {
			strong = strong || score.tier < nameLoose
			hits = append(hits, hit{n, score, matcher.highlights(matcher.subject(n.Path))})
		}
	}
	if opts.Sort != "relevance" && strong {
		hits = slices.DeleteFunc(hits, func(h hit) bool { return h.match.tier == nameLoose })
	}
	slices.SortFunc(hits, func(a, b hit) int {
		if opts.Sort == "relevance" {
			return compareSearchHit(searchHit{path: a.node.Path, score: scoreName(a.node.Path, a.match)}, searchHit{path: b.node.Path, score: scoreName(b.node.Path, b.match)})
		}
		c := compareListing(a.node, b.node, opts.Sort)
		if opts.Order == "desc" {
			return -c
		}
		return c
	})
	total := len(hits)
	if opts.Sort == "relevance" && len(hits) > r.SearchTopK {
		hits = hits[:r.SearchTopK]
	}
	truncated := len(hits) < total
	out := Page{Items: []Node{}, Total: &total, Truncated: &truncated}
	ranges := make([][]MatchRange, 0, len(hits))
	for _, h := range hits {
		out.Items = append(out.Items, h.node)
		ranges = append(ranges, h.ranges)
	}
	if len(matcher.tokens) > 0 {
		out.Matches = &ranges
	}
	return out
}
