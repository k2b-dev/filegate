package filegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpadapter "github.com/k2b-dev/filegate/v7/adapter/http"
	"github.com/k2b-dev/filegate/v7/domain"
)

func TestSearchMatchQuery(t *testing.T) {
	for _, mode := range []string{"", "exact", "fuzzy"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/v1/roots/files/search" || q.Get("q") != "2026 bericht" || q.Get("path") != "docs" || q.Get("match") != mode {
					t.Errorf("unexpected URL %s", r.URL)
				}
				if mode == "" && q.Has("match") {
					t.Error("default match must be omitted")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"items":[]}`))
			}))
			defer server.Close()
			client, err := New(server.URL, "secret")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Root("files").Search(context.Background(), "2026 bericht", "docs", ListingOptions{Match: mode}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSearchQueryByteLimit(t *testing.T) {
	handler := httpadapter.New([]*domain.Root{{Config: domain.RootConfig{Name: "files"}}}, httpadapter.Options{Token: "secret"})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exact", "fuzzy"} {
		for _, query := range []string{strings.Repeat("a", 8193), strings.Repeat("é", 4096) + "a", strings.Repeat(" ", 8193)} {
			_, err := client.Root("files").Search(context.Background(), query, ".", ListingOptions{Match: mode})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || apiErr.Code != "invalid_argument" || apiErr.Message == "" {
				t.Fatalf("mode=%s oversized raw query: %v", mode, err)
			}
		}
	}
}
