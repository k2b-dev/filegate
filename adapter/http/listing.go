package httpadapter

import (
	"github.com/k2b-dev/filegate/v7/domain"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// time.Parse also accepts malformed RFC 3339 hours, offsets and fractions.
// Check wire syntax first; parsing then validates the calendar date.
var searchTimestampSyntax = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func listingOptions(r *http.Request) domain.ListingOptions {
	q := r.URL.Query()
	return domain.ListingOptions{After: q.Get("after"), Limit: paramInt(r, "limit", 100), MaxEntries: paramInt(r, "maxEntries", 100000), Sort: q.Get("sort"), Order: q.Get("order"), Type: q.Get("type"), Match: q.Get("match")}
}

// Search filters are intentionally not parsed for directory listings.
func searchOptions(r *http.Request) (domain.ListingOptions, error) {
	o := listingOptions(r)
	q := r.URL.Query()
	if q.Has("ext") {
		o.Ext = strings.Split(strings.Join(q["ext"], ","), ",")
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
		raw := q.Get(bound.name)
		if !searchTimestampSyntax.MatchString(raw) {
			return o, domain.ErrInvalid
		}
		v, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return o, domain.ErrInvalid
		}
		*bound.dst = &v
	}
	return o, nil
}
