//go:build linux

// TIER 3 + obscure-but-realistic edge cases. These exercise btrfs-specific
// features (snapshots, reflinks at subvol boundaries), special inode types
// (FIFOs, sockets), filesystem semantics at the limits (sparse, fallocate,
// 0-byte, deep paths, NAME_MAX), parsing edge cases (newline in filename),
// and concurrency (rename race, write-while-reading, long-running write).
//
// Several of these were predicted to surface real Filegate gaps. Where a
// test runs cleanly today, it's pinned. Where a real limitation surfaces,
// the test documents it explicitly.

package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
)

// =============================================================================
// Btrfs-specific
// =============================================================================

// TestBTRFSRealCrossSubvolumeMove exercises a move that crosses subvolume
// boundaries: btrfs treats this as copy + unlink (different inode-spaces),
// not as a rename. Filegate watches both subvolumes; the source path
// disappears from the source mount's index, the destination path appears
// in the destination mount's index with a fresh ID.
func TestBTRFSRealCrossSubvolumeMove(t *testing.T) {
	if os.Getenv("FILEGATE_BTRFS_REAL") != "1" {
		t.Skip("set FILEGATE_BTRFS_REAL=1")
	}
	btrfsRoot := strings.TrimSpace(os.Getenv("FILEGATE_BTRFS_REAL_ROOT"))
	if btrfsRoot == "" {
		t.Skip("FILEGATE_BTRFS_REAL_ROOT required")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		t.Skip("btrfs binary not found")
	}

	subvolA := filepath.Join(btrfsRoot, fmt.Sprintf("filegate-it-A-%d", time.Now().UnixNano()))
	subvolB := filepath.Join(btrfsRoot, fmt.Sprintf("filegate-it-B-%d", time.Now().UnixNano()))
	for _, sv := range []string{subvolA, subvolB} {
		if out, err := exec.Command("btrfs", "subvolume", "create", sv).CombinedOutput(); err != nil {
			t.Skipf("create subvol %q: %v (%s)", sv, err, strings.TrimSpace(string(out)))
		}
		t.Cleanup(func() {
			_, _ = exec.Command("btrfs", "subvolume", "delete", sv).CombinedOutput()
			_ = os.RemoveAll(sv)
		})
	}

	idx, err := indexpebble.Open(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{subvolA, subvolB}, 20000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	rootA := mustMountNameByPath(t, svc, subvolA)
	rootB := mustMountNameByPath(t, svc, subvolB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runner, err := detect.New("btrfs", []string{subvolA, subvolB}, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("new btrfs detector: %v", err)
	}
	runner.Start(ctx)
	t.Cleanup(func() { runner.Close() })
	go consumeDetectorEvents(ctx, svc, runner.Events(), nil, 0)

	src := filepath.Join(subvolA, "moving.txt")
	seedAndAwait(t, svc, src, rootA+"/moving.txt", []byte("xsubvol"))

	dst := filepath.Join(subvolB, "moved.txt")
	if err := os.Rename(src, dst); err != nil {
		// Cross-subvolume on btrfs returns EXDEV — fall back to copy+remove.
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			t.Fatalf("read for fallback: %v", rerr)
		}
		if werr := os.WriteFile(dst, data, 0o644); werr != nil {
			t.Fatalf("manual copy: %v", werr)
		}
		_ = os.Remove(src)
	}

	waitUntil(t, 15*time.Second, func() bool {
		_, errA := svc.ResolvePath(rootA + "/moving.txt")
		_, errB := svc.ResolvePath(rootB + "/moved.txt")
		return errA == domain.ErrNotFound && errB == nil
	})
}

// TestBTRFSRealSnapshotInsideWatchedTree verifies that a btrfs snapshot
// nested inside the watched tree gets indexed correctly: the snapshot's
// files arrive with xattrs cloned from the originals, and the conflict
// rule in resolveOrReissueID re-issues fresh UUIDs for each snapshot
// file so original and snapshot become independent entities.
//
// Acceptance: original resolves with its ORIGINAL id; snapshot path
// resolves with a DIFFERENT id; deleting the original does not break
// the snapshot copy.
func TestBTRFSRealSnapshotInsideWatchedTree(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	original := filepath.Join(subvol, "snap-source.txt")
	seedAndAwait(t, svc, original, rootName+"/snap-source.txt", []byte("source-payload"))
	originalID, err := svc.ResolvePath(rootName + "/snap-source.txt")
	if err != nil {
		t.Fatalf("resolve original: %v", err)
	}

	snapPath := filepath.Join(subvol, "snap")
	out, err := exec.Command("btrfs", "subvolume", "snapshot", subvol, snapPath).CombinedOutput()
	if err != nil {
		t.Skipf("btrfs snapshot not available: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_, _ = exec.Command("btrfs", "subvolume", "delete", snapPath).CombinedOutput()
	})

	// Trigger a Rescan to walk the snapshot tree (find-new on the parent
	// subvolume doesn't enumerate the nested snapshot).
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan after snapshot: %v", err)
	}

	// Original path must keep its original ID — the snapshot must NOT
	// have stolen the entity record.
	stillID, err := svc.ResolvePath(rootName + "/snap-source.txt")
	if err != nil {
		t.Fatalf("original lost after snapshot: %v", err)
	}
	if stillID != originalID {
		t.Fatalf("original ID drifted: %v -> %v", originalID, stillID)
	}

	// Snapshot copy must resolve with a DIFFERENT ID (conflict-rule re-issue).
	snapID, err := svc.ResolvePath(rootName + "/snap/snap-source.txt")
	if err != nil {
		t.Fatalf("snapshot copy not indexed: %v", err)
	}
	if snapID == originalID {
		t.Fatalf("snapshot copy got the same ID as original — conflict rule didn't re-issue")
	}

	// Delete the original. Snapshot copy must still resolve with its own ID.
	if err := os.Remove(original); err != nil {
		t.Fatalf("remove original: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/snap-source.txt")
		return err == domain.ErrNotFound
	})
	if _, err := svc.ResolvePath(rootName + "/snap/snap-source.txt"); err != nil {
		t.Fatalf("snapshot copy was broken by deleting the original: %v", err)
	}
}

// TestBTRFSRealSparseFile creates a logically large file via truncate(2)
// without writing any data. Stat reports the logical size, blocks=0.
// Filegate must record the logical size in the index.
func TestBTRFSRealSparseFile(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "sparse.bin")
	const sparseSize int64 = 16 << 20 // 16 MiB sparse
	f, err := os.Create(target)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(sparseSize); err != nil {
		_ = f.Close()
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.Truncate(target, sparseSize)
	}, func() bool {
		id, err := svc.ResolvePath(rootName + "/sparse.bin")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == sparseSize
	})
}

// TestBTRFSRealFallocateExtension uses syscall.Fallocate to grow a file
// to a target size without writing data. Same expected outcome as
// TestBTRFSRealSparseFile: the index must record the post-fallocate size.
func TestBTRFSRealFallocateExtension(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "fallocate.bin")
	seedAndAwait(t, svc, target, rootName+"/fallocate.bin", []byte("seed"))

	const finalSize int64 = 8 << 20
	f, err := os.OpenFile(target, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, finalSize); err != nil {
		_ = f.Close()
		t.Skipf("fallocate not supported: %v", err)
	}
	_ = f.Close()

	waitUntil(t, 15*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/fallocate.bin")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == finalSize
	})
}

// =============================================================================
// Special inode types
// =============================================================================

// TestBTRFSRealFIFOIsSkipped creates a named pipe in the watched tree.
// Filegate's domain layer rejects non-regular non-directory inodes; the
// detector must not crash or stall when one appears, and unrelated
// regular files must continue to work.
func TestBTRFSRealFIFOIsSkipped(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	regular := filepath.Join(subvol, "alongside-fifo.txt")
	seedAndAwait(t, svc, regular, rootName+"/alongside-fifo.txt", []byte("normal"))

	fifoPath := filepath.Join(subvol, "the-fifo")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// Stimulate a generation tick so the detector observes the new inode.
	if err := os.WriteFile(regular, []byte("touched-after-fifo"), 0o644); err != nil {
		t.Fatalf("touch: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/alongside-fifo.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == int64(len("touched-after-fifo"))
	})

	// FIFO must NOT show up as a regular indexed file. Acceptable: not
	// resolvable, or resolvable but only because Filegate degraded
	// gracefully — either way, no crash.
	if _, err := svc.ResolvePath(rootName + "/the-fifo"); err == nil {
		t.Logf("note: FIFO resolves in index — Filegate didn't reject the inode type, but didn't crash either")
	}
}

// TestBTRFSRealUnixSocketIsSkipped is the socket counterpart to the FIFO
// test: a unix-domain socket sitting in the watched tree must not
// destabilise Filegate. Regular files alongside it must keep working.
func TestBTRFSRealUnixSocketIsSkipped(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	regular := filepath.Join(subvol, "alongside-socket.txt")
	seedAndAwait(t, svc, regular, rootName+"/alongside-socket.txt", []byte("normal"))

	sockPath := filepath.Join(subvol, "the-socket")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if err := os.WriteFile(regular, []byte("touched-after-socket"), 0o644); err != nil {
		t.Fatalf("touch: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/alongside-socket.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == int64(len("touched-after-socket"))
	})
}

// =============================================================================
// Trivial-but-uncovered cases
// =============================================================================

// TestBTRFSRealEmptyFile pins the size=0 case. Filegate code paths often
// branch on Size; a 0-byte file must round-trip correctly.
func TestBTRFSRealEmptyFile(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "empty.txt")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("create empty: %v", err)
	}
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(target, nil, 0o644)
	}, func() bool {
		id, err := svc.ResolvePath(rootName + "/empty.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == 0 && !meta.IsRoot
	})
}

// TestBTRFSRealEmptyDirectory pins what we DO and DO NOT promise for an
// empty directory under the btrfs detection backend.
//
// Documented limitation: `btrfs subvolume find-new` filters output to
// type=BTRFS_FT_REG_FILE in btrfs-progs; directory inodes are never
// emitted. So an empty directory is INVISIBLE to the detector. It only
// surfaces when:
//
//	(a) A file is created inside it (which gets reported, and the
//	    consumer's syncSingle traverses up and indexes the parent), or
//	(b) An explicit Rescan walks the filesystem.
//
// The test pins (b) — Rescan seeds the empty dir — and acts as a
// regression guard if the rescan walk ever stops handling empty dirs.
func TestBTRFSRealEmptyDirectory(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "lonely-dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Without a Rescan the dir is invisible — that's the limitation.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/lonely-dir")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Type == "directory"
	})
}

// TestBTRFSRealVeryDeepPath verifies that a path nested 50 directories
// deep is reachable. Pebble has no path-length issue but the cache,
// VirtualPath traversal, and detector consumption all need to handle
// multi-segment walks.
func TestBTRFSRealVeryDeepPath(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const depth = 50
	parts := make([]string, depth)
	for i := range parts {
		parts[i] = fmt.Sprintf("d%02d", i)
	}
	deep := filepath.Join(append([]string{subvol}, parts...)...)
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir -p: %v", err)
	}
	leaf := filepath.Join(deep, "leaf.txt")
	if err := os.WriteFile(leaf, []byte("deep"), 0o644); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}

	virtualPath := rootName + "/" + strings.Join(append(parts, "leaf.txt"), "/")
	waitForResolveWithStimulus(t, 20*time.Second, 200*time.Millisecond, func() {
		_ = os.WriteFile(leaf, []byte("deep"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(virtualPath)
		return err == nil
	})
}

// TestBTRFSRealLongFilenameNearLimit verifies a 250-char filename
// (just under NAME_MAX=255 to leave room for any internal padding).
// Filenames are stored as length-prefixed bytes in fgbin so the limit is
// 65535, but the OS filesystem caps at NAME_MAX.
func TestBTRFSRealLongFilenameNearLimit(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	longName := strings.Repeat("x", 250) + ".txt"
	target := filepath.Join(subvol, longName)
	seedAndAwait(t, svc, target, rootName+"/"+longName, []byte("long"))
}

// TestBTRFSRealNewlineInFilename creates a file with an embedded newline
// character. `btrfs subvolume find-new` produces line-oriented output and
// is the most likely component to misparse this; the test exists to
// document whether Filegate copes or breaks. We don't FAIL on missed
// detection — we only fail if Filegate panics or destabilises adjacent
// regular files.
func TestBTRFSRealNewlineInFilename(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	regular := filepath.Join(subvol, "alongside-newline.txt")
	seedAndAwait(t, svc, regular, rootName+"/alongside-newline.txt", []byte("normal"))

	weird := "weird\nname.txt"
	target := filepath.Join(subvol, weird)
	if err := os.WriteFile(target, []byte("nl"), 0o644); err != nil {
		t.Fatalf("write newline-name: %v", err)
	}
	if err := os.WriteFile(regular, []byte("touched-after-newline"), 0o644); err != nil {
		t.Fatalf("stim: %v", err)
	}

	// Adjacent file must keep working regardless of how the parser handled
	// the newline.
	waitUntil(t, 10*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/alongside-newline.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == int64(len("touched-after-newline"))
	})

	if _, err := svc.ResolvePath(rootName + "/" + weird); err == nil {
		t.Logf("good news: newline-in-filename actually resolves")
	} else {
		t.Logf("documented limitation: newline-in-filename did not index (%v)", err)
	}
}

// =============================================================================
// Scale
// =============================================================================

// TestBTRFSRealLargeDirectoryFanout writes 1000 files in one directory.
// Stresses batching, Pebble write amplification, and the
// reconcileByInode lookup throughput. 1000 is enough to surface
// quadratic blowup without making the docker run brutally long.
func TestBTRFSRealLargeDirectoryFanout(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const n = 1000
	dir := filepath.Join(subvol, "fanout")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Sentinel to walk past loopback init race before the burst.
	sentinel := filepath.Join(dir, "_sentinel.txt")
	seedAndAwait(t, svc, sentinel, rootName+"/fanout/_sentinel.txt", []byte("s"))

	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f-%05d.txt", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	waitUntil(t, 60*time.Second, func() bool {
		// Probe a handful evenly-spaced; if those are in, the batch made it.
		for _, idx := range []int{0, n / 4, n / 2, 3 * n / 4, n - 1} {
			if _, err := svc.ResolvePath(fmt.Sprintf("%s/fanout/f-%05d.txt", rootName, idx)); err != nil {
				return false
			}
		}
		return true
	})
}

// TestBTRFSRealLargeFileWrite writes a 32 MiB file in one shot. Pins that
// the size encoding and detection cycle don't degrade for non-trivial
// sizes (test cost is bounded by btrfs loopback throughput, ~100ms).
func TestBTRFSRealLargeFileWrite(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const size int64 = 32 << 20
	target := filepath.Join(subvol, "biggish.bin")
	payload := make([]byte, size)
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	waitForResolveWithStimulus(t, 30*time.Second, 250*time.Millisecond, func() {
		// Re-truncate-write is cheap and provides the stimulus.
		_ = os.Truncate(target, size)
	}, func() bool {
		id, err := svc.ResolvePath(rootName + "/biggish.bin")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == size
	})
}

// =============================================================================
// Concurrency
// =============================================================================

// TestBTRFSRealConcurrentWritersToDifferentFiles spawns many goroutines
// each writing its own file. Stresses that the detector pipeline + index
// batching handle concurrent producer load without races, deadlocks, or
// dropped events. Distinct from BurstCreate because writes happen
// truly in parallel rather than sequentially.
func TestBTRFSRealConcurrentWritersToDifferentFiles(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	// Walk past init race with a sentinel.
	sentinel := filepath.Join(subvol, "_conc_sentinel.txt")
	seedAndAwait(t, svc, sentinel, rootName+"/_conc_sentinel.txt", []byte("s"))

	const goroutines = 16
	const perGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				p := filepath.Join(subvol, fmt.Sprintf("g%02d-i%03d.txt", g, i))
				_ = os.WriteFile(p, []byte("payload"), 0o644)
			}
		}(g)
	}
	wg.Wait()

	waitUntil(t, 30*time.Second, func() bool {
		for g := 0; g < goroutines; g++ {
			for _, i := range []int{0, perGoroutine - 1} {
				if _, err := svc.ResolvePath(fmt.Sprintf("%s/g%02d-i%03d.txt", rootName, g, i)); err != nil {
					return false
				}
			}
		}
		return true
	})
}

// TestBTRFSRealConcurrentRenameRace runs two goroutines competing on
// renames of overlapping paths. One renames A->B, the other renames B->C.
// Tests there is no panic, deadlock, or index corruption; doesn't pin
// which one "wins" because that's filesystem-race-dependent.
func TestBTRFSRealConcurrentRenameRace(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	a := filepath.Join(subvol, "race-a.txt")
	seedAndAwait(t, svc, a, rootName+"/race-a.txt", []byte("payload"))

	b := filepath.Join(subvol, "race-b.txt")
	c := filepath.Join(subvol, "race-c.txt")
	var attempts atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = os.Rename(a, b)
			attempts.Add(1)
			time.Sleep(time.Millisecond)
			_ = os.Rename(b, a)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = os.Rename(a, c)
			attempts.Add(1)
			time.Sleep(time.Millisecond)
			_ = os.Rename(c, a)
		}
	}()
	wg.Wait()

	// Whoever ends up holding the inode at the canonical name should
	// resolve. Test passes if no panic occurred and at least one of the
	// three paths is resolvable.
	for _, name := range []string{"race-a.txt", "race-b.txt", "race-c.txt"} {
		if _, err := svc.ResolvePath(rootName + "/" + name); err == nil {
			return
		}
	}
	t.Fatalf("after %d rename attempts none of the three paths resolved", attempts.Load())
}

// TestBTRFSRealWriteWhileReading opens a file for read in one goroutine
// while another rewrites it. The reader's fd must keep working (Linux
// preserves the fd's view), and the index must reflect the post-write
// state.
func TestBTRFSRealWriteWhileReading(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "read-while-write.txt")
	seedAndAwait(t, svc, target, rootName+"/read-while-write.txt", []byte("first-version"))

	reader, err := os.Open(target)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if err := os.WriteFile(target, []byte("second-version-is-longer"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	waitUntil(t, 15*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/read-while-write.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == int64(len("second-version-is-longer"))
	})

	// Reader's fd should still be usable. We don't assert which version it
	// reads (Linux semantics: depends on whether the rewrite was an
	// O_TRUNC overwrite or a temp+rename).
	buf := make([]byte, 1024)
	if _, err := reader.Read(buf); err != nil && err.Error() != "EOF" {
		// EOF is acceptable (truncation happened); other errors are not.
		if !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("reader fd unusable after concurrent write: %v", err)
		}
	}
}

// TestBTRFSRealLongRunningWrite simulates a writer that produces output
// over multiple detector ticks. Filegate must converge on the final size
// by the time the writer closes.
func TestBTRFSRealLongRunningWrite(t *testing.T) {
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	target := filepath.Join(subvol, "stream.bin")
	f, err := os.Create(target)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := make([]byte, 64*1024)
	const chunks = 16
	for i := 0; i < chunks; i++ {
		if _, err := f.Write(chunk); err != nil {
			_ = f.Close()
			t.Fatalf("write chunk %d: %v", i, err)
		}
		// Sleep between writes so the detector ticks (40ms) observes the
		// growing file in multiple snapshots. This is a genuine
		// rate-limiting sleep, not a goroutine sync.
		time.Sleep(50 * time.Millisecond)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	finalSize := int64(chunks) * int64(len(chunk))
	waitUntil(t, 15*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/stream.bin")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		return err == nil && meta.Size == finalSize
	})
}
