//go:build linux

package domain_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// These tests pin the create-vs-update-vs-move event semantics. After the
// emission-from-syncSingle/syncSubtree refactor, every public mutation is
// expected to publish exactly one semantically-correct event:
//
//   - new entity                  → EventCreated
//   - mutated existing entity     → EventUpdated
//   - rename / re-parent          → EventMoved
//   - removed entity              → EventDeleted (covered in service_event_publishing_linux_test.go)
//
// A regression here would mean either a missing emission (subscribers go
// silent) or a wrong type (audit logs and webhook routers mis-classify).

func TestCreateChildEmitsEventCreated(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	bus.reset()

	child, err := svc.CreateChild(root.ID, "fresh.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated, got %d: %#v", len(created), created)
	}
	if created[0].ID != child.ID {
		t.Fatalf("EventCreated.ID = %s, want %s", created[0].ID, child.ID)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("CreateChild leaked EventUpdated alongside EventCreated: %#v", updates)
	}
}

func TestMkdirRelativeEmitsCreatedOnNewLeaf(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	bus.reset()

	leaf, err := svc.MkdirRelative(root.ID, "a/b/c", true, nil, domain.ConflictError)
	if err != nil {
		t.Fatalf("MkdirRelative: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated, got %d: %#v", len(created), created)
	}
	if created[0].ID != leaf.ID {
		t.Fatalf("EventCreated.ID = %s, want %s (leaf)", created[0].ID, leaf.ID)
	}
}

func TestMkdirRelativeIdempotentSkipEmitsNothing(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	if _, err := svc.MkdirRelative(root.ID, "exists", false, nil, domain.ConflictError); err != nil {
		t.Fatalf("first MkdirRelative: %v", err)
	}
	bus.reset()

	// Second call with ConflictSkip on an existing leaf is a no-op.
	if _, err := svc.MkdirRelative(root.ID, "exists", false, nil, domain.ConflictSkip); err != nil {
		t.Fatalf("idempotent MkdirRelative: %v", err)
	}

	if events := bus.snapshot(); len(events) != 0 {
		t.Fatalf("idempotent MkdirRelative emitted unexpected events: %#v", events)
	}
}

func TestUpdateNodeRenameEmitsEventMoved(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	file, err := svc.CreateChild(root.ID, "old.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	bus.reset()

	newName := "new.txt"
	if _, err := svc.UpdateNode(file.ID, &newName, nil, false); err != nil {
		t.Fatalf("UpdateNode rename: %v", err)
	}

	moved := filterByType(bus.snapshot(), domain.EventMoved)
	if len(moved) != 1 {
		t.Fatalf("want 1 EventMoved, got %d: %#v", len(moved), moved)
	}
	if moved[0].ID != file.ID {
		t.Fatalf("EventMoved.ID = %s, want %s", moved[0].ID, file.ID)
	}
	if !strings.HasSuffix(moved[0].Path, "/new.txt") {
		t.Fatalf("EventMoved.Path = %q, want post-rename path ending in /new.txt", moved[0].Path)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("rename leaked EventUpdated: %#v", updates)
	}
}

func TestUpdateNodeOwnershipOnlyEmitsEventUpdated(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	file, err := svc.CreateChild(root.ID, "x.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	bus.reset()

	if _, err := svc.UpdateNode(file.ID, nil, &domain.Ownership{Mode: "640"}, false); err != nil {
		t.Fatalf("UpdateNode ownership: %v", err)
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) != 1 {
		t.Fatalf("want 1 EventUpdated, got %d: %#v", len(updates), updates)
	}
	if updates[0].ID != file.ID {
		t.Fatalf("EventUpdated.ID = %s, want %s", updates[0].ID, file.ID)
	}
	if moved := filterByType(bus.snapshot(), domain.EventMoved); len(moved) != 0 {
		t.Fatalf("ownership-only update leaked EventMoved: %#v", moved)
	}
}

func TestTransferCopyEmitsEventCreated(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	src, err := svc.CreateChild(root.ID, "src.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild src: %v", err)
	}
	dstParent, err := svc.CreateChild(root.ID, "dst-parent", true, nil)
	if err != nil {
		t.Fatalf("CreateChild dst: %v", err)
	}
	bus.reset()

	copied, err := svc.Transfer(domain.TransferRequest{
		Op:             "copy",
		SourceID:       src.ID,
		TargetParentID: dstParent.ID,
		TargetName:     "copy.txt",
		OnConflict:     domain.ConflictError,
	})
	if err != nil {
		t.Fatalf("Transfer copy: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated, got %d: %#v", len(created), created)
	}
	if created[0].ID != copied.ID {
		t.Fatalf("EventCreated.ID = %s, want %s (copy id)", created[0].ID, copied.ID)
	}
	if created[0].ID == src.ID {
		t.Fatalf("EventCreated.ID matches source — copy should produce a fresh ID")
	}
}

func TestWriteContentByVirtualPathEmitsCreatedOnNewFile(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	bus.reset()

	meta, created, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/new.txt",
		strings.NewReader("hello"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("WriteContentByVirtualPath: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for fresh path")
	}

	createdEvents := filterByType(bus.snapshot(), domain.EventCreated)
	if len(createdEvents) != 1 {
		t.Fatalf("want 1 EventCreated, got %d: %#v", len(createdEvents), createdEvents)
	}
	if createdEvents[0].ID != meta.ID {
		t.Fatalf("EventCreated.ID = %s, want %s", createdEvents[0].ID, meta.ID)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("new-file PUT leaked EventUpdated: %#v", updates)
	}
}

func TestWriteContentByVirtualPathEmitsUpdatedOnOverwrite(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	if _, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/x.txt",
		strings.NewReader("v1"),
		domain.ConflictError,
	); err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	bus.reset()

	meta, created, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/x.txt",
		strings.NewReader("v2"),
		domain.ConflictOverwrite,
	)
	if err != nil {
		t.Fatalf("overwrite PUT: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on overwrite")
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) != 1 {
		t.Fatalf("want 1 EventUpdated on overwrite, got %d: %#v", len(updates), updates)
	}
	if updates[0].ID != meta.ID {
		t.Fatalf("EventUpdated.ID = %s, want %s", updates[0].ID, meta.ID)
	}
	if createdEvents := filterByType(bus.snapshot(), domain.EventCreated); len(createdEvents) != 0 {
		t.Fatalf("overwrite leaked EventCreated: %#v", createdEvents)
	}
}

func TestReplaceFileEmitsCreatedOnNewSlot(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath root: %v", err)
	}
	src := filepath.Join(rootAbs, ".tmp-src")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write tmp src: %v", err)
	}
	bus.reset()

	meta, err := svc.ReplaceFile(root.ID, "fresh.bin", src, nil, domain.ConflictError)
	if err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated, got %d: %#v", len(created), created)
	}
	if created[0].ID != meta.ID {
		t.Fatalf("EventCreated.ID = %s, want %s", created[0].ID, meta.ID)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("fresh-slot ReplaceFile leaked EventUpdated: %#v", updates)
	}
}

func TestReplaceFileEmitsUpdatedOnOverwrite(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath root: %v", err)
	}
	existing, err := svc.CreateChild(root.ID, "target.bin", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	src := filepath.Join(rootAbs, ".tmp-src")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write tmp src: %v", err)
	}
	bus.reset()

	meta, err := svc.ReplaceFile(root.ID, "target.bin", src, nil, domain.ConflictOverwrite)
	if err != nil {
		t.Fatalf("ReplaceFile overwrite: %v", err)
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) != 1 {
		t.Fatalf("want 1 EventUpdated, got %d: %#v", len(updates), updates)
	}
	if updates[0].ID != existing.ID || updates[0].ID != meta.ID {
		t.Fatalf("EventUpdated.ID = %s, want %s (preserved on overwrite)",
			updates[0].ID, existing.ID)
	}
	if createdEvents := filterByType(bus.snapshot(), domain.EventCreated); len(createdEvents) != 0 {
		t.Fatalf("overwrite ReplaceFile leaked EventCreated: %#v", createdEvents)
	}
}

func TestSyncAbsPathEmitsCreatedForNewExternalFile(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath: %v", err)
	}
	abs := filepath.Join(rootAbs, "external.txt")
	if err := os.WriteFile(abs, []byte("from outside"), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}
	bus.reset()

	if err := svc.SyncAbsPath(abs); err != nil {
		t.Fatalf("SyncAbsPath: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated for newly-detected file, got %d: %#v", len(created), created)
	}
	if !strings.HasSuffix(created[0].Path, "/external.txt") {
		t.Fatalf("EventCreated.Path = %q, want suffix /external.txt", created[0].Path)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("first detector sync leaked EventUpdated: %#v", updates)
	}
}

func TestSyncAbsPathEmitsUpdatedForKnownFile(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	file, err := svc.CreateChild(root.ID, "known.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	abs, err := svc.ResolveAbsPath(file.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath: %v", err)
	}
	// Modify the file externally so syncSingle has something to refresh.
	if err := os.WriteFile(abs, []byte("modified"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	bus.reset()

	if err := svc.SyncAbsPath(abs); err != nil {
		t.Fatalf("SyncAbsPath: %v", err)
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) != 1 {
		t.Fatalf("want 1 EventUpdated for known-file resync, got %d: %#v", len(updates), updates)
	}
	if updates[0].ID != file.ID {
		t.Fatalf("EventUpdated.ID = %s, want %s", updates[0].ID, file.ID)
	}
	if created := filterByType(bus.snapshot(), domain.EventCreated); len(created) != 0 {
		t.Fatalf("known-file resync leaked EventCreated: %#v", created)
	}
}

// ReconcileDirectory's role is to close gaps the detector's inode stream
// doesn't cover. When it discovers an on-disk child the index doesn't
// know about, that child is materially new — pin EventCreated so the
// audit trail and any thumbnail pre-warmer can see externally-arrived
// files (e.g. a hardlink rename btrfs find-new missed).
func TestReconcileDirectoryEmitsCreatedForNewChildren(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath: %v", err)
	}
	// Create a file directly on disk WITHOUT going through Service. It is
	// invisible to the index until ReconcileDirectory walks the parent.
	hidden := filepath.Join(rootAbs, "out-of-band.txt")
	if err := os.WriteFile(hidden, []byte("appeared externally"), 0o644); err != nil {
		t.Fatalf("write out-of-band file: %v", err)
	}
	bus.reset()

	if err := svc.ReconcileDirectory(rootAbs); err != nil {
		t.Fatalf("ReconcileDirectory: %v", err)
	}

	created := filterByType(bus.snapshot(), domain.EventCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 EventCreated for newly-discovered child, got %d: %#v", len(created), created)
	}
	if !strings.HasSuffix(created[0].Path, "/out-of-band.txt") {
		t.Fatalf("EventCreated.Path = %q, want suffix /out-of-band.txt", created[0].Path)
	}
	if updates := filterByType(bus.snapshot(), domain.EventUpdated); len(updates) != 0 {
		t.Fatalf("ReconcileDirectory leaked EventUpdated for new child: %#v", updates)
	}
}

// WriteContent on an existing file is the canonical "metadata change to
// known entity" path. Pinned alongside SyncAbsPath's known-file emission
// so the audit-log subscriber can rely on EventUpdated meaning "ID
// existed and its bytes/metadata changed".
func TestWriteContentEmitsExactlyOneEventUpdated(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	file, err := svc.CreateChild(root.ID, "w.txt", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	bus.reset()

	if err := svc.WriteContent(file.ID, strings.NewReader("payload")); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) != 1 {
		t.Fatalf("want 1 EventUpdated, got %d: %#v", len(updates), updates)
	}
	if updates[0].ID != file.ID {
		t.Fatalf("EventUpdated.ID = %s, want %s", updates[0].ID, file.ID)
	}
	if other := len(bus.snapshot()) - len(updates); other != 0 {
		t.Fatalf("WriteContent emitted unrelated events: %#v", bus.snapshot())
	}
}
