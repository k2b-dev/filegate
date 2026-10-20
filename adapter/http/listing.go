package httpadapter

import (
	"fmt"
	"github.com/k2b-dev/filegate/v7/domain"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxHeaderBytes is the daemon's combined request-line and header limit.
// Listing responses reserve additional space for client-added headers.
const MaxHeaderBytes = 64 << 10
const continuationHeaderMargin = 4 << 10

// A cursor can fit its own bound while its URL-escaped base makes page two
// too large for net/http. Check the complete canonical continuation request
// before sending any page, including all query parameters and current headers.
func checkListingContinuation(r *http.Request, next string) error {
	if next == "" {
		return nil
	}
	query := r.URL.Query()
	query.Set("after", next)
	// Preserve the incoming wire encoding too: clients may escape spaces as
	// %20 or tildes as %7E, which Query.Encode would shorten. Remove the old
	// cursor so each page is charged for exactly one continuation.
	rawQuerySize := 0
	for _, parameter := range strings.Split(r.URL.RawQuery, "&") {
		name, _, _ := strings.Cut(parameter, "=")
		decoded, _ := url.QueryUnescape(name)
		if parameter != "" && decoded != "after" {
			rawQuerySize += len(parameter) + 1
		}
	}
	rawQuerySize += len("after=") + len(url.QueryEscape(next))
	querySize := max(len(query.Encode()), rawQuerySize)
	size := len(r.Method) + 1 + len(r.URL.EscapedPath()) + 1 + querySize + len(" HTTP/1.1\r\n")
	size += len("Host: ") + len(r.Host) + len("\r\n") + len("\r\n")
	for name, values := range r.Header {
		for _, value := range values {
			size += len(name) + len(": ") + len(value) + len("\r\n")
		}
	}
	if size > MaxHeaderBytes-continuationHeaderMargin {
		return fmt.Errorf("%w: listing continuation request exceeds %d bytes including headers", domain.ErrLimit, MaxHeaderBytes-continuationHeaderMargin)
	}
	return nil
}

// time.Parse also accepts malformed RFC 3339 hours, offsets and fractions.
// Check wire syntax first; parsing then validates the calendar date.
var searchTimestampSyntax = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func listingOptions(r *http.Request) domain.ListingOptions {
	q := r.URL.Query()
	return domain.ListingOptions{After: q.Get("after"), Limit: paramInt(r, "limit", 100), MaxEntries: paramInt(r, "maxEntries", 100000), Sort: q.Get("sort"), Order: q.Get("order"), Type: q.Get("type"), Match: q.Get("match"), In: q.Get("in")}
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
