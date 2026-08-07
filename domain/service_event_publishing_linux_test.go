//go:build linux

package domain_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// recordedBus collects every event the service publishes so tests can pin
// the exact emission shape per mutation surface.
type recordedBus struct {
	mu     sync.Mutex
	events []domain.Event
	inner  domain.EventBus
}

func newRecordedBus() *recordedBus {
	return &recordedBus{inner: eventbus.New()}
}

func (r *recordedBus) Publish(event domain.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	r.inner.Publish(event)
}

func (r *recordedBus) Subscribe(eventType domain.EventType, handler func(domain.Event)) {
	r.inner.Subscribe(eventType, handler)
}

func (r *recordedBus) snapshot() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.Event, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordedBus) reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

func newServiceWithRecordedBus(t *testing.T) (*domain.Service, *recordedBus, func()) {
	t.Helper()
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := newRecordedBus()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	bus.reset() // discard whatever the bootstrap scan emitted
	return svc, bus, func() { _ = idx.Close() }
}

// TestDeleteSubtreePublishesExactlyOneEventDeleted pins that the centralised
// publish in deleteSubtree fires regardless of which caller invoked it, and
// that the legacy double-publish (caller-publish AFTER deleteSubtree) is
// gone.
func TestDeleteSubtreePublishesExactlyOneEventDeleted(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	dir, err := svc.CreateChild(root.ID, "to-delete", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := svc.CreateChild(dir.ID, "child.txt", false, nil); err != nil {
		t.Fatalf("mkfile: %v", err)
	}
	bus.reset()

	if err := svc.Delete(dir.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deleteEvents := filterByType(bus.snapshot(), domain.EventDeleted)
	if len(deleteEvents) != 1 {
		t.Fatalf("expected exactly 1 EventDeleted, got %d: %#v", len(deleteEvents), deleteEvents)
	}
	if deleteEvents[0].ID != dir.ID {
		t.Fatalf("EventDeleted.ID = %s, want %s", deleteEvents[0].ID, dir.ID)
	}
	if deleteEvents[0].Path == "" {
		t.Fatalf("EventDeleted.Path is empty — Path lookup before deleteSubtree tear-down regressed")
	}
}

// TestTransferMoveEmitsSingleEventMoved pins that Transfer move emits one
// EventMoved for the subtree root and does NOT degenerate into per-
// descendant noise (5 descendants → not 5+ events). The recursive-ownership
// path also goes through syncSubtree, which must remain emission-silent.
func TestTransferMoveEmitsSingleEventMoved(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	src, err := svc.CreateChild(root.ID, "src", true, nil)
	if err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := svc.CreateChild(src.ID, "f"+itoaLocal(i)+".txt", false, nil); err != nil {
			t.Fatalf("mkfile: %v", err)
		}
	}
	dstParent, err := svc.CreateChild(root.ID, "dst-parent", true, nil)
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	bus.reset()

	recursive := true
	if _, err := svc.Transfer(domain.TransferRequest{
		Op:                 "move",
		SourceID:           src.ID,
		TargetParentID:     dstParent.ID,
		TargetName:         "moved",
		OnConflict:         domain.ConflictError,
		Ownership:          &domain.Ownership{Mode: "640"},
		RecursiveOwnership: &recursive,
	}); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	moved := filterByType(bus.snapshot(), domain.EventMoved)
	if len(moved) != 1 {
		t.Fatalf("expected exactly 1 EventMoved, got %d: %#v", len(moved), moved)
	}
	if moved[0].ID != src.ID {
		t.Fatalf("EventMoved.ID = %s, want %s", moved[0].ID, src.ID)
	}
	// And — the per-descendant noise guard. No more than a handful total
	// events even though the subtree has 5 children.
	if total := len(bus.snapshot()); total > 4 {
		t.Fatalf("Transfer move produced %d events (per-descendant emission regressed?): %#v",
			total, bus.snapshot())
	}
}

// TestSyncSinglePublishesEventUpdated pins the existing per-file emission so
// it isn't accidentally lost during future refactors.
func TestSyncSinglePublishesEventUpdated(t *testing.T) {
	svc, bus, cleanup := newServiceWithRecordedBus(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	file, err := svc.CreateChild(root.ID, "x.txt", false, nil)
	if err != nil {
		t.Fatalf("mkfile: %v", err)
	}
	bus.reset()

	if err := svc.WriteContent(file.ID, strings.NewReader("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	updates := filterByType(bus.snapshot(), domain.EventUpdated)
	if len(updates) == 0 {
		t.Fatalf("WriteContent produced no EventUpdated")
	}
	found := false
	for _, e := range updates {
		if e.ID == file.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no EventUpdated for file ID %s, got: %#v", file.ID, updates)
	}
}

func filterByType(events []domain.Event, t domain.EventType) []domain.Event {
	out := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func itoaLocal(i int) string {
	s := ""
	if i == 0 {
		return "0"
	}
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// Compile-time guard: keep these event types referenced so the recorded-bus
// helpers in this file fail to build if the type is renamed.
var _ = []domain.EventType{
	domain.EventCreated, domain.EventUpdated, domain.EventDeleted,
	domain.EventMoved, domain.EventScanned,
}

// Make sure the test suite touches `time` so a future minor change to imports
// doesn't accidentally break the Linux test build.
var _ = time.Now
