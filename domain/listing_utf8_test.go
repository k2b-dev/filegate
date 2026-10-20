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
					names := []string{"a-\xff.pdf", "a-\xfe.pdf", "b.pdf"}
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
						names = []string{"b.pdf", "a-\xfe.pdf", "a-\xff.pdf"}
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
			raw, err := base64.RawURLEncoding.DecodeString(encodeListingCursor(cursor))
			var decoded listingCursor
			if err != nil || json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(decoded, cursor) {
				t.Fatalf("cursor lost key bytes: %+v -> %+v (%v)", cursor, decoded, err)
			}
		})
	}
}

func TestListingLongestPathPagination(t *testing.T) {
	// Fifteen NAME_MAX components plus a NAME_MAX child fill PATH_MAX - 1
	// bytes. Indexed child rows need no on-disk files, only an openable base.
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
	// Maximal relative Linux path and basename, including non-UTF-8 bytes.
	name := strings.Repeat("\xff", 255)
	n := Node{Path: strings.Repeat(name+"/", 15) + name}
	uuid := "00000000-0000-7000-8000-000000000000"
	for _, kind := range []string{"path", "name", "size", "modified", "snapshot"} {
		t.Run(kind, func(t *testing.T) {
			c := listingCursor{
				Query: strings.Repeat("f", 64), Instance: uuid,
				Epoch: ^uint64(0), Generation: uuid,
				Last: []byte(orderedNodeKey(n, kind)),
			}
			if kind == "snapshot" {
				c.Last = nil
				c.Snapshot = uuid
				c.Offset = int(^uint(0) >> 1)
			}
			after := encodeListingCursor(c)
			if len(after) > listingCursorLimit {
				t.Fatalf("cursor length %d exceeds %d", len(after), listingCursorLimit)
			}
			if _, err := normalizeListing(ListingOptions{After: after}); err != nil {
				t.Fatalf("maximum cursor rejected: %v", err)
			}
			t.Logf("maximum %s cursor: %d characters (limit %d)", kind, len(after), listingCursorLimit)
		})
	}
	if _, err := normalizeListing(ListingOptions{After: strings.Repeat("a", listingCursorLimit+1)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized cursor accepted: %v", err)
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
