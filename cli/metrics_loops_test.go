package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	s3adapter "github.com/k2b-dev/filegate/v3/adapter/s3"
	"github.com/k2b-dev/filegate/v3/infra/detect"
	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// scrapeReg renders a registry's exposition text for assertions.
func scrapeReg(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status=%d", rec.Code)
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, body, line string) {
	t.Helper()
	if !strings.Contains(body, line) {
		t.Errorf("metrics output missing line:\n  want: %s", line)
	}
}

// TestRecordCleanupResult pins the cleanup-loop → counter wiring.
func TestRecordCleanupResult(t *testing.T) {
	reg := metrics.New(metrics.BuildInfo{}, nil)
	recordCleanupResult(reg, s3adapter.MultipartCleanupResult{
		DoneRetired:    3,
		AbortedRetired: 2,
		StuckAborted:   1,
		Errors:         4,
	})
	body := scrapeReg(t, reg)
	mustContain(t, body, `filegate_multipart_cleanup_retired_total{reason="done"} 3`)
	mustContain(t, body, `filegate_multipart_cleanup_retired_total{reason="aborted"} 2`)
	mustContain(t, body, `filegate_multipart_cleanup_retired_total{reason="stuck"} 1`)
	mustContain(t, body, `filegate_multipart_cleanup_errors_total 4`)
}

// TestRecordCleanupResultNilSafe: a nil registry must not panic.
func TestRecordCleanupResultNilSafe(t *testing.T) {
	recordCleanupResult(nil, s3adapter.MultipartCleanupResult{DoneRetired: 1})
}

// TestRecordDetectorEvents pins the detector → counter wiring,
// including the type bucketing.
func TestRecordDetectorEvents(t *testing.T) {
	reg := metrics.New(metrics.BuildInfo{}, nil)
	batch := []detect.Event{
		{Type: detect.EventCreated},
		{Type: detect.EventCreated},
		{Type: detect.EventChanged},
		{Type: detect.EventDeleted},
		{Type: detect.EventUnknown},
	}
	recordDetectorEvents(reg, batch)
	body := scrapeReg(t, reg)
	mustContain(t, body, `filegate_detector_events_total{type="created"} 2`)
	mustContain(t, body, `filegate_detector_events_total{type="changed"} 1`)
	mustContain(t, body, `filegate_detector_events_total{type="deleted"} 1`)
	mustContain(t, body, `filegate_detector_events_total{type="unknown"} 1`)
}

func TestRecordDetectorEventsNilSafe(t *testing.T) {
	recordDetectorEvents(nil, []detect.Event{{Type: detect.EventCreated}})
}
