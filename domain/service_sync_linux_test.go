//go:build linux

package domain_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func TestSyncAbsPathIndexesCreatedFile(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	absPath := filepath.Join(rootAbs, "external.txt")
	if err := os.WriteFile(absPath, []byte("external"), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	if err := svc.SyncAbsPath(absPath); err != nil {
		t.Fatalf("sync abs path: %v", err)
	}

	id, err := svc.ResolvePath(root.Name + "/external.txt")
	if err != nil {
		t.Fatalf("resolve synced path: %v", err)
	}
	meta, err := svc.GetFile(id)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if meta.Type != "file" {
		t.Fatalf("type=%q, want file", meta.Type)
	}
}

func TestRemoveAbsPathDropsDeletedFileFromIndex(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	created, err := svc.CreateChild(root.ID, "gone.txt", false, nil)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	absPath, err := svc.ResolveAbsPath(created.ID)
	if err != nil {
		t.Fatalf("resolve abs path: %v", err)
	}
	if err := os.Remove(absPath); err != nil {
		t.Fatalf("remove file from fs: %v", err)
	}

	if err := svc.RemoveAbsPath(absPath); err != nil {
		t.Fatalf("remove abs path from index: %v", err)
	}

	_, err = svc.ResolvePath(root.Name + "/gone.txt")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("resolve after removal err=%v, want ErrNotFound", err)
	}
}

func TestWriteContentPreservesIDAndOwnership(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	created, err := svc.CreateChild(root.ID, "stable.txt", false, nil)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	before, err := svc.GetFile(created.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	if err := svc.WriteContent(created.ID, strings.NewReader("updated")); err != nil {
		t.Fatalf("write content: %v", err)
	}

	after, err := svc.GetFile(created.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("id changed: before=%s after=%s", before.ID.String(), after.ID.String())
	}
	if after.UID != before.UID || after.GID != before.GID || after.Mode != before.Mode {
		t.Fatalf("ownership changed: before(uid=%d gid=%d mode=%o) after(uid=%d gid=%d mode=%o)",
			before.UID, before.GID, before.Mode, after.UID, after.GID, after.Mode)
	}

	rc, _, isDir, err := svc.OpenContent(created.ID)
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	if isDir {
		t.Fatalf("expected file content")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	if string(body) != "updated" {
		t.Fatalf("content=%q, want=%q", string(body), "updated")
	}
}

func TestWriteContentRejectsSymlinkTarget(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	created, err := svc.CreateChild(root.ID, "symlink-write.txt", false, nil)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	targetAbs, err := svc.ResolveAbsPath(created.ID)
	if err != nil {
		t.Fatalf("resolve target abs: %v", err)
	}
	outsideFile, err := os.CreateTemp("", "filegate-outside-*")
	if err != nil {
		t.Fatalf("create outside file: %v", err)
	}
	outside := outsideFile.Name()
	if _, err := outsideFile.WriteString("outside"); err != nil {
		_ = outsideFile.Close()
		t.Fatalf("write outside: %v", err)
	}
	if err := outsideFile.Close(); err != nil {
		t.Fatalf("close outside: %v", err)
	}
	defer os.Remove(outside)
	if err := os.Remove(targetAbs); err != nil {
		t.Fatalf("remove target file: %v", err)
	}
	if err := os.Symlink(outside, targetAbs); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	err = svc.WriteContent(created.ID, strings.NewReader("should-not-write"))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("write err=%v, want ErrForbidden", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if string(data) != "outside" {
		t.Fatalf("outside content changed: %q", string(data))
	}
}

type exdevStore struct {
	*filesystem.Store
}

func (s *exdevStore) Rename(_, _ string) error {
	return &os.LinkError{Op: "rename", Err: syscall.EXDEV}
}

func newServiceWithStore(t *testing.T, store domain.Store) (*domain.Service, func()) {
	t.Helper()

	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, store, bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	return svc, func() { _ = idx.Close() }
}

func TestReplaceFileFallbackPreservesSourceID(t *testing.T) {
	store := &exdevStore{Store: filesystem.New()}
	svc, cleanup := newServiceWithStore(t, store)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root abs: %v", err)
	}

	srcDir := filepath.Join(rootAbs, ".fg-uploads", "fallback")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	srcPath := filepath.Join(srcDir, "data.part")
	if err := os.WriteFile(srcPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write src part: %v", err)
	}

	// Pre-stamp the .part file with a fresh UUID, simulating a chunked
	// upload that pre-allocated its target ID. ReplaceFile's fallback
	// path must carry that ID through to the final destination. The
	// upload ID must NOT also be claimed by another existing entity —
	// resolveOrReissueID would otherwise (correctly) re-issue to avoid
	// xattr-clone aliasing; that distinct case is exercised by the
	// snapshot/cp-a tests in cli/.
	sourceID := domain.FileID(uuid.Must(uuid.NewV7()))
	if err := store.SetID(srcPath, sourceID); err != nil {
		t.Fatalf("set source id on part: %v", err)
	}

	out, err := svc.ReplaceFile(root.ID, "final.txt", srcPath, nil, domain.ConflictError)
	if err != nil {
		t.Fatalf("replace file: %v", err)
	}
	if out.ID != sourceID {
		t.Fatalf("final id=%s, want source id=%s", out.ID.String(), sourceID.String())
	}

	finalAbs, err := svc.ResolveAbsPath(out.ID)
	if err != nil {
		t.Fatalf("resolve final abs: %v", err)
	}
	content, err := os.ReadFile(finalAbs)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(content) != "payload" {
		t.Fatalf("content=%q, want payload", string(content))
	}
	if _, err := os.Stat(srcPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected source part removed, err=%v", err)
	}
}
