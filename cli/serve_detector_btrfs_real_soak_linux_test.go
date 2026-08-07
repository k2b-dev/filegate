//go:build linux

// Real-btrfs soak / chaos tests. Disk-bounded: every test operates on a
// FIXED pool of paths, so footprint stays constant regardless of how
// long the test runs. None of these run in normal CI — they need
// FILEGATE_BTRFS_SOAK=1, in spirit of the existing FILEGATE_SOAK and
// FILEGATE_CHAOS gates that the poll-detector tests use.
//
// Duration is parameterized via FILEGATE_BTRFS_SOAK_DURATION (Go
// duration string, e.g. "30s", "5m", "4h"). Default is short enough to
// be a smoke run when the gate is enabled but a developer hasn't picked
// a real soak length.
//
// Every test ends with a strong convergence check: the on-disk readdir
// set must match the index's children set for the affected directory.
// Detector / dir-sync / cache state has to be eventually consistent;
// these tests fail if it isn't.

package cli

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// ============================================================================
// Soak helpers
// ============================================================================

const defaultSoakDuration = 30 * time.Second

// requireSoak skips the test unless FILEGATE_BTRFS_SOAK=1 is set. Mirrors
// the FILEGATE_SOAK / FILEGATE_CHAOS pattern used by the existing
// poll-detector tests in serve_detector_linux_test.go.
func requireSoak(t *testing.T) {
	t.Helper()
	if os.Getenv("FILEGATE_BTRFS_SOAK") != "1" {
		t.Skip("FILEGATE_BTRFS_SOAK=1 required for soak tests")
	}
}

// soakDuration reads FILEGATE_BTRFS_SOAK_DURATION as a Go duration string
// (e.g. "5m", "4h"). A typo MUST fail the test rather than silently fall
// back — operators kicking off long soaks need to know if their value
// didn't parse.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("FILEGATE_BTRFS_SOAK_DURATION")
	if raw == "" {
		return defaultSoakDuration
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("FILEGATE_BTRFS_SOAK_DURATION=%q: %v", raw, err)
	}
	if d <= 0 {
		t.Fatalf("FILEGATE_BTRFS_SOAK_DURATION=%q: must be positive", raw)
	}
	return d
}

// snapshotDirNames returns the alphabetically sorted set of regular-file
// and directory names under absPath (symlinks skipped, matching
// Filegate's policy). Used by convergence assertions.
func snapshotDirNames(t *testing.T, absPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(absPath)
	if err != nil {
		t.Fatalf("readdir %q: %v", absPath, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// indexedChild captures the per-entry view of a directory's children
// that the index returns. We compare these against on-disk readdir entries.
type indexedChild struct {
	Name  string
	ID    domain.FileID
	IsDir bool
	Size  int64
}

// snapshotIndexChildren returns the alphabetically sorted child entries
// the index reports for virtualPath. Returns (entries, ok=false) if the
// directory itself isn't yet indexed — the caller is expected to keep
// polling rather than fatal-on-first-miss.
func snapshotIndexChildren(t *testing.T, svc *domain.Service, virtualPath string) ([]indexedChild, bool) {
	t.Helper()
	parentID, err := svc.ResolvePath(virtualPath)
	if err != nil {
		return nil, false
	}
	out := make([]indexedChild, 0, 32)
	cursor := ""
	for {
		page, err := svc.ListNodeChildren(parentID, cursor, 1000, false)
		if err != nil {
			return nil, false
		}
		if page == nil || len(page.Items) == 0 {
			break
		}
		for _, item := range page.Items {
			out = append(out, indexedChild{
				Name:  item.Name,
				ID:    item.ID,
				IsDir: item.Type == "directory",
				Size:  item.Size,
			})
		}
		if len(page.Items) < 1000 {
			break
		}
		cursor = page.Items[len(page.Items)-1].Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

// indexedNames extracts just the sorted name list from a snapshot.
func indexedNames(s []indexedChild) []string {
	out := make([]string, len(s))
	for i, c := range s {
		out[i] = c.Name
	}
	return out
}

// convergenceCheck describes a single converged-state assertion: the
// on-disk readdir at absPath equals the index's children for virtualPath
// AND, if checkSizes is true, the indexed sizes match on-disk stat sizes.
type convergenceCheck struct {
	absPath     string
	virtualPath string
	checkSizes  bool
}

// assertConvergence eventually verifies that the on-disk readdir of
// absPath matches the index's children for virtualPath. Requires the
// match to hold for THREE consecutive polls (so a transient mid-batch
// snapshot can't false-positive). Optionally also asserts size equality
// per entry to catch size-update regressions that would otherwise pass
// the names-only check.
func assertConvergence(t *testing.T, svc *domain.Service, c convergenceCheck, deadline time.Duration) {
	t.Helper()
	const requiredConsecutive = 3
	const pollInterval = 200 * time.Millisecond
	end := time.Now().Add(deadline)
	stable := 0
	var lastDisk []string
	var lastIdx []indexedChild
	for time.Now().Before(end) {
		disk := snapshotDirNames(t, c.absPath)
		idx, ok := snapshotIndexChildren(t, svc, c.virtualPath)
		if !ok {
			stable = 0
			time.Sleep(pollInterval)
			continue
		}
		idxNames := indexedNames(idx)
		converged := equalStringSets(disk, idxNames)
		if converged && c.checkSizes {
			converged = sizesMatch(t, c.absPath, idx)
		}
		if converged {
			stable++
			if stable >= requiredConsecutive {
				return
			}
		} else {
			stable = 0
			lastDisk, lastIdx = disk, idx
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("convergence not reached within %s for %q\n  on-disk (%d): %v\n  in-index  (%d): %v",
		deadline, c.absPath, len(lastDisk), trimList(lastDisk, 20), len(lastIdx), trimList(indexedNames(lastIdx), 20))
}

// sizesMatch verifies for every indexed entry that its stat'd size equals
// what the index reports. Catches size-update regressions that name-only
// convergence would silently pass.
func sizesMatch(t *testing.T, dirAbs string, idx []indexedChild) bool {
	t.Helper()
	for _, c := range idx {
		if c.IsDir {
			continue
		}
		info, err := os.Stat(filepath.Join(dirAbs, c.Name))
		if err != nil {
			// Disk entry vanished mid-poll — caller will retry.
			return false
		}
		if info.Size() != c.Size {
			return false
		}
	}
	return true
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trimList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	out := make([]string, 0, n+1)
	out = append(out, s[:n]...)
	out = append(out, fmt.Sprintf("...(+%d more)", len(s)-n))
	return out
}

// goroutineLeakSnapshot records a baseline goroutine count and returns
// a checker that FAILS the test if the post-run count exceeds baseline
// by more than tolerance. Filegate's bounded-async patterns mean any
// real growth across a soak loop is a leak.
func goroutineLeakSnapshot(t *testing.T, tolerance int) func() {
	t.Helper()
	// Settle for a moment so any background init goroutines stabilize.
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	return func() {
		// Give cleanups (subscriber drains etc.) a moment after the load.
		time.Sleep(500 * time.Millisecond)
		after := runtime.NumGoroutine()
		grew := after - before
		t.Logf("goroutine count: before=%d after=%d delta=%d tolerance=%d", before, after, grew, tolerance)
		if grew > tolerance {
			t.Errorf("goroutine leak suspected: count grew by %d (before=%d, after=%d, tolerance=%d)",
				grew, before, after, tolerance)
		}
	}
}

// memSnapshot returns a checker that fails on heap growth above
// thresholdMiB. Small drift between GCs is normal; persistent growth
// across a soak iteration is a leak signal.
func memSnapshot(t *testing.T, thresholdMiB float64) func() {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	return func() {
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		growthMiB := float64(growth) / (1024 * 1024)
		t.Logf("heap growth: %.2f MiB (before=%.1f MiB, after=%.1f MiB, threshold=%.1f MiB)",
			growthMiB,
			float64(before.HeapAlloc)/(1024*1024),
			float64(after.HeapAlloc)/(1024*1024),
			thresholdMiB)
		if growthMiB > thresholdMiB {
			t.Errorf("heap growth %.2f MiB exceeds threshold %.1f MiB — possible leak",
				growthMiB, thresholdMiB)
		}
	}
}

// poolOp picks a pseudo-random operation against a fixed pool of paths.
// Distribution is roughly: 40% write, 30% delete, 20% rewrite-existing,
// 10% rename to another slot. This keeps the steady-state count of
// existing files roughly stable.
type poolOp int

const (
	opWrite poolOp = iota
	opDelete
	opRewrite
	opRename
)

func pickPoolOp(r *rand.Rand) poolOp {
	switch r.Intn(10) {
	case 0, 1, 2, 3:
		return opWrite
	case 4, 5, 6:
		return opDelete
	case 7, 8:
		return opRewrite
	default:
		return opRename
	}
}

// applyPoolOp does one operation against a randomly-picked slot of the
// pool. Errors on individual operations are tolerated (a delete of a
// non-existent slot is benign, etc.) but the total error count is
// reported by the caller.
func applyPoolOp(r *rand.Rand, dir string, poolSize int) error {
	slot := r.Intn(poolSize)
	target := filepath.Join(dir, fmt.Sprintf("slot-%05d.txt", slot))
	op := pickPoolOp(r)
	payload := []byte(fmt.Sprintf("op=%d slot=%d", op, slot))
	switch op {
	case opWrite, opRewrite:
		return os.WriteFile(target, payload, 0o644)
	case opDelete:
		err := os.Remove(target)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case opRename:
		alt := filepath.Join(dir, fmt.Sprintf("slot-%05d.txt", r.Intn(poolSize)))
		if alt == target {
			return nil
		}
		err := os.Rename(target, alt)
		// Renames will fail if source doesn't exist; that's expected churn.
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return nil
}

// ============================================================================
// 1. Sustained churn — single goroutine, fixed pool
// ============================================================================

// TestBTRFSRealSoakSustainedChurn runs a single-goroutine churn against a
// fixed pool of 1000 paths in one directory. Disk footprint stays bounded
// (≤ pool size × payload). At the end, the index's children set must
// equal the on-disk readdir set.
func TestBTRFSRealSoakSustainedChurn(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "churn")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Sentinel write under the dir so dir itself gets indexed before
	// the loop starts (mkdir alone is invisible to find-new).
	sentinel := filepath.Join(dir, "_sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	const poolSize = 1000
	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops, errs atomic.Int64
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		if err := applyPoolOp(r, dir, poolSize); err != nil {
			errs.Add(1)
		}
		ops.Add(1)
	}
	elapsed := time.Since(start)
	t.Logf("churn done: %d ops in %s (%.0f ops/sec), %d errors",
		ops.Load(), elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Convergence: dir-sync + detector should have caught up. Generous
	// deadline because a long-running churn produces a big tail of
	// pending dir-syncs.
	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/churn"}, 30*time.Second)
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 2. Concurrent chaos — many goroutines, shared pool
// ============================================================================

// TestBTRFSRealSoakConcurrentChaos hammers a shared pool from many
// goroutines. Tests Filegate's behavior under concurrent producer load:
// detector queue, dir-sync coalescing, Pebble batch contention, and any
// unsynchronized state in the consumer.
func TestBTRFSRealSoakConcurrentChaos(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "chaos")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := filepath.Join(dir, "_sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	const (
		poolSize   = 1000
		goroutines = 16
	)
	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, goroutines+5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops, errs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(g*1000)))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := applyPoolOp(r, dir, poolSize); err != nil {
					errs.Add(1)
				}
				ops.Add(1)
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("chaos done: %d ops across %d goroutines in %s (%.0f ops/sec), %d errors",
		ops.Load(), goroutines, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/chaos"}, 30*time.Second)
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 3. Directory fanout — many parents, dir-sync scaling
// ============================================================================

// TestBTRFSRealSoakDirectoryFanout spreads load across many parent dirs
// to stress dir-sync at scale. Each detector batch may dirty hundreds
// of parents, exercising the per-parent reconciliation throughput.
func TestBTRFSRealSoakDirectoryFanout(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const (
		dirs        = 100
		slotsPerDir = 50
	)
	dirAbsPaths := make([]string, dirs)
	dirVirtualPaths := make([]string, dirs)
	for i := 0; i < dirs; i++ {
		dirAbsPaths[i] = filepath.Join(subvol, fmt.Sprintf("d-%03d", i))
		dirVirtualPaths[i] = fmt.Sprintf("%s/d-%03d", rootName, i)
		if err := os.Mkdir(dirAbsPaths[i], 0o755); err != nil {
			t.Fatalf("mkdir %d: %v", i, err)
		}
		// Sentinel inside each dir so the dir itself becomes indexed.
		sentinel := filepath.Join(dirAbsPaths[i], "_sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
			t.Fatalf("sentinel %d: %v", i, err)
		}
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var ops, errs atomic.Int64
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		dir := dirAbsPaths[r.Intn(dirs)]
		if err := applyPoolOp(r, dir, slotsPerDir); err != nil {
			errs.Add(1)
		}
		ops.Add(1)
	}
	elapsed := time.Since(start)
	t.Logf("fanout done: %d ops across %d dirs in %s (%.0f ops/sec), %d errors",
		ops.Load(), dirs, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Convergence: pick a sample of dirs, verify each. Checking all 100
	// is fine but slower; the sample catches regression patterns just
	// as well.
	// Check ALL dirs, not a sample. A per-directory reconciliation bug
	// could otherwise hide in unchecked zones.
	for i := 0; i < dirs; i++ {
		assertConvergence(t, svc, convergenceCheck{absPath: dirAbsPaths[i], virtualPath: dirVirtualPaths[i]}, 30*time.Second)
	}
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 4. Rolling window — log-rotation pattern
// ============================================================================

// TestBTRFSRealSoakRollingWindow simulates log rotation: keep a sliding
// window of N most recent files, drop older. Index entry count should
// stay bounded near window size — a leak would manifest as monotonic
// growth.
func TestBTRFSRealSoakRollingWindow(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "rolling")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := filepath.Join(dir, "_sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	const window = 100
	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops atomic.Int64
	start := time.Now()
	seq := 0
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		// Append a new file.
		newFile := filepath.Join(dir, fmt.Sprintf("log-%010d.txt", seq))
		_ = os.WriteFile(newFile, []byte(fmt.Sprintf("entry %d", seq)), 0o644)
		seq++
		// Drop oldest if window exceeded.
		if seq > window {
			old := filepath.Join(dir, fmt.Sprintf("log-%010d.txt", seq-window-1))
			_ = os.Remove(old)
		}
		ops.Add(1)
		// Tiny rate limit so we don't spin-burn CPU on a tight loop.
		time.Sleep(time.Microsecond)
	}
	elapsed := time.Since(start)
	t.Logf("rolling-window done: %d ops in %s (%.0f ops/sec)",
		ops.Load(), elapsed, float64(ops.Load())/elapsed.Seconds())

	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/rolling"}, 30*time.Second)
	// Bonus: explicit bound check on indexed children count.
	idx, ok := snapshotIndexChildren(t, svc, rootName+"/rolling")
	if !ok {
		t.Fatalf("rolling-window dir not indexed at end of soak")
	}
	// Window + sentinel + maybe one in-flight; allow some slack for the
	// last cycle's race.
	if len(idx) > window+5 {
		t.Fatalf("indexed children = %d, expected ≈ %d (sliding-window leak?)", len(idx), window)
	}
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 5. Detector restart resilience — load survives shutdown/restart cycles
// ============================================================================

// TestBTRFSRealSoakDetectorRestartResilience runs sustained churn while
// periodically restarting the gateway service. Each restart triggers a
// fresh detector + index reopen + Rescan. Final state must converge.
// Catches: goroutine leaks across restarts, index-state corruption from
// abrupt shutdown mid-batch, detector failing to re-arm cleanly.
func TestBTRFSRealSoakDetectorRestartResilience(t *testing.T) {
	requireSoak(t)
	f := newRestartableFixture(t)

	dir := filepath.Join(f.subvol, "restartable")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := filepath.Join(dir, "_sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	const poolSize = 500
	duration := soakDuration(t)
	// Aim for a restart roughly every 1/4 of the soak duration, capped
	// to keep the test responsive.
	restartEvery := duration / 4
	if restartEvery < time.Second {
		restartEvery = time.Second
	}
	checkGoroutines := goroutineLeakSnapshot(t, 10)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// Background load: continuously churn the pool.
	var ops, errs atomic.Int64
	var loadWG sync.WaitGroup
	loadWG.Add(1)
	go func() {
		defer loadWG.Done()
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := applyPoolOp(r, dir, poolSize); err != nil {
				errs.Add(1)
			}
			ops.Add(1)
		}
	}()

	// Restart loop.
	restarts := 0
	restartTicker := time.NewTicker(restartEvery)
	defer restartTicker.Stop()
restartLoop:
	for {
		select {
		case <-ctx.Done():
			break restartLoop
		case <-restartTicker.C:
			f.reopen()
			if err := f.svc.Rescan(); err != nil {
				t.Errorf("rescan after restart %d failed: %v", restarts, err)
			}
			restarts++
		}
	}
	loadWG.Wait()
	t.Logf("restart-resilience: %d ops, %d restarts in %s, %d errors",
		ops.Load(), restarts, duration, errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}
	if restarts == 0 {
		t.Fatalf("zero restarts in %s — duration too short or restart cadence broken", duration)
	}

	// Final Rescan to settle, then convergence.
	if err := f.svc.Rescan(); err != nil {
		t.Fatalf("final rescan: %v", err)
	}
	assertConvergence(t, f.svc, convergenceCheck{absPath: dir, virtualPath: f.rootName + "/restartable"}, 30*time.Second)
	checkGoroutines()
}

// ============================================================================
// 6. High concurrency tiny pool — extreme contention serialization test
// ============================================================================

// TestBTRFSRealSoakHighConcurrencyTinyPool stresses Filegate with way more
// goroutines than pool slots. 64 goroutines fighting over 100 paths means
// every slot is hit ~0.64 times per goroutine cycle — heavy contention on
// the same inode in flight at the detector + dir-sync layers. Tests the
// serialization properties of the consumer pipeline under pathological
// concurrency.
func TestBTRFSRealSoakHighConcurrencyTinyPool(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "tinypool")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_sentinel.txt"), []byte("s"), 0o644); err != nil {
		t.Fatalf("sentinel: %v", err)
	}

	const (
		poolSize   = 100
		goroutines = 64
	)
	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, goroutines+5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops, errs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(g*1009)))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := applyPoolOp(r, dir, poolSize); err != nil {
					errs.Add(1)
				}
				ops.Add(1)
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("tiny-pool done: %d ops × %d goroutines on %d slots in %s (%.0f ops/sec), %d errors",
		ops.Load(), goroutines, poolSize, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/tinypool"}, 60*time.Second)
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 7. Hardlink churn — link/unlink cycle at high rate
// ============================================================================

// TestBTRFSRealSoakHardlinkChurn continuously creates and removes hard
// links across a small set of base files. Tests the dir-sync hardlink-
// safety contract (nlink>1 short-circuit in PutEntity) under sustained
// load — a regression that broke aliases on rapid link/unlink cycles
// would manifest as missing entries at convergence time.
func TestBTRFSRealSoakHardlinkChurn(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "hl-churn")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Seed: 20 base files. The churn cycles aliases through the rest.
	const (
		baseFiles  = 20
		aliasSlots = 200
	)
	for i := 0; i < baseFiles; i++ {
		base := filepath.Join(dir, fmt.Sprintf("base-%02d.txt", i))
		if err := os.WriteFile(base, []byte(fmt.Sprintf("base-%d", i)), 0o644); err != nil {
			t.Fatalf("seed base %d: %v", i, err)
		}
	}
	// Initial Rescan so the bases are indexed before the churn starts.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("seed rescan: %v", err)
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var ops, errs atomic.Int64
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		base := filepath.Join(dir, fmt.Sprintf("base-%02d.txt", r.Intn(baseFiles)))
		alias := filepath.Join(dir, fmt.Sprintf("alias-%03d.txt", r.Intn(aliasSlots)))
		// Random link or unlink. Linking to an already-linked alias
		// returns EEXIST; unlinking missing is ENOENT. Both benign.
		if r.Intn(2) == 0 {
			err := os.Link(base, alias)
			if err != nil && !os.IsExist(err) {
				errs.Add(1)
			}
		} else {
			err := os.Remove(alias)
			if err != nil && !os.IsNotExist(err) {
				errs.Add(1)
			}
		}
		ops.Add(1)
	}
	elapsed := time.Since(start)
	t.Logf("hardlink-churn done: %d link/unlink ops in %s (%.0f ops/sec), %d errors",
		ops.Load(), elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Force a Rescan because hardlink ops don't reliably bump btrfs
	// generation; without it, the alias slots may not be in the index.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("final rescan: %v", err)
	}
	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/hl-churn"}, 30*time.Second)
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 8. Deep tree churn — 10-level nesting with activity at every level
// ============================================================================

// TestBTRFSRealSoakDeepTreeChurn builds a 10-level deep directory tree
// with files at every level, then churns operations across the entire
// tree. Stresses path-resolution caching, parent-walk recursion in
// syncSingle, and dir-sync correctness when many parents at different
// depths get dirty simultaneously.
func TestBTRFSRealSoakDeepTreeChurn(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const (
		depth         = 10
		slotsPerLevel = 50
	)
	// Build the tree.
	dirAbsByLevel := make([]string, depth+1)
	dirVPByLevel := make([]string, depth+1)
	dirAbsByLevel[0] = filepath.Join(subvol, "deep")
	dirVPByLevel[0] = rootName + "/deep"
	if err := os.Mkdir(dirAbsByLevel[0], 0o755); err != nil {
		t.Fatalf("mkdir level 0: %v", err)
	}
	for level := 1; level <= depth; level++ {
		dirAbsByLevel[level] = filepath.Join(dirAbsByLevel[level-1], fmt.Sprintf("level-%02d", level))
		dirVPByLevel[level] = dirVPByLevel[level-1] + fmt.Sprintf("/level-%02d", level)
		if err := os.Mkdir(dirAbsByLevel[level], 0o755); err != nil {
			t.Fatalf("mkdir level %d: %v", level, err)
		}
		// Sentinel at every level so the dirs themselves get indexed.
		if err := os.WriteFile(filepath.Join(dirAbsByLevel[level], "_sentinel.txt"), []byte("s"), 0o644); err != nil {
			t.Fatalf("sentinel level %d: %v", level, err)
		}
	}
	// Initial Rescan to seed the whole tree.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("initial rescan: %v", err)
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var ops, errs atomic.Int64
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		level := r.Intn(depth + 1)
		if err := applyPoolOp(r, dirAbsByLevel[level], slotsPerLevel); err != nil {
			errs.Add(1)
		}
		ops.Add(1)
	}
	elapsed := time.Since(start)
	t.Logf("deep-tree-churn done: %d ops across %d levels in %s (%.0f ops/sec), %d errors",
		ops.Load(), depth+1, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Convergence at every level — the tree is small enough.
	for level := 0; level <= depth; level++ {
		assertConvergence(t, svc, convergenceCheck{absPath: dirAbsByLevel[level], virtualPath: dirVPByLevel[level]}, 30*time.Second)
	}
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 9. Large file churn — files cycling 0 ↔ 10 MiB
// ============================================================================

// TestBTRFSRealSoakLargeFileChurn cycles files between empty (truncate
// to 0) and 10 MiB. Stresses size encoding, fallocate-style growth,
// detector emission for repeated mid-size writes, and Pebble's handling
// of large index updates per inode under sustained churn.
func TestBTRFSRealSoakLargeFileChurn(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "bigfiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const (
		poolSize  = 20
		largeSize = 10 << 20 // 10 MiB
	)
	// Pre-allocate the pool with empty files so we don't keep allocating
	// new inodes (would defeat the bounded-disk claim).
	for i := 0; i < poolSize; i++ {
		p := filepath.Join(dir, fmt.Sprintf("big-%02d.bin", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("seed rescan: %v", err)
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	payload := make([]byte, largeSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	var ops, errs atomic.Int64
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		p := filepath.Join(dir, fmt.Sprintf("big-%02d.bin", r.Intn(poolSize)))
		// Cycle: truncate to 0 OR write the 10 MiB payload.
		if r.Intn(2) == 0 {
			if err := os.Truncate(p, 0); err != nil {
				errs.Add(1)
			}
		} else {
			if err := os.WriteFile(p, payload, 0o644); err != nil {
				errs.Add(1)
			}
		}
		ops.Add(1)
	}
	elapsed := time.Since(start)
	t.Logf("large-file-churn done: %d ops on %d × %s files in %s (%.0f ops/sec), %d errors",
		ops.Load(), poolSize, formatBytes(largeSize), elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Settle every file to size 0 so we have a deterministic target for
	// the size assertion. During the churn the index may have observed
	// any of N intermediate sizes; we want to verify that AFTER load
	// stops, the steady-state size eventually converges.
	for i := 0; i < poolSize; i++ {
		p := filepath.Join(dir, fmt.Sprintf("big-%02d.bin", i))
		if err := os.Truncate(p, 0); err != nil {
			t.Fatalf("settle truncate %d: %v", i, err)
		}
	}
	// Force a Rescan as a backstop — the detector may have a queue tail
	// that's still draining the churn's stale-size events when our
	// settle truncates land. Rescan re-reads each file's current size
	// authoritatively.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("settle rescan: %v", err)
	}

	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/bigfiles", checkSizes: true}, 30*time.Second)
	checkGoroutines()
	checkMem()
}

func formatBytes(n int64) string {
	if n >= (1 << 20) {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	if n >= (1 << 10) {
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// ============================================================================
// 10. Adversarial rename storm — A↔B cyclic at high frequency
// ============================================================================

// TestBTRFSRealSoakAdversarialRenameStorm hammers cyclic A→C, B→A, C→B
// renames at high frequency. The 3-way swap is the worst case for the
// inode-tracking + child-entry consistency logic — it forces the
// detector + dir-sync to chase a moving target. A regression in the
// stale-child cleanup or the resolveOrReissueID conflict logic would
// produce divergent index state under this load.
func TestBTRFSRealSoakAdversarialRenameStorm(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	dir := filepath.Join(subvol, "rename-storm")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const pairs = 50
	// Seed: pairs of files (a-XX, b-XX). Each pair is independently
	// swap-cycled by the loop.
	for i := 0; i < pairs; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("a-%02d.txt", i)), []byte(fmt.Sprintf("a-%d", i)), 0o644); err != nil {
			t.Fatalf("seed a%d: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("b-%02d.txt", i)), []byte(fmt.Sprintf("b-%d", i)), 0o644); err != nil {
			t.Fatalf("seed b%d: %v", i, err)
		}
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("seed rescan: %v", err)
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, 5)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	const renameWorkers = 8
	var ops, errs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(renameWorkers)
	start := time.Now()
	for w := 0; w < renameWorkers; w++ {
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w*1019)))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				i := r.Intn(pairs)
				a := filepath.Join(dir, fmt.Sprintf("a-%02d.txt", i))
				b := filepath.Join(dir, fmt.Sprintf("b-%02d.txt", i))
				c := filepath.Join(dir, fmt.Sprintf("c-%02d.txt", i))
				// 3-step swap. Errors here are expected because workers
				// race over the same pair; ENOENT/EEXIST are tolerated,
				// other errors count as failures.
				for _, err := range []error{
					os.Rename(a, c),
					os.Rename(b, a),
					os.Rename(c, b),
				} {
					if err == nil {
						continue
					}
					if os.IsNotExist(err) || os.IsExist(err) {
						continue
					}
					errs.Add(1)
				}
				ops.Add(3)
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("rename-storm done: %d rename ops × %d workers on %d pairs in %s (%.0f ops/sec), %d errors",
		ops.Load(), renameWorkers, pairs, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	// Force convergence rescan. Cyclic renames don't always bump btrfs
	// generation in a way find-new emits.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("final rescan: %v", err)
	}
	assertConvergence(t, svc, convergenceCheck{absPath: dir, virtualPath: rootName + "/rename-storm"}, 30*time.Second)
	checkGoroutines()
	checkMem()
}

// ============================================================================
// 11. Combined chaos monkey — every pattern mixed
// ============================================================================

// TestBTRFSRealSoakCombinedChaosMonkey is the kitchen-sink test: 32
// goroutines, each running a different mix of operations across a tree
// of dirs. Some create+delete, some rename, some hardlink, some
// truncate. No specific pattern — the whole point is that under random
// chaos with many concurrent strategies, Filegate must still converge.
//
// This is the integration-level "did anything anywhere break" test.
func TestBTRFSRealSoakCombinedChaosMonkey(t *testing.T) {
	requireSoak(t)
	subvol := setupRealBTRFSSubvol(t)
	svc, rootName, _ := startRealBTRFSDetector(t, subvol)

	const (
		dirs        = 20
		slotsPerDir = 200
		goroutines  = 32
	)
	dirAbsPaths := make([]string, dirs)
	dirVPs := make([]string, dirs)
	for i := 0; i < dirs; i++ {
		dirAbsPaths[i] = filepath.Join(subvol, fmt.Sprintf("zone-%02d", i))
		dirVPs[i] = fmt.Sprintf("%s/zone-%02d", rootName, i)
		if err := os.Mkdir(dirAbsPaths[i], 0o755); err != nil {
			t.Fatalf("mkdir zone %d: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(dirAbsPaths[i], "_sentinel.txt"), []byte("s"), 0o644); err != nil {
			t.Fatalf("sentinel zone %d: %v", i, err)
		}
	}

	duration := soakDuration(t)
	checkGoroutines := goroutineLeakSnapshot(t, goroutines+10)
	checkMem := memSnapshot(t, 5)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var ops, errs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(g*1013)))
			// Each goroutine picks a strategy and sticks with it for a
			// while, then rotates. Mimics real-world workload diversity.
			strategy := g % 5
			lastSwitch := time.Now()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if time.Since(lastSwitch) > time.Second {
					strategy = (strategy + 1) % 5
					lastSwitch = time.Now()
				}
				dir := dirAbsPaths[r.Intn(dirs)]
				switch strategy {
				case 0: // pool churn
					if err := applyPoolOp(r, dir, slotsPerDir); err != nil {
						errs.Add(1)
					}
				case 1: // hardlink dance
					base := filepath.Join(dir, fmt.Sprintf("hl-base-%d.txt", r.Intn(10)))
					alias := filepath.Join(dir, fmt.Sprintf("hl-alias-%d.txt", r.Intn(20)))
					_ = os.WriteFile(base, []byte("h"), 0o644)
					_ = os.Link(base, alias)
					_ = os.Remove(alias)
				case 2: // truncate cycle
					p := filepath.Join(dir, fmt.Sprintf("trunc-%d.txt", r.Intn(20)))
					_ = os.WriteFile(p, []byte("xxxxxxxxxxxxxxxxxxxx"), 0o644)
					_ = os.Truncate(p, int64(r.Intn(20)))
				case 3: // rename swap
					a := filepath.Join(dir, fmt.Sprintf("ra-%d.txt", r.Intn(10)))
					b := filepath.Join(dir, fmt.Sprintf("rb-%d.txt", r.Intn(10)))
					_ = os.WriteFile(a, []byte("a"), 0o644)
					_ = os.Rename(a, b)
				case 4: // mixed metadata change
					p := filepath.Join(dir, fmt.Sprintf("mod-%d.txt", r.Intn(20)))
					_ = os.WriteFile(p, []byte("m"), 0o644)
					_ = os.Chmod(p, os.FileMode(0o600+r.Intn(0o077)))
				}
				ops.Add(1)
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("chaos-monkey done: %d ops × %d goroutines × %d zones in %s (%.0f ops/sec), %d errors",
		ops.Load(), goroutines, dirs, elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
	if n := errs.Load(); n > 0 {
		t.Errorf("unexpected operation errors: %d", n)
	}

	if err := svc.Rescan(); err != nil {
		t.Fatalf("final rescan: %v", err)
	}
	// Check ALL zones — chaos monkey is the kitchen-sink test, an
	// unchecked zone would defeat the "did anything anywhere break"
	// claim.
	for i := 0; i < dirs; i++ {
		assertConvergence(t, svc, convergenceCheck{absPath: dirAbsPaths[i], virtualPath: dirVPs[i]}, 60*time.Second)
	}
	checkGoroutines()
	checkMem()
}
