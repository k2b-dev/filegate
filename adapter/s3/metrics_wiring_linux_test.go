//go:build linux

package s3

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/infra/metrics"
)

// scrapeMetrics renders the registry's exposition text (what Prometheus
// scrapes) so tests can assert on the wire format directly — no reach
// into the metrics package's unexported counters.
func scrapeMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status=%d", rec.Code)
	}
	return rec.Body.String()
}

// assertRequestCount checks the requests_total series for an exact
// adapter/op/status_class label set equals want. Prometheus renders
// labels alphabetically: adapter, op, status_class.
func assertRequestCount(t *testing.T, scrape, adapter, op, class string, want int) {
	t.Helper()
	line := fmt.Sprintf(`filegate_http_requests_total{adapter="%s",op="%s",status_class="%s"} %d`,
		adapter, op, class, want)
	if !strings.Contains(scrape, line) {
		t.Errorf("missing/mismatched metric line:\n  want: %s", line)
	}
}

// TestMetricsOpLabelForS3Ops drives real signed S3 requests through the
// metrics middleware wrapping the real S3 handler, and asserts the
// per-op label propagates from the dispatcher's SetOp call up to the
// requests_total counter — end-to-end proof the holder-in-context
// plumbing works through the actual dispatch path, including
// CopyObject's re-label inside the handler.
func TestMetricsOpLabelForS3Ops(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	reg := metrics.New(metrics.BuildInfo{}, nil)
	wrapped := reg.Middleware("s3")(handler)

	put := func(key string, body []byte) {
		req := httptest.NewRequest(http.MethodPut, "/"+mount+"/"+key, bytes.NewReader(body))
		req.Host = "example.com"
		signRequestPayload(req, body)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("seed PUT %s status=%d body=%s", key, rec.Code, rec.Body.String())
		}
	}
	put("a.txt", []byte("hello"))

	getReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/a.txt", nil)
	getReq.Host = "example.com"
	signRequestPayload(getReq, nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), getReq)

	headReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/a.txt", nil)
	headReq.Host = "example.com"
	signRequestPayload(headReq, nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), headReq)

	// CopyObject (PUT with x-amz-copy-source) must re-label from
	// PutObject to CopyObject inside the handler.
	copyReq := httptest.NewRequest(http.MethodPut, "/"+mount+"/b.txt", nil)
	copyReq.Host = "example.com"
	copyReq.Header.Set("x-amz-copy-source", "/"+mount+"/a.txt")
	signRequestPayload(copyReq, nil)
	cRec := httptest.NewRecorder()
	wrapped.ServeHTTP(cRec, copyReq)
	if cRec.Code != http.StatusOK {
		t.Fatalf("CopyObject status=%d body=%s", cRec.Code, cRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/?list-type=2", nil)
	listReq.Host = "example.com"
	signRequestPayload(listReq, nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), listReq)

	scrape := scrapeMetrics(t, reg)
	assertRequestCount(t, scrape, "s3", "PutObject", "2xx", 1) // only the seed; copy is separate
	assertRequestCount(t, scrape, "s3", "GetObject", "2xx", 1)
	assertRequestCount(t, scrape, "s3", "HeadObject", "2xx", 1)
	assertRequestCount(t, scrape, "s3", "CopyObject", "2xx", 1)
	assertRequestCount(t, scrape, "s3", "ListObjectsV2", "2xx", 1)
	// in_flight must have returned to 0.
	if !strings.Contains(scrape, `filegate_http_requests_in_flight{adapter="s3"} 0`) {
		t.Errorf("in_flight{s3} not back to 0 after requests")
	}
}

// TestMetricsErrorStatusClass: a forbidden-bucket request (403) lands
// in the 4xx class — confirming status capture reflects real handler
// outcomes. The op label is the HTTP method ("GET") rather than
// "GetObject": authorization is rejected in the dispatcher BEFORE the
// per-op SetOp runs, so pre-dispatch denials carry the coarser method
// label. That's an accepted KISS tradeoff (still useful: method +
// status_class) — duplicating the full dispatch classification just to
// label denied requests isn't worth it.
func TestMetricsErrorStatusClass(t *testing.T) {
	const (
		access = "AKIAMETRICS000000001"
		secret = "secret-metrics-fillerfillerfillerfiller0"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: []string{"alpha"}},
	})
	defer cleanup()

	reg := metrics.New(metrics.BuildInfo{}, nil)
	wrapped := reg.Middleware("s3")(handler)

	// GET on a forbidden bucket → 403 AccessDenied, rejected pre-dispatch.
	req := httptest.NewRequest(http.MethodGet, "/beta/x", nil)
	req.Host = "example.com"
	signWithKey(req, nil, access, secret)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden GET status=%d, want 403", rec.Code)
	}
	assertRequestCount(t, scrapeMetrics(t, reg), "s3", "GET", "4xx", 1)
}

// TestMetricsRatelimitRejectedCounter pins the dedicated rate-limit
// rejection counter wired through NewHandler's Options.Metrics. The
// generic 5xx HTTP metric isn't enough — operators want to alert
// specifically on rate-limiting, not all 503s.
func TestMetricsRatelimitRejectedCounter(t *testing.T) {
	reg := metrics.New(metrics.BuildInfo{}, nil)
	svc, _, mount, cleanup := newTestServer(t)
	defer cleanup()

	const (
		access = "AKIARLCOUNTER0000001"
		secret = "secret-rl-counter-fillerfillerfillerfill"
	)
	handler, err := NewHandler(svc, Options{
		Region: testRegion,
		Keys: []KeyEntry{
			{AccessKey: access, SecretKey: secret, Buckets: []string{"*"},
				RequestsPerSecond: 1, Burst: 1},
		},
		Metrics: reg,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	doReq := func() int {
		r := httptest.NewRequest(http.MethodGet, "/"+mount+"?list-type=2", nil)
		r.Host = "example.com"
		signWithKey(r, nil, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	doReq() // burst token
	if code := doReq(); code != http.StatusServiceUnavailable {
		t.Fatalf("second request status=%d, want 503", code)
	}
	doReq() // another rejection

	if !strings.Contains(scrapeMetrics(t, reg), "filegate_s3_ratelimit_rejected_total 2") {
		t.Errorf("ratelimit_rejected_total != 2 after two throttled requests")
	}
}

// TestMetricsCompletePhaseHistogram drives a real multipart Complete
// through a metrics-wired handler and asserts all four sub-phases are
// observed exactly once — the trace-substitute histogram.
func TestMetricsCompletePhaseHistogram(t *testing.T) {
	reg := metrics.New(metrics.BuildInfo{}, nil)
	svc, _, mount, cleanup := newTestServer(t)
	defer cleanup()

	handler, err := NewHandler(svc, Options{
		Region:    testRegion,
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Metrics:   reg,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	uploadID := initMultipart(t, handler, mount, "phases.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "phases.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "phases.bin", uploadID, 2, p2)
	rec := completeMultipart(t, handler, mount, "phases.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status=%d body=%s", rec.Code, rec.Body.String())
	}

	body := scrapeMetrics(t, reg)
	for _, phase := range []string{"concat", "lock_wait", "hash", "pebble_batch"} {
		line := fmt.Sprintf(`filegate_multipart_complete_phase_seconds_count{phase="%s"} 1`, phase)
		if !strings.Contains(body, line) {
			t.Errorf("complete-phase histogram missing observation for phase %q\n  want: %s", phase, line)
		}
	}
}
