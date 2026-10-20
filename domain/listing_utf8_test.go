package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Guard against lossy JSON string encoding of arbitrary Linux filename bytes.
func TestListingInvalidUTF8Pagination(t *testing.T) {
	for _, mode := range []string{"directory", "exact", "relevance", "relevance-filtered", "live"} {
		relevance := strings.HasPrefix(mode, "relevance")
		for _, sortBy := range []string{"path", "name", "size", "modified"} {
			if relevance && sortBy != "path" {
				continue
			}
			for _, order := range []string{"asc", "desc"} {
				t.Run(mode+"/"+sortBy+"/"+order, func(t *testing.T) {
					at := time.Unix(1000, 0).UTC()
					names := []string{"a-\xff.pdf", "a-\xfe.pdf", "b.pdf", "\xff.pdf"}
					nodes := make([]Node, len(names))
					for i, name := range names {
						nodes[i] = Node{Path: name, ID: fmt.Sprintf("%x", name), Modified: at}
					}
					r := indexedListingRoot(t, nodes)
					r.now = func() time.Time { return at }
					files := &listingFiles{base: t.TempDir()}
					r.Files = files
					if mode == "live" {
						r.Config.Index = false
						for _, name := range names {
							if err := os.WriteFile(filepath.Join(files.base, name), nil, 0600); err != nil {
								t.Fatal(err)
							}
							if err := os.Chtimes(filepath.Join(files.base, name), at, at); err != nil {
								t.Fatal(err)
							}
						}
					}
					opts := ListingOptions{Limit: 1, Sort: sortBy, Order: order}
					if relevance {
						opts.Sort = "relevance"
						r.SearchTopK = len(names)
						// A weaker hit must stay outside the retained top-K pages.
						changes, err := r.indexChanges(Node{Path: "zzzz.pdf", ID: "outside-top-k", Modified: at}, nil)
						if err != nil {
							t.Fatal(err)
						}
						if err := r.State.Batch(changes); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "relevance-filtered" {
						opts.Type = "files"
					}
					slices.Sort(names)
					if relevance {
						// Equal match ranks prefer shorter basenames, then raw paths.
						names = []string{"b.pdf", "\xff.pdf", "a-\xfe.pdf", "a-\xff.pdf"}
					} else if order == "desc" {
						slices.Reverse(names)
					}
					for i := 0; ; i++ {
						if i > len(names) {
							t.Fatal("pagination did not terminate")
						}
						var page Page
						var err error
						if mode == "directory" {
							page, err = r.List(context.Background(), ".", opts)
						} else {
							page, err = r.Search(context.Background(), "pdf", ".", opts)
						}
						if err != nil {
							t.Fatal(err)
						}
						if i < len(names) {
							want := names[i]
							if mode != "live" {
								// Primary JSON rows still expose the existing sanitized path.
								b, _ := json.Marshal(want)
								if err := json.Unmarshal(b, &want); err != nil {
									t.Fatal(err)
								}
							}
							if len(page.Items) != 1 || page.Items[0].Path != want || mode != "live" && page.Items[0].ID != fmt.Sprintf("%x", names[i]) {
								t.Fatalf("page %d: %+v, want %q", i, page, want)
							}
						} else if len(page.Items) != 0 {
							t.Fatalf("extra items: %+v", page)
						}
						if page.Next == "" {
							if i < len(names)-1 {
								t.Fatalf("pagination skipped items at page %d", i)
							}
							break
						}
						opts.After = page.Next
					}
				})
			}
		}
	}
}

func TestSearchPathInvalidUTF8BaseHighlights(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "size", "modified", "relevance"} {
		for _, match := range []string{"exact", "fuzzy"} {
			t.Run(sortBy+"/"+match, func(t *testing.T) {
				base := "docs-\xff"
				r := indexedListingRoot(t, []Node{{Path: base + "/x-\xfe/report-a.pdf"}, {Path: base + "/x-\xfe/report-b.pdf"}})
				r.now = func() time.Time { return time.Unix(1000, 0) }
				opts := ListingOptions{In: "path", Sort: sortBy, Match: match, Limit: 1}
				for _, suffix := range []string{"a", "b"} {
					page, err := r.Search(context.Background(), "report", base, opts)
					want := "docs-�/x-�/report-" + suffix + ".pdf"
					if err != nil || len(page.Items) != 1 || page.Items[0].Path != want || page.Matches == nil || !reflect.DeepEqual(*page.Matches, [][]MatchRange{{{4, 10}}}) {
						t.Fatalf("page: %+v ranges=%v err=%v", page, page.Matches, err)
					}
					opts.After = page.Next
				}
			})
		}
	}
}

func TestListingCursorArbitraryKeyBytes(t *testing.T) {
	for value := range 256 {
		t.Run(fmt.Sprintf("%02x", value), func(t *testing.T) {
			key := "i/generation/a-" + string([]byte{byte(value)}) + ".pdf"
			cursor := listingCursor{Last: []byte(key)}
			encoded, err := encodeListingCursor(cursor, listingCursorLimit)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(encoded)
			var decoded listingCursor
			if err != nil || json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(decoded, cursor) {
				t.Fatalf("cursor lost key bytes: %+v -> %+v (%v)", cursor, decoded, err)
			}
		})
	}
}

func TestListingLongPathPagination(t *testing.T) {
	// This base can be opened by the fixture's os.Open. Filegate's component-wise
	// traversal also supports longer paths, covered by the deep-path test below.
	base := strings.TrimSuffix(strings.Repeat(strings.Repeat("d", 254)+"\xff/", 15), "/")
	files := &listingFiles{base: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(files.base, base), 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1000, 0).UTC()
	var nodes []Node
	for _, letter := range []string{"a", "b", "c"} {
		name := "needle-" + letter + strings.Repeat("\xff", 247)
		p := base + "/" + name
		if len(name) != 255 || len(p) != 4095 {
			t.Fatalf("fixture lengths: name=%d path=%d", len(name), len(p))
		}
		nodes = append(nodes, Node{Path: p, ID: letter, Modified: at})
	}
	for _, mode := range []string{"directory", "subtree", "root"} {
		for _, sortBy := range []string{"path", "name", "size", "modified", "relevance"} {
			if mode == "directory" && sortBy == "relevance" {
				continue
			}
			for _, order := range []string{"asc", "desc"} {
				t.Run(mode+"/"+sortBy+"/"+order, func(t *testing.T) {
					r := indexedListingRoot(t, nodes)
					r.Files = files
					r.now = func() time.Time { return at }
					opts := ListingOptions{Limit: 1, Sort: sortBy, Order: order}
					var ids []string
					maxCursor := 0
					for pages := 0; ; pages++ {
						if pages >= len(nodes) {
							t.Fatal("pagination did not terminate")
						}
						var page Page
						var err error
						if mode == "directory" {
							page, err = r.List(context.Background(), base, opts)
						} else {
							scope := base
							if mode == "root" {
								scope = "."
							}
							page, err = r.Search(context.Background(), "needle", scope, opts)
						}
						if err != nil {
							t.Fatalf("page %d after %d-character cursor: %v", pages, len(opts.After), err)
						}
						if len(page.Items) != 1 {
							t.Fatalf("page %d: got %d items", pages, len(page.Items))
						}
						ids = append(ids, page.Items[0].ID)
						maxCursor = max(maxCursor, len(page.Next))
						if page.Next == "" {
							break
						}
						opts.After = page.Next
					}
					want := []string{"a", "b", "c"}
					if order == "desc" && sortBy != "relevance" {
						slices.Reverse(want)
					}
					if !slices.Equal(ids, want) {
						t.Fatalf("got IDs %v, want %v", ids, want)
					}
					t.Logf("maximum cursor: %d characters", maxCursor)
				})
			}
		}
	}
}

func TestListingCursorLengthBound(t *testing.T) {
	if listingCursorLimit != 32<<10 {
		t.Fatalf("cursor limit = %d, want 32 KiB", listingCursorLimit)
	}
	if _, err := normalizeListing(ListingOptions{After: strings.Repeat("a", listingCursorLimit)}); err != nil {
		t.Fatalf("cursor at limit rejected: %v", err)
	}
	if _, err := normalizeListing(ListingOptions{After: strings.Repeat("a", listingCursorLimit+1)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized cursor accepted: %v", err)
	}
}

func TestListingDeepPathPagination(t *testing.T) {
	for _, components := range []int{40, 80} {
		base := strings.Repeat(strings.Repeat("d", 254)+"\xff/", components)
		at := time.Unix(1000, 0).UTC()
		var nodes []Node
		for _, letter := range []string{"a", "b", "c"} {
			nodes = append(nodes, Node{Path: base + "needle-" + letter + strings.Repeat("\xff", 247), ID: letter, Modified: at})
		}
		for _, sortBy := range []string{"path", "name", "size", "modified"} {
			for _, order := range []string{"asc", "desc"} {
				t.Run(fmt.Sprintf("%d/%s/%s", components, sortBy, order), func(t *testing.T) {
					r := indexedListingRoot(t, nodes)
					r.Files = &listingFiles{base: t.TempDir()}
					r.now = func() time.Time { return at }
					opts := ListingOptions{Limit: 1, Sort: sortBy, Order: order}
					if components == 80 {
						page, err := r.Search(context.Background(), "needle", ".", opts)
						if !errors.Is(err, ErrLimit) || !reflect.DeepEqual(page, Page{}) {
							t.Fatalf("oversized continuation: err=%v, next length=%d, items=%d", err, len(page.Next), len(page.Items))
						}
						// A terminal page needs no cursor, even for very deep paths.
						opts.Limit = len(nodes)
						page, err = r.Search(context.Background(), "needle", ".", opts)
						if err != nil || len(page.Items) != len(nodes) || page.Next != "" {
							t.Fatalf("terminal page: err=%v, next length=%d, items=%d", err, len(page.Next), len(page.Items))
						}
						return
					}
					var ids []string
					for pages := 0; ; pages++ {
						if pages >= len(nodes) {
							t.Fatal("pagination did not terminate")
						}
						page, err := r.Search(context.Background(), "needle", ".", opts)
						if err != nil || len(page.Items) != 1 {
							t.Fatalf("page %d after %d-character cursor: items=%d err=%v", pages, len(opts.After), len(page.Items), err)
						}
						ids = append(ids, page.Items[0].ID)
						if page.Next == "" {
							break
						}
						if len(page.Next) <= 16384 || len(page.Next) > 32<<10 {
							t.Fatalf("deep-path cursor length = %d", len(page.Next))
						}
						opts.After = page.Next
					}
					want := []string{"a", "b", "c"}
					if order == "desc" {
						slices.Reverse(want)
					}
					if !slices.Equal(ids, want) {
						t.Fatalf("got IDs %v, want %v", ids, want)
					}
				})
			}
		}
	}
}

func TestListingCursorBindsRawBase(t *testing.T) {
	for _, sortBy := range []string{"path", "name", "relevance"} {
		t.Run(sortBy, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "docs-\xff/a.pdf"}, {Path: "docs-\xff/b.pdf"}, {Path: "docs-\xfe/c.pdf"}})
			r.now = func() time.Time { return time.Unix(1000, 0) }
			opts := ListingOptions{Limit: 1, Sort: sortBy}
			page, err := r.Search(context.Background(), "pdf", "docs-\xff", opts)
			if err != nil || page.Next == "" {
				t.Fatalf("first page: %+v %v", page, err)
			}
			opts.After = page.Next
			if _, err := r.Search(context.Background(), "pdf", "docs-\xfe", opts); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("cursor accepted different raw base: %v", err)
			}
		})
	}
}

func TestRawPathSnapshotSort(t *testing.T) {
	names := []string{"a-�z.pdf", "a-\xfe.pdf", "a-\xff.pdf"}
	for _, sortBy := range []string{"path", "name", "size", "modified"} {
		for _, order := range []string{"asc", "desc"} {
			t.Run(sortBy+"/"+order, func(t *testing.T) {
				var nodes []Node
				for _, name := range names {
					nodes = append(nodes, Node{Path: "base/" + name, ID: fmt.Sprintf("%x", name), Modified: time.Unix(1000, 0).UTC()})
				}
				r := indexedListingRoot(t, nodes)
				r.now = func() time.Time { return time.Unix(1000, 0) }
				opts := ListingOptions{Sort: sortBy, Order: order, Match: "fuzzy", Limit: 1}
				want := slices.Clone(names)
				if order == "desc" {
					slices.Reverse(want)
				}
				for _, name := range want {
					page, err := r.Search(context.Background(), "pdf", "base", opts)
					if err != nil || len(page.Items) != 1 || page.Items[0].ID != fmt.Sprintf("%x", name) {
						t.Fatalf("raw sort: page=%+v err=%v want=%q", page, err, name)
					}
					opts.After = page.Next
				}
				if opts.After != "" {
					t.Fatal("extra page")
				}
			})
		}
	}
}

func TestListingCursorBudgetBeforeCacheAdmission(t *testing.T) {
	for _, mode := range []string{"directory", "exact", "snapshot", "relevance"} {
		t.Run(mode, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "needle-a"}, {Path: "needle-b"}, {Path: "needle-c"}})
			r.Files = &listingFiles{base: t.TempDir()}
			at := time.Unix(1000, 0).UTC()
			r.now = func() time.Time { return at }
			opts := ListingOptions{Limit: 1}
			if mode == "snapshot" {
				opts.Sort = "name"
				opts.Match = "fuzzy"
			}
			if mode == "relevance" {
				opts.Sort = "relevance"
			}
			query := func(o ListingOptions) (Page, error) {
				if mode == "directory" {
					return r.List(context.Background(), ".", o)
				}
				return r.Search(context.Background(), "needle", ".", o)
			}
			first, err := query(opts)
			if err != nil || first.Next == "" {
				t.Fatal(first, err)
			}
			count, bytes := len(r.listings), r.listingBytes
			opts.CursorBudget = 1
			if _, err := query(opts); !errors.Is(err, ErrLimit) {
				t.Fatalf("ignored cursor budget: %v", err)
			}
			if len(r.listings) != count || r.listingBytes != bytes {
				t.Fatal("rejected page entered cache")
			}
			opts.After = first.Next
			opts.CursorBudget = listingCursorLimit + 1
			if _, err := query(opts); err != nil {
				t.Fatalf("budget changed cursor fingerprint: %v", err)
			}
			opts.After = ""
			opts.Limit = 3
			opts.CursorBudget = -1
			if page, err := query(opts); err != nil || page.Next != "" {
				t.Fatalf("terminal page needs no budget: %+v %v", page, err)
			}
		})
	}
}
