//go:build linux

package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/k2b-dev/filegate/v3/adapter/http"
	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

const (
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRegion    = "us-east-1"
)

// newTestServer wires up a real domain.Service + Pebble index +
// S3 router for end-to-end tests. The mount-name is sanitized so it
// passes ValidateBucketName in M0/M1 paths.
func newTestServer(t *testing.T) (svc *domain.Service, handler http.Handler, mount string, cleanup func()) {
	t.Helper()
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	// Pre-rename the temp dir to a guaranteed-valid bucket name —
	// t.TempDir's leaf is a random hex string, lowercased fits the
	// rules but the leading digit could trip something. Easier to
	// move the basePath under a "data" subdir.
	mountPath := baseDir + "/data"
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatalf("mkdir mount: %v", err)
	}
	svc, err = domain.NewService(idx, filesystem.New(), bus, []string{mountPath}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	handler, err = NewHandler(svc, Options{
		Region:    testRegion,
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
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
	return svc, handler, "data", cleanup
}

// TestUserMetadataHeadersAreLowercase pins the contract that
// x-amz-meta-* response headers are emitted lowercase on the wire.
// AWS S3 emits them lowercase; SDK parsers (boto3, awscli) strip
// the prefix and keep the remaining case as-is, so a title-cased
// "X-Amz-Meta-Author" surfaces as ".Metadata.Author" instead of
// ".Metadata.author" and breaks code that round-trips metadata.
//
// Pre-fix Go's http.Header.Set canonicalized the key into Title-
// Case. The fix writes directly to the map with a lowercased key
// so net/http leaves it alone on the wire.
func TestUserMetadataHeadersAreLowercase(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("body")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/m.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("x-amz-meta-author", "alice")
	req.Header.Set("x-amz-meta-build-id", "42")
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	hReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/m.txt", nil)
	hReq.Host = "example.com"
	signRequestPayload(hReq, nil)
	hRec := httptest.NewRecorder()
	handler.ServeHTTP(hRec, hReq)

	// httptest's recorder preserves header case via its Header
	// map. Look up by the lowercase form directly via the helper —
	// it does map access without Go's canonicalization, so it sees
	// the wire-form keys filegate emits.
	got := hRec.Header()
	if v := getMetaHeader(got, "x-amz-meta-author"); v != "alice" {
		t.Errorf("x-amz-meta-author not present as lowercase header; got headers: %v", got)
	}
	if v := getMetaHeader(got, "x-amz-meta-build-id"); v != "42" {
		t.Errorf("x-amz-meta-build-id not present as lowercase header; got headers: %v", got)
	}
}

// getMetaHeader looks up an x-amz-meta-* response header without
// going through http.Header.Get's canonicalization (which would
// look for "X-Amz-Meta-Foo" when our wire-lowercase storage is
// keyed "x-amz-meta-foo"). Tests use this so they exercise the
// lowercase form clients see on the wire.
func getMetaHeader(h http.Header, name string) string {
	values := h[strings.ToLower(name)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// signRequestPayload signs an http.Request with SigV4 over the given
// payload — equivalent to what a real S3 client SDK does.
func signRequestPayload(req *http.Request, payload []byte) {
	hash := sha256.Sum256(payload)
	signRequest(req, testAccessKey, testSecretKey, testRegion, hex.EncodeToString(hash[:]), time.Now())
}

// TestPutGetRoundTrip pins the object lifecycle: a signed PUT writes
// bytes, a signed GET reads them back with matching ETag and
// Content-Length. The ETag returned by PUT must equal the response
// body's MD5.
func TestPutGetRoundTrip(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("hello, s3 world")
	expectedMD5 := md5.Sum(body)
	expectedETag := hex.EncodeToString(expectedMD5[:])

	// PUT
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/k.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	gotETag := strings.Trim(rec.Header().Get("ETag"), `"`)
	if gotETag != expectedETag {
		t.Fatalf("PUT ETag=%q, want %q", gotETag, expectedETag)
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/k.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, body) {
		t.Fatalf("GET body=%q, want %q", got, body)
	}
	if got := strings.Trim(rec.Header().Get("ETag"), `"`); got != expectedETag {
		t.Fatalf("GET ETag=%q, want %q", got, expectedETag)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Fatalf("GET Content-Length=%q, want %d", got, len(body))
	}
}

func TestRESTUploadParadigmsRemainReadableViaS3(t *testing.T) {
	svc, s3Handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	const token = "rest-upload-cross-protocol-token"
	restHandler := httpadapter.NewRouter(svc, httpadapter.RouterOptions{
		BearerToken:           token,
		AccessLogEnabled:      false,
		MaxChunkBytes:         64 << 20,
		MaxUploadBytes:        64 << 20,
		MaxSessionUploadBytes: 64 << 20,
		UploadMinFreeBytes:    1 << 20,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
	})
	if closer, ok := restHandler.(interface{ Close() error }); ok {
		defer closer.Close()
	}

	uploadSessionBody := []byte("session-upload bytes visible to s3")
	session := createRESTUploadSessionForS3Test(t, restHandler, token, apiv1.UploadSessionCreateRequest{
		Path:        mount + "/from-session.txt",
		Size:        int64(len(uploadSessionBody)),
		Checksum:    sha256ForS3Test(uploadSessionBody),
		SegmentSize: 8,
		OnConflict:  "error",
	})
	for _, segment := range session.Segments {
		part := uploadSessionBody[segment.Offset : segment.Offset+segment.Size]
		req := httptest.NewRequest(http.MethodPut, "/v1/uploads/sessions/"+session.ID+"/segments/"+fmt.Sprint(segment.Index), bytes.NewReader(part))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Segment-Checksum", sha256ForS3Test(part))
		rec := httptest.NewRecorder()
		restHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST session segment %d status=%d body=%s", segment.Index, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	restHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST session commit status=%d body=%s", rec.Code, rec.Body.String())
	}

	directBody := []byte("direct-upload bytes visible to s3")
	directReq := apiv1.DirectUploadURLRequest{
		Path:             mount + "/from-direct.txt",
		ExpiresInSeconds: 60,
		ContentType:      "text/plain",
		MaxBytes:         int64(len(directBody)),
	}
	raw, err := json.Marshal(directReq)
	if err != nil {
		t.Fatalf("marshal direct request: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/uploads/direct", bytes.NewReader(raw))
	req.Host = "filegate.local"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	restHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("direct URL create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var direct apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(rec.Body).Decode(&direct); err != nil {
		t.Fatalf("decode direct URL: %v", err)
	}
	req = httptest.NewRequest(http.MethodPut, direct.UploadURL, bytes.NewReader(directBody))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	restHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("direct PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	assertS3ObjectBytesAndETag(t, s3Handler, mount, "from-session.txt", uploadSessionBody)
	assertS3ObjectBytesAndETag(t, s3Handler, mount, "from-direct.txt", directBody)
}

func createRESTUploadSessionForS3Test(t *testing.T, handler http.Handler, token string, body apiv1.UploadSessionCreateRequest) apiv1.UploadSessionResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal session request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads/sessions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("REST session create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out apiv1.UploadSessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return out
}

func assertS3ObjectBytesAndETag(t *testing.T, handler http.Handler, mount, key string, body []byte) {
	t.Helper()
	wantMD5 := md5.Sum(body)
	wantETag := hex.EncodeToString(wantMD5[:])

	headReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/"+key, nil)
	headReq.Host = "example.com"
	signRequestPayload(headReq, nil)
	headRec := httptest.NewRecorder()
	handler.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("S3 HEAD %s status=%d body=%s", key, headRec.Code, headRec.Body.String())
	}
	if got := strings.Trim(headRec.Header().Get("ETag"), `"`); got != wantETag {
		t.Fatalf("S3 HEAD %s ETag=%q, want %q", key, got, wantETag)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/"+key, nil)
	getReq.Host = "example.com"
	signRequestPayload(getReq, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("S3 GET %s status=%d body=%s", key, getRec.Code, getRec.Body.String())
	}
	if !bytes.Equal(getRec.Body.Bytes(), body) {
		t.Fatalf("S3 GET %s body=%q, want %q", key, getRec.Body.Bytes(), body)
	}
	if got := strings.Trim(getRec.Header().Get("ETag"), `"`); got != wantETag {
		t.Fatalf("S3 GET %s ETag=%q, want %q", key, got, wantETag)
	}
}

func sha256ForS3Test(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPutContentMD5Verified: a Content-MD5 header that matches the
// body succeeds; one that doesn't fails with BadDigest.
func TestPutContentMD5Verified(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("body for md5")
	correctMD5 := md5.Sum(body)
	correctMD5b64 := base64.StdEncoding.EncodeToString(correctMD5[:])

	// Correct Content-MD5
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/m1.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("Content-MD5", correctMD5b64)
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct Content-MD5 PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Wrong Content-MD5 → BadDigest
	wrongMD5 := md5.Sum([]byte("DIFFERENT"))
	wrongMD5b64 := base64.StdEncoding.EncodeToString(wrongMD5[:])
	req = httptest.NewRequest(http.MethodPut, "/"+mount+"/m2.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("Content-MD5", wrongMD5b64)
	signRequestPayload(req, body)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong Content-MD5 PUT status=%d, want 400 (BadDigest)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BadDigest") {
		t.Fatalf("wrong Content-MD5 PUT body should mention BadDigest, got %q", rec.Body.String())
	}
}

// TestPutIfNoneMatchAny: With "If-None-Match: *", a PUT that lands
// on an existing key must fail with PreconditionFailed.
func TestPutIfNoneMatchAny(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("v1")
	doPut := func(extraHeaders map[string]string) int {
		req := httptest.NewRequest(http.MethodPut, "/"+mount+"/cond.txt", bytes.NewReader(body))
		req.Host = "example.com"
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		signRequestPayload(req, body)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := doPut(nil); got != http.StatusOK {
		t.Fatalf("first PUT status=%d", got)
	}
	if got := doPut(map[string]string{"If-None-Match": "*"}); got != http.StatusPreconditionFailed {
		t.Fatalf("If-None-Match * PUT status=%d, want 412", got)
	}
}

// TestRangeRequest: GET with Range: bytes=N-M returns 206 with the
// requested slice and proper Content-Range.
func TestRangeRequest(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/range.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d", rec.Code)
	}

	cases := []struct {
		hdr            string
		wantStatus     int
		wantBody       string
		wantRangeHdr   string
		wantContentLen string
	}{
		{"bytes=0-3", http.StatusPartialContent, "0123", "bytes 0-3/10", "4"},
		{"bytes=5-7", http.StatusPartialContent, "567", "bytes 5-7/10", "3"},
		{"bytes=8-", http.StatusPartialContent, "89", "bytes 8-9/10", "2"},
		{"bytes=-3", http.StatusPartialContent, "789", "bytes 7-9/10", "3"},
	}
	for _, tc := range cases {
		req = httptest.NewRequest(http.MethodGet, "/"+mount+"/range.txt", nil)
		req.Host = "example.com"
		req.Header.Set("Range", tc.hdr)
		signRequestPayload(req, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("Range %q: status=%d, want %d body=%s", tc.hdr, rec.Code, tc.wantStatus, rec.Body.String())
			continue
		}
		if rec.Body.String() != tc.wantBody {
			t.Errorf("Range %q: body=%q, want %q", tc.hdr, rec.Body.String(), tc.wantBody)
		}
		if got := rec.Header().Get("Content-Range"); got != tc.wantRangeHdr {
			t.Errorf("Range %q: Content-Range=%q, want %q", tc.hdr, got, tc.wantRangeHdr)
		}
		if got := rec.Header().Get("Content-Length"); got != tc.wantContentLen {
			t.Errorf("Range %q: Content-Length=%q, want %q", tc.hdr, got, tc.wantContentLen)
		}
	}
}

// TestUserMetadataRoundTrip: x-amz-meta-* headers on PUT are
// preserved and surface on GET/HEAD.
func TestUserMetadataRoundTrip(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("test")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/m.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("x-amz-meta-author", "alice")
	req.Header.Set("x-amz-meta-purpose", "demo")
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	// HEAD must echo the metadata.
	req = httptest.NewRequest(http.MethodHead, "/"+mount+"/m.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d", rec.Code)
	}
	if got := getMetaHeader(rec.Header(), "x-amz-meta-author"); got != "alice" {
		t.Errorf("x-amz-meta-author=%q, want alice", got)
	}
	if got := getMetaHeader(rec.Header(), "x-amz-meta-purpose"); got != "demo" {
		t.Errorf("x-amz-meta-purpose=%q, want demo", got)
	}
}

// TestDeleteIdempotent: DELETE on a known key returns 204; DELETE on
// a missing key also returns 204 (S3 idempotent semantics).
func TestDeleteIdempotent(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Create.
	body := []byte("doomed")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/d.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// First delete: should succeed with 204.
	req = httptest.NewRequest(http.MethodDelete, "/"+mount+"/d.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first DELETE status=%d, want 204", rec.Code)
	}

	// Second delete (now non-existent): also 204 per S3 idempotent rule.
	req = httptest.NewRequest(http.MethodDelete, "/"+mount+"/d.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second DELETE status=%d, want 204", rec.Code)
	}

	// GET on a deleted key → NoSuchKey.
	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/d.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status=%d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NoSuchKey") {
		t.Errorf("GET after DELETE body should contain NoSuchKey: %q", rec.Body.String())
	}
}

// TestListBucketsReturnsConfiguredMounts: GET / with a valid signed
// request returns the ListAllMyBucketsResult including the
// configured mount.
func TestListBucketsReturnsConfiguredMounts(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListBuckets status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), mount) {
		t.Errorf("ListBuckets body missing %q: %s", mount, rec.Body.String())
	}
}

// TestRejectInvalidObjectKey: keys violating the M0 subset (e.g.
// trailing slash, empty segment, .., reserved namespace) get 400
// InvalidArgument.
func TestRejectInvalidObjectKey(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	cases := []string{
		"trailing/",
		"a//b",
		"a/./b",
		"a/../b",
		".fg-versions/leak",
		".fg-uploads/leak",
		"nested/.fg-versions/leak",
		"nested/.fg-uploads/leak",
	}
	for _, key := range cases {
		req := httptest.NewRequest(http.MethodPut, "/"+mount+"/"+key, strings.NewReader("x"))
		req.Host = "example.com"
		signRequestPayload(req, []byte("x"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("key=%q PUT status=%d, want 400", key, rec.Code)
		}
	}
}

// TestDeleteRefusesDirectoryKey: DELETE on a key that resolves to a
// filegate directory must NOT recursively delete the prefix; it
// should return 204 idempotent (S3 has no concept of a directory
// object in path-style).
func TestDeleteRefusesDirectoryKey(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Create a file under "folder/leaf.txt" — that creates the
	// "folder" directory as a side effect.
	body := []byte("leaf")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/folder/leaf.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// DELETE on "folder" — should be 204 idempotent.
	req = httptest.NewRequest(http.MethodDelete, "/"+mount+"/folder", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE on dir status=%d, want 204", rec.Code)
	}
	// The leaf must still exist — DELETE on the dir-key didn't
	// recursively wipe.
	if _, err := svc.GetFileByVirtualPath("/" + mount + "/folder/leaf.txt"); err != nil {
		t.Fatalf("leaf should survive dir-key DELETE: %v", err)
	}
}

// TestConditionalHeadNotModified: HEAD with If-None-Match matching
// the current ETag returns 304 (mirrors GET behavior).
func TestConditionalHeadNotModified(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("head-cond")
	expectedMD5 := md5.Sum(body)
	expectedETag := hex.EncodeToString(expectedMD5[:])

	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/h.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodHead, "/"+mount+"/h.txt", nil)
	req.Host = "example.com"
	req.Header.Set("If-None-Match", `"`+expectedETag+`"`)
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("HEAD If-None-Match status=%d, want 304", rec.Code)
	}
}

// TestRangeUnsatisfiableReturns416: a range outside object bounds
// returns 416 with Content-Range: bytes */<size> and an
// InvalidRange XML body.
func TestRangeUnsatisfiableReturns416(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("0123456789")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/r.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/r.txt", nil)
	req.Host = "example.com"
	req.Header.Set("Range", "bytes=100-200")
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-bounds Range status=%d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", len(body)) {
		t.Errorf("Content-Range=%q, want bytes */%d", got, len(body))
	}
	if !strings.Contains(rec.Body.String(), "InvalidRange") {
		t.Errorf("body should mention InvalidRange: %q", rec.Body.String())
	}
}

// TestRangeOnEmptyObject: any Range on a 0-byte object → 416.
func TestRangeOnEmptyObject(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/empty.txt", bytes.NewReader(nil))
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/empty.txt", nil)
	req.Host = "example.com"
	req.Header.Set("Range", "bytes=-3")
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("Range on empty status=%d, want 416", rec.Code)
	}
}

// TestPutIfMatch: If-Match with the current ETag → success;
// If-Match with a stale ETag → 412 PreconditionFailed and the
// existing object stays untouched.
func TestPutIfMatch(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	v1 := []byte("v1")
	v1MD5 := md5.Sum(v1)
	v1ETag := hex.EncodeToString(v1MD5[:])

	// Create.
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/cas.txt", bytes.NewReader(v1))
	req.Host = "example.com"
	signRequestPayload(req, v1)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d", rec.Code)
	}

	// Compare-and-swap with current ETag → success, body becomes v2.
	v2 := []byte("v2 final")
	v2MD5 := md5.Sum(v2)
	v2ETag := hex.EncodeToString(v2MD5[:])
	req = httptest.NewRequest(http.MethodPut, "/"+mount+"/cas.txt", bytes.NewReader(v2))
	req.Host = "example.com"
	req.Header.Set("If-Match", `"`+v1ETag+`"`)
	signRequestPayload(req, v2)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CAS PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Trim(rec.Header().Get("ETag"), `"`); got != v2ETag {
		t.Errorf("CAS PUT ETag=%q, want %q", got, v2ETag)
	}

	// Compare-and-swap with the OLD (now stale) ETag → 412.
	req = httptest.NewRequest(http.MethodPut, "/"+mount+"/cas.txt", bytes.NewReader([]byte("v3")))
	req.Host = "example.com"
	req.Header.Set("If-Match", `"`+v1ETag+`"`) // stale
	signRequestPayload(req, []byte("v3"))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale-CAS PUT status=%d, want 412", rec.Code)
	}

	// The object should still be v2 after the failed CAS.
	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/cas.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Body.Bytes(); !bytes.Equal(got, v2) {
		t.Errorf("after failed CAS body=%q, want v2", got)
	}
}

// TestPutIfMatchOnMissingObject: PutObject with If-Match against a
// non-existent key fails (you can't compare-and-swap against
// nothing). AWS returns 412 PreconditionFailed.
func TestPutIfMatchOnMissingObject(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("x")
	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/missing.txt", bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("If-Match", `"any"`)
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match on missing status=%d, want 412", rec.Code)
	}
}

// TestDeleteIfMatch: DELETE with If-Match matching the current
// ETag succeeds; with a stale ETag, returns 412 and the object
// stays.
func TestDeleteIfMatch(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("delete-me")
	bodyMD5 := md5.Sum(body)
	bodyETag := hex.EncodeToString(bodyMD5[:])

	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/del.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// DELETE with stale ETag → 412.
	req = httptest.NewRequest(http.MethodDelete, "/"+mount+"/del.txt", nil)
	req.Host = "example.com"
	req.Header.Set("If-Match", `"deadbeef"`)
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match DELETE status=%d, want 412", rec.Code)
	}

	// Object should still be there.
	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/del.txt", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after stale-DELETE GET status=%d, want 200", rec.Code)
	}

	// DELETE with current ETag → 204 success.
	req = httptest.NewRequest(http.MethodDelete, "/"+mount+"/del.txt", nil)
	req.Host = "example.com"
	req.Header.Set("If-Match", `"`+bodyETag+`"`)
	signRequestPayload(req, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("matching If-Match DELETE status=%d, want 204", rec.Code)
	}
}

// TestConditionalGet: If-None-Match with the current ETag returns
// 304 Not Modified.
func TestConditionalGet(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	body := []byte("etag-test")
	expectedMD5 := md5.Sum(body)
	expectedETag := hex.EncodeToString(expectedMD5[:])

	req := httptest.NewRequest(http.MethodPut, "/"+mount+"/c.txt", bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// If-None-Match with the actual ETag → 304.
	req = httptest.NewRequest(http.MethodGet, "/"+mount+"/c.txt", nil)
	req.Host = "example.com"
	req.Header.Set("If-None-Match", `"`+expectedETag+`"`)
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status=%d, want 304", rec.Code)
	}
}
