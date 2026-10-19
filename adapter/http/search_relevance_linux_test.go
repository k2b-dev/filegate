//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http/httptest"
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
}
