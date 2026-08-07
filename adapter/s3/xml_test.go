package s3

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

func TestStatusForInsufficientStorage(t *testing.T) {
	if got := statusFor(errInsufficientStorage); got != http.StatusInsufficientStorage {
		t.Fatalf("status=%d, want=%d", got, http.StatusInsufficientStorage)
	}
}

func TestMapDomainErrorInsufficientStorage(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/data/object.bin", nil)
	rec := httptest.NewRecorder()

	mapDomainError(rec, req, domain.ErrInsufficientStorage, "data", "object.bin")

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status=%d, want=%d body=%s", rec.Code, http.StatusInsufficientStorage, rec.Body.String())
	}
}
