//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

func TestSearchRelevanceHTTPPage(t *testing.T) {
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
	root, err := domain.NewRoot(domain.RootConfig{Name: "files", Path: data}, files, state, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	root.SearchTopK = 1
	for _, name := range []string{"report", "report.pdf"} {
		if err := os.WriteFile(filepath.Join(data, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	handler := New([]*domain.Root{root}, Options{Token: "secret"})
	req := httptest.NewRequest("GET", "/v1/roots/files/search?q=report&sort=relevance&order=desc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var page api.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(page.Items) != 1 || page.Items[0].Path != "report" || page.Total == nil || *page.Total != 2 || page.Truncated == nil || !*page.Truncated || page.Matches == nil || len(*page.Matches) != 1 || (*page.Matches)[0][0] != (api.MatchRange{0, 6}) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	// Missing live hits must use the restart error clients already recognize.
	root.SearchTopK = 2
	req = httptest.NewRequest("GET", "/v1/roots/files/search?q=report&sort=relevance&limit=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || page.Next == "" {
		t.Fatalf("first pageable result: %d %s", w.Code, w.Body)
	}
	if err := os.Remove(filepath.Join(data, "report.pdf")); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/v1/roots/files/search?q=report&sort=relevance&limit=1&after="+url.QueryEscape(page.Next), nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var body api.Error
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 409 || body.Error != "cursor_invalid" || body.Message == "" {
		t.Fatalf("missing hit: %d %s", w.Code, w.Body)
	}
}
