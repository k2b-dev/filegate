//go:build linux

package domain_test

import (
	"errors"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

func TestTransferMoveUpdatesIndexWithoutGlobalRescan(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	src, err := svc.CreateChild(root, "src", true, nil)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := svc.CreateChild(src.ID, "a.txt", false, nil); err != nil {
		t.Fatalf("create file: %v", err)
	}

	recursive := false
	out, err := svc.Transfer(domain.TransferRequest{
		Op:                 "move",
		SourceID:           src.ID,
		TargetParentID:     root,
		TargetName:         "dst",
		OnConflict:         "error",
		RecursiveOwnership: &recursive,
	})
	if err != nil {
		t.Fatalf("transfer move: %v", err)
	}
	if out.ID != src.ID {
		t.Fatalf("moved ID changed: got %s want %s", out.ID.String(), src.ID.String())
	}

	mount := svc.ListRoot()[0].Name
	if _, err := svc.ResolvePath(mount + "/src"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old path should be gone, err=%v", err)
	}
	if _, err := svc.ResolvePath(mount + "/dst/a.txt"); err != nil {
		t.Fatalf("new path should resolve: %v", err)
	}
}

func TestTransferCopyOverwriteDropsOverwrittenSubtreeFromIndex(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	src, err := svc.CreateChild(root, "src", true, nil)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := svc.CreateChild(src.ID, "new.txt", false, nil); err != nil {
		t.Fatalf("create source file: %v", err)
	}

	dst, err := svc.CreateChild(root, "dst", true, nil)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if _, err := svc.CreateChild(dst.ID, "old.txt", false, nil); err != nil {
		t.Fatalf("create old file: %v", err)
	}

	if _, err := svc.Transfer(domain.TransferRequest{
		Op:             "copy",
		SourceID:       src.ID,
		TargetParentID: root,
		TargetName:     "dst",
		OnConflict:     "overwrite",
	}); err != nil {
		t.Fatalf("transfer copy overwrite: %v", err)
	}

	mount := svc.ListRoot()[0].Name
	if _, err := svc.ResolvePath(mount + "/dst/old.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("overwritten stale entry must be removed from index, err=%v", err)
	}
	if _, err := svc.ResolvePath(mount + "/dst/new.txt"); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

func TestDeleteRemovesSubtreeFromIndex(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	dir, err := svc.CreateChild(root, "to-delete", true, nil)
	if err != nil {
		t.Fatalf("create dir: %v", err)
	}
	if _, err := svc.CreateChild(dir.ID, "child.txt", false, nil); err != nil {
		t.Fatalf("create child file: %v", err)
	}

	if err := svc.Delete(dir.ID); err != nil {
		t.Fatalf("delete dir: %v", err)
	}

	mount := svc.ListRoot()[0].Name
	if _, err := svc.ResolvePath(mount + "/to-delete"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted root dir should be gone: %v", err)
	}
	if _, err := svc.ResolvePath(mount + "/to-delete/child.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted subtree child should be gone: %v", err)
	}
}
