package httpadapter

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
)

func TestSearchFiltersParsing(t *testing.T) {
	q := url.Values{"ext": {"PDF,docx"}, "minSize": {"0"}, "maxSize": {"9223372036854775807"}, "modifiedAfter": {"2026-01-01T01:00:00+01:00"}, "modifiedBefore": {"2026-02-01T00:00:00.123456789Z"}}
	r := httptest.NewRequest("GET", "/v1/roots/files/search?"+q.Encode(), nil)
	o, err := searchOptions(r)
	if err != nil || !reflect.DeepEqual(o.Ext, []string{"PDF", "docx"}) || o.MinSize == nil || *o.MinSize != 0 || o.MaxSize == nil || *o.MaxSize != 9223372036854775807 || o.ModifiedAfter == nil || o.ModifiedAfter.UTC().Format(time.RFC3339) != "2026-01-01T00:00:00Z" || o.ModifiedBefore == nil || o.ModifiedBefore.Nanosecond() != 123456789 {
		t.Fatalf("parsed %+v: %v", o, err)
	}
	o = listingOptions(r)
	if len(o.Ext) != 0 || o.MinSize != nil || o.MaxSize != nil || o.ModifiedAfter != nil || o.ModifiedBefore != nil {
		t.Fatal("directory listing parsed search filters")
	}
	o, err = searchOptions(httptest.NewRequest("GET", "/", nil))
	if err != nil || o.MinSize != nil || o.MaxSize != nil || o.ModifiedAfter != nil || o.ModifiedBefore != nil {
		t.Fatalf("absent filters: %+v %v", o, err)
	}
}

func TestSearchFiltersInvalidHTTP(t *testing.T) {
	h := New([]*domain.Root{{Config: domain.RootConfig{Name: "files"}}}, Options{Token: "secret"})
	for _, suffix := range []string{
		"ext=", "ext=.pdf", "ext=pdf,", "ext=,pdf", "ext=pdf,,docx",
		"minSize=", "minSize=-1", "minSize=1.5", "minSize=1e3", "minSize=%2B1", "minSize=9223372036854775808",
		"maxSize=", "maxSize=-1", "maxSize=1.5", "maxSize=abc", "minSize=2&maxSize=1",
		"modifiedAfter=", "modifiedAfter=2026-01-01", "modifiedAfter=invalid", "modifiedBefore=", "modifiedBefore=2026-13-01T00:00:00Z",
		"modifiedAfter=9999-12-31T23:59:59-01:00", "modifiedBefore=9999-12-31T23:59:59-01:00",
		"modifiedAfter=0000-01-01T00:00:00%2B01:00", "modifiedBefore=0000-01-01T00:00:00%2B01:00",
	} {
		req := httptest.NewRequest("GET", "/v1/roots/files/search?"+suffix, nil)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var body api.Error
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != 400 || body.Error != "invalid_argument" || body.Message == "" {
			t.Fatalf("%s: %d %+v", suffix, w.Code, body)
		}
	}
}
