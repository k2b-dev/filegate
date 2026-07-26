//go:build linux

package httpadapter

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
)

func sha256Prefixed(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func createUploadSession(t *testing.T, r http.Handler, path string, content []byte, segmentSize int64, direct bool) apiv1.UploadSessionResponse {
	t.Helper()
	body := apiv1.UploadSessionCreateRequest{
		Path:        path,
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: segmentSize,
		OnConflict:  "error",
	}
	if direct {
		body.Direct = &apiv1.UploadSessionDirectRequest{
			ExpiresInSeconds: 60,
			Allow:            []string{"putSegment", "status", "commit", "abort"},
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal session body: %v", err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var out apiv1.UploadSessionResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return out
}

func putSessionSegment(t *testing.T, r http.Handler, sessionID string, index int, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := authedJSONRequest(http.MethodPut, "/v1/uploads/sessions/"+sessionID+"/segments/"+strconv.Itoa(index), body)
	req.Header.Set("X-Segment-Checksum", sha256Prefixed(body))
	r.ServeHTTP(w, req)
	return w
}

func TestUploadSessionPutCommitAndResolve(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("hello resumable upload")
	session := createUploadSession(t, r, root.Name+"/uploads/resumable.txt", content, 8, false)
	if session.ID == "" || session.TotalSegments != 3 {
		t.Fatalf("unexpected session: %#v", session)
	}

	for _, seg := range session.Segments {
		part := content[seg.Offset : seg.Offset+seg.Size]
		w := putSessionSegment(t, r, session.ID, seg.Index, part)
		if w.Result().StatusCode != http.StatusOK {
			t.Fatalf("put segment %d status=%d body=%s", seg.Index, w.Result().StatusCode, w.Body.String())
		}
	}

	commit := httptest.NewRecorder()
	r.ServeHTTP(commit, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil))
	if commit.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", commit.Result().StatusCode, commit.Body.String())
	}
	var committed apiv1.UploadSessionCommitResponse
	if err := json.NewDecoder(commit.Result().Body).Decode(&committed); err != nil {
		t.Fatalf("decode commit response: %v", err)
	}
	wantMD5 := md5.Sum(content)
	if committed.Node.ETag != hex.EncodeToString(wantMD5[:]) {
		t.Fatalf("commit ETag=%q, want %x", committed.Node.ETag, wantMD5)
	}
	if committed.Node.SHA256 != sha256Prefixed(content) {
		t.Fatalf("commit SHA256=%q, want %q", committed.Node.SHA256, sha256Prefixed(content))
	}
	if _, err := svc.ResolvePath(root.Name + "/uploads/resumable.txt"); err != nil {
		t.Fatalf("resolve committed file: %v", err)
	}
	committedID, err := domain.ParseFileID(committed.Node.ID)
	if err != nil {
		t.Fatalf("parse committed id: %v", err)
	}
	persisted, err := svc.GetFile(committedID)
	if err != nil {
		t.Fatalf("get committed file: %v", err)
	}
	if persisted.ETag != committed.Node.ETag || persisted.SHA256 != committed.Node.SHA256 {
		t.Fatalf("persisted hashes ETag=%q SHA256=%q, want %q %q",
			persisted.ETag, persisted.SHA256, committed.Node.ETag, committed.Node.SHA256)
	}
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootAbs, "uploads", "resumable.txt"))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content=%q want %q", got, content)
	}

	retry := httptest.NewRecorder()
	r.ServeHTTP(retry, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil))
	if retry.Result().StatusCode != http.StatusOK {
		t.Fatalf("retry commit status=%d body=%s", retry.Result().StatusCode, retry.Body.String())
	}
}

func TestUploadSessionDuplicateSegmentIdempotency(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("abcdef")
	session := createUploadSession(t, r, root.Name+"/dup.bin", content, 3, false)

	first := putSessionSegment(t, r, session.ID, 0, []byte("abc"))
	if first.Result().StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Result().StatusCode, first.Body.String())
	}
	again := putSessionSegment(t, r, session.ID, 0, []byte("abc"))
	if again.Result().StatusCode != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", again.Result().StatusCode, again.Body.String())
	}
	bad := putSessionSegment(t, r, session.ID, 0, []byte("xyz"))
	if bad.Result().StatusCode != http.StatusConflict {
		t.Fatalf("conflicting duplicate status=%d body=%s", bad.Result().StatusCode, bad.Body.String())
	}
}

func TestUploadSessionDuplicateSegmentRestoresMissingFile(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("abcdef")
	session := createUploadSession(t, r, root.Name+"/restore.bin", content, 3, false)

	first := putSessionSegment(t, r, session.ID, 0, []byte("abc"))
	if first.Result().StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Result().StatusCode, first.Body.String())
	}
	segments, err := svc.ListUploadSegments(session.ID)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments=%d err=%v", len(segments), err)
	}
	if err := os.Remove(segments[0].Path); err != nil {
		t.Fatalf("remove segment file: %v", err)
	}
	again := putSessionSegment(t, r, session.ID, 0, []byte("abc"))
	if again.Result().StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", again.Result().StatusCode, again.Body.String())
	}
	if _, err := os.Stat(segments[0].Path); err != nil {
		t.Fatalf("segment was not restored: %v", err)
	}
}

func TestUploadSessionRejectsConflictRename(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("x")
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/rename.bin",
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: 1,
		OnConflict:  "rename",
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Result().StatusCode, http.StatusBadRequest, w.Body.String())
	}
}

func TestUploadSessionCreateAndAbortDoNotPublishParentDirs(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("deferred parents")
	session := createUploadSession(t, r, root.Name+"/a/b/c/file.txt", content, int64(len(content)), false)
	if _, err := svc.ResolvePath(root.Name + "/a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("parent directory visible before commit: %v", err)
	}

	abort := httptest.NewRecorder()
	r.ServeHTTP(abort, authedJSONRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil))
	if abort.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("abort status=%d body=%s", abort.Result().StatusCode, abort.Body.String())
	}
	if _, err := svc.ResolvePath(root.Name + "/a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("parent directory visible after abort: %v", err)
	}
}

func TestUploadSessionCommitCreatesDeferredParentDirs(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("commit creates parents")
	session := createUploadSession(t, r, root.Name+"/nested/upload/file.txt", content, 7, false)
	for _, seg := range session.Segments {
		part := content[seg.Offset : seg.Offset+seg.Size]
		w := putSessionSegment(t, r, session.ID, seg.Index, part)
		if w.Result().StatusCode != http.StatusOK {
			t.Fatalf("put segment %d status=%d body=%s", seg.Index, w.Result().StatusCode, w.Body.String())
		}
	}

	commit := httptest.NewRecorder()
	r.ServeHTTP(commit, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil))
	if commit.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", commit.Result().StatusCode, commit.Body.String())
	}
	if _, err := svc.ResolvePath(root.Name + "/nested/upload/file.txt"); err != nil {
		t.Fatalf("resolve committed file: %v", err)
	}
}

func TestUploadSessionAbortRemovesUnindexedSegmentFiles(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("orphan segment")
	session := createUploadSession(t, r, root.Name+"/orphan.bin", content, int64(len(content)), false)
	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	orphanPath := segmentPath(*stored, 0)
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o700); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}
	if err := os.WriteFile(orphanPath, content, 0o600); err != nil {
		t.Fatalf("write orphan segment: %v", err)
	}

	abort := httptest.NewRecorder()
	r.ServeHTTP(abort, authedJSONRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil))
	if abort.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("abort status=%d body=%s", abort.Result().StatusCode, abort.Body.String())
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan segment still exists: %v", err)
	}
}

func TestUploadSessionRejectsReservedNamespace(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("x")
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/.fg-uploads/segments/pwn.bin",
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: 1,
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want=%d body=%s", w.Result().StatusCode, http.StatusForbidden, w.Body.String())
	}
}

func TestUploadSessionRescanSkipsReservedNamespace(t *testing.T) {
	_, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootAbs, ".fg-uploads", "segments"), 0o700); err != nil {
		t.Fatalf("mkdir reserved namespace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootAbs, ".fg-uploads", "segments", "leak.part"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write leak: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := svc.ResolvePath(root.Name + "/.fg-uploads"); err == nil {
		t.Fatalf(".fg-uploads was indexed")
	}
}

func TestUploadSessionRejectsHugeSegmentPlan(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxSessionUploadBytes: 20_000,
		MaxUploadBytes:        20_000,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/too-many.bin",
		Size:        maxUploadSessionSegments + 1,
		Checksum:    "sha256:" + strings.Repeat("0", 64),
		SegmentSize: 1,
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Result().StatusCode, http.StatusBadRequest, w.Body.String())
	}
}

func TestUploadSessionBatchRollsBackOnFailure(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	if _, err := svc.CreateChild(root.ID, "exists.bin", false, nil); err != nil {
		t.Fatalf("create existing: %v", err)
	}
	body := apiv1.UploadSessionBatchCreateRequest{
		SegmentSize: 1,
		Uploads: []apiv1.UploadSessionCreateRequest{
			{Path: root.Name + "/created-first.bin", Size: 1, Checksum: sha256Prefixed([]byte("a"))},
			{Path: root.Name + "/exists.bin", Size: 1, Checksum: sha256Prefixed([]byte("b"))},
		},
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions:batch", raw))
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", w.Result().StatusCode, http.StatusConflict, w.Body.String())
	}
	sessions, err := svc.ListUploadSessions(domain.UploadSessionInProgress)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("batch rollback left sessions: %#v", sessions)
	}
}

func TestUploadSessionDirectBaseURLIgnoresUntrustedForwardedHost(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxSessionUploadBytes: 10 << 20,
		MaxUploadBytes:        10 << 20,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("x")
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/direct-host.bin",
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: 1,
		Direct:      &apiv1.UploadSessionDirectRequest{},
	}
	raw, _ := json.Marshal(body)
	req := authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw)
	req.Host = "files.local"
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var out apiv1.UploadSessionResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Direct == nil || !strings.HasPrefix(out.Direct.BaseURL, "http://files.local/") {
		t.Fatalf("baseUrl=%q", out.Direct)
	}
}

func TestUploadSessionDirectRejectsInvalidAllow(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("x")
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/bad-allow.bin",
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: 1,
		Direct:      &apiv1.UploadSessionDirectRequest{Allow: []string{"putSegments"}},
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", w.Result().StatusCode, http.StatusBadRequest, w.Body.String())
	}
}

func TestUploadSessionCommitRecoversAfterReplaceBeforeWitness(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("recover committed file")
	session := createUploadSession(t, r, root.Name+"/recover.bin", content, 8, false)
	for _, seg := range session.Segments {
		part := content[seg.Offset : seg.Offset+seg.Size]
		w := putSessionSegment(t, r, session.ID, seg.Index, part)
		if w.Result().StatusCode != http.StatusOK {
			t.Fatalf("put segment %d status=%d body=%s", seg.Index, w.Result().StatusCode, w.Body.String())
		}
	}

	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	completePath := filepath.Join(t.TempDir(), "complete.bin")
	if err := os.WriteFile(completePath, content, 0o644); err != nil {
		t.Fatalf("write complete file: %v", err)
	}
	if _, err := svc.ReplaceFile(stored.ParentID, stored.Filename, completePath, nil, domain.ConflictError); err != nil {
		t.Fatalf("replace file: %v", err)
	}
	stored.Phase = domain.UploadSessionCommitting
	if err := svc.UpdateUploadSession(*stored); err != nil {
		t.Fatalf("mark committing: %v", err)
	}

	commit := httptest.NewRecorder()
	r.ServeHTTP(commit, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil))
	if commit.Result().StatusCode != http.StatusOK {
		t.Fatalf("recover commit status=%d body=%s", commit.Result().StatusCode, commit.Body.String())
	}
	if _, err := svc.LookupUploadCommitRecord(session.ID); err != nil {
		t.Fatalf("commit record was not recovered: %v", err)
	}
}

func TestUploadSessionAmbiguousCommitDoesNotOverwriteNewerFile(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	oldContent := []byte("old upload bytes")
	session := createUploadSession(t, r, root.Name+"/ambiguous.bin", oldContent, 8, false)
	for _, seg := range session.Segments {
		part := oldContent[seg.Offset : seg.Offset+seg.Size]
		w := putSessionSegment(t, r, session.ID, seg.Index, part)
		if w.Result().StatusCode != http.StatusOK {
			t.Fatalf("put segment %d status=%d body=%s", seg.Index, w.Result().StatusCode, w.Body.String())
		}
	}
	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	stored.Phase = domain.UploadSessionCommitting
	if err := svc.UpdateUploadSession(*stored); err != nil {
		t.Fatalf("mark committing: %v", err)
	}
	newer, err := svc.CreateChild(root.ID, "ambiguous.bin", false, nil)
	if err != nil {
		t.Fatalf("create newer file: %v", err)
	}
	if err := svc.WriteContent(newer.ID, bytes.NewReader([]byte("newer bytes"))); err != nil {
		t.Fatalf("write newer file: %v", err)
	}

	commit := httptest.NewRecorder()
	r.ServeHTTP(commit, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+session.ID+"/commit", nil))
	if commit.Result().StatusCode != http.StatusConflict {
		t.Fatalf("commit status=%d want=%d body=%s", commit.Result().StatusCode, http.StatusConflict, commit.Body.String())
	}
	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	got, err := os.ReadFile(filepath.Join(rootAbs, "ambiguous.bin"))
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(got) != "newer bytes" {
		t.Fatalf("ambiguous retry overwrote newer file: %q", got)
	}
}

type blockingEOFReader struct {
	data     []byte
	started  chan struct{}
	release  chan struct{}
	sentData bool
}

func (r *blockingEOFReader) Read(p []byte) (int, error) {
	if !r.sentData {
		r.sentData = true
		copy(p, r.data)
		close(r.started)
		return len(r.data), nil
	}
	<-r.release
	return 0, io.EOF
}

func TestUploadSessionAbortBeatsInFlightSegmentPersistence(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("blocked segment")
	session := createUploadSession(t, r, root.Name+"/abort-race.bin", content, int64(len(content)), false)
	reader := &blockingEOFReader{data: content, started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v1/uploads/sessions/"+session.ID+"/segments/0", reader)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Segment-Checksum", sha256Prefixed(content))
		r.ServeHTTP(w, req)
		done <- w
	}()
	<-reader.started

	abort := httptest.NewRecorder()
	r.ServeHTTP(abort, authedJSONRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil))
	if abort.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("abort status=%d body=%s", abort.Result().StatusCode, abort.Body.String())
	}
	close(reader.release)
	put := <-done
	if put.Result().StatusCode != http.StatusConflict {
		t.Fatalf("put status=%d want=%d body=%s", put.Result().StatusCode, http.StatusConflict, put.Body.String())
	}
	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if stored.Phase != domain.UploadSessionAborted {
		t.Fatalf("phase=%s want aborted", stored.Phase)
	}
}

func TestUploadSessionDirectTokenCanPutAndCommit(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		PublicURL:             "https://files.example.test",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxSessionUploadBytes: 10 << 20,
		MaxUploadBytes:        1024,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("browser direct session")
	session := createUploadSession(t, r, root.Name+"/direct/session.txt", content, int64(len(content)), true)
	if session.Direct == nil || !strings.HasPrefix(session.Direct.BaseURL, "https://files.example.test/v1/uploads/sessions/") {
		t.Fatalf("missing direct token: %#v", session.Direct)
	}

	put := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, session.Direct.BaseURL+"/segments/0", bytes.NewReader(content))
	req.Header.Set("Filegate-Upload-Session", session.Direct.Token)
	req.Header.Set("X-Segment-Checksum", sha256Prefixed(content))
	r.ServeHTTP(put, req)
	if put.Result().StatusCode != http.StatusOK {
		t.Fatalf("direct put status=%d body=%s", put.Result().StatusCode, put.Body.String())
	}

	commit := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, session.Direct.BaseURL+"/commit", nil)
	req.Header.Set("Filegate-Upload-Session", session.Direct.Token)
	r.ServeHTTP(commit, req)
	if commit.Result().StatusCode != http.StatusOK {
		t.Fatalf("direct commit status=%d body=%s", commit.Result().StatusCode, commit.Body.String())
	}
	if _, err := svc.ResolvePath(root.Name + "/direct/session.txt"); err != nil {
		t.Fatalf("resolve direct upload: %v", err)
	}
}

// A session token is a capability for exactly one session. Nothing else in the
// suite pinned that, so a scoping regression would have shipped silently.
func TestUploadSessionTokenIsScopedToOneSession(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("scoped token payload")
	victim := createUploadSession(t, r, root.Name+"/scoped/victim.txt", content, int64(len(content)), true)
	attacker := createUploadSession(t, r, root.Name+"/scoped/attacker.txt", content, int64(len(content)), true)
	if victim.Direct == nil || attacker.Direct == nil {
		t.Fatalf("expected direct tokens on both sessions")
	}

	cases := []struct {
		name   string
		method string
		target string
		body   io.Reader
	}{
		{"status", http.MethodGet, "/v1/uploads/sessions/" + victim.ID, nil},
		{"putSegment", http.MethodPut, "/v1/uploads/sessions/" + victim.ID + "/segments/0", bytes.NewReader(content)},
		{"commit", http.MethodPost, "/v1/uploads/sessions/" + victim.ID + "/commit", nil},
		{"abort", http.MethodDelete, "/v1/uploads/sessions/" + victim.ID, nil},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, tc.body)
		req.Header.Set("Filegate-Upload-Session", attacker.Direct.Token)
		r.ServeHTTP(w, req)
		if w.Result().StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with another session's token: status=%d, want 401", tc.name, w.Result().StatusCode)
		}
	}

	// The victim session must still be usable afterwards.
	if got := putSessionSegment(t, r, victim.ID, 0, content); got.Result().StatusCode != http.StatusOK {
		t.Fatalf("victim segment put status=%d body=%s", got.Result().StatusCode, got.Body.String())
	}
}

// Abort is destructive, so it must refuse both anonymous callers and tokens
// that were minted without the abort scope.
func TestUploadSessionAbortRequiresAbortScope(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("abort scope payload")
	body := apiv1.UploadSessionCreateRequest{
		Path:        root.Name + "/scoped/no-abort.txt",
		Size:        int64(len(content)),
		Checksum:    sha256Prefixed(content),
		SegmentSize: int64(len(content)),
		OnConflict:  "error",
		Direct: &apiv1.UploadSessionDirectRequest{
			ExpiresInSeconds: 60,
			Allow:            []string{"putSegment", "status"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions", raw))
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var session apiv1.UploadSessionResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&session); err != nil {
		t.Fatalf("decode: %v", err)
	}

	anonymous := httptest.NewRecorder()
	r.ServeHTTP(anonymous, httptest.NewRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil))
	if anonymous.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous abort status=%d, want 401", anonymous.Result().StatusCode)
	}

	scoped := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil)
	req.Header.Set("Filegate-Upload-Session", session.Direct.Token)
	r.ServeHTTP(scoped, req)
	if scoped.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("abort without the abort scope status=%d, want 401", scoped.Result().StatusCode)
	}

	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if stored.Phase != domain.UploadSessionInProgress {
		t.Fatalf("phase=%q, want the session untouched", stored.Phase)
	}
}

// Aborting frees the staged bytes but leaves an aborted row behind, so a late
// segment PUT cannot resurrect a cancelled upload.
func TestUploadSessionAbortFreesBytesAndKeepsTheRow(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("abort payload")
	session := createUploadSession(t, r, root.Name+"/gc/aborted.txt", content, int64(len(content)), false)
	if got := putSessionSegment(t, r, session.ID, 0, content); got.Result().StatusCode != http.StatusOK {
		t.Fatalf("segment put status=%d", got.Result().StatusCode)
	}
	segs, err := svc.ListUploadSegments(session.ID)
	if err != nil || len(segs) != 1 {
		t.Fatalf("segments=%d err=%v", len(segs), err)
	}

	abort := httptest.NewRecorder()
	r.ServeHTTP(abort, authedJSONRequest(http.MethodDelete, "/v1/uploads/sessions/"+session.ID, nil))
	if abort.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("abort status=%d body=%s", abort.Result().StatusCode, abort.Body.String())
	}

	stored, err := svc.LookupUploadSession(session.ID)
	if err != nil {
		t.Fatalf("lookup after abort: %v", err)
	}
	if stored.Phase != domain.UploadSessionAborted {
		t.Fatalf("phase=%q, want aborted", stored.Phase)
	}
	if _, statErr := os.Stat(segs[0].Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("segment file %s still present after abort", segs[0].Path)
	}
	late := putSessionSegment(t, r, session.ID, 0, content)
	if late.Result().StatusCode != http.StatusConflict {
		t.Fatalf("segment PUT after abort status=%d, want 409", late.Result().StatusCode)
	}
}

// The expiry sweep is the other half of GC: without it the aborted rows above
// would accumulate in the index forever.
func TestUploadSessionSweepRemovesExpiredRows(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          10 * time.Millisecond,
		UploadCleanupInterval: 20 * time.Millisecond,
		MaxChunkBytes:         10 << 20,
		MaxSessionUploadBytes: 10 << 20,
		MaxUploadBytes:        10 << 20,
	})
	defer cleanup()
	_ = r

	root := svc.ListRoot()[0]
	stale := domain.UploadSession{
		ID:            "upl_" + strings.Repeat("a", 32),
		Path:          root.Name + "/gc/stale.bin",
		ParentID:      root.ID,
		Filename:      "stale.bin",
		Size:          1024,
		SegmentSize:   1024,
		TotalSegments: 1,
		Phase:         domain.UploadSessionAborted,
		CreatedAt:     time.Now().Add(-time.Hour).UnixMilli(),
		UpdatedAt:     time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := svc.CreateUploadSession(stale); err != nil {
		t.Fatalf("create stale session: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := svc.LookupUploadSession(stale.ID)
		if errors.Is(err, domain.ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale session still present after the sweep window: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
