package httpadapter

import (
	"github.com/k2b-dev/filegate/v5/domain"
	"net/http"
)

func listingOptions(r *http.Request) domain.ListingOptions {
	q := r.URL.Query()
	return domain.ListingOptions{After: q.Get("after"), Limit: paramInt(r, "limit", 100), MaxEntries: paramInt(r, "maxEntries", 100000), Sort: q.Get("sort"), Order: q.Get("order"), Type: q.Get("type")}
}
