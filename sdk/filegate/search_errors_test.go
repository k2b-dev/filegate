package filegate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchPreservesTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{429, "search_busy"}, {409, "cursor_invalid"}, {413, "limit_exceeded"}} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":%q,"message":"retry query"}`, tc.code)
			}))
			defer server.Close()
			client, err := New(server.URL, "backend")
			if err != nil {
				t.Fatal(err)
			}
			root := client.Root("documents")
			for _, call := range []func() error{
				func() error { _, err := root.Search(context.Background(), "name", ".", ListingOptions{}); return err },
				func() error { _, err := root.List(context.Background(), ".", ListingOptions{}); return err },
			} {
				err := call()
				var apiError *APIError
				if !errors.As(err, &apiError) || apiError.Status != tc.status || apiError.Code != tc.code || apiError.Message != "retry query" {
					t.Fatalf("listing error changed: %v", err)
				}
			}
		})
	}
}
