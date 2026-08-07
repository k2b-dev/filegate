package pebble

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v3/domain"
)

func TestIndexCloseRaceNoPanic(t *testing.T) {
	idx, err := Open(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	id := domain.FileID(uuid.MustParse("019cb9ae-76c1-7807-ba50-cbb05a08ec6c"))
	putErr := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{
			ID:       id,
			ParentID: domain.FileID{},
			Name:     "x",
			IsDir:    false,
			Size:     1,
			Mtime:    time.Now().UnixMilli(),
			UID:      1000,
			GID:      1000,
			Mode:     0o644,
		})
		return nil
	})
	if putErr != nil {
		t.Fatalf("seed entity: %v", putErr)
	}

	const workers = 16
	const minOpsPerWorker = 50

	stop := make(chan struct{})
	var wg sync.WaitGroup
	panicCh := make(chan interface{}, 1)
	ops := make([]atomic.Int64, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					select {
					case panicCh <- rec:
					default:
					}
				}
			}()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = idx.GetEntity(id)
				ops[slot].Add(1)
			}
		}(w)
	}

	// Wait until every worker has actually run several iterations. This makes
	// sure Close races real in-flight reads instead of just a few that
	// happened to start instantly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := true
		for i := range ops {
			if ops[i].Load() < minOpsPerWorker {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workers did not warm up in time")
		}
	}

	_ = idx.Close()
	close(stop)
	wg.Wait()

	select {
	case rec := <-panicCh:
		t.Fatalf("unexpected panic during close race: %v", rec)
	default:
	}

	_, err = idx.GetEntity(id)
	if !errors.Is(err, ErrIndexClosed) && !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("get after close err=%v, want index closed/unavailable", err)
	}
}

// TestPanicInBatchIsRecoveredAsError pins two past failure modes of the
// panic-recovery path: (1) recoverIntoError called markFatal, which took
// i.mu for writing while the panicking goroutine still held the read
// lock — a self-deadlock on every recovered panic; (2) several methods
// recovered into a local variable instead of a named return slot, so the
// caller saw a nil error with zero-value results.
func TestPanicInBatchIsRecoveredAsError(t *testing.T) {
	idx, err := Open(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	err = idx.Batch(func(b domain.Batch) error { panic("injected") })
	if err == nil {
		t.Fatal("batch with panicking fn returned nil error")
	}
	if !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("batch err=%v, want ErrIndexUnavailable", err)
	}

	// The recovered panic must mark the whole index unavailable.
	id := domain.FileID(uuid.MustParse("019cb9ae-76c1-7807-ba50-cbb05a08ec6c"))
	if _, err := idx.GetEntity(id); !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("get after panic err=%v, want ErrIndexUnavailable", err)
	}
	if _, err := idx.ListChildren(domain.FileID{}, domain.ChildCursor{}, 10); !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("list after panic err=%v, want ErrIndexUnavailable", err)
	}
}

func TestIndexReturnsClosedForBatchAfterClose(t *testing.T) {
	idx, err := Open(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err = idx.Batch(func(b domain.Batch) error { return nil })
	if !errors.Is(err, ErrIndexClosed) {
		t.Fatalf("batch err=%v, want ErrIndexClosed", err)
	}
}
