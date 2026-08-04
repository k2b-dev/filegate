//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
)

// TestVersioningSoak drives the versioning subsystem under sustained load
// and asserts that:
//
//   - No goroutine leaks on shutdown.
//   - The blob/Pebble accounting stays consistent (blob count == version
//     count after the pruner has caught up).
//   - The pruner respects the bucketed retention contract under
//     concurrent writes.
//   - Manual snapshots + pin/unpin work alongside auto-capture without
//     deadlock.
//
// Gated behind FILEGATE_VERSIONING_SOAK=1 so the standard test suite
// stays fast. Honours FILEGATE_VERSIONING_SOAK_DURATION (default 30s).
func TestVersioningSoak(t *testing.T) {
	if os.Getenv("FILEGATE_VERSIONING_SOAK") != "1" {
		t.Skip("set FILEGATE_VERSIONING_SOAK=1 to run versioning soak")
	}

	duration := 30 * time.Second
	if raw := strings.TrimSpace(os.Getenv("FILEGATE_VERSIONING_SOAK_DURATION")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			duration = d
		}
	}

	// Allow the test driver to point base storage at a real btrfs
	// mount. Without the override we use a fresh tmpfs t.TempDir(),
	// which exercises the copy-fallback path (no FICLONE). The
	// btrfs-real soak script sets FILEGATE_VERSIONING_SOAK_BASE_DIR
	// to a freshly-created subvolume to drive the reflink path under
	// load.
	var baseDir string
	if raw := strings.TrimSpace(os.Getenv("FILEGATE_VERSIONING_SOAK_BASE_DIR")); raw != "" {
		baseDir = raw
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatalf("ensure base dir %s: %v", baseDir, err)
		}
		// Clean any leftover .fg-versions from a previous run so the
		// final blob/metadata accounting starts from zero.
		_ = os.RemoveAll(filepath.Join(baseDir, ".fg-versions"))
	} else {
		baseDir = t.TempDir()
	}
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 32<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	bus := eventbus.New()
	t.Cleanup(func() { bus.Close() })
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 4096)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	cfg := domain.VersioningConfig{
		Cooldown:               40 * time.Millisecond,
		MinSizeForAutoV1:       0,
		MaxLabelBytes:          2048,
		MaxPinnedPerFile:       50,
		PinnedGraceAfterDelete: 1 * time.Second,
		PrunerInterval:         200 * time.Millisecond,
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: 5 * time.Second, MaxCount: 10},
			{KeepFor: time.Hour, MaxCount: 5},
		},
	}
	svc.EnableVersioning(cfg, true)

	root := svc.ListRoot()[0]
	rootName := root.Name

	// Bounded pool of files exercised concurrently. Keeping the pool
	// small ensures every file accumulates a meaningful version
	// history during the soak window. Override via env for stress
	// scenarios (e.g. 32-128 files for race-detector runs).
	filePool := 8
	if raw := strings.TrimSpace(os.Getenv("FILEGATE_VERSIONING_SOAK_FILE_POOL")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1024 {
			filePool = n
		}
	}
	type fileSlot struct {
		id   atomic.Pointer[domain.FileID]
		path string
	}
	pool := make([]*fileSlot, 0, filePool)
	for i := 0; i < filePool; i++ {
		path := fmt.Sprintf("soak-%03d.bin", i)
		meta, _, err := svc.WriteContentByVirtualPath(
			"/"+rootName+"/"+path,
			strings.NewReader(fmt.Sprintf("seed-%d", i)),
			domain.ConflictError,
		)
		if err != nil {
			t.Fatalf("seed file %d: %v", i, err)
		}
		slot := &fileSlot{path: path}
		id := meta.ID
		slot.id.Store(&id)
		pool = append(pool, slot)
	}

	// Capture goroutine baseline AFTER setup so the seed-write
	// goroutines and Pebble's startup goroutines don't show up as
	// "leaks" at the end. Capture twice with a small sleep so any
	// transient goroutine from the index init has settled.
	time.Sleep(50 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	prunerDone := make(chan struct{})
	go runVersioningPruner(ctx, svc, cfg.PrunerInterval, nil, &lifecycleState{}, prunerDone)

	var (
		writes    atomic.Int64
		snapshots atomic.Int64
		pins      atomic.Int64
		restores  atomic.Int64
		deletes   atomic.Int64
		recreates atomic.Int64
		// Error counters split by class so we can see which failure
		// dominates. ErrNotFound and ErrConflict are expected fallout
		// from the unsynchronised op-mix (stale slot.id after a
		// concurrent delete+recreate, or a recreate that lost the race
		// to another worker). errsOther is the bug-signal bucket — any
		// non-zero value means we hit something we don't understand.
		errsNotFound atomic.Int64
		errsConflict atomic.Int64
		errsOther    atomic.Int64
	)
	classify := func(err error) {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			errsNotFound.Add(1)
		case errors.Is(err, domain.ErrConflict):
			errsConflict.Add(1)
		default:
			errsOther.Add(1)
		}
	}

	// Worker count scales with the file pool so contention stays
	// meaningful at larger scales. Default 4; cap at 16 to avoid
	// overwhelming small CI runners.
	workers := 4
	if filePool > 32 {
		workers = 8
	}
	if filePool > 128 {
		workers = 16
	}

	// poolMu protects pool slot reassignment when the delete worker
	// recreates a file under the same path with a fresh ID. Workers
	// load the slot's id atomically, so no lock needed for the common
	// read path.
	var poolMu sync.RWMutex

	// Workers do a random mix of writes / snapshots / pin / unpin /
	// restore / delete+recreate. The delete+recreate branch exercises
	// the deleteSubtree orphan-mark path under load — previously this
	// op-mix only added versions, never tore down files.
	doneWorkers := make(chan struct{}, workers)
	for w := 0; w < workers; w++ {
		go func(seed int64) {
			defer func() { doneWorkers <- struct{}{} }()
			rnd := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				poolMu.RLock()
				slot := pool[rnd.Intn(len(pool))]
				poolMu.RUnlock()
				idPtr := slot.id.Load()
				if idPtr == nil {
					continue
				}
				slotID := *idPtr
				switch rnd.Intn(20) {
				case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10: // ~55% writes
					body := fmt.Sprintf("write-%d", rnd.Int63())
					if err := svc.WriteContent(slotID, strings.NewReader(body)); err != nil {
						classify(err)
						continue
					}
					writes.Add(1)
				case 11, 12, 13, 14: // 20% manual snapshots
					if _, err := svc.SnapshotVersion(slotID, "soak"); err != nil {
						classify(err)
						continue
					}
					snapshots.Add(1)
				case 15, 16: // 10% pin/unpin shuffle
					listed, err := svc.ListVersions(slotID, domain.VersionID{}, 100)
					if err != nil || len(listed.Items) == 0 {
						continue
					}
					target := listed.Items[rnd.Intn(len(listed.Items))]
					if rnd.Intn(2) == 0 {
						label := "soak-pin"
						if _, err := svc.PinVersion(slotID, target.VersionID, &label); err == nil {
							pins.Add(1)
						}
					} else {
						_, _ = svc.UnpinVersion(slotID, target.VersionID)
					}
				case 17, 18: // 10% restore in-place
					listed, err := svc.ListVersions(slotID, domain.VersionID{}, 100)
					if err != nil || len(listed.Items) == 0 {
						continue
					}
					target := listed.Items[rnd.Intn(len(listed.Items))]
					if _, _, err := svc.RestoreVersion(slotID, target.VersionID, domain.RestoreOptions{}); err != nil {
						classify(err)
						continue
					}
					restores.Add(1)
				case 19: // 5% delete + recreate same path
					// Concurrent deletes hit the orphan-mark path
					// (deleteSubtree's per-descendant lock) and also
					// expose any race between in-flight ops on slot.id.
					if err := svc.Delete(slotID); err != nil {
						classify(err)
						continue
					}
					deletes.Add(1)
					meta, _, err := svc.WriteContentByVirtualPath(
						"/"+rootName+"/"+slot.path,
						strings.NewReader("recreated"),
						domain.ConflictError,
					)
					if err != nil {
						classify(err)
						continue
					}
					newID := meta.ID
					slot.id.Store(&newID)
					recreates.Add(1)
				}
				time.Sleep(time.Duration(rnd.Intn(5)) * time.Millisecond)
			}
		}(int64(w + 1))
	}

	for i := 0; i < workers; i++ {
		<-doneWorkers
	}
	cancel()
	<-prunerDone

	// Run one final pruner pass synchronously so any blobs whose
	// metadata was just deleted in the last 200ms get removed before
	// we measure consistency. Without this, the assertion below
	// observes a window where metadata is gone but blob is still on
	// disk, giving false-positive "drift" warnings under heavy load.
	if _, err := svc.PruneVersions(); err != nil {
		t.Logf("final prune: %v", err)
	}

	// Final invariant check: blob count == metadata count for each
	// LIVE file. After delete+recreate ops the original file IDs are
	// orphans whose blobs may still be in the grace window — we walk
	// the .fg-versions tree directly to make sure even those are
	// accounted for.
	currentLiveIDs := make(map[domain.FileID]struct{}, len(pool))
	totalMeta := 0
	for _, slot := range pool {
		idPtr := slot.id.Load()
		if idPtr == nil {
			continue
		}
		currentLiveIDs[*idPtr] = struct{}{}
		listed, err := svc.ListVersions(*idPtr, domain.VersionID{}, 1000)
		if err != nil {
			t.Fatalf("ListVersions %s: %v", *idPtr, err)
		}
		totalMeta += len(listed.Items)
	}

	// Walk every per-file blob directory under .fg-versions and
	// count what's still on disk. For each file-id directory, the
	// blob count must not exceed the metadata count by more than a
	// few entries (the pruner's delete-metadata-then-delete-blob
	// pattern allows a small in-flight window).
	versionsRoot := filepath.Join(baseDir, ".fg-versions")
	totalBlobs := 0
	if entries, err := os.ReadDir(versionsRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			fid, err := domain.ParseFileID(e.Name())
			if err != nil {
				t.Logf("unexpected non-uuid entry under .fg-versions: %s", e.Name())
				continue
			}
			blobDir := filepath.Join(versionsRoot, e.Name())
			blobs, err := os.ReadDir(blobDir)
			if err != nil {
				t.Fatalf("read blob dir %s: %v", blobDir, err)
			}
			totalBlobs += len(blobs)

			meta, err := svc.ListVersions(fid, domain.VersionID{}, 1000)
			metaCount := 0
			if err == nil {
				metaCount = len(meta.Items)
			} else if !strings.Contains(err.Error(), "not found") {
				t.Fatalf("ListVersions for %s: %v", fid, err)
			}
			// "not found" is expected for a fully-pruned + deleted
			// file id whose blob dir hasn't been GC'd yet — that's
			// the expected steady state, not a bug. The drift check
			// against the on-disk blob count still applies.
			drift := len(blobs) - metaCount
			if drift > 5 {
				_, isLive := currentLiveIDs[fid]
				t.Fatalf("blob/metadata drift for %s (live=%v): blobs=%d meta=%d",
					fid, isLive, len(blobs), metaCount)
			}
		}
	}

	// Goroutine leak check: every worker + the pruner have signalled
	// done at this point. Allow some slack for runtime/Pebble
	// background goroutines that take a moment to settle.
	time.Sleep(100 * time.Millisecond)
	postGoroutines := runtime.NumGoroutine()
	if postGoroutines > baselineGoroutines+5 {
		t.Errorf("goroutine leak: baseline=%d post=%d (delta=%d)",
			baselineGoroutines, postGoroutines, postGoroutines-baselineGoroutines)
	}

	totalOps := writes.Load() + snapshots.Load() + pins.Load() + restores.Load() + deletes.Load() + recreates.Load()
	totalErrs := errsNotFound.Load() + errsConflict.Load() + errsOther.Load()
	t.Logf("soak summary: duration=%s pool=%d workers=%d writes=%d snapshots=%d pins=%d restores=%d deletes=%d recreates=%d errs=%d (notFound=%d conflict=%d other=%d) totalMeta=%d totalBlobs=%d goroutines=%d->%d",
		duration, filePool, workers, writes.Load(), snapshots.Load(), pins.Load(),
		restores.Load(), deletes.Load(), recreates.Load(),
		totalErrs, errsNotFound.Load(), errsConflict.Load(), errsOther.Load(),
		totalMeta, totalBlobs, baselineGoroutines, postGoroutines)

	// errsOther is the bug-signal bucket — anything other than
	// ErrNotFound/ErrConflict means we hit a failure mode we don't
	// expect from the unsynchronised op-mix. Allow a tiny absolute
	// floor (3) to absorb genuine flake (e.g. transient i/o), but
	// fail the test if the rate exceeds 0.5% of total ops.
	otherCount := errsOther.Load()
	if otherCount > 3 && otherCount*200 > totalOps {
		t.Errorf("unexpected error rate: errsOther=%d / totalOps=%d (>0.5%%) — investigate test logs",
			otherCount, totalOps)
	}

	// Bucket retention should have kept the per-file version count well
	// below the unconstrained "every write captures" upper bound. With
	// our buckets (10 + 5 = 15 max live) plus pinned (≤ 50) the
	// per-file ceiling is 65. Use a generous 80 to absorb scheduling
	// noise.
	for _, slot := range pool {
		idPtr := slot.id.Load()
		if idPtr == nil {
			continue
		}
		listed, err := svc.ListVersions(*idPtr, domain.VersionID{}, 1000)
		if err != nil {
			// Slot may have been deleted by a worker without
			// recreate completing; skip — the on-disk drift
			// check above already covers correctness.
			continue
		}
		if len(listed.Items) > 80 {
			t.Fatalf("file %s has %d versions — pruner did not bound retention",
				slot.path, len(listed.Items))
		}
	}
}
