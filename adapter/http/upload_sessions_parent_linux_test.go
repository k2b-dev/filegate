//go:build linux

package httpadapter

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
)

// Committing into a deep path creates the whole chain and resolves it.
//
// The chain is created by one recursive mkdir rather than one call per level.
// Calling per level produced the same directories but re-acquired a path lock
// and re-walked its own prefix each time, which is the dominant cost of
// committing into a deep tree.
func TestCommitCreatesDeepParentChain(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("deep")
	path := root.Name + "/a/b/c/d/e/deep.txt"
	session := createUploadSession(t, r, path, content, int64(len(content)), false)

	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	if w := commitSession(t, r, session.ID); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	// Every level has to be resolvable, not merely present on disk: the index
	// is what the API answers from.
	for _, rel := range []string{"a", "a/b", "a/b/c", "a/b/c/d", "a/b/c/d/e", "a/b/c/d/e/deep.txt"} {
		if _, err := svc.ResolvePath(root.Name + "/" + rel); err != nil {
			t.Errorf("%s does not resolve after commit: %v", rel, err)
		}
	}
}

// The rollback list holds exactly the levels this commit created.
//
// This exercises ensureSessionParent directly because the interesting failure is
// not reachable through the HTTP surface: a file standing where a directory
// belongs can only exist if every level above it already does, so the recursive
// mkdir fails with nothing created. The list still has to be right, because the
// publish step after it can fail for its own reasons and that closure is what
// runs.
func TestParentRollbackListCoversOnlyNewLevels(t *testing.T) {
	_, svc, cleanup := newTestRouter(t)
	defer cleanup()

	manager := newUploadSessionManager(svc, "test-token", newLiveConfig(RouterOptions{
		MaxChunkBytes:         1 << 20,
		MaxSessionUploadBytes: 1 << 30,
	}), 4, time.Hour, 0)
	defer manager.Close()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	// "existing" predates the call and holds a file, so it is not this commit's
	// to undo. Everything below it is new.
	if err := os.MkdirAll(filepath.Join(rootAbs, "existing"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootAbs, "existing", "resident.txt"), []byte("stay"), 0o644); err != nil {
		t.Fatalf("write resident: %v", err)
	}

	session := domain.UploadSession{
		Path:     root.Name + "/existing/fresh/deeper/file.txt",
		ParentID: root.ID,
	}
	parentID, rollback, err := manager.ensureSessionParent(session)
	if err != nil {
		t.Fatalf("ensureSessionParent: %v", err)
	}
	if parentID.IsZero() {
		t.Fatal("ensureSessionParent returned a zero parent id")
	}
	for _, rel := range []string{"existing/fresh", "existing/fresh/deeper"} {
		if _, err := svc.ResolvePath(root.Name + "/" + rel); err != nil {
			t.Fatalf("%s was not created: %v", rel, err)
		}
	}

	rollback()

	// The two new levels are empty, so the rollback takes them.
	for _, rel := range []string{"existing/fresh/deeper", "existing/fresh"} {
		if _, err := os.Lstat(filepath.Join(rootAbs, rel)); !os.IsNotExist(err) {
			t.Errorf("%s survived the rollback: %v", rel, err)
		}
	}
	// The pre-existing level and its content are untouched.
	if _, err := os.Lstat(filepath.Join(rootAbs, "existing", "resident.txt")); err != nil {
		t.Errorf("pre-existing content was removed: %v", err)
	}
}

// A file standing where a directory belongs fails the commit and survives it.
func TestCommitThroughAFileFailsWithoutDamagingIt(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootAbs, "keep"), 0o755); err != nil {
		t.Fatalf("mkdir keep: %v", err)
	}
	blocker := filepath.Join(rootAbs, "keep", "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	content := []byte("never lands")
	session := createUploadSession(t, r, root.Name+"/keep/blocker/deeper/file.txt", content, int64(len(content)), false)
	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	if commit := commitSession(t, r, session.ID); commit.Result().StatusCode == http.StatusOK {
		t.Fatalf("commit succeeded through a file: %s", commit.Body.String())
	}

	got, err := os.ReadFile(blocker)
	if err != nil || string(got) != "not a dir" {
		t.Errorf("blocker = %q, %v; want it untouched", got, err)
	}
	if info, err := os.Lstat(blocker); err != nil || info.IsDir() {
		t.Errorf("blocker is no longer a plain file: %v", err)
	}
}

// Directories the commit found are not the commit's to remove.
//
// The rollback list is built from the levels that were absent beforehand. Built
// from the resolved chain instead, a failure would delete directories another
// upload was still filling.
func TestFailedCommitKeepsPreExistingEmptyDirectories(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	// An empty directory tree that exists before the commit. Empty is the case
	// that matters: rollbackEmptyDirs only removes empty directories, so a
	// populated one would pass this test for the wrong reason.
	preExisting := filepath.Join(rootAbs, "reserved", "slot")
	if err := os.MkdirAll(preExisting, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(preExisting, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	content := []byte("blocked")
	path := root.Name + "/reserved/slot/blocker/file.txt"
	session := createUploadSession(t, r, path, content, int64(len(content)), false)
	if w := putSessionSegment(t, r, session.ID, 0, content); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("put segment status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	if commit := commitSession(t, r, session.ID); commit.Result().StatusCode == http.StatusOK {
		t.Fatalf("commit succeeded through a file: %s", commit.Body.String())
	}

	for _, rel := range []string{"reserved", "reserved/slot"} {
		if _, err := os.Lstat(filepath.Join(rootAbs, rel)); err != nil {
			t.Errorf("%s was removed by a failed commit that did not create it: %v", rel, err)
		}
	}
}

// Staged artifacts are removed by deriving their paths, not by scanning the
// stage directory, which every session on the mount shares. A multi-segment
// session is the case that would notice an off-by-one in the derivation.
func TestMultiSegmentCommitRemovesEveryStagedSegment(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	content := []byte("several segments worth of content, split up")
	session := createUploadSession(t, r, root.Name+"/multi/file.txt", content, 8, false)
	if session.TotalSegments < 3 {
		t.Fatalf("want a multi-segment session, got %d", session.TotalSegments)
	}

	for _, seg := range session.Segments {
		part := content[seg.Offset : seg.Offset+seg.Size]
		if w := putSessionSegment(t, r, session.ID, seg.Index, part); w.Result().StatusCode != http.StatusOK {
			t.Fatalf("put segment %d status=%d body=%s", seg.Index, w.Result().StatusCode, w.Body.String())
		}
	}

	staged, err := svc.ListUploadSegments(session.ID)
	if err != nil || len(staged) != session.TotalSegments {
		t.Fatalf("list segments: %v (%d of %d)", err, len(staged), session.TotalSegments)
	}

	if w := commitSession(t, r, session.ID); w.Result().StatusCode != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}

	for _, segment := range staged {
		if _, err := os.Lstat(segment.Path); !os.IsNotExist(err) {
			t.Errorf("staged segment %s survived commit: %v", segment.Path, err)
		}
	}

	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootAbs, "multi", "file.txt"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("published content = %q, want %q", got, content)
	}
}
