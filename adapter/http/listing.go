package httpadapter

import (
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

// Charge the request target exactly as received, including empty query fields.
// Cursors use base64url, so their byte length is also their escaped URL length.
func listingCursorBudget(r *http.Request) int {
	size := len(r.Method) + 1 + len(r.RequestURI) + 1 + len(r.Proto) + len("\r\n")
	size += len("Host: ") + len(r.Host) + len("\r\n") + len("\r\n")
	for name, values := range r.Header {
		for _, value := range values {
			size += len(name) + len(": ") + len(value) + len("\r\n")
		}
	}
	budget := MaxHeaderBytes - continuationHeaderMargin - size
	// Replace only the first accepted after value. Any duplicate fields remain
	// charged, conservatively, rather than giving extra continuation capacity.
	for _, parameter := range strings.Split(r.URL.RawQuery, "&") {
		// Match URL.Query: invalid escapes and unescaped semicolons skip a field.
		if strings.Contains(parameter, ";") {
			continue
		}
		name, value, _ := strings.Cut(parameter, "=")
		decoded, err := url.QueryUnescape(name)
		if err == nil && decoded == "after" {
			if _, err := url.QueryUnescape(value); err != nil {
				continue
			}
			// A bare after field also needs its '=' when the cursor is appended.
			if !strings.Contains(parameter, "=") {
				budget--
			}
			return nonzeroCursorBudget(budget + len(value))
		}
	}
	return nonzeroCursorBudget(budget - len("&after="))
}

// Zero selects the domain default; an exhausted HTTP budget must stay exhausted.
func nonzeroCursorBudget(budget int) int {
	if budget == 0 {
		return -1
	}
	return budget
}

// time.Parse also accepts malformed RFC 3339 hours, offsets and fractions.
// Check wire syntax first; parsing then validates the calendar date.
var searchTimestampSyntax = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func listingOptions(r *http.Request) domain.ListingOptions {
	q := r.URL.Query()
	return domain.ListingOptions{CursorBudget: listingCursorBudget(r), After: q.Get("after"), Limit: paramInt(r, "limit", 100), MaxEntries: paramInt(r, "maxEntries", 100000), Sort: q.Get("sort"), Order: q.Get("order"), Type: q.Get("type"), Match: q.Get("match"), In: q.Get("in")}
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
