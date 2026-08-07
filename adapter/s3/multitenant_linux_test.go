//go:build linux

package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// newMultiTenantTestServer wires up a service with multiple mounts +
// a router configured with the given Keys list. Mount names are
// "alpha", "beta", "gamma" so tests can pin per-key access patterns.
func newMultiTenantTestServer(t *testing.T, keys []KeyEntry) (svc *domain.Service, handler http.Handler, cleanup func()) {
	t.Helper()
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	mountPaths := []string{baseDir + "/alpha", baseDir + "/beta", baseDir + "/gamma"}
	for _, p := range mountPaths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	svc, err = domain.NewService(idx, filesystem.New(), bus, mountPaths, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	handler, err = NewHandler(svc, Options{
		Region: testRegion,
		Keys:   keys,
	})
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new handler: %v", err)
	}
	cleanup = func() {
		testSvcGlobal = nil
		bus.Close()
		_ = idx.Close()
	}
	testSvcGlobal = svc
	return svc, handler, cleanup
}

// signWithKey signs a request using the supplied (accessKey, secretKey)
// pair so multi-tenant tests can simulate different callers without
// touching the global testAccessKey/testSecretKey.
func signWithKey(req *http.Request, payload []byte, accessKey, secretKey string) {
	hash := sha256.Sum256(payload)
	signRequest(req, accessKey, secretKey, testRegion, hex.EncodeToString(hash[:]), time.Now())
}

// TestMultiTenantListBucketsFilteredByKey: ListBuckets returns only
// the buckets in the requesting key's whitelist — never the full
// mount list. A second key with a disjoint whitelist sees its own
// subset; an admin key with "*" sees all three.
func TestMultiTenantListBucketsFilteredByKey(t *testing.T) {
	const (
		k1Access = "AKIAFIRSTKEY00000001"
		k1Secret = "secret-for-first-key-fillerfillerfiller"
		k2Access = "AKIASECONDKEY0000002"
		k2Secret = "secret-for-second-key-fillerfillerfille"
		kAAccess = "AKIAADMINKEY00000003"
		kASecret = "secret-for-admin-key-fillerfillerfillrr"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: k1Access, SecretKey: k1Secret, Buckets: []string{"alpha"}},
		{AccessKey: k2Access, SecretKey: k2Secret, Buckets: []string{"beta", "gamma"}},
		{AccessKey: kAAccess, SecretKey: kASecret, Buckets: []string{"*"}},
	})
	defer cleanup()

	listFor := func(access, secret string) []string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "example.com"
		signWithKey(req, nil, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ListBuckets status=%d body=%s", rec.Code, rec.Body.String())
		}
		var res listAllMyBucketsResult
		_ = xml.Unmarshal(rec.Body.Bytes(), &res)
		out := []string{}
		for _, b := range res.Buckets.Bucket {
			out = append(out, b.Name)
		}
		return out
	}

	if got := listFor(k1Access, k1Secret); !equalSorted(got, []string{"alpha"}) {
		t.Errorf("k1 ListBuckets=%v, want [alpha]", got)
	}
	if got := listFor(k2Access, k2Secret); !equalSorted(got, []string{"beta", "gamma"}) {
		t.Errorf("k2 ListBuckets=%v, want [beta gamma]", got)
	}
	if got := listFor(kAAccess, kASecret); !equalSorted(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("admin ListBuckets=%v, want [alpha beta gamma]", got)
	}
}

// TestMultiTenantBucketAccessForbidden: a key trying to access a
// bucket NOT in its whitelist gets AccessDenied — and crucially,
// the response cannot distinguish "bucket exists" from "bucket
// doesn't exist". Both forbidden + nonexistent return the same 403
// AccessDenied so an attacker can't probe for bucket names.
func TestMultiTenantBucketAccessForbidden(t *testing.T) {
	const (
		access = "AKIASCOPEDKEY0000001"
		secret = "secret-for-scoped-key-fillerfillerfill"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: []string{"alpha"}},
	})
	defer cleanup()

	probe := func(method, path string) (*httptest.ResponseRecorder, []byte) {
		body := []byte("dummy")
		var r *http.Request
		switch method {
		case http.MethodPut:
			r = httptest.NewRequest(method, path, bytes.NewReader(body))
		default:
			body = nil
			r = httptest.NewRequest(method, path, nil)
		}
		r.Host = "example.com"
		signWithKey(r, body, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec, body
	}

	// Permitted bucket: should NOT be 403 (anything else is fine —
	// we're testing the authorization gate, not the operation).
	rec, _ := probe(http.MethodGet, "/alpha")
	if rec.Code == http.StatusForbidden {
		t.Errorf("permitted bucket should NOT return 403, got body=%s", rec.Body.String())
	}

	// Forbidden but EXISTING bucket: 403 AccessDenied.
	rec, _ = probe(http.MethodGet, "/beta")
	if rec.Code != http.StatusForbidden {
		t.Errorf("forbidden bucket GET status=%d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AccessDenied") {
		t.Errorf("forbidden bucket body=%s, want AccessDenied", rec.Body.String())
	}

	// Nonexistent bucket (also not in whitelist): same 403 — does
	// NOT leak that the bucket is missing.
	rec, _ = probe(http.MethodGet, "/nosuchbucket")
	if rec.Code != http.StatusForbidden {
		t.Errorf("nonexistent bucket GET status=%d, want 403 (existence must not leak)", rec.Code)
	}

	// Same isolation on object-level ops.
	rec, _ = probe(http.MethodPut, "/beta/file.txt")
	if rec.Code != http.StatusForbidden {
		t.Errorf("forbidden object PUT status=%d, want 403", rec.Code)
	}
	rec, _ = probe(http.MethodGet, "/beta/anything")
	if rec.Code != http.StatusForbidden {
		t.Errorf("forbidden object GET status=%d, want 403", rec.Code)
	}
	rec, _ = probe(http.MethodDelete, "/beta/anything")
	if rec.Code != http.StatusForbidden {
		t.Errorf("forbidden object DELETE status=%d, want 403", rec.Code)
	}
	rec, _ = probe(http.MethodHead, "/beta")
	if rec.Code != http.StatusForbidden {
		t.Errorf("forbidden bucket HEAD status=%d, want 403", rec.Code)
	}
}

// TestMultiTenantWildcardAccess: a key with the "*" wildcard can
// reach every configured mount.
func TestMultiTenantWildcardAccess(t *testing.T) {
	const (
		access = "AKIAWILDCARDKEY00001"
		secret = "secret-wildcard-key-fillerfillerfillerfill"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: []string{"*"}},
	})
	defer cleanup()

	for _, b := range []string{"alpha", "beta", "gamma"} {
		req := httptest.NewRequest(http.MethodHead, "/"+b, nil)
		req.Host = "example.com"
		signWithKey(req, nil, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("wildcard key was forbidden on bucket %q", b)
		}
	}
}

// TestMultiTenantEmptyWhitelistDenies: a key with an empty Buckets
// slice authenticates successfully but every bucket op returns 403.
// Useful for staging revocation without removing the entry.
func TestMultiTenantEmptyWhitelistDenies(t *testing.T) {
	const (
		access = "AKIANOACCESSKEY00001"
		secret = "secret-no-access-key-fillerfillerfilfil"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: nil},
	})
	defer cleanup()

	// ListBuckets should succeed but return zero buckets (the key
	// itself is valid).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "example.com"
	signWithKey(req, nil, access, secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListBuckets with empty whitelist status=%d, want 200", rec.Code)
	}
	var res listAllMyBucketsResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Buckets.Bucket) != 0 {
		t.Errorf("empty whitelist returned %d buckets, want 0", len(res.Buckets.Bucket))
	}

	// Any specific-bucket op is 403.
	req = httptest.NewRequest(http.MethodHead, "/alpha", nil)
	req.Host = "example.com"
	signWithKey(req, nil, access, secret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty-whitelist HEAD bucket status=%d, want 403", rec.Code)
	}
}

// TestNewHandlerRejectsDuplicateAccessKey: two Keys entries with
// the same access key must be rejected at startup. This catches an
// operator who pasted a key twice — silent override would be a
// confusing security hazard.
func TestNewHandlerRejectsDuplicateAccessKey(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()
	bus := eventbus.New()
	defer bus.Close()
	mountPath := baseDir + "/data"
	_ = os.MkdirAll(mountPath, 0o755)
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{mountPath}, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	_, err = NewHandler(svc, Options{
		Region: testRegion,
		Keys: []KeyEntry{
			{AccessKey: "DUPLICATEKEY00000001", SecretKey: "secret-1", Buckets: []string{"data"}},
			{AccessKey: "DUPLICATEKEY00000001", SecretKey: "secret-2", Buckets: []string{"data"}},
		},
	})
	if err == nil {
		t.Fatalf("duplicate access key should have been rejected at construction")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("error=%q, want mention of duplicate", err.Error())
	}
}

// TestNewHandlerRejectsUnknownBucketInWhitelist: a Keys entry whose
// whitelist references a mount that doesn't exist must fail at
// startup. Catches typos and misconfigurations early — an operator
// who whitelists "buckte" instead of "bucket" gets a clean error
// instead of silently no-access for that key.
func TestNewHandlerRejectsUnknownBucketInWhitelist(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()
	bus := eventbus.New()
	defer bus.Close()
	mountPath := baseDir + "/data"
	_ = os.MkdirAll(mountPath, 0o755)
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{mountPath}, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	_, err = NewHandler(svc, Options{
		Region: testRegion,
		Keys: []KeyEntry{
			{AccessKey: "AKIA00000000000000A1", SecretKey: "valid-secret", Buckets: []string{"buckte"}},
		},
	})
	if err == nil {
		t.Fatalf("unknown bucket in whitelist should be rejected")
	}
	if !strings.Contains(err.Error(), "buckte") {
		t.Errorf("error=%q, want mention of unknown bucket", err.Error())
	}
}

// TestLegacySingleTenantStillWorks: AccessKey/SecretKey at the top
// level (M1 backward compatibility) still grants access to every
// mount via the synthesized "*" entry.
func TestLegacySingleTenantStillWorks(t *testing.T) {
	const (
		legacyAccess = "AKIALEGACYKEY0000001"
		legacySecret = "secret-legacy-fillerfillerfillerfillerff"
	)
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	defer bus.Close()
	defer idx.Close()
	mountPaths := []string{baseDir + "/alpha", baseDir + "/beta"}
	for _, p := range mountPaths {
		_ = os.MkdirAll(p, 0o755)
	}
	svc, err := domain.NewService(idx, filesystem.New(), bus, mountPaths, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler, err := NewHandler(svc, Options{
		Region:    testRegion,
		AccessKey: legacyAccess,
		SecretKey: legacySecret,
	})
	if err != nil {
		t.Fatalf("NewHandler legacy single-tenant: %v", err)
	}

	for _, b := range []string{"alpha", "beta"} {
		req := httptest.NewRequest(http.MethodHead, "/"+b, nil)
		req.Host = "example.com"
		signWithKey(req, nil, legacyAccess, legacySecret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("legacy single-tenant key forbidden on bucket %q", b)
		}
	}
}

// TestRateLimitReturnsSlowDownAndRetryAfter pins the end-to-end
// contract: a key whose rate limit is exhausted gets 503
// SlowDown with a Retry-After header. Real SDKs (boto3, awscli,
// rclone) honour this with exponential backoff.
func TestRateLimitReturnsSlowDownAndRetryAfter(t *testing.T) {
	const (
		access = "AKIATHROTTLED0000001"
		secret = "secret-throttled-fillerfillerfillerfille"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: []string{"*"},
			RequestsPerSecond: 2, Burst: 2}, // tight, easy to exhaust
	})
	defer cleanup()

	// Use GET (not HEAD) so the error body is delivered too —
	// writeError suppresses bodies on HEAD per HTTP semantics.
	doList := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/alpha?list-type=2", nil)
		r.Host = "example.com"
		signWithKey(r, nil, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	// First two requests use the burst — must succeed.
	for i := 0; i < 2; i++ {
		rec := doList()
		if rec.Code == http.StatusServiceUnavailable {
			t.Fatalf("burst request #%d throttled, want admitted", i+1)
		}
	}
	// Third request — bucket empty, must SlowDown.
	rec := doList()
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("over-burst request status=%d, want 503 SlowDown", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SlowDown") {
		t.Errorf("body should contain SlowDown, got %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("503 SlowDown response missing Retry-After header")
	}
}

// TestNewHandlerRejectsNegativeRPS pins startup validation:
// requests_per_second < 0 is a typo, not a "disable" signal.
// 0 is the documented disable value; negative would silently
// remove the operator's intended limit. We fail loudly at
// construction.
func TestNewHandlerRejectsNegativeRPS(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()
	bus := eventbus.New()
	defer bus.Close()
	mountPath := baseDir + "/data"
	_ = os.MkdirAll(mountPath, 0o755)
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{mountPath}, 1000)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}

	_, err = NewHandler(svc, Options{
		Region: testRegion,
		Keys: []KeyEntry{
			{AccessKey: "AKIANEGATIVE00000001", SecretKey: "ok-secret-fillerfillerfillerfillerf",
				Buckets: []string{"data"}, RequestsPerSecond: -1},
		},
	})
	if err == nil {
		t.Fatalf("negative RPS should have been rejected at construction")
	}
	if !strings.Contains(err.Error(), "requests_per_second") {
		t.Errorf("error=%q, want mention of requests_per_second", err.Error())
	}
}

// TestRateLimitFiresBeforeBodyBinding pins the codex-flagged
// invariant: a throttled key's PUT must hit the SlowDown
// response BEFORE verifyRequest reads the body. Otherwise a
// flooded key still forces per-request body buffering even
// though it should be paying nothing.
//
// We verify by sending a PUT with a body that would normally be
// hex-SHA256-validated (forcing verifyRequest to pre-read it),
// counting bytes consumed from the body reader. A throttled
// SlowDown response means zero bytes consumed.
func TestRateLimitFiresBeforeBodyBinding(t *testing.T) {
	const (
		access = "AKIAPRELIMITKEY00001"
		secret = "secret-prelimit-fillerfillerfillerfille"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: access, SecretKey: secret, Buckets: []string{"*"},
			RequestsPerSecond: 1, Burst: 1},
	})
	defer cleanup()

	// Drain the bucket with a cheap GET.
	doGet := httptest.NewRequest(http.MethodGet, "/alpha?list-type=2", nil)
	doGet.Host = "example.com"
	signWithKey(doGet, nil, access, secret)
	gRec := httptest.NewRecorder()
	handler.ServeHTTP(gRec, doGet)
	if gRec.Code == http.StatusServiceUnavailable {
		t.Fatalf("burst GET throttled, want admitted")
	}

	// Now PUT a 1 MiB body with a counting reader. If rate-limit
	// fires before body binding, the reader is never touched.
	body := bytes.Repeat([]byte("x"), 1024*1024)
	type countingReader struct {
		src       *bytes.Reader
		readCount int64
	}
	cr := &countingReader{src: bytes.NewReader(body)}
	// Wrap in a closure-based reader so we can count.
	read := func(p []byte) (int, error) {
		n, err := cr.src.Read(p)
		cr.readCount += int64(n)
		return n, err
	}

	pReq := httptest.NewRequest(http.MethodPut, "/alpha/throttle-target.bin", io.NopCloser(readerFunc(read)))
	pReq.Host = "example.com"
	pReq.ContentLength = int64(len(body))
	signWithKey(pReq, body, access, secret) // hex-SHA256 mode forces body verify
	pRec := httptest.NewRecorder()
	handler.ServeHTTP(pRec, pReq)

	if pRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-burst PUT status=%d, want 503 SlowDown", pRec.Code)
	}
	if cr.readCount > 0 {
		t.Errorf("body reader was consumed (%d bytes) before rate-limit fired — body buffering not bypassed", cr.readCount)
	}
}

// TestRateLimitDoesNotApplyToUnconfiguredKeys: a key without
// RequestsPerSecond MUST never be throttled, no matter how many
// requests it issues. Catches a regression where the limiter
// would default to throttling unconfigured keys.
func TestRateLimitDoesNotApplyToUnconfiguredKeys(t *testing.T) {
	const (
		throttledKey = "AKIATHROTTLED0000002"
		throttledSec = "secret-throttled-2-fillerfillerfillerff"
		unlimitedKey = "AKIAUNLIMITED0000003"
		unlimitedSec = "secret-unlimited-fillerfillerfillerfille"
	)
	_, handler, cleanup := newMultiTenantTestServer(t, []KeyEntry{
		{AccessKey: throttledKey, SecretKey: throttledSec, Buckets: []string{"*"},
			RequestsPerSecond: 1, Burst: 1},
		{AccessKey: unlimitedKey, SecretKey: unlimitedSec, Buckets: []string{"*"}},
	})
	defer cleanup()

	doHead := func(access, secret string) int {
		r := httptest.NewRequest(http.MethodHead, "/alpha", nil)
		r.Host = "example.com"
		signWithKey(r, nil, access, secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}

	// Drain the throttled key.
	doHead(throttledKey, throttledSec)
	if code := doHead(throttledKey, throttledSec); code != http.StatusServiceUnavailable {
		t.Errorf("throttled key second request status=%d, want 503", code)
	}
	// Unlimited key must STILL succeed many times.
	for i := 0; i < 20; i++ {
		if code := doHead(unlimitedKey, unlimitedSec); code == http.StatusServiceUnavailable {
			t.Fatalf("unlimited key throttled at iteration %d (status=%d) — bucket leak between keys", i, code)
		}
	}
}

// readerFunc adapts a Read closure to io.Reader so tests can
// instrument byte counts without writing a full struct.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// equalSorted reports whether a and b contain the same strings,
// order-independent.
func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
	}
	for _, c := range count {
		if c != 0 {
			return false
		}
	}
	return true
}
