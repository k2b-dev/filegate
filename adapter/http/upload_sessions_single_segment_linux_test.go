//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode identity unavailable on this platform")
	}
	return uint64(st.Ino)
}

func commitSession(t *testing.T, r http.Handler, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/uploads/sessions/"+sessionID+"/commit", nil))
	return w
}

// A one-segment commit must move the staged segment rather than copy it.
//
// Commit dominated every upload-session run in the many-small-files benchmark
// -- 470s against 193s of segment PUTs for 5000 log files -- because assembly
// read each staged segment back and wrote the whole file a second time. Most
// small-file uploads are exactly one segment, so the copy was pure overhead.
//
// Identity is the assertion that actually distinguishes the two: a rename keeps
// the inode, a copy cannot. Comparing timings would prove nothing on a warm
// page cache.
func TestSingleSegmentCommitMovesTheSegment(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("one segment, moved rather than copied")
	session := createUploadSession(t, r, root.Name+"/single/moved.txt", content, int64(len(content)), false)
	if session.TotalSegments != 1 {
		t.Fatalf("want a single-segment session, got %d", session.TotalSegments)
	}

	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	staged, err := svc.ListUploadSegments(session.ID)
	if err != nil || len(staged) != 1 {
		t.Fatalf("list segments: %v (%d segments)", err, len(staged))
	}
	stagedInode := inodeOf(t, staged[0].Path)

	if w := commitSession(t, r, session.ID); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	published := filepath.Join(rootAbs, "single", "moved.txt")

	got, err := os.ReadFile(published)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("published content = %q, want %q", got, content)
	}
	if published := inodeOf(t, published); published != stagedInode {
		t.Errorf("published inode %d differs from staged segment inode %d; the segment was copied, not moved", published, stagedInode)
	}
	if _, err := os.Lstat(staged[0].Path); !os.IsNotExist(err) {
		t.Errorf("staged segment still present after commit: %v", err)
	}
}

// Publication is a single rename, so a crash can never expose a prefix.
//
// The bytes reach their destination by renaming the assembled file over it.
// Rename either happened or did not, which is what makes a crash between
// publishing the bytes and writing the index safe: the destination goes from
// absent to complete in one step, and the index is rebuildable from the
// filesystem. This pins the part that would break silently -- an assembly step
// that wrote into the destination directly, or left a partial file behind.
func TestSingleSegmentCommitLeavesNoPartialFile(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte(strings.Repeat("partial-publish-guard;", 4096))
	session := createUploadSession(t, r, root.Name+"/single/atomic.txt", content, int64(len(content)), false)

	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	published := filepath.Join(rootAbs, "single", "atomic.txt")

	// Nothing may appear at the destination before commit runs.
	if _, err := os.Lstat(published); !os.IsNotExist(err) {
		t.Fatalf("destination exists before commit: %v", err)
	}

	if w := commitSession(t, r, session.ID); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	got, err := os.ReadFile(published)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("published %d bytes, want %d; a partial file was published", len(got), len(content))
	}

	// No staging residue may survive a successful commit, in the mount or
	// beside it. A leftover .tmp is how a half-written assembly would show up.
	var residue []string
	_ = filepath.WalkDir(rootAbs, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".complete") {
			residue = append(residue, path)
		}
		return nil
	})
	if len(residue) > 0 {
		t.Errorf("staging residue left behind: %v", residue)
	}
}

// A commit that fails after assembly must still be retryable.
//
// The move consumes the staged segment, so a second attempt cannot reassemble
// from it. Commit therefore reuses an already-assembled file, which is sound
// because a recorded segment is immutable: re-uploading different bytes is
// refused with a conflict, so the assembled file can only hold what the session
// declared, and the checksum is verified on every attempt regardless.
//
// A destination conflict is the realistic way to land here, and it is
// recoverable -- the operator removes the blocker and retries.
func TestSingleSegmentCommitRetriesAfterConflict(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("retry me after the conflict clears")
	session := createUploadSession(t, r, root.Name+"/single/retry.txt", content, int64(len(content)), false)

	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	blocker := filepath.Join(rootAbs, "single", "retry.txt")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// onConflict=error, so this fails inside the publish step -- after the
	// segment has already been moved into the assembled file.
	first := commitSession(t, r, session.ID)
	if first.Result().StatusCode != http.StatusConflict {
		t.Fatalf("first commit status=%d body=%s, want 409", first.Result().StatusCode, first.Body.String())
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}

	second := commitSession(t, r, session.ID)
	if second.Result().StatusCode != http.StatusOK {
		t.Fatalf("retry after clearing the conflict status=%d body=%s", second.Result().StatusCode, second.Body.String())
	}
	var out apiv1.UploadSessionCommitResponse
	if err := json.NewDecoder(second.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode commit: %v", err)
	}
	if out.Checksum != session.Checksum {
		t.Errorf("commit checksum = %q, want %q", out.Checksum, session.Checksum)
	}

	got, err := os.ReadFile(blocker)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("published content = %q, want %q", got, content)
	}
}

// A rejected commit must not publish anything, however cheap assembly became.
func TestSingleSegmentCommitRejectsChecksumMismatch(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	declared := []byte("what the session promised")
	session := createUploadSession(t, r, root.Name+"/single/mismatch.txt", declared, int64(len(declared)), false)

	// Same length, different bytes: the size check cannot catch this, so the
	// whole-file SHA-256 comparison has to.
	actual := []byte("what the client actually!")
	if len(actual) != len(declared) {
		t.Fatalf("test setup: lengths differ (%d vs %d)", len(actual), len(declared))
	}

	w := httptest.NewRecorder()
	req := authedJSONRequest(http.MethodPut, "/v1/uploads/sessions/"+session.ID+"/segments/0", actual)
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	commit := commitSession(t, r, session.ID)
	if commit.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("commit status=%d body=%s, want 400", commit.Result().StatusCode, commit.Body.String())
	}

	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootAbs, "single", "mismatch.txt")); !os.IsNotExist(err) {
		t.Errorf("a mismatched upload was published: %v", err)
	}
	if _, err := svc.ResolvePath(root.Name + "/single/mismatch.txt"); err == nil {
		t.Error("a mismatched upload is resolvable through the index")
	} else if !isNotFoundErr(err) {
		t.Errorf("unexpected resolve error: %v", err)
	}
}

func isNotFoundErr(err error) bool {
	return err != nil && (err == domain.ErrNotFound || strings.Contains(err.Error(), "not found"))
}
