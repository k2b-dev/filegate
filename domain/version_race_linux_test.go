//go:build linux

package domain_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func versioningRaceService(t *testing.T, cfg domain.VersioningConfig) (*domain.Service, func()) {
	t.Helper()
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 50 * time.Millisecond
	}
	if cfg.MaxLabelBytes == 0 {
		cfg.MaxLabelBytes = 2048
	}
	if cfg.MaxPinnedPerFile == 0 {
		cfg.MaxPinnedPerFile = 1000
	}
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("svc: %v", err)
	}
	svc.EnableVersioning(cfg, true)
	return svc, func() {
		bus.Close()
		_ = idx.Close()
	}
}

// TestDeleteSubtreeMarksConcurrentChildSnapshotOrphan pins the
// per-descendant lock contract added in commit fdfbcc9. A directory
// delete + an aggressive concurrent SnapshotVersion(child) must NOT
// leave any version of the deleted child with DeletedAt=0; otherwise
// the orphan-grace policy doesn't apply and the blob leaks forever.
//
// Strategy: hammer SnapshotVersion(child) from N goroutines while a
// single Delete(parent_dir) runs on the main goroutine. After the
// dust settles, every version of the child must be marked orphan.
func TestDeleteSubtreeMarksConcurrentChildSnapshotOrphan(t *testing.T) {
	svc, cleanup := versioningRaceService(t, domain.VersioningConfig{
		PinnedGraceAfterDelete: 24 * time.Hour, // long enough that no purge runs
		MinSizeForAutoV1:       0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	dir, err := svc.CreateChild(root.ID, "race-dir", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	child, err := svc.CreateChild(dir.ID, "child.bin", false, nil)
	if err != nil {
		t.Fatalf("mkfile: %v", err)
	}
	if err := svc.WriteContent(child.ID, strings.NewReader("seed-bytes-for-race")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	stop := atomic.Bool{}
	var wg sync.WaitGroup
	const workers = 8
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for !stop.Load() {
				// Errors are expected once the file is deleted —
				// we're stress-testing the lock, not asserting
				// per-call success.
				_, _ = svc.SnapshotVersion(child.ID, "race")
			}
		}()
	}

	// Let the workers ramp up and start landing snapshots, then delete.
	time.Sleep(20 * time.Millisecond)
	if err := svc.Delete(dir.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stop.Store(true)
	wg.Wait()

	// Every version of the deleted child MUST be orphan-marked.
	listed, err := svc.ListVersions(child.ID, domain.VersionID{}, 1000)
	if err != nil {
		t.Fatalf("ListVersions after delete: %v", err)
	}
	if len(listed.Items) == 0 {
		t.Fatalf("expected at least the seed/snap versions to remain")
	}
	for _, v := range listed.Items {
		if v.DeletedAt == 0 {
			t.Fatalf("version %s slipped past orphan-mark (DeletedAt=0): %#v", v.VersionID, v)
		}
	}
}

// TestPrunerReFetchesPinnedStateBeforeDeleting pins the
// pruner-vs-pin race fix. The pruner's ForEachFileVersions sees an
// unpinned version, then a Pin lands, then the pruner makes its
// decision. Without the lock + re-fetch, the pruner would still
// see the stale unpinned snapshot and delete the blob — leaving a
// pinned metadata row whose content is 404. The pinned version
// MUST survive prune.
func TestPrunerReFetchesPinnedStateBeforeDeleting(t *testing.T) {
	svc, cleanup := versioningRaceService(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
		// Aggressive policy: delete everything older than 1ns.
		// Without the fetch-inside-lock, the auto-V1 captured at
		// create time would be deleted before we get a chance to
		// pin it.
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: time.Nanosecond, MaxCount: 0},
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/save.bin",
		strings.NewReader("save-this"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	listed, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("V1 not captured")
	}
	target := listed.Items[0]

	// Race: 50 iterations of "pin then immediately prune". The pin
	// happens before the pruner acquires its per-file lock, so the
	// pruner's in-lock re-fetch must see the pinned state and skip
	// the version. Without the re-fetch, the version would be
	// selected for deletion based on the pre-pin snapshot from
	// ForEachFileVersions.
	for i := 0; i < 50; i++ {
		// Re-pin (idempotent) so we always start from a known state.
		label := "race-pin"
		if _, err := svc.PinVersion(meta.ID, target.VersionID, &label); err != nil {
			t.Fatalf("pin iter %d: %v", i, err)
		}
		if _, err := svc.PruneVersions(); err != nil {
			t.Fatalf("prune iter %d: %v", i, err)
		}
		// The pinned version must still be present and pinned.
		survivors, err := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
		if err != nil {
			t.Fatalf("ListVersions iter %d: %v", i, err)
		}
		var found *domain.VersionMeta
		for j := range survivors.Items {
			if survivors.Items[j].VersionID == target.VersionID {
				found = &survivors.Items[j]
				break
			}
		}
		if found == nil {
			t.Fatalf("iter %d: pruner deleted pinned version", i)
		}
		if !found.Pinned {
			t.Fatalf("iter %d: pinned flag lost: %#v", i, found)
		}
	}
}
