package filegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
