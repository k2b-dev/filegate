//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
)

// setupRealBTRFSSubvol gates on FILEGATE_BTRFS_REAL=1 + FILEGATE_BTRFS_REAL_ROOT
// being set, then creates a fresh subvolume under the configured btrfs root and
// returns its path along with a cleanup function. Used by every real-btrfs
// edge-case test in this file so the gate logic stays in one place.
func setupRealBTRFSSubvol(t *testing.T) string {
	t.Helper()
	if os.Getenv("FILEGATE_BTRFS_REAL") != "1" {
		t.Skip("set FILEGATE_BTRFS_REAL=1 to run real btrfs test")
	}
	btrfsRoot := strings.TrimSpace(os.Getenv("FILEGATE_BTRFS_REAL_ROOT"))
	if btrfsRoot == "" {
		t.Skip("FILEGATE_BTRFS_REAL_ROOT is required")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs command not found")
	}

	subvol := filepath.Join(btrfsRoot, fmt.Sprintf("filegate-it-%d", time.Now().UnixNano()))
	if out, err := exec.Command("btrfs", "subvolume", "create", subvol).CombinedOutput(); err != nil {
		t.Skipf("cannot create btrfs subvolume %q: %v (%s)", subvol, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_, _ = exec.Command("btrfs", "subvolume", "delete", subvol).CombinedOutput()
		_ = os.RemoveAll(subvol)
	})
	return subvol
}

// startRealBTRFSDetector wires up a service rooted at the given subvol with a
// real btrfs detector and runs the consumer goroutine. Returns the service,
// the mount name for ResolvePath, and the eventbus so tests that need to
// observe domain events can subscribe. Cancellation is automatic via
// t.Cleanup.
func startRealBTRFSDetector(t *testing.T, subvol string) (*domain.Service, string, domain.EventBus) {
	t.Helper()
	idx, err := indexpebble.Open(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{subvol}, 20000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	rootName := mustMountNameByPath(t, svc, subvol)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runner, err := detect.New("btrfs", []string{subvol}, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("new btrfs detector: %v", err)
	}
	runner.Start(ctx)
	t.Cleanup(func() { runner.Close() })
	go consumeDetectorEvents(ctx, svc, runner.Events(), nil, 0)

	return svc, rootName, bus
}

// seedAndAwait writes payload to absPath and blocks until ResolvePath of
// virtualPath returns nil. Encapsulates the common "create file then
// wait for the index to catch up" pattern so individual tests don't
// repeat the same 8-line waitForResolveWithStimulus boilerplate.
//
// For tests that need a more elaborate condition (size match,
// multiple paths, custom stimulus), call waitForResolveWithStimulus
// directly instead.
func seedAndAwait(t *testing.T, svc *domain.Service, absPath, virtualPath string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(absPath, payload, 0o644); err != nil {
		t.Fatalf("seed %q: %v", absPath, err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(absPath, payload, 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(virtualPath)
		return err == nil
	})
}

// TestBTRFSRealBurstCreate verifies that 50 files written in a tight burst all
// reach the index. find-new reports many inodes per generation; this catches
// any batching/coalescing bug in the detector or consumer that would drop
// events under load.
func TestBTRFSRealBurstCreate(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const n = 50
	// Write a sentinel first to walk past the loopback-btrfs init race window.
	sentinel := filepath.Join(subvol, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel write: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(sentinel, []byte("s"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/sentinel.txt")
		return err == nil
	})

	for i := 0; i < n; i++ {
		p := filepath.Join(subvol, fmt.Sprintf("burst-%03d.txt", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("payload-%d", i)), 0o644); err != nil {
			t.Fatalf("burst write %d: %v", i, err)
		}
	}

	waitUntil(t, 30*time.Second, func() bool {
		for i := 0; i < n; i++ {
			if _, err := svc.ResolvePath(fmt.Sprintf("%s/burst-%03d.txt", rootName, i)); err != nil {
				return false
			}
		}
		return true
	})
}

// TestBTRFSRealRenameWithinSubvol verifies that renaming a file within the
// same subvolume is reflected in the index: the old path becomes unresolvable
// and the new path resolves. Btrfs keeps the same inode across an in-subvol
// rename, so this exercises the inode-based reconciliation in
// service.reconcileByInode plus PutEntity's stale-child cleanup. Without
// either, the index would happily index the new path while leaving the old
// one as a stale lookup target.
func TestBTRFSRealRenameWithinSubvol(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	src := filepath.Join(subvol, "rename-src.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(src, []byte("hello"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/rename-src.txt")
		return err == nil
	})

	dst := filepath.Join(subvol, "rename-dst.txt")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Rename must produce both effects: new path appears, old path disappears.
	// We poll on each independently so the failure message identifies which side
	// regressed.
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/rename-dst.txt")
		return err == nil
	})
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/rename-src.txt")
		return err == domain.ErrNotFound
	})
}

// TestBTRFSRealNestedDirectoryCreate verifies that the detector traverses
// newly-created subdirectories rather than only emitting events for top-level
// inodes. Mirrors a common real-world pattern (extracting an archive, etc.).
func TestBTRFSRealNestedDirectoryCreate(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	// Sentinel to bypass loopback init race before exercising the nested case.
	sentinel := filepath.Join(subvol, "nested-sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel write: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(sentinel, []byte("s"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/nested-sentinel.txt")
		return err == nil
	})

	deep := filepath.Join(subvol, "level1", "level2", "level3")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir -p: %v", err)
	}
	leaves := []string{"a.txt", "b.txt", "c.txt"}
	for _, name := range leaves {
		if err := os.WriteFile(filepath.Join(deep, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	waitUntil(t, 20*time.Second, func() bool {
		if _, err := svc.ResolvePath(rootName + "/level1/level2/level3"); err != nil {
			return false
		}
		for _, name := range leaves {
			if _, err := svc.ResolvePath(rootName + "/level1/level2/level3/" + name); err != nil {
				return false
			}
		}
		return true
	})
}

// TestBTRFSRealBulkDelete verifies that deleting many files is reflected in the
// index. find-new does not report deletes directly (the inode is gone before
// the next generation scan); the detector must rely on a different mechanism
// (parent-dir generation change + rescan, or stat-based reconciliation) to
// notice. If this test fails, it documents a real gap rather than a flake.
func TestBTRFSRealBulkDelete(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const n = 20
	// Sentinel to bypass loopback init race; subsequent writes ride the warm
	// detector.
	sentinel := filepath.Join(subvol, "bulk-sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel write: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(sentinel, []byte("s"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/bulk-sentinel.txt")
		return err == nil
	})

	for i := 0; i < n; i++ {
		p := filepath.Join(subvol, fmt.Sprintf("bulk-%02d.txt", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	waitUntil(t, 20*time.Second, func() bool {
		for i := 0; i < n; i++ {
			if _, err := svc.ResolvePath(fmt.Sprintf("%s/bulk-%02d.txt", rootName, i)); err != nil {
				return false
			}
		}
		return true
	})

	for i := 0; i < n; i++ {
		p := filepath.Join(subvol, fmt.Sprintf("bulk-%02d.txt", i))
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
	}
	waitUntil(t, 20*time.Second, func() bool {
		for i := 0; i < n; i++ {
			_, err := svc.ResolvePath(fmt.Sprintf("%s/bulk-%02d.txt", rootName, i))
			if err != domain.ErrNotFound {
				return false
			}
		}
		return true
	})
}

// TestBTRFSRealXattrSelfLoopGuard verifies that filegate's own
// `user.filegate.id` xattr write does not feed back into the btrfs detector
// and trigger an endless cycle of re-syncs. After initial detection settles,
// any further EventUpdated traffic for the same path indicates a feedback
// loop. We measure activity over a fixed quiet window — a sleep is the
// honest tool here, since we are explicitly waiting on the absence of events.
func TestBTRFSRealXattrSelfLoopGuard(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, bus := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "xattr-loop.txt")
	if err := os.WriteFile(target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(target, []byte("payload"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/xattr-loop.txt")
		return err == nil
	})

	// Settle window: let any in-flight detector activity from the initial
	// detection drain before we start counting.
	time.Sleep(500 * time.Millisecond)

	var updates atomic.Int64
	bus.Subscribe(domain.EventUpdated, func(ev domain.Event) {
		// Only count events for our target path; ignore unrelated traffic so a
		// hot subvol root sync doesn't poison the count.
		if strings.Contains(ev.Path, "xattr-loop.txt") {
			updates.Add(1)
		}
	})

	// Quiet window: if the xattr write fed back into find-new we would see a
	// steady stream of events here. 2s is enough to span several detector ticks
	// at 40ms interval (~50 ticks).
	time.Sleep(2 * time.Second)

	got := updates.Load()
	if got > 2 {
		t.Fatalf("xattr self-loop suspected: %d EventUpdated for unchanged file in 2s quiet window", got)
	}
}

// TestBTRFSRealRenameAcrossDirectories covers the cross-directory case for
// inode-based reconciliation: a file moves from dir-a/ to dir-b/ within the
// same subvolume. This is structurally distinct from the same-directory
// rename because it exercises both stale-child cleanup AND the inode
// secondary index lookup (the candidate path is under a completely
// different parent).
func TestBTRFSRealRenameAcrossDirectories(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dirA := filepath.Join(subvol, "dir-a")
	dirB := filepath.Join(subvol, "dir-b")
	if err := os.Mkdir(dirA, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.Mkdir(dirB, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}

	src := filepath.Join(dirA, "x.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(src, []byte("payload"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/dir-a/x.txt")
		return err == nil
	})

	dst := filepath.Join(dirB, "x.txt")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename across dirs: %v", err)
	}

	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/dir-b/x.txt")
		return err == nil
	})
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/dir-a/x.txt")
		return err == domain.ErrNotFound
	})
}

// TestBTRFSRealHardLinkLeavesBothPathsValid verifies the safety guard in
// reconcileByInode: when the same inode is referenced by multiple paths
// (nlink > 1), the reconciler must not invalidate either of them. Without
// the nlink>1 short-circuit, the detector seeing one of the two paths
// would happily delete the other.
//
// Note on detection: btrfs `find-new` is generation-based, not directory-
// entry-based, so creating a hard link via link(2) does not bump the inode
// generation and is therefore invisible to the detector. We work around
// this by triggering a manual Rescan after the link, which walks the
// filesystem and indexes both names. Once both paths sit in the index,
// the reconciler's behavior is the actual thing under test.
func TestBTRFSRealHardLinkLeavesBothPathsValid(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	primary := filepath.Join(subvol, "hl-primary.txt")
	if err := os.WriteFile(primary, []byte("shared"), 0o644); err != nil {
		t.Fatalf("write primary: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(primary, []byte("shared"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/hl-primary.txt")
		return err == nil
	})

	alias := filepath.Join(subvol, "hl-alias.txt")
	if err := os.Link(primary, alias); err != nil {
		t.Fatalf("hard link: %v", err)
	}

	// link(2) does not bump btrfs generation; force a rescan so both names
	// land in the index. Without this, only the primary path is indexed
	// and the test couldn't tell apart "alias was never indexed" from
	// "alias was indexed and then wrongly removed".
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		_, errA := svc.ResolvePath(rootName + "/hl-primary.txt")
		_, errB := svc.ResolvePath(rootName + "/hl-alias.txt")
		return errA == nil && errB == nil
	})

	// Now bump the primary so the detector sees the (still-shared) inode
	// again. A buggy reconciler with no nlink check would treat one of the
	// two indexed paths as stale and drop it.
	if err := os.WriteFile(primary, []byte("touch"), 0o644); err != nil {
		t.Fatalf("touch primary: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/hl-primary.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == int64(len("touch"))
	})

	// Both paths must still resolve.
	if _, err := svc.ResolvePath(rootName + "/hl-primary.txt"); err != nil {
		t.Fatalf("primary disappeared after touch: %v", err)
	}
	if _, err := svc.ResolvePath(rootName + "/hl-alias.txt"); err != nil {
		t.Fatalf("alias was wrongly invalidated by reconciler: %v", err)
	}
}

// TestBTRFSRealInodeReuseDoesNotResurrectDeletedEntry verifies that when a
// file is deleted and a new file later happens to land on the same inode
// number, the reconciler does the right thing: the old entity is gone, the
// new entity is fresh, and there's no cross-talk via the secondary inode
// mapping. This is a high-volatility scenario on btrfs (where inode reuse
// can happen relatively quickly under churn).
func TestBTRFSRealInodeReuseDoesNotResurrectDeletedEntry(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	first := filepath.Join(subvol, "reuse-1.txt")
	if err := os.WriteFile(first, []byte("first"), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(first, []byte("first"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/reuse-1.txt")
		return err == nil
	})

	if err := os.Remove(first); err != nil {
		t.Fatalf("remove first: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/reuse-1.txt")
		return err == domain.ErrNotFound
	})

	// Create a fresh file at a different path. If btrfs gives it the same
	// inode as the deleted one, the secondary-inode index must not have a
	// stale entry pointing back at "reuse-1.txt".
	second := filepath.Join(subvol, "reuse-2.txt")
	if err := os.WriteFile(second, []byte("second"), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(second, []byte("second"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/reuse-2.txt")
		return err == nil
	})

	// Final guarantee: the deleted path stays unresolvable, even if its
	// former inode now backs the new file.
	if _, err := svc.ResolvePath(rootName + "/reuse-1.txt"); err != domain.ErrNotFound {
		t.Fatalf("deleted path resurrected via inode reuse: err=%v", err)
	}
}
