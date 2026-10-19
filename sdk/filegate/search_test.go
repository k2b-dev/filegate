package filegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestSearchRelevancePageDecoding(t *testing.T) {
	for _, body := range []string{
		`{"items":[{"path":"Müller.pdf"}],"total":42,"truncated":true,"matches":[[[0,6]]],"next":"cursor"}`,
		`{"items":[],"total":0,"truncated":false,"matches":[]}`,
		`{"items":[],"matches":[]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if q := r.URL.Query(); q.Get("sort") != "relevance" || q.Get("order") != "desc" {
				t.Errorf("query: %v", q)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		client, err := New(server.URL, "secret")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		page, err := client.Root("docs").Search(context.Background(), "muller", ".", ListingOptions{Sort: "relevance", Order: "desc"})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if page.Matches == nil || (page.Total != nil) != strings.Contains(body, `"total"`) || (page.Truncated != nil) != strings.Contains(body, `"truncated"`) {
			t.Fatalf("%s: %+v", body, page)
		}
		if len(page.Items) > 0 && (len(*page.Matches) != 1 || (*page.Matches)[0][0] != (MatchRange{0, 6}) || *page.Total != 42 || !*page.Truncated || page.Next != "cursor") {
			t.Fatalf("decoded: %+v", page)
		}
		if len(page.Items) == 0 && page.Total != nil && (*page.Total != 0 || *page.Truncated) {
			t.Fatalf("empty: %+v", page)
		}
	}
}

func TestSearchFiltersForwarding(t *testing.T) {
	after := time.Date(2026, 1, 1, 1, 0, 0, 123456789, time.FixedZone("offset", 3621))
	before := after.Add(time.Hour).In(time.FixedZone("negative offset", -3621))
	minSize, maxSize := int64(0), int64(9223372036854775807)
	opts := ListingOptions{Ext: []string{"PDF", "docx"}, MinSize: &minSize, MaxSize: &maxSize, ModifiedAfter: &after, ModifiedBefore: &before}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if strings.HasSuffix(r.URL.Path, "/search") {
			if !q.Has("q") || q.Get("q") != "" || q.Get("ext") != "PDF,docx" || q.Get("minSize") != "0" || q.Get("maxSize") != "9223372036854775807" || q.Get("modifiedAfter") != after.UTC().Format(time.RFC3339Nano) || q.Get("modifiedBefore") != before.UTC().Format(time.RFC3339Nano) {
				t.Errorf("unexpected filters: %s", r.URL)
			}
		} else {
			for _, key := range []string{"ext", "minSize", "maxSize", "modifiedAfter", "modifiedBefore"} {
				if q.Has(key) {
					t.Errorf("directory listing sent search filter %s", key)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Root("files").Search(context.Background(), "", ".", opts); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Root("files").List(context.Background(), ".", opts); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestSearchFiltersOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"ext", "minSize", "maxSize", "modifiedAfter", "modifiedBefore"} {
			if r.URL.Query().Has(key) {
				t.Errorf("sent absent filter %s", key)
			}
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Root("files").Search(context.Background(), "", ".", ListingOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFiltersTimestampUTC(t *testing.T) {
	var want time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"modifiedAfter", "modifiedBefore"} {
			if raw := r.URL.Query().Get(key); raw != "" {
				got, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil || !got.Equal(want) || !strings.HasSuffix(raw, "Z") {
					t.Errorf("%s=%q: parsed %v, want %v: %v", key, raw, got, want, err)
				}
			}
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{-24 * 60 * 60, -3621, 3621, 24 * 60 * 60} {
		at := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.FixedZone("offset", offset))
		want = at
		for _, opts := range []ListingOptions{{ModifiedAfter: &at}, {ModifiedBefore: &at}} {
			if _, err := client.Root("files").Search(context.Background(), "", ".", opts); err != nil {
				t.Fatalf("offset=%d filters=%+v: %v", offset, opts, err)
			}
		}
	}
}

func TestSearchPathForwarding(t *testing.T) {
	for _, in := range []string{"", "name", "path"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("in") != in || q.Has("in") != (in != "") || q.Get("path") != "docs" || q.Get("q") != "rechnung 2026" {
				t.Errorf("unexpected URL: %s", r.URL)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"path":"docs/Rechnungen/2026/scan01.pdf"}],"matches":[[[0,8],[11,15]]]}`))
		}))
		client, err := New(server.URL, "secret")
		if err != nil {
			t.Fatal(err)
		}
		page, err := client.Root("files").Search(context.Background(), "rechnung 2026", "docs", ListingOptions{In: in})
		server.Close()
		if err != nil || page.Matches == nil || (*page.Matches)[0][0] != (MatchRange{0, 8}) {
			t.Fatalf("%+v %v", page, err)
		}
	}
}
