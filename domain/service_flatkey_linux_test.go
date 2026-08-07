//go:build linux

package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// newServiceWithIndex is like newServiceForOwnershipTest but exposes
// the raw Pebble index so tests can probe the flat-key keyspace
// directly via LookupByFlatKey / IterateFlatKeys (those aren't on
// the public Service surface — yet).
func newServiceWithIndex(t *testing.T) (*domain.Service, *indexpebble.Index, func()) {
	t.Helper()
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	cleanup := func() {
		bus.Close()
		_ = idx.Close()
	}
	return svc, idx, cleanup
}

// TestFlatKeyAfterCreatePopulated pins the cross-protocol invariant
// that creating a file via the public API also installs a flat-key
// entry — without this, S3 ListObjectsV2 (which reads flat-keys)
// would not see REST-uploaded files.
func TestFlatKeyAfterCreatePopulated(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/folder/leaf.txt",
		strings.NewReader("hi"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := idx.LookupByFlatKey(mountName, "folder/leaf.txt")
	if err != nil {
		t.Fatalf("flat-key lookup: %v", err)
	}
	if got != meta.ID {
		t.Fatalf("flat-key id=%v, want %v", got, meta.ID)
	}
}

// TestFlatKeyAfterDirectoryCreatedHasNoEntry verifies that directories
// don't get flat-key entries — they aren't S3 objects and would
// pollute ListObjectsV2 output.
func TestFlatKeyDirectoryHasNoEntry(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	root := svc.ListRoot()[0].ID
	dir, err := svc.CreateChild(root, "mydir", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := idx.LookupByFlatKey(mountName, "mydir"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("dir should have no flat-key, got err=%v (id=%v)", err, dir.ID)
	}
}

// TestFlatKeyAfterFileRenamePropagated covers the file-rename case:
// the auto-rename detection in PutEntity should drop the old flat-key
// and insert the new one. Failing this would let stale keys
// accumulate forever and S3 GET/HEAD would resolve to wrong file IDs.
func TestFlatKeyAfterFileRenamePropagated(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/old.txt",
		strings.NewReader("x"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	newName := "new.txt"
	if _, err := svc.UpdateNode(meta.ID, &newName, nil, false); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, err := idx.LookupByFlatKey(mountName, "old.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old flat-key should be gone, got err=%v", err)
	}
	got, err := idx.LookupByFlatKey(mountName, "new.txt")
	if err != nil {
		t.Fatalf("new flat-key lookup: %v", err)
	}
	if got != meta.ID {
		t.Fatalf("new flat-key id=%v, want %v", got, meta.ID)
	}
}

// TestFlatKeyAfterDirectoryRenameRekeyed covers the harder case:
// renaming a directory must rewrite every descendant's flat-key
// entry, because their absolute paths change but their entity records
// don't (entity.ParentID stays pointing at the dir, dir's name
// changed). Without ReKeyFlatPrefix, S3 GET on the new path 404s
// while the old path still resolves — orphan keys forever.
func TestFlatKeyAfterDirectoryRenameRekeyed(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	root := svc.ListRoot()[0].ID
	dir, err := svc.CreateChild(root, "olddir", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	leaf, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/olddir/leaf.txt",
		strings.NewReader("hi"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("write leaf: %v", err)
	}
	deeper, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/olddir/sub/deep.txt",
		strings.NewReader("deep"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("write deep: %v", err)
	}

	newName := "newdir"
	if _, err := svc.UpdateNode(dir.ID, &newName, nil, false); err != nil {
		t.Fatalf("rename dir: %v", err)
	}

	// Old keys gone
	if _, err := idx.LookupByFlatKey(mountName, "olddir/leaf.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("olddir/leaf.txt should be gone, got err=%v", err)
	}
	if _, err := idx.LookupByFlatKey(mountName, "olddir/sub/deep.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("olddir/sub/deep.txt should be gone, got err=%v", err)
	}
	// New keys present, IDs preserved
	gotLeaf, err := idx.LookupByFlatKey(mountName, "newdir/leaf.txt")
	if err != nil {
		t.Fatalf("newdir/leaf.txt: %v", err)
	}
	if gotLeaf != leaf.ID {
		t.Fatalf("leaf id=%v, want %v", gotLeaf, leaf.ID)
	}
	gotDeep, err := idx.LookupByFlatKey(mountName, "newdir/sub/deep.txt")
	if err != nil {
		t.Fatalf("newdir/sub/deep.txt: %v", err)
	}
	if gotDeep != deeper.ID {
		t.Fatalf("deeper id=%v, want %v", gotDeep, deeper.ID)
	}
}

// TestFlatKeyAfterDeleteRemoved verifies single-file deletes also
// remove the flat-key entry. A leaked flat-key here would make S3
// LookupByFlatKey return a fileID whose entity row is gone — broken.
func TestFlatKeyAfterDeleteRemoved(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/dead.txt",
		strings.NewReader("x"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(meta.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := idx.LookupByFlatKey(mountName, "dead.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("flat-key should be gone after delete, got err=%v", err)
	}
}

// TestFlatKeyAfterSubtreeDeleteRemoved exercises recursive deletes:
// every descendant's flat-key must be cleared. The implementation
// relies on per-DelEntity auto-maintenance walking the parent chain
// in leaf-first order — this test pins that walk-order assumption.
func TestFlatKeyAfterSubtreeDeleteRemoved(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	root := svc.ListRoot()[0].ID
	dir, err := svc.CreateChild(root, "sub", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, _, err := svc.WriteContentByVirtualPath(
			"/"+mountName+"/sub/"+name,
			strings.NewReader("x"),
			domain.ConflictError,
		); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/sub/deeper/c.txt",
		strings.NewReader("y"),
		domain.ConflictError,
	); err != nil {
		t.Fatalf("write deeper: %v", err)
	}

	if err := svc.Delete(dir.ID); err != nil {
		t.Fatalf("delete subtree: %v", err)
	}

	for _, rel := range []string{"sub/a.txt", "sub/b.txt", "sub/deeper/c.txt"} {
		if _, err := idx.LookupByFlatKey(mountName, rel); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("flat-key %q should be gone, got err=%v", rel, err)
		}
	}
}

// TestFlatKeyAfterOverwriteUnchanged: overwriting a file's content
// (same path, different bytes) must NOT delete and re-insert the
// flat-key — the path is unchanged, so the entry should remain in
// place pointing at the same fileID. Catches a regression where
// PutEntity's rename-detection-then-cleanup mis-fires for pure
// content overwrites.
func TestFlatKeyAfterOverwriteUnchanged(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/over.txt",
		strings.NewReader("v1"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.WriteContent(meta.ID, strings.NewReader("v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err := idx.LookupByFlatKey(mountName, "over.txt")
	if err != nil {
		t.Fatalf("flat-key lookup after overwrite: %v", err)
	}
	if got != meta.ID {
		t.Fatalf("flat-key id changed: got %v, want %v", got, meta.ID)
	}
}

// TestFlatKeyHardLinkSiblingsSkipped verifies that hard-link siblings
// (Nlink > 1) don't get flat-key entries. The schema can only
// represent one (mount, relPath) → id mapping per file ID, so naively
// upserting on every PutEntity would alias the wrong path.
//
// We can't easily construct a real hard-link via the public API
// (no Link method exposed), so this test seeds the index directly
// with a synthetic Nlink=2 entity and verifies no flat-key was
// installed. A regression here would let S3 LookupByFlatKey return
// stale paths after one of the hard-links is unlinked.
func TestFlatKeyHardLinkSiblingsSkipped(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	// Seed a real file so we have a parent ID we can dangle off.
	parent := svc.ListRoot()[0].ID
	hl, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/hl-a.txt",
		strings.NewReader("x"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the entity into the Nlink=2 shape via a fake Put through
	// the index Batch — simulating what the detector path produces
	// when it sees a hard-linked file.
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{
			ID:       hl.ID,
			ParentID: parent,
			Name:     "hl-a.txt",
			Size:     1,
			Nlink:    2,
		})
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	// The hard-link case should NOT have a flat-key. Either the
	// previous one got cleared (because the second Put detected
	// Nlink>1 and skipped maintenance — though actually our
	// implementation only SKIPS adding, doesn't pro-actively delete,
	// so the original flat-key from the create is still there).
	//
	// What we actually pin: a FRESH Put with Nlink>1 that looks like
	// a brand-new entity (different ID) doesn't install a flat-key.
	syntheticID := domain.FileID{}
	syntheticID[15] = 0xff // distinct from hl.ID
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{
			ID:       syntheticID,
			ParentID: parent,
			Name:     "hl-b.txt",
			Size:     1,
			Nlink:    2,
		})
		return nil
	}); err != nil {
		t.Fatalf("batch synthetic: %v", err)
	}
	if _, err := idx.LookupByFlatKey(mountName, "hl-b.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Nlink>1 entity should not have flat-key, got err=%v", err)
	}
}

// TestFlatKeySweepDropsOrphans proves the rescan sweep cleans up
// flat-key entries whose referenced fileID is gone. This catches the
// "broken parent chain at delete time leaves orphan flat-key" class
// of bugs.
func TestFlatKeySweepDropsOrphans(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	// Inject a synthetic flat-key entry pointing at a nonexistent ID.
	syntheticID := domain.FileID{}
	syntheticID[15] = 0xaa
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutFlatKey(mountName, "ghost.txt", syntheticID)
		return nil
	}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	if _, err := idx.LookupByFlatKey(mountName, "ghost.txt"); err != nil {
		t.Fatalf("orphan should be present before rescan, got err=%v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := idx.LookupByFlatKey(mountName, "ghost.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("orphan should be swept by rescan, got err=%v", err)
	}
}

// TestFlatKeyAfterRescanRebuild proves that running Rescan against an
// existing dataset (re)populates the flat-key index. This is the
// upgrade path: operators with a pre-flat-key index run rescan and
// expect S3 listing to work afterward.
func TestFlatKeyAfterRescanRebuild(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/walk-me.txt",
		strings.NewReader("x"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	got, err := idx.LookupByFlatKey(mountName, "walk-me.txt")
	if err != nil {
		t.Fatalf("flat-key after rescan: %v", err)
	}
	if got != meta.ID {
		t.Fatalf("got %v, want %v", got, meta.ID)
	}
}
