//go:build linux

package domain_test

import (
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestTransferMoveInvalidatesDescendantPathCache pins the targeted
// cache invalidation on directory moves: descendants of a moved
// directory must resolve to their NEW virtual path even when their
// id→path mapping was hot before the move. The old implementation
// purged the whole cache (hiding bugs); the targeted one relies on
// reParentNode invalidating the old prefix explicitly.
func TestTransferMoveInvalidatesDescendantPathCache(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	dir, err := svc.CreateChild(root.ID, "a", true, nil)
	if err != nil {
		t.Fatalf("create dir: %v", err)
	}
	sub, err := svc.CreateChild(dir.ID, "sub", true, nil)
	if err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	file, err := svc.CreateChild(sub.ID, "f.txt", false, nil)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}

	// Warm the id→path cache for the descendant.
	warm, err := svc.GetFile(file.ID)
	if err != nil {
		t.Fatalf("warm GetFile: %v", err)
	}
	if !strings.HasSuffix(warm.Path, "/a/sub/f.txt") {
		t.Fatalf("pre-move path=%q, want .../a/sub/f.txt", warm.Path)
	}

	if _, err := svc.Transfer(domain.TransferRequest{
		Op:             "move",
		SourceID:       dir.ID,
		TargetParentID: root.ID,
		TargetName:     "b",
	}); err != nil {
		t.Fatalf("move dir: %v", err)
	}

	moved, err := svc.GetFile(file.ID)
	if err != nil {
		t.Fatalf("post-move GetFile: %v", err)
	}
	if !strings.HasSuffix(moved.Path, "/b/sub/f.txt") {
		t.Fatalf("post-move path=%q, want .../b/sub/f.txt (stale cache?)", moved.Path)
	}
}
