//go:build linux

package httpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

func TestRawPathIDDownloadLease(t *testing.T) {
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
	root, err := domain.NewRoot(domain.RootConfig{Name: "files", Path: data, Index: true, Managed: true}, files, state, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	handler := New([]*domain.Root{root}, Options{Token: "secret", PublicURL: "http://example.com"})
	for _, p := range []string{"�.pdf", "\xff.pdf", "mid-�.pdf", "mid-\xfe.pdf"} {
		n, err := root.Put(context.Background(), p, strings.NewReader(p), domain.WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(api.DownloadRequest{FileID: n.ID})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/v1/roots/files/downloads/direct", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		var lease api.DirectURL
		if err := json.Unmarshal(w.Body.Bytes(), &lease); err != nil {
			t.Fatal(err)
		}
		if w.Code != 201 || lease.URL == "" {
			t.Fatalf("lease for %q: %d %s", p, w.Code, w.Body)
		}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", lease.URL, nil))
		if w.Code != 200 || w.Body.String() != p {
			t.Errorf("ID lease for %q: %d %q", p, w.Code, w.Body.String())
		}
	}
}
