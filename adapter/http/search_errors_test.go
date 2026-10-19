package httpadapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
)

func TestSearchBusyHTTPError(t *testing.T) {
	for _, err := range []error{domain.ErrSearchBusy, fmt.Errorf("search: %w", domain.ErrSearchBusy)} {
		response := httptest.NewRecorder()
		fail(response, err)
		var body api.Error
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusTooManyRequests || body.Error != "search_busy" || body.Message != err.Error() || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("unexpected busy response: %d %+v %v", response.Code, body, response.Header())
		}
	}
}
