package httpadapter

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	api "github.com/k2b-dev/filegate/v6/api/v1"
	"github.com/k2b-dev/filegate/v6/domain"
)

func TestSpecificConflictHTTPErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"path", domain.ErrPathConflict, "path_conflict"},
		{"wrapped_path", fmt.Errorf("publication: %w", domain.ErrPathConflict), "path_conflict"},
		{"unclassified_native_exists", os.ErrExist, "conflict"},
		{"idempotency", domain.ErrIdempotencyConflict, "idempotency_conflict"},
		{"wrapped_idempotency", fmt.Errorf("session: %w", domain.ErrIdempotencyConflict), "idempotency_conflict"},
		{"structural", domain.ErrConflict, "conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			fail(response, test.err)
			var body api.Error
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != 409 || body.Error != test.code {
				t.Fatalf("got %d %+v, want409 %s", response.Code, body, test.code)
			}
		})
	}
}
