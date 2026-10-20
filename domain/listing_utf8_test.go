package domain

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

func TestListingCursorRejectsLegacyFormat(t *testing.T) {
	for _, sortBy := range []string{"path", "relevance"} {
		t.Run(sortBy, func(t *testing.T) {
			r := indexedListingRoot(t, []Node{{Path: "a.pdf"}, {Path: "b.pdf"}})
			r.now = func() time.Time { return time.Unix(1000, 0) }
			opts, err := normalizeListing(ListingOptions{Limit: 1, Sort: sortBy})
			if err != nil {
				t.Fatal(err)
			}
			page, err := r.Search(context.Background(), "pdf", ".", opts)
			if err != nil || page.Next == "" {
				t.Fatalf("first page: %+v %v", page, err)
			}
			// Recreate the pre-fix fingerprint and wire encoding, including the
			// snapshot form without a last key. Neither format may resume.
			topK := 0
			if sortBy == "relevance" {
				topK = DefaultSearchTopK
			}
			legacyQuery, err := json.Marshal(struct {
				Path, Query string
				Recursive   bool
				Options     ListingOptions
				Execution   *ExecutionIdentity
				TopK        int
			}{".", "pdf", true, opts, nil, topK})
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(legacyQuery)
			raw, err := base64.RawURLEncoding.DecodeString(page.Next)
			var cursor map[string]any
			if err != nil || json.Unmarshal(raw, &cursor) != nil {
				t.Fatalf("decode first cursor: %v", err)
			}
			cursor["q"] = hex.EncodeToString(sum[:])
			if last, ok := cursor["k"].(string); ok {
				key, err := base64.StdEncoding.DecodeString(last)
				if err != nil {
					t.Fatal(err)
				}
				cursor["k"] = string(key)
			}
			raw, err = json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			opts.After = base64.RawURLEncoding.EncodeToString(raw)
			if _, err := r.Search(context.Background(), "pdf", ".", opts); !errors.Is(err, ErrCursorInvalid) {
				t.Fatalf("legacy cursor accepted: %v", err)
			}
		})
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
