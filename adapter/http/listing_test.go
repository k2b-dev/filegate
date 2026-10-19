package httpadapter

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
)

func TestListingMatchParsing(t *testing.T) {
	for _, value := range []string{"", "exact", "fuzzy", "unknown"} {
		r := httptest.NewRequest("GET", "/v1/roots/files/search?match="+value, nil)
		if got := listingOptions(r).Match; got != value {
			t.Fatalf("match=%q parsed %q", value, got)
		}
	}
}

func TestSearchMatchInvalidHTTP(t *testing.T) {
	h := New([]*domain.Root{{Config: domain.RootConfig{Name: "files"}}}, Options{Token: "secret"})
	for _, suffix := range []string{"match=unknown", "match=Fuzzy", "q=a+b+c+d+e+f+g+h+i"} {
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
