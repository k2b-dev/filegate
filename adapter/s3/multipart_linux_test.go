//go:build linux

package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// initMultipart helper: POST ?uploads → returns uploadId and asserts
// success.
func initMultipart(t *testing.T, handler http.Handler, mount, key string, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+mount+"/"+key+"?uploads", nil)
	req.Host = "example.com"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res initiateMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("XML decode: %v", err)
	}
	if res.UploadID == "" {
		t.Fatalf("empty UploadId")
	}
	return res.UploadID
}

// uploadPart helper: PUT ?partNumber=N&uploadId=X → returns ETag.
func uploadPart(t *testing.T, handler http.Handler, mount, key, uploadID string, partNumber int, body []byte) string {
	t.Helper()
	url := fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", mount, key, partNumber, uploadID)
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UploadPart part=%d status=%d body=%s", partNumber, rec.Code, rec.Body.String())
	}
	return strings.Trim(rec.Header().Get("ETag"), `"`)
}

// TestCreateMultipartUploadRoundTrip: CreateMultipartUpload returns
// uploadId; the on-disk staging dir and Pebble active-state row are present.
func TestCreateMultipartUploadRoundTrip(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "obj.bin", map[string]string{
		"Content-Type":      "application/octet-stream",
		"x-amz-meta-author": "alice",
	})

	mountAbs := lookupMountAbs(t, handler, mount)
	stageDir := stageDirFor(mountAbs, uploadID)
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("stage dir missing: %v", err)
	}
	upload := lookupActiveUpload(t, svc, uploadID)
	if upload.Bucket != mount || upload.Key != "obj.bin" {
		t.Errorf("active upload bucket/key=%q/%q, want %q/obj.bin", upload.Bucket, upload.Key, mount)
	}
	if upload.StageDir != stageDir {
		t.Errorf("active upload StageDir=%q, want %q", upload.StageDir, stageDir)
	}
	if upload.ContentType != "application/octet-stream" {
		t.Errorf("active upload ContentType=%q", upload.ContentType)
	}
	if got := upload.UserMetadata["author"]; got != "alice" {
		t.Errorf("UserMetadata[author]=%q, want alice", got)
	}
	if upload.Phase != domain.MultipartUploadInProgress {
		t.Errorf("Phase=%q, want in_progress", upload.Phase)
	}
}

// TestUploadPartStoresMD5: each UploadPart writes its part body
// + records the part-MD5 in active Pebble state. Duplicate UploadPart for
// same partNumber overwrites.
func TestUploadPartStoresMD5(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)

	// Part 1
	p1 := []byte("part-one-bytes")
	p1MD5 := md5.Sum(p1)
	gotETag := uploadPart(t, handler, mount, "k.bin", uploadID, 1, p1)
	if gotETag != hex.EncodeToString(p1MD5[:]) {
		t.Errorf("part1 ETag=%q, want %q", gotETag, hex.EncodeToString(p1MD5[:]))
	}

	// Part 2
	p2 := []byte("part-two-bytes-different")
	uploadPart(t, handler, mount, "k.bin", uploadID, 2, p2)

	parts := lookupActiveParts(t, svc, uploadID)
	if len(parts) != 2 {
		t.Fatalf("active parts=%d, want 2", len(parts))
	}
	if got := parts[1].ETag; got != hex.EncodeToString(p1MD5[:]) {
		t.Errorf("active Parts[1].ETag=%q", got)
	}
	if got := parts[1].Size; got != int64(len(p1)) {
		t.Errorf("active Parts[1].Size=%d, want %d", got, len(p1))
	}

	// Duplicate UploadPart for partNumber=1 with different bytes →
	// overwrites.
	p1New := []byte("DIFFERENT-PART-ONE-BYTES-NOW")
	p1NewMD5 := md5.Sum(p1New)
	gotNewETag := uploadPart(t, handler, mount, "k.bin", uploadID, 1, p1New)
	if gotNewETag != hex.EncodeToString(p1NewMD5[:]) {
		t.Errorf("duplicate part1 ETag=%q", gotNewETag)
	}
	parts = lookupActiveParts(t, svc, uploadID)
	if got := parts[1].ETag; got != hex.EncodeToString(p1NewMD5[:]) {
		t.Errorf("active state after duplicate Parts[1].ETag=%q, want updated", got)
	}
}

func TestUploadPartWriteSlotSerializesConcurrentParts(t *testing.T) {
	svc, _, mount, cleanup := newTestServer(t)
	defer cleanup()

	handler, err := NewHandler(svc, Options{
		Region:              testRegion,
		AccessKey:           testAccessKey,
		SecretKey:           testSecretKey,
		MaxConcurrentWrites: 1,
	})
	if err != nil {
		t.Fatalf("new limited handler: %v", err)
	}

	uploadA := initMultipart(t, handler, mount, "a.bin", nil)
	uploadB := initMultipart(t, handler, mount, "b.bin", nil)

	bodyA := newBlockingPartBody([]byte("first part"))
	reqA := unsignedPayloadRequest(http.MethodPut, fmt.Sprintf("/%s/a.bin?partNumber=1&uploadId=%s", mount, uploadA), bodyA)
	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		handler.ServeHTTP(recA, reqA)
		close(doneA)
	}()
	select {
	case <-bodyA.waiting:
	case <-doneA:
		t.Fatalf("first UploadPart completed before blocking body reached EOF; status=%d body=%s", recA.Code, recA.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatalf("first UploadPart did not reach blocking body read")
	}

	reqB := unsignedPayloadRequest(http.MethodPut, fmt.Sprintf("/%s/b.bin?partNumber=1&uploadId=%s", mount, uploadB), bytes.NewReader([]byte("second part")))
	recB := httptest.NewRecorder()
	doneB := make(chan struct{})
	go func() {
		handler.ServeHTTP(recB, reqB)
		close(doneB)
	}()

	select {
	case <-doneB:
		t.Fatalf("second UploadPart completed while first write slot was still held; status=%d body=%s", recB.Code, recB.Body.String())
	case <-time.After(100 * time.Millisecond):
		// Expected: second request is waiting for the single write slot.
	}

	bodyA.release()
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatalf("first UploadPart did not complete after release")
	}
	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatalf("second UploadPart did not complete after first slot released")
	}
	if recA.Code != http.StatusOK {
		t.Fatalf("first UploadPart status=%d body=%s", recA.Code, recA.Body.String())
	}
	if recB.Code != http.StatusOK {
		t.Fatalf("second UploadPart status=%d body=%s", recB.Code, recB.Body.String())
	}
}

func TestUploadPartAllowsParallelSameUploadParts(t *testing.T) {
	svc, _, mount, cleanup := newTestServer(t)
	defer cleanup()

	handler, err := NewHandler(svc, Options{
		Region:              testRegion,
		AccessKey:           testAccessKey,
		SecretKey:           testSecretKey,
		MaxConcurrentWrites: 64,
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	uploadID := initMultipart(t, handler, mount, "same-upload.bin", nil)
	bodyA := newBlockingPartBody([]byte("first part"))
	reqA := unsignedPayloadRequest(http.MethodPut, fmt.Sprintf("/%s/same-upload.bin?partNumber=1&uploadId=%s", mount, uploadID), bodyA)
	recA := httptest.NewRecorder()
	doneA := make(chan struct{})
	go func() {
		handler.ServeHTTP(recA, reqA)
		close(doneA)
	}()
	select {
	case <-bodyA.waiting:
	case <-doneA:
		t.Fatalf("first UploadPart completed before blocking body reached EOF; status=%d body=%s", recA.Code, recA.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatalf("first UploadPart did not reach blocking body read")
	}

	bodyB := newBlockingPartBody([]byte("second part"))
	reqB := unsignedPayloadRequest(http.MethodPut, fmt.Sprintf("/%s/same-upload.bin?partNumber=2&uploadId=%s", mount, uploadID), bodyB)
	recB := httptest.NewRecorder()
	doneB := make(chan struct{})
	go func() {
		handler.ServeHTTP(recB, reqB)
		close(doneB)
	}()

	select {
	case <-bodyB.waiting:
		// Expected: different part numbers of the same upload can stream in parallel.
	case <-doneB:
		t.Fatalf("second UploadPart completed while first same-upload part was still active; status=%d body=%s", recB.Code, recB.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatalf("second UploadPart did not start while first same-upload part was still active")
	}

	bodyA.release()
	bodyB.release()
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatalf("first UploadPart did not complete after release")
	}
	if recA.Code != http.StatusOK {
		t.Fatalf("first UploadPart status=%d body=%s", recA.Code, recA.Body.String())
	}

	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatalf("second UploadPart did not complete after release")
	}
	if recB.Code != http.StatusOK {
		t.Fatalf("second UploadPart status=%d body=%s", recB.Code, recB.Body.String())
	}

	parts := lookupActiveParts(t, svc, uploadID)
	if len(parts) != 2 {
		t.Fatalf("active parts=%d, want 2", len(parts))
	}
	if _, ok := parts[1]; !ok {
		t.Fatalf("active state missing part 1")
	}
	if _, ok := parts[2]; !ok {
		t.Fatalf("active state missing part 2")
	}
}

func TestNoSpaceErrorDetection(t *testing.T) {
	if !isNoSpaceError(syscall.ENOSPC) {
		t.Fatalf("syscall.ENOSPC was not detected")
	}
}

func unsignedPayloadRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "example.com"
	signRequest(req, testAccessKey, testSecretKey, testRegion, sigUnsignedBody, time.Now())
	return req
}

type blockingPartBody struct {
	data    []byte
	sent    bool
	blocked bool
	waiting chan struct{}
	unblock chan struct{}
}

func newBlockingPartBody(data []byte) *blockingPartBody {
	return &blockingPartBody{
		data:    data,
		waiting: make(chan struct{}),
		unblock: make(chan struct{}),
	}
}

func (b *blockingPartBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.data), nil
	}
	if !b.blocked {
		b.blocked = true
		close(b.waiting)
		<-b.unblock
	}
	return 0, io.EOF
}

func (b *blockingPartBody) release() {
	close(b.unblock)
}

// TestUploadPartContentMD5: client-supplied Content-MD5 must match
// the body; mismatch returns BadDigest and preserves any previous
// valid part for the same partNumber.
func TestUploadPartContentMD5(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	mountAbs := lookupMountAbs(t, handler, mount)
	partPath := partPathFor(stageDirFor(mountAbs, uploadID), 1)

	valid := []byte("valid existing part")
	validETag := uploadPart(t, handler, mount, "k.bin", uploadID, 1, valid)

	body := []byte("real body")
	wrongMD5 := md5.Sum([]byte("DIFFERENT"))
	wrongB64 := base64.StdEncoding.EncodeToString(wrongMD5[:])

	url := fmt.Sprintf("/%s/k.bin?partNumber=1&uploadId=%s", mount, uploadID)
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Host = "example.com"
	req.Header.Set("Content-MD5", wrongB64)
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong Content-MD5 UploadPart status=%d, want 400 (BadDigest)", rec.Code)
	}

	parts := lookupActiveParts(t, svc, uploadID)
	if len(parts) != 1 {
		t.Fatalf("after bad-digest duplicate active state has %d parts, want 1", len(parts))
	}
	if got := parts[1].ETag; got != validETag {
		t.Errorf("after bad-digest duplicate part ETag=%q, want original %q", got, validETag)
	}
	onDisk, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatalf("read part after bad digest: %v", err)
	}
	if !bytes.Equal(onDisk, valid) {
		t.Errorf("bad-digest duplicate changed existing part bytes")
	}
}

// TestUploadPartRejectsBadPartNumber: out-of-range partNumber → 400.
func TestUploadPartRejectsBadPartNumber(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	for _, n := range []string{"0", "10001", "abc", "-1"} {
		url := fmt.Sprintf("/%s/k.bin?partNumber=%s&uploadId=%s", mount, n, uploadID)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("x")))
		req.Host = "example.com"
		signRequestPayload(req, []byte("x"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("partNumber=%s status=%d, want 400", n, rec.Code)
		}
	}
}

// TestListParts: ListParts returns parts in ascending PartNumber
// order with stored ETags.
func TestListParts(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	uploadPart(t, handler, mount, "k.bin", uploadID, 3, []byte("p3"))
	uploadPart(t, handler, mount, "k.bin", uploadID, 1, []byte("p1"))
	uploadPart(t, handler, mount, "k.bin", uploadID, 2, []byte("p2"))

	url := fmt.Sprintf("/%s/k.bin?uploadId=%s", mount, uploadID)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListParts status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res listPartsResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Parts) != 3 {
		t.Fatalf("Parts count=%d, want 3", len(res.Parts))
	}
	for i, want := range []int{1, 2, 3} {
		if res.Parts[i].PartNumber != want {
			t.Errorf("Parts[%d].PartNumber=%d, want %d", i, res.Parts[i].PartNumber, want)
		}
	}
}

// TestListMultipartUploads: lists all in-progress uploads in the
// bucket, sorted oldest-first.
func TestListMultipartUploads(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	u1 := initMultipart(t, handler, mount, "first.bin", nil)
	u2 := initMultipart(t, handler, mount, "second.bin", nil)

	req := httptest.NewRequest(http.MethodGet, "/"+mount+"?uploads", nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListMultipartUploads status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res listMultipartUploadsResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Uploads) != 2 {
		t.Fatalf("Uploads=%d, want 2", len(res.Uploads))
	}
	// oldest first → u1 before u2 (unless they shared the same ms
	// timestamp; t.UnixMilli has ms resolution so it's possible.
	// just verify both ids are present)
	got := map[string]bool{}
	for _, u := range res.Uploads {
		got[u.UploadID] = true
	}
	if !got[u1] || !got[u2] {
		t.Errorf("missing upload id in listing: have %v", got)
	}
}

// TestAbortMultipartUpload: removes the staging dir; idempotent on
// a missing upload.
func TestAbortMultipartUpload(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "doomed.bin", nil)
	uploadPart(t, handler, mount, "doomed.bin", uploadID, 1, []byte("x"))

	mountAbs := lookupMountAbs(t, handler, mount)
	stageDir := stageDirFor(mountAbs, uploadID)
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("stage dir should exist before abort: %v", err)
	}

	url := fmt.Sprintf("/%s/doomed.bin?uploadId=%s", mount, uploadID)
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Abort status=%d, want 204", rec.Code)
	}
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Errorf("stage dir should be gone after abort, got err=%v", err)
	}

	// Idempotent: second abort same uploadId → still 204.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, url, nil)
	req.Host = "example.com"
	signRequestPayload(req, nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("idempotent Abort status=%d, want 204", rec.Code)
	}
}

// TestUploadPartUnknownUploadId: UploadPart with an uploadId that
// doesn't exist returns NoSuchUpload (NoSuchKey 404 in our wire
// for now; AWS uses NoSuchUpload but the HTTP status is the same).
func TestUploadPartUnknownUploadId(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	url := fmt.Sprintf("/%s/x.bin?partNumber=1&uploadId=00000000000000000000000000000000", mount)
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("x")))
	req.Host = "example.com"
	signRequestPayload(req, []byte("x"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uploadId status=%d, want 404", rec.Code)
	}
}

// completeMultipart helper: POST ?uploadId=X with the given parts
// XML and returns the response recorder.
func completeMultipart(t *testing.T, handler http.Handler, mount, key, uploadID string, parts []completeRequestPart) *httptest.ResponseRecorder {
	t.Helper()
	body := completeMultipartUploadRequest{Parts: parts}
	raw, err := xml.Marshal(body)
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	url := fmt.Sprintf("/%s/%s?uploadId=%s", mount, key, uploadID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Host = "example.com"
	signRequestPayload(req, raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// makePartBody returns a deterministic byte slice of the given size.
// Pattern is repeating "P<n>." so different parts produce different
// MD5s even at identical sizes.
func makePartBody(partNum, size int) []byte {
	out := make([]byte, size)
	tag := byte('A' + (partNum-1)%26)
	for i := range out {
		out[i] = tag
	}
	return out
}

// expectedCompositeETag computes the composite ETag the same way
// Complete does: hex(MD5(concat-of-part-md5-bytes)) + "-N".
func expectedCompositeETag(parts [][]byte) string {
	composite := md5.New()
	for _, p := range parts {
		s := md5.Sum(p)
		composite.Write(s[:])
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(composite.Sum(nil)), len(parts))
}

// TestCompleteMultipartUploadRoundTrip pins the happy path: two
// 5 MiB parts + one small final part → Complete succeeds, returns
// composite ETag, GET returns assembled body with same ETag.
func TestCompleteMultipartUploadRoundTrip(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "big.bin", map[string]string{
		"Content-Type":      "application/octet-stream",
		"x-amz-meta-author": "alice",
	})

	const minSize = 5 * 1024 * 1024
	p1 := makePartBody(1, minSize)
	p2 := makePartBody(2, minSize)
	p3 := makePartBody(3, 1024) // last part can be small

	e1 := uploadPart(t, handler, mount, "big.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "big.bin", uploadID, 2, p2)
	e3 := uploadPart(t, handler, mount, "big.bin", uploadID, 3, p3)

	rec := completeMultipart(t, handler, mount, "big.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
		{PartNumber: 3, ETag: e3},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res completeMultipartUploadResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode complete result: %v", err)
	}
	wantETag := expectedCompositeETag([][]byte{p1, p2, p3})
	gotETag := strings.Trim(res.ETag, `"`)
	if gotETag != wantETag {
		t.Errorf("composite ETag=%q, want %q", gotETag, wantETag)
	}
	if res.Bucket != mount || res.Key != "big.bin" {
		t.Errorf("Bucket/Key=%q/%q", res.Bucket, res.Key)
	}

	// GET should return the assembled body and the same composite ETag.
	getReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/big.bin", nil)
	getReq.Host = "example.com"
	signRequestPayload(getReq, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET after Complete status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	gotBody := getRec.Body.Bytes()
	wantBody := append(append(append([]byte{}, p1...), p2...), p3...)
	if !bytes.Equal(gotBody, wantBody) {
		t.Errorf("GET body length=%d, want %d", len(gotBody), len(wantBody))
	}
	if got := strings.Trim(getRec.Header().Get("ETag"), `"`); got != wantETag {
		t.Errorf("GET ETag=%q, want %q", got, wantETag)
	}
	if got := getRec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("GET Content-Type=%q", got)
	}
	if got := getMetaHeader(getRec.Header(), "x-amz-meta-author"); got != "alice" {
		t.Errorf("GET x-amz-meta-author=%q, want alice", got)
	}
}

// TestCompleteMultipartUploadIdempotent: a retried Complete with the
// same uploadId returns the same result (composite ETag) without
// re-doing the install.
func TestCompleteMultipartUploadIdempotent(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 256)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)

	parts := []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	}

	first := completeMultipart(t, handler, mount, "obj.bin", uploadID, parts)
	if first.Code != http.StatusOK {
		t.Fatalf("first Complete status=%d body=%s", first.Code, first.Body.String())
	}
	var firstRes completeMultipartUploadResult
	_ = xml.Unmarshal(first.Body.Bytes(), &firstRes)

	second := completeMultipart(t, handler, mount, "obj.bin", uploadID, parts)
	if second.Code != http.StatusOK {
		t.Fatalf("second Complete status=%d body=%s", second.Code, second.Body.String())
	}
	var secondRes completeMultipartUploadResult
	_ = xml.Unmarshal(second.Body.Bytes(), &secondRes)
	if firstRes.ETag != secondRes.ETag {
		t.Errorf("retry ETag=%q, first=%q (must be identical)", secondRes.ETag, firstRes.ETag)
	}
}

// TestCompleteMultipartUploadInvalidPart: Complete fails when a
// referenced PartNumber wasn't uploaded.
func TestCompleteMultipartUploadInvalidPart(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	uploadPart(t, handler, mount, "k.bin", uploadID, 1, makePartBody(1, 5*1024*1024))

	rec := completeMultipart(t, handler, mount, "k.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: "deadbeefdeadbeefdeadbeefdeadbeef"}, // wrong ETag
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong-etag Complete status=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "InvalidPart") {
		t.Errorf("body should mention InvalidPart, got %s", rec.Body.String())
	}
}

// TestCompleteMultipartUploadOutOfOrder: parts in descending order
// → InvalidPartOrder.
func TestCompleteMultipartUploadOutOfOrder(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 5*1024*1024)
	e1 := uploadPart(t, handler, mount, "k.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "k.bin", uploadID, 2, p2)

	rec := completeMultipart(t, handler, mount, "k.bin", uploadID, []completeRequestPart{
		{PartNumber: 2, ETag: e2},
		{PartNumber: 1, ETag: e1},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("descending parts Complete status=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "InvalidPartOrder") {
		t.Errorf("body should mention InvalidPartOrder, got %s", rec.Body.String())
	}
}

// TestCompleteMultipartUploadEntityTooSmall: a non-final part under
// 5 MiB → EntityTooSmall.
func TestCompleteMultipartUploadEntityTooSmall(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "k.bin", nil)
	tinyP1 := makePartBody(1, 1024) // << 5 MiB
	tailP2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "k.bin", uploadID, 1, tinyP1)
	e2 := uploadPart(t, handler, mount, "k.bin", uploadID, 2, tailP2)

	rec := completeMultipart(t, handler, mount, "k.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("small-part Complete status=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "EntityTooSmall") {
		t.Errorf("body should mention EntityTooSmall, got %s", rec.Body.String())
	}
}

// TestCompleteMultipartUploadSinglePart: a single small part (the
// final and only part) is allowed — the 5 MiB rule applies only to
// non-final parts.
func TestCompleteMultipartUploadSinglePart(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "tiny.bin", nil)
	p1 := makePartBody(1, 1024)
	e1 := uploadPart(t, handler, mount, "tiny.bin", uploadID, 1, p1)

	rec := completeMultipart(t, handler, mount, "tiny.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("single-part Complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	wantETag := expectedCompositeETag([][]byte{p1})
	var res completeMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &res)
	if got := strings.Trim(res.ETag, `"`); got != wantETag {
		t.Errorf("ETag=%q, want %q", got, wantETag)
	}
}

// TestCompleteMultipartUploadUnknownUploadID: a Complete with an
// uploadId that doesn't exist returns NoSuchUpload.
func TestCompleteMultipartUploadUnknownUploadID(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	rec := completeMultipart(t, handler, mount, "x.bin", "00000000000000000000000000000000",
		[]completeRequestPart{{PartNumber: 1, ETag: "deadbeefdeadbeefdeadbeefdeadbeef"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uploadId Complete status=%d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NoSuchUpload") {
		t.Errorf("body should mention NoSuchUpload, got %s", rec.Body.String())
	}
}

// TestCompleteMultipartUploadCleansStagingOnSuccess: after a
// successful Complete, parts/ and complete.tmp are gone while active
// state moves to done and durable-record replay handles idempotent retry.
// Pre-fix the parts directory leaked permanently, doubling the
// effective storage of every multipart upload.
func TestCompleteMultipartUploadCleansStagingOnSuccess(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)

	rec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status=%d body=%s", rec.Code, rec.Body.String())
	}

	mountAbs := lookupMountAbs(t, handler, mount)
	stageDir := stageDirFor(mountAbs, uploadID)

	// parts/ must be gone.
	partsDir := stageDir + "/" + multipartPartsDirName
	if _, err := os.Stat(partsDir); !os.IsNotExist(err) {
		t.Errorf("parts dir should be removed after Complete, got err=%v", err)
	}
	// complete.tmp must be gone.
	if _, err := os.Stat(stageDir + "/" + multipartCompleteTmp); !os.IsNotExist(err) {
		t.Errorf("complete.tmp should be removed after Complete, got err=%v", err)
	}
	got := lookupActiveUpload(t, svc, uploadID)
	if got.Phase != domain.MultipartUploadDone {
		t.Errorf("active upload Phase=%q, want done", got.Phase)
	}
	if got.CompositeETag == "" {
		t.Errorf("active upload CompositeETag should be set")
	}

	// Idempotent retry still works from the durable record, not parts or manifest state.
	rec2 := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry after staging cleanup status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestUploadPartRejectedDuringCommit: once Complete starts (active state
// flips to phase=committing), concurrent UploadPart for the same
// uploadId must be rejected. Pre-fix UploadPart could overwrite a
// part-file between Complete's validate and concat steps, breaking
// the composite-ETag invariant.
func TestUploadPartRejectedDuringCommit(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	uploadPart(t, handler, mount, "obj.bin", uploadID, 1, makePartBody(1, 5*1024*1024))

	upload := lookupActiveUpload(t, svc, uploadID)
	upload.Phase = domain.MultipartUploadCommitting
	if err := svc.UpdateActiveMultipartUpload(*upload); err != nil {
		t.Fatalf("force committing active upload: %v", err)
	}

	body := []byte("racey")
	url := fmt.Sprintf("/%s/obj.bin?partNumber=2&uploadId=%s", mount, uploadID)
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("UploadPart accepted while phase=committing — race not closed")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("UploadPart during committing status=%d, want 400 InvalidRequest", rec.Code)
	}
}

// TestMultipartETagSurvivesDetectorResync pins the regression
// surfaced by the M4 Docker smoke test: a poll-detector cycle that
// re-syncs a freshly-multipart-uploaded file used to clobber the
// composite ETag (and every other S3-extension field) by running
// the entity through buildEntityMetadata, which has no concept
// of S3 fields. Pre-fix HEAD returned an empty ETag a few seconds
// after Complete; post-fix the composite survives detector
// re-syncs as long as the on-disk size + mtime + inode haven't
// changed.
func TestMultipartETagSurvivesDetectorResync(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "obj.bin", map[string]string{
		"Content-Type":      "image/png",
		"x-amz-meta-author": "alice",
	})
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)
	rec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var compRes completeMultipartUploadResult
	_ = xml.Unmarshal(rec.Body.Bytes(), &compRes)
	wantETag := strings.Trim(compRes.ETag, `"`)
	if !strings.Contains(wantETag, "-") {
		t.Fatalf("Complete returned non-composite ETag %q — test setup wrong", wantETag)
	}

	// Simulate a detector poll touching the file.
	mountAbs := lookupMountAbs(t, handler, mount)
	if err := svc.SyncAbsPath(mountAbs + "/obj.bin"); err != nil {
		t.Fatalf("SyncAbsPath: %v", err)
	}

	// HEAD should still report the composite ETag + S3 metadata.
	hReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/obj.bin", nil)
	hReq.Host = "example.com"
	signRequestPayload(hReq, nil)
	hRec := httptest.NewRecorder()
	handler.ServeHTTP(hRec, hReq)
	if hRec.Code != http.StatusOK {
		t.Fatalf("HEAD after sync status=%d", hRec.Code)
	}
	gotETag := strings.Trim(hRec.Header().Get("ETag"), `"`)
	if gotETag != wantETag {
		t.Errorf("HEAD ETag=%q after detector sync, want preserved %q", gotETag, wantETag)
	}
	if got := hRec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("HEAD Content-Type=%q after detector sync, want preserved image/png", got)
	}
	if got := getMetaHeader(hRec.Header(), "x-amz-meta-author"); got != "alice" {
		t.Errorf("HEAD x-amz-meta-author=%q after detector sync, want preserved 'alice'", got)
	}
}

// TestConditionalWriteAgainstMultipartETag pins the contract that
// If-Match / If-None-Match on PUT and DELETE compare against the
// CURRENT effective ETag of the target — which is the COMPOSITE
// ETag for a multipart-uploaded object. A client that does HEAD,
// gets the composite ETag back, and sends If-Match with that
// value must be allowed to overwrite or delete; a client sending
// the underlying whole-body MD5 must be rejected.
//
// The bug class this catches: conditional logic that compares
// against the wrong ETag form (whole-body MD5 vs composite). A
// regression here would make multipart-uploaded objects
// effectively un-overwritable via conditional clients.
func TestConditionalWriteAgainstMultipartETag(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Seed a multipart-uploaded object.
	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)
	rec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed Complete status=%d", rec.Code)
	}

	// HEAD to discover the composite ETag we expose to clients.
	hReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/obj.bin", nil)
	hReq.Host = "example.com"
	signRequestPayload(hReq, nil)
	hRec := httptest.NewRecorder()
	handler.ServeHTTP(hRec, hReq)
	composite := strings.Trim(hRec.Header().Get("ETag"), `"`)
	if !strings.Contains(composite, "-") {
		t.Fatalf("composite ETag has no -N suffix: %q", composite)
	}

	// Compute the underlying whole-body MD5 (what an internal
	// observer might think the ETag is).
	wholeBody := append(append([]byte{}, p1...), p2...)
	wholeMD5sum := md5.Sum(wholeBody)
	wholeMD5 := hex.EncodeToString(wholeMD5sum[:])

	// PUT with If-Match: <composite> should SUCCEED.
	body := []byte("conditional overwrite via composite")
	pReq := httptest.NewRequest(http.MethodPut, "/"+mount+"/obj.bin", bytes.NewReader(body))
	pReq.Host = "example.com"
	pReq.Header.Set("If-Match", `"`+composite+`"`)
	signRequestPayload(pReq, body)
	pRec := httptest.NewRecorder()
	handler.ServeHTTP(pRec, pReq)
	if pRec.Code != http.StatusOK {
		t.Errorf("PUT If-Match: <composite> status=%d, want 200; body=%s", pRec.Code, pRec.Body.String())
	}

	// Re-seed the multipart object (the previous PUT replaced it).
	uploadID2 := initMultipart(t, handler, mount, "obj.bin", nil)
	e1 = uploadPart(t, handler, mount, "obj.bin", uploadID2, 1, p1)
	e2 = uploadPart(t, handler, mount, "obj.bin", uploadID2, 2, p2)
	rec = completeMultipart(t, handler, mount, "obj.bin", uploadID2, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-seed Complete status=%d", rec.Code)
	}

	// PUT with If-Match: <wholeMD5> should FAIL with 412 — the
	// effective ETag is the composite, not the whole-body MD5.
	pReq2 := httptest.NewRequest(http.MethodPut, "/"+mount+"/obj.bin", bytes.NewReader(body))
	pReq2.Host = "example.com"
	pReq2.Header.Set("If-Match", `"`+wholeMD5+`"`)
	signRequestPayload(pReq2, body)
	pRec2 := httptest.NewRecorder()
	handler.ServeHTTP(pRec2, pReq2)
	if pRec2.Code != http.StatusPreconditionFailed {
		t.Errorf("PUT If-Match: <whole-MD5> status=%d, want 412 (effective ETag is composite)", pRec2.Code)
	}
}

// TestCompleteOverExistingFullAssertion extends the existing
// fileID-preservation test with byte/ETag/listing assertions.
// Pre-fix the test only confirmed the fileID was preserved; an
// overwrite that kept the ID but failed to install new bytes or
// metadata would have passed. This pins the full overwrite
// contract.
func TestCompleteOverExistingFullAssertion(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Seed via single-PUT with old bytes + old metadata.
	oldBody := []byte("OLD CONTENT — should be replaced")
	oldReq := httptest.NewRequest(http.MethodPut, "/"+mount+"/over.bin", bytes.NewReader(oldBody))
	oldReq.Host = "example.com"
	oldReq.Header.Set("Content-Type", "text/x-old")
	oldReq.Header.Set("x-amz-meta-source", "single-put")
	signRequestPayload(oldReq, oldBody)
	oldRec := httptest.NewRecorder()
	handler.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusOK {
		t.Fatalf("seed PUT status=%d", oldRec.Code)
	}

	// Multipart-overwrite with new bytes + new metadata.
	uploadID := initMultipart(t, handler, mount, "over.bin", map[string]string{
		"Content-Type":      "text/x-new",
		"x-amz-meta-source": "multipart",
	})
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "over.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "over.bin", uploadID, 2, p2)
	cRec := completeMultipart(t, handler, mount, "over.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if cRec.Code != http.StatusOK {
		t.Fatalf("Complete overwrite status=%d", cRec.Code)
	}
	var cRes completeMultipartUploadResult
	_ = xml.Unmarshal(cRec.Body.Bytes(), &cRes)
	composite := strings.Trim(cRes.ETag, `"`)

	// HEAD: ETag must be composite, Content-Type + meta must be
	// the new values, size must be parts size.
	hReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/over.bin", nil)
	hReq.Host = "example.com"
	signRequestPayload(hReq, nil)
	hRec := httptest.NewRecorder()
	handler.ServeHTTP(hRec, hReq)
	if got := strings.Trim(hRec.Header().Get("ETag"), `"`); got != composite {
		t.Errorf("HEAD ETag=%q, want composite %q", got, composite)
	}
	if got := hRec.Header().Get("Content-Type"); got != "text/x-new" {
		t.Errorf("HEAD Content-Type=%q, want text/x-new (was text/x-old)", got)
	}
	if got := getMetaHeader(hRec.Header(), "x-amz-meta-source"); got != "multipart" {
		t.Errorf("HEAD x-amz-meta-source=%q, want 'multipart' (was 'single-put')", got)
	}
	wantSize := int64(len(p1) + len(p2))
	if got, _ := strconv.ParseInt(hRec.Header().Get("Content-Length"), 10, 64); got != wantSize {
		t.Errorf("HEAD Content-Length=%d, want %d", got, wantSize)
	}

	// GET: body must be the multipart bytes, byte-for-byte.
	gReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/over.bin", nil)
	gReq.Host = "example.com"
	signRequestPayload(gReq, nil)
	gRec := httptest.NewRecorder()
	handler.ServeHTTP(gRec, gReq)
	wantBody := append(append([]byte{}, p1...), p2...)
	if !bytes.Equal(gRec.Body.Bytes(), wantBody) {
		t.Errorf("GET body length=%d, want %d (full multipart payload)", gRec.Body.Len(), len(wantBody))
	}

	// ListObjectsV2: entry must show the new size + composite ETag.
	lReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/?list-type=2&prefix=over.bin", nil)
	lReq.Host = "example.com"
	signRequestPayload(lReq, nil)
	lRec := httptest.NewRecorder()
	handler.ServeHTTP(lRec, lReq)
	var lRes listBucketResultV2
	if err := xml.Unmarshal(lRec.Body.Bytes(), &lRes); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(lRes.Contents) != 1 {
		t.Fatalf("ListObjectsV2 returned %d entries, want 1", len(lRes.Contents))
	}
	if lRes.Contents[0].Size != wantSize {
		t.Errorf("ListObjectsV2 Size=%d, want %d", lRes.Contents[0].Size, wantSize)
	}
	if got := strings.Trim(lRes.Contents[0].ETag, `"`); got != composite {
		t.Errorf("ListObjectsV2 ETag=%q, want composite %q", got, composite)
	}
}

// TestCreateMultipartUploadEnforcesUserMetadataBudget pins parity
// with the single-PUT path: x-amz-meta-* headers exceeding the
// 2 KiB JSON-encoded budget are rejected at CreateMultipartUpload
// time. Without this, a client could smuggle arbitrarily large
// metadata blobs through the multipart path — inconsistent with
// PutObject and a resource-cost foothold on active multipart state.
func TestCreateMultipartUploadEnforcesUserMetadataBudget(t *testing.T) {
	_, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// At-budget: pad value sized so the JSON-encoded blob is
	// just under the 2 KiB cap.
	okPad := strings.Repeat("a", 1900)
	req := httptest.NewRequest(http.MethodPost, "/"+mount+"/ok-mp.bin?uploads", nil)
	req.Host = "example.com"
	req.Header.Set("x-amz-meta-pad", okPad)
	signRequestPayload(req, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("at-budget metadata multipart create status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Over-budget: pad large enough to push JSON over 2 KiB.
	bigPad := strings.Repeat("a", 4000)
	req2 := httptest.NewRequest(http.MethodPost, "/"+mount+"/over-mp.bin?uploads", nil)
	req2.Host = "example.com"
	req2.Header.Set("x-amz-meta-pad", bigPad)
	signRequestPayload(req2, nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("over-budget metadata multipart create status=%d, want 400 InvalidArgument", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "InvalidArgument") {
		t.Errorf("body should mention InvalidArgument, got %s", rec2.Body.String())
	}
}

// TestSyncSingleClearsETagOnInPlaceRewrite covers the rsync
// --inplace -t edge case codex called out: an external tool that
// rewrites file content in place AND restores the original mtime
// would otherwise look unchanged to the cheap stat-based
// preservation check. The hash-verification fallback catches it.
//
// Without rehashing, this test would falsely preserve the stale
// ETagMD5/MultipartETag, making HEAD report wrong identity for the
// modified file.
func TestSyncSingleClearsETagOnInPlaceRewrite(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Seed via S3 PutObject — establishes a known ETagMD5.
	body := []byte("original content                                                ")
	putForTest(t, handler, mount, "obj.txt", body)

	mountAbs := lookupMountAbs(t, handler, mount)
	dst := mountAbs + "/obj.txt"

	// Capture pre-state stat.
	preStat, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	preMtime := preStat.ModTime()

	// Simulate rsync --inplace -t: rewrite content (different
	// bytes, SAME size), then reset mtime to the original.
	newBody := []byte("changed content                                                 ")
	if len(newBody) != len(body) {
		t.Fatalf("test setup: rewrite bytes must match original size (%d vs %d)", len(newBody), len(body))
	}
	if err := os.WriteFile(dst, newBody, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(dst, preMtime, preMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Detector poll: SyncAbsPath. Pre-fix this preserved the old
	// ETag because (size, mtime, inode) all matched. Post-fix the
	// content hash is verified and the mismatch clears S3 fields.
	if err := svc.SyncAbsPath(dst); err != nil {
		t.Fatalf("SyncAbsPath: %v", err)
	}

	// HEAD should show a different ETag (or empty if it's been
	// cleared without recompute) — anything but the OLD ETag.
	hReq := httptest.NewRequest(http.MethodHead, "/"+mount+"/obj.txt", nil)
	hReq.Host = "example.com"
	signRequestPayload(hReq, nil)
	hRec := httptest.NewRecorder()
	handler.ServeHTTP(hRec, hReq)
	gotETag := strings.Trim(hRec.Header().Get("ETag"), `"`)

	preMD5 := md5.Sum(body)
	preETag := hex.EncodeToString(preMD5[:])
	if gotETag == preETag {
		t.Errorf("HEAD ETag=%q still matches pre-rewrite hash — in-place rewrite was not detected", gotETag)
	}
}

// TestRecoveryViaNewHandlerProductionPath pins that
// NewHandler runs the recovery sweep on construction — the
// production restart code path. The pre-existing recovery tests
// call recoverPendingMultipartUploads directly, which would still
// PASS even if NewHandler forgot to invoke it. This test catches
// that wiring regression.
func TestRecoveryViaNewHandlerProductionPath(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Run a full multipart Complete to land a durable record.
	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)
	rec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed Complete status=%d", rec.Code)
	}

	// Force the active state back to phase=committing — simulating
	// the crash window where the durable record was committed but
	// the done-state write didn't land. Also clear the parts/ dir
	// to simulate the cleanup having happened.
	upload := lookupActiveUpload(t, svc, uploadID)
	upload.Phase = domain.MultipartUploadCommitting
	upload.WholeBodyMD5 = ""
	upload.CompletedFileID = ""
	upload.CompletedAt = 0
	if err := svc.UpdateActiveMultipartUpload(*upload); err != nil {
		t.Fatalf("force committing active upload: %v", err)
	}

	// Construct a SECOND handler via NewHandler — this is the
	// production restart code path. It must run the recovery
	// sweep and reconcile the committing active upload to done.
	handler2, err := NewHandler(svc, Options{
		Region:    testRegion,
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// ListMultipartUploads must NOT show this upload as in-progress.
	lReq := httptest.NewRequest(http.MethodGet, "/"+mount+"/?uploads", nil)
	lReq.Host = "example.com"
	signRequestPayload(lReq, nil)
	lRec := httptest.NewRecorder()
	handler2.ServeHTTP(lRec, lReq)
	if lRec.Code != http.StatusOK {
		t.Fatalf("ListMultipartUploads status=%d", lRec.Code)
	}
	var lRes listMultipartUploadsResult
	_ = xml.Unmarshal(lRec.Body.Bytes(), &lRes)
	for _, u := range lRes.Uploads {
		if u.UploadID == uploadID {
			t.Errorf("post-restart ListMultipartUploads still surfaces uploadId %s — recovery sweep didn't run via NewHandler", uploadID)
		}
	}

	recovered := lookupActiveUpload(t, svc, uploadID)
	if recovered.Phase != domain.MultipartUploadDone {
		t.Errorf("active upload Phase=%q after NewHandler recovery, want done", recovered.Phase)
	}
}

// TestRecoverCommittingActiveUploadNoRecord: an active upload in phase=committing
// with no durable record is left untouched (the client will retry
// Complete to redrive the install).
func TestRecoverCommittingActiveUploadNoRecord(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	uploadID := initMultipart(t, handler, mount, "stuck.bin", nil)
	uploadPart(t, handler, mount, "stuck.bin", uploadID, 1, makePartBody(1, 5*1024*1024))

	upload := lookupActiveUpload(t, svc, uploadID)
	upload.Phase = domain.MultipartUploadCommitting
	upload.CompositeETag = "fake-etag-1"
	if err := svc.UpdateActiveMultipartUpload(*upload); err != nil {
		t.Fatalf("force committing active upload: %v", err)
	}

	recoverPendingMultipartUploads(svc)

	got := lookupActiveUpload(t, svc, uploadID)
	if got.Phase != domain.MultipartUploadCommitting {
		t.Errorf("Phase=%q after recovery, want committing (no record means client retry redrives)", got.Phase)
	}
}

// TestRecoverCommittingActiveUploadWithRecord: an active upload in phase=committing
// whose durable record DOES exist is promoted to phase=done so listing
// stops showing it as in-progress.
func TestRecoverCommittingActiveUploadWithRecord(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Run a complete Complete to land a durable record.
	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)
	rec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Simulate a crash that only updated phase to committing (not done).
	upload := lookupActiveUpload(t, svc, uploadID)
	upload.Phase = domain.MultipartUploadCommitting
	upload.WholeBodyMD5 = ""
	upload.CompletedFileID = ""
	upload.CompletedAt = 0
	if err := svc.UpdateActiveMultipartUpload(*upload); err != nil {
		t.Fatalf("force committing active upload: %v", err)
	}

	recoverPendingMultipartUploads(svc)

	got := lookupActiveUpload(t, svc, uploadID)
	if got.Phase != domain.MultipartUploadDone {
		t.Errorf("Phase=%q after recovery, want done", got.Phase)
	}
	if got.CompletedFileID == "" {
		t.Errorf("CompletedFileID should be backfilled from durable record")
	}
}

// TestCompleteMultipartUploadOverwritesExisting: Complete onto a
// pre-existing object overwrites it, preserving the original fileID.
func TestCompleteMultipartUploadOverwritesExisting(t *testing.T) {
	svc, handler, mount, cleanup := newTestServer(t)
	defer cleanup()

	// Pre-write an object via PUT so it has a stable fileID.
	body := []byte("v1")
	url := "/" + mount + "/obj.bin"
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Host = "example.com"
	signRequestPayload(req, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status=%d", rec.Code)
	}
	originalID, err := svc.ResolvePath("/" + mount + "/obj.bin")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}

	// Now do a multipart Complete with new bytes.
	uploadID := initMultipart(t, handler, mount, "obj.bin", nil)
	p1 := makePartBody(1, 5*1024*1024)
	p2 := makePartBody(2, 1024)
	e1 := uploadPart(t, handler, mount, "obj.bin", uploadID, 1, p1)
	e2 := uploadPart(t, handler, mount, "obj.bin", uploadID, 2, p2)

	cRec := completeMultipart(t, handler, mount, "obj.bin", uploadID, []completeRequestPart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	})
	if cRec.Code != http.StatusOK {
		t.Fatalf("overwrite Complete status=%d body=%s", cRec.Code, cRec.Body.String())
	}

	// FileID should be preserved across the overwrite.
	newID, err := svc.ResolvePath("/" + mount + "/obj.bin")
	if err != nil {
		t.Fatalf("ResolvePath after Complete: %v", err)
	}
	if newID != originalID {
		t.Errorf("fileID after overwrite=%v, want preserved %v", newID, originalID)
	}
}

// helpers

// lookupMountAbs reads the mount's abs path from the test service.
// The handler doesn't expose it; in tests we pass svc to the helper
// as a workaround. The newTestServer setup stashes the svc into
// testSvcGlobal for tests that don't want to thread it through
// every helper call.
func lookupMountAbs(t *testing.T, _ http.Handler, mount string) string {
	t.Helper()
	if testSvcGlobal == nil {
		t.Fatalf("testSvcGlobal not set; helpers must run after newTestServer")
	}
	for _, root := range testSvcGlobal.ListRoot() {
		if root.Name == mount {
			abs, err := testSvcGlobal.ResolveAbsPath(root.ID)
			if err != nil {
				t.Fatalf("ResolveAbsPath: %v", err)
			}
			return abs
		}
	}
	t.Fatalf("mount %q not found", mount)
	return ""
}

func lookupActiveUpload(t *testing.T, svc *domain.Service, uploadID string) *domain.ActiveMultipartUpload {
	t.Helper()
	upload, err := svc.LookupActiveMultipartUpload(uploadID)
	if err != nil {
		t.Fatalf("LookupActiveMultipartUpload(%s): %v", uploadID, err)
	}
	return upload
}

func lookupActiveParts(t *testing.T, svc *domain.Service, uploadID string) map[int]domain.ActiveMultipartPart {
	t.Helper()
	parts, err := svc.ListActiveMultipartParts(uploadID)
	if err != nil {
		t.Fatalf("ListActiveMultipartParts(%s): %v", uploadID, err)
	}
	out := make(map[int]domain.ActiveMultipartPart, len(parts))
	for _, part := range parts {
		out[part.PartNumber] = part
	}
	return out
}

// testSvcGlobal: set by newTestServer so multipart tests can look
// up the mount-abs path without threading svc through every helper.
// Tests run sequentially in this package so the global is safe.
var testSvcGlobal *domain.Service
