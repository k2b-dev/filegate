//go:build linux

package httpadapter

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

func TestListingContinuationRequestBudget(t *testing.T) {
	for _, endpoint := range []string{"entries", "search"} {
		for _, oversized := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/oversized=%v", endpoint, oversized), func(t *testing.T) {
				data := t.TempDir()
				files, err := filesystem.Open(data)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { files.Close() })
				state, err := pebble.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { state.Close() })
				root, err := domain.NewRoot(domain.RootConfig{Name: "files", Path: data, Index: true}, files, state, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				components := 48
				if oversized {
					components = 60
				}
				base := ""
				for range components {
					base = path.Join(base, strings.Repeat("€", 85))
					if err := files.Mkdir(base, 0700); err != nil {
						t.Fatal(err)
					}
				}
				var gen string
				if err := state.Get("index/generation", &gen); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"needle-a.pdf", "needle-b.pdf", "needle-c.pdf"} {
					p := base + "/" + name
					n := domain.Node{Path: p, Modified: time.Unix(1000, 0).UTC()}
					if err := state.Put("i/"+gen+"/"+p, n); err != nil {
						t.Fatal(err)
					}
					if err := state.Put("q/"+gen+"/d/"+hex.EncodeToString([]byte(base))+"/path/"+p, p); err != nil {
						t.Fatal(err)
					}
				}
				q := url.Values{"path": {base}, "limit": {"1"}}
				var probe domain.Page
				if endpoint == "entries" {
					probe, err = root.List(context.Background(), base, domain.ListingOptions{Limit: 1})
				} else {
					// The full query also matters: search's base-relative cursor can be
					// short, while a root-wide cursor and other parameters can be large.
					q.Set("path", ".")
					q.Set("q", "needle")
					q.Set("scopeHint", base)
					probe, err = root.Search(context.Background(), "needle", ".", domain.ListingOptions{Limit: 1})
				}
				if err != nil || probe.Next == "" {
					t.Fatalf("probe: %v", err)
				}
				endpointPath := "/v1/roots/files/" + endpoint
				nextQ := url.Values{}
				for k, v := range q {
					nextQ[k] = append([]string(nil), v...)
				}
				nextQ.Set("after", probe.Next)
				requestLine := len("GET ") + len(endpointPath) + 1 + len(nextQ.Encode()) + len(" HTTP/1.1\r\n")
				const budget = 60 << 10
				if oversized {
					if requestLine <= 64<<10 {
						t.Fatalf("fixture does not reproduce overflow: %d", requestLine)
					}
				} else {
					padding := budget - requestLine - 128 - len("&padding=")
					if padding < 0 {
						t.Fatalf("fixture too long: %d", requestLine)
					}
					q.Set("padding", strings.Repeat("p", padding))
				}
				handler := New([]*domain.Root{root}, Options{Token: "secret"})
				for pageIndex := 0; pageIndex < 3; pageIndex++ {
					req := httptest.NewRequest("GET", endpointPath+"?"+q.Encode(), nil)
					req.Header.Set("Authorization", "Bearer secret")
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					if oversized {
						var body api.Error
						if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
							t.Fatal(err)
						}
						if w.Code != http.StatusRequestEntityTooLarge || body.Error != "limit_exceeded" {
							t.Fatalf("unusable continuation: status=%d error=%q", w.Code, body.Error)
						}
						return
					}
					var page api.Page
					if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
						t.Fatal(err)
					}
					if w.Code != 200 || len(page.Items) != 1 || !strings.HasSuffix(page.Items[0].Path, fmt.Sprintf("needle-%c.pdf", 'a'+pageIndex)) {
						t.Fatalf("page %d: status=%d items=%v", pageIndex, w.Code, page.Items)
					}
					if pageIndex == 2 {
						if page.Next != "" {
							t.Fatal("terminal page has cursor")
						}
						return
					}
					if page.Next == "" {
						t.Fatal("missing continuation")
					}
					q.Set("after", page.Next)
					target := endpointPath + "?" + q.Encode()
					if length := len("GET ") + len(target) + len(" HTTP/1.1\r\n"); length > budget || length < budget-256 {
						t.Fatalf("continuation request line %d is not just under budget", length)
					}
				}
			})
		}
	}
}

func TestListingContinuationWireEncodingAndHeaders(t *testing.T) {
	for _, encoded := range []string{"%7E", "%20", "%2A"} {
		req := httptest.NewRequest("GET", "/v1/roots/files/entries?padding="+strings.Repeat(encoded, 20000), nil)
		if budget := listingCursorBudget(req); budget >= 10000 {
			t.Errorf("ignored wire encoding %s: budget=%d", encoded, budget)
		}
		// Replacing the old cursor must not charge it a second time.
		req = httptest.NewRequest("GET", "/v1/roots/files/entries?padding="+strings.Repeat(encoded, 16000)+"&%61fter="+strings.Repeat("c", 10000), nil)
		if budget := listingCursorBudget(req); budget < 10000 {
			t.Errorf("counted old cursor twice for %s: budget=%d", encoded, budget)
		}
	}
	req := httptest.NewRequest("GET", "/v1/roots/files/entries", nil)
	req.Header.Set("Authorization", strings.Repeat("t", 60000))
	if budget := listingCursorBudget(req); budget >= 2000 {
		t.Errorf("ignored headers: budget=%d", budget)
	}
}

func TestListingContinuationEmptyQueryFields(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/roots/files/entries?padding=p"+strings.Repeat("&", 40000), nil)
	if budget := listingCursorBudget(req); budget >= 30000 {
		t.Fatalf("empty fields bypassed budget: budget=%d", budget)
	}
}

func TestRejectedListingPagesPreserveCachedObservation(t *testing.T) {
	data := t.TempDir()
	files, err := filesystem.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	state, err := pebble.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	root, err := domain.NewRoot(domain.RootConfig{Name: "files", Path: data, Index: true}, files, state, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"needle-a", "needle-b"} {
		if _, err := root.Put(context.Background(), p, strings.NewReader("data"), domain.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	h := New([]*domain.Root{root}, Options{Token: "secret"})
	const target = "/v1/roots/files/search?q=needle&match=fuzzy&sort=relevance&limit=1"
	request := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	first := request(target)
	var page domain.Page
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || first.Code != 200 || page.Next == "" {
		t.Fatalf("first page: %d %s %v", first.Code, first.Body.String(), err)
	}
	for range 16 {
		rejected := request(target + "&padding=" + strings.Repeat("p", 61200))
		if rejected.Code != 413 {
			t.Fatalf("rejected page: %d %s", rejected.Code, rejected.Body.String())
		}
	}
	next := request(target + "&after=" + url.QueryEscape(page.Next))
	if next.Code != 200 {
		t.Fatalf("rejected pages evicted valid observation: %d %s", next.Code, next.Body.String())
	}
}

func TestListingContinuationSkipsMalformedOldAfter(t *testing.T) {
	for _, invalidSuffix := range []string{"%zz", ";invalid"} {
		req := httptest.NewRequest("GET", "/v1/roots/files/entries?after="+strings.Repeat("p", 40000)+invalidSuffix+"&after="+strings.Repeat("c", 10000), nil)
		if len(req.URL.Query().Get("after")) != 10000 {
			t.Fatal("fixture did not skip malformed field")
		}
		if budget := listingCursorBudget(req); budget >= 30000 {
			t.Fatalf("credited skipped after field: budget=%d", budget)
		}
	}
}
