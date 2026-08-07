//go:build linux

package domain_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// realBTRFSSubvol carves a fresh subvolume out of the configured btrfs
// root and registers cleanup. Skipped unless the FILEGATE_BTRFS_REAL
// gate is set — same convention as the detector real-btrfs tests.
func realBTRFSSubvol(t *testing.T) string {
	t.Helper()
	if os.Getenv("FILEGATE_BTRFS_REAL") != "1" {
		t.Skip("set FILEGATE_BTRFS_REAL=1 to run real btrfs versioning test")
	}
	root := strings.TrimSpace(os.Getenv("FILEGATE_BTRFS_REAL_ROOT"))
	if root == "" {
		t.Skip("FILEGATE_BTRFS_REAL_ROOT is required")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs command not found")
	}
	subvol := filepath.Join(root, fmt.Sprintf("filegate-versioning-%d", time.Now().UnixNano()))
	if out, err := exec.Command("btrfs", "subvolume", "create", subvol).CombinedOutput(); err != nil {
		t.Skipf("subvol create %q failed: %v (%s)", subvol, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_, _ = exec.Command("btrfs", "subvolume", "delete", subvol).CombinedOutput()
		_ = os.RemoveAll(subvol)
	})
	return subvol
}

func newServiceOnRealBTRFS(t *testing.T, subvol string, cfg domain.VersioningConfig) (*domain.Service, func()) {
	t.Helper()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	svc, err := domain.NewService(idx, filesystem.New(), eventbus.New(), []string{subvol}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	// NOTE: do NOT default PinnedGraceAfterDelete to non-zero — the
	// orphan-flow test passes 0 to mean "purge on the next pass".
	if cfg.MaxLabelBytes == 0 {
		cfg.MaxLabelBytes = 2048
	}
	if cfg.MaxPinnedPerFile == 0 {
		cfg.MaxPinnedPerFile = 100
	}
	svc.EnableVersioning(cfg, true)
	return svc, func() { _ = idx.Close() }
}

// TestVersioningEndToEndOverRealBTRFS exercises the full feature stack
// (capture, list, restore, snapshot, pin, unpin, prune) against a real
// btrfs filesystem so the FICLONE reflink path is the one being used —
// not the in-process copy fallback that runs in tmpfs CI.
func TestVersioningEndToEndOverRealBTRFS(t *testing.T) {
	subvol := realBTRFSSubvol(t)
	svc, cleanup := newServiceOnRealBTRFS(t, subvol, domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	rootName := root.Name

	// 1. Create a file with V1 captured automatically.
	const v1Content = "version-one-content"
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/journey.bin",
		strings.NewReader(v1Content),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	v1List, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(v1List.Items) != 1 || v1List.Items[0].Size != int64(len(v1Content)) {
		t.Fatalf("V1 not captured correctly: %#v", v1List.Items)
	}

	// 2. Wait past cooldown, overwrite, V2 should auto-capture the OLD bytes.
	time.Sleep(80 * time.Millisecond)
	const v2Content = "version-two-different-content"
	if err := svc.WriteContent(meta.ID, strings.NewReader(v2Content)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	v2List, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(v2List.Items) < 2 {
		t.Fatalf("expected >= 2 versions after overwrite, got %d", len(v2List.Items))
	}

	// 3. Manual snapshot ignores cooldown; produces a pinned version.
	snap, err := svc.SnapshotVersion(meta.ID, "manual-checkpoint")
	if err != nil {
		t.Fatalf("SnapshotVersion: %v", err)
	}
	if !snap.Pinned || snap.Label != "manual-checkpoint" {
		t.Fatalf("snapshot not pinned/labeled: %#v", snap)
	}

	// 4. Restore in-place to V1: the live file should hold v1Content; the
	//    pre-restore state (v2Content) should be captured as an
	//    additional version.
	target := v1List.Items[0]
	postSnapList, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	beforeRestoreCount := len(postSnapList.Items)

	time.Sleep(80 * time.Millisecond)
	restored, asNew, err := svc.RestoreVersion(meta.ID, target.VersionID, domain.RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreVersion in-place: %v", err)
	}
	if asNew {
		t.Fatalf("expected in-place restore, got AsNew=true")
	}
	if restored.ID != meta.ID {
		t.Fatalf("in-place restore changed ID: got %s, want %s", restored.ID, meta.ID)
	}
	abs, _ := svc.ResolveAbsPath(meta.ID)
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != v1Content {
		t.Fatalf("restored content=%q, want %q", got, v1Content)
	}
	postRestoreList, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(postRestoreList.Items) <= beforeRestoreCount {
		t.Fatalf("restore did not snapshot pre-restore state: before=%d after=%d",
			beforeRestoreCount, len(postRestoreList.Items))
	}

	// 5. Restore the same target as a NEW file; verify naming + content.
	newMeta, asNew, err := svc.RestoreVersion(meta.ID, target.VersionID, domain.RestoreOptions{
		AsNewFile: true,
	})
	if err != nil {
		t.Fatalf("RestoreVersion as-new: %v", err)
	}
	if !asNew {
		t.Fatalf("expected AsNew=true")
	}
	if newMeta.Name != "journey-restored.bin" {
		t.Fatalf("as-new name=%q, want journey-restored.bin", newMeta.Name)
	}
	if newMeta.ID == meta.ID {
		t.Fatalf("as-new restore reused source ID")
	}
	newAbs, _ := svc.ResolveAbsPath(newMeta.ID)
	gotNew, _ := os.ReadFile(newAbs)
	if string(gotNew) != v1Content {
		t.Fatalf("as-new file content=%q, want %q", gotNew, v1Content)
	}

	// 6. Verify all version blobs live INSIDE the btrfs subvol's
	//    .fg-versions directory. Blob placement matters because
	//    reflinks require an intra-fs source/dest.
	versionsDir := filepath.Join(subvol, ".fg-versions")
	if _, err := os.Stat(versionsDir); err != nil {
		t.Fatalf("expected .fg-versions/ on btrfs mount: %v", err)
	}

	// 7. Pin -> unpin lifecycle on an existing version.
	pinTarget := postRestoreList.Items[0]
	label := "pinned-via-test"
	pinned, err := svc.PinVersion(meta.ID, pinTarget.VersionID, &label)
	if err != nil {
		t.Fatalf("PinVersion: %v", err)
	}
	if !pinned.Pinned || pinned.Label != label {
		t.Fatalf("PinVersion result wrong: %#v", pinned)
	}
	unpinned, err := svc.UnpinVersion(meta.ID, pinTarget.VersionID)
	if err != nil {
		t.Fatalf("UnpinVersion: %v", err)
	}
	if unpinned.Pinned {
		t.Fatalf("UnpinVersion did not clear Pinned: %#v", unpinned)
	}
}

// TestVersioningPrunerAndOrphanFlowOverRealBTRFS pins the
// snapshot-then-delete-then-prune lifecycle against the real reflink
// machinery.
func TestVersioningPrunerAndOrphanFlowOverRealBTRFS(t *testing.T) {
	subvol := realBTRFSSubvol(t)
	svc, cleanup := newServiceOnRealBTRFS(t, subvol, domain.VersioningConfig{
		Cooldown:               50 * time.Millisecond,
		MinSizeForAutoV1:       0,
		PinnedGraceAfterDelete: 0, // immediate orphan purge
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: time.Hour, MaxCount: 1}, // keep at most 1 live version
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	const path = "doomed.bin"
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/"+path,
		strings.NewReader("v1"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, content := range []string{"v2", "v3", "v4"} {
		time.Sleep(80 * time.Millisecond)
		if err := svc.WriteContent(meta.ID, strings.NewReader(content)); err != nil {
			t.Fatalf("overwrite %s: %v", content, err)
		}
	}
	beforePrune, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(beforePrune.Items) < 3 {
		t.Fatalf("expected >= 3 versions before prune, got %d", len(beforePrune.Items))
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions live: %v", err)
	}
	if stats.VersionsDeleted == 0 {
		t.Fatalf("pruner did not reduce live versions: %#v", stats)
	}

	afterPrune, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(afterPrune.Items) > 1 {
		t.Fatalf("after prune expected <=1 live version, got %d", len(afterPrune.Items))
	}

	// Now delete the source file → versions enter orphan state.
	if err := svc.Delete(meta.ID); err != nil {
		t.Fatalf("Delete file: %v", err)
	}
	stats, err = svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions orphan: %v", err)
	}
	if stats.OrphansPurged == 0 {
		t.Fatalf("orphan purge did not run: %#v", stats)
	}
}

// TestVersioningConcurrentSnapshotsOverRealBTRFS pins parallel snapshots
// on a btrfs file: each goroutine reflinks the same source bytes
// independently, the per-file lock keeps Pebble updates well-formed,
// and the pinned cap is respected.
func TestVersioningConcurrentSnapshotsOverRealBTRFS(t *testing.T) {
	subvol := realBTRFSSubvol(t)
	svc, cleanup := newServiceOnRealBTRFS(t, subvol, domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxPinnedPerFile: 8,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/concurrent.bin",
		strings.NewReader(strings.Repeat("X", 65536)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = svc.SnapshotVersion(meta.ID, fmt.Sprintf("worker-%d", i))
		}(i)
	}
	wg.Wait()

	listed, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	pinnedCount := 0
	for _, v := range listed.Items {
		if v.Pinned {
			pinnedCount++
		}
	}
	if pinnedCount > 8 {
		t.Fatalf("pinned cap violated: %d > 8", pinnedCount)
	}
}
