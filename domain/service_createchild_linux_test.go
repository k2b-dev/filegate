//go:build linux

package domain_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestCreateChildConflictPreservesExistingContent pins the atomic
// no-replace semantics of CreateChild: a name collision must surface
// as ErrConflict and never truncate the existing file (the previous
// Stat-then-OpenWrite(O_TRUNC) sequence had a clobber window).
func TestCreateChildConflictPreservesExistingContent(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	absPath := filepath.Join(rootAbs, "keep.txt")
	if err := os.WriteFile(absPath, []byte("precious"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	if err := svc.SyncAbsPath(absPath); err != nil {
		t.Fatalf("sync existing file: %v", err)
	}

	if _, err := svc.CreateChild(root.ID, "keep.txt", false, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("create over existing file err=%v, want ErrConflict", err)
	}
	if _, err := svc.CreateChild(root.ID, "keep.txt", true, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mkdir over existing file err=%v, want ErrConflict", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "precious" {
		t.Fatalf("existing content clobbered: %q", data)
	}

	if _, err := svc.CreateChild(root.ID, "adir", true, nil); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	if _, err := svc.CreateChild(root.ID, "adir", true, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mkdir over existing dir err=%v, want ErrConflict", err)
	}
}
