package httpadapter

import (
	"github.com/k2b-dev/filegate/v7/domain"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func listingOptions(r *http.Request) domain.ListingOptions {
	q := r.URL.Query()
	return domain.ListingOptions{After: q.Get("after"), Limit: paramInt(r, "limit", 100), MaxEntries: paramInt(r, "maxEntries", 100000), Sort: q.Get("sort"), Order: q.Get("order"), Type: q.Get("type"), Match: q.Get("match")}
}

// Search filters are intentionally not parsed for directory listings.
func searchOptions(r *http.Request) (domain.ListingOptions, error) {
	o := listingOptions(r)
	q := r.URL.Query()
	if q.Has("ext") {
		o.Ext = strings.Split(q.Get("ext"), ",")
	}
	for _, bound := range []struct {
		name string
		dst  **int64
	}{{"minSize", &o.MinSize}, {"maxSize", &o.MaxSize}} {
		if !q.Has(bound.name) {
			continue
		}
		raw := q.Get(bound.name)
		if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return o, domain.ErrInvalid
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return o, domain.ErrInvalid
		}
		*bound.dst = &v
	}
	for _, bound := range []struct {
		name string
		dst  **time.Time
	}{{"modifiedAfter", &o.ModifiedAfter}, {"modifiedBefore", &o.ModifiedBefore}} {
		if !q.Has(bound.name) {
			continue
		}
		v, err := time.Parse(time.RFC3339, q.Get(bound.name))
		if err != nil {
			return o, domain.ErrInvalid
		}
		*bound.dst = &v
	}
	return o, nil
}
