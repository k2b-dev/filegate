//go:build linux

package cli

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/detect"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
)

func TestConsumeDetectorEventsWithPollerSyncsExternalChanges(t *testing.T) {
	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller := detect.NewPoller([]string{root}, 25*time.Millisecond)
	poller.Start(ctx)
	defer poller.Close()

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		consumeDetectorEvents(ctx, svc, poller.Events(), nil, 0)
	}()
	defer func() {
		cancel()
		<-consumerDone
	}()

	// Reset the poller's per-directory mtime cache via ForceRescan: this
	// guarantees the next poll cycle will *re-walk* the mount and seed
	// knownFiles with an explicit size/mtime entry for the first write.
	// Without that seeding, a later in-place overwrite at the same wall-clock
	// millisecond can be folded into the same poll cycle as the create and
	// produce no EventChanged. We do NOT wait for the consumer to drain the
	// resulting EventUnknown — that part is intentionally eventually
	// consistent and the per-step waitUntil() calls below cover it.
	if err := poller.ForceRescan(ctx); err != nil {
		t.Fatalf("initial force rescan: %v", err)
	}

	abs := filepath.Join(root, "from-poller.txt")
	if err := os.WriteFile(abs, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	waitUntil(t, 6*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/from-poller.txt")
		return err == nil
	})

	if err := os.WriteFile(abs, []byte("v2-updated"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	waitUntil(t, 6*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/from-poller.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		if err != nil {
			return false
		}
		return meta.Size == int64(len("v2-updated"))
	})

	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	waitUntil(t, 6*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/from-poller.txt")
		return err == domain.ErrNotFound
	})
}

func TestConsumeDetectorEventsPeriodicReconciliationRepairsMissedChange(t *testing.T) {
	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	missed := filepath.Join(root, "missed-empty-dir")
	if err := os.Mkdir(missed, 0o755); err != nil {
		t.Fatalf("create external directory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan []detect.Event)
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumeDetectorEvents(ctx, svc, events, nil, 5*time.Millisecond)
	}()

	waitUntil(t, 5*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/missed-empty-dir")
		return err == nil
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detector consumer did not stop")
	}
}

func TestApplyDetectorBatchPollLikeSyncsExternalChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	abs := filepath.Join(root, "external.txt")
	if err := os.WriteFile(abs, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	if err := applyDetectorBatch(svc, []detect.Event{{Type: detect.EventCreated, Base: root, AbsPath: abs, IsDir: false}}); err != nil {
		t.Fatalf("apply created: %v", err)
	}
	id, err := svc.ResolvePath(rootName + "/external.txt")
	if err != nil {
		t.Fatalf("resolve created file: %v", err)
	}
	meta, err := svc.GetFile(id)
	if err != nil {
		t.Fatalf("get created file: %v", err)
	}
	if meta.Size != int64(len("v1")) {
		t.Fatalf("size=%d, want=%d", meta.Size, len("v1"))
	}

	if err := os.WriteFile(abs, []byte("v2-updated"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	if err := applyDetectorBatch(svc, []detect.Event{{Type: detect.EventChanged, Base: root, AbsPath: abs, IsDir: false}}); err != nil {
		t.Fatalf("apply changed: %v", err)
	}
	meta, err = svc.GetFile(id)
	if err != nil {
		t.Fatalf("get changed file: %v", err)
	}
	if meta.Size != int64(len("v2-updated")) {
		t.Fatalf("size=%d, want=%d", meta.Size, len("v2-updated"))
	}

	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if err := applyDetectorBatch(svc, []detect.Event{{Type: detect.EventDeleted, Base: root, AbsPath: abs, IsDir: false}}); err != nil {
		t.Fatalf("apply deleted: %v", err)
	}
	if _, err := svc.ResolvePath(rootName + "/external.txt"); err != domain.ErrNotFound {
		t.Fatalf("resolve after delete err=%v, want ErrNotFound", err)
	}
}

func TestApplyDetectorBatchBTRFSLikeUnknownRescansOnlyAffectedMount(t *testing.T) {
	t.Parallel()

	rootA := t.TempDir()
	rootB := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{rootA, rootB})
	defer cleanup()
	nameA := mustMountNameByPath(t, svc, rootA)
	nameB := mustMountNameByPath(t, svc, rootB)

	fileA := filepath.Join(rootA, "a.txt")
	if err := os.WriteFile(fileA, []byte("a"), 0o644); err != nil {
		t.Fatalf("write fileA: %v", err)
	}
	fileB := filepath.Join(rootB, "b.txt")
	if err := os.WriteFile(fileB, []byte("b"), 0o644); err != nil {
		t.Fatalf("write fileB: %v", err)
	}

	if err := applyDetectorBatch(svc, []detect.Event{
		{Type: detect.EventCreated, Base: rootA, AbsPath: fileA, IsDir: false},
		{Type: detect.EventCreated, Base: rootB, AbsPath: fileB, IsDir: false},
	}); err != nil {
		t.Fatalf("apply created: %v", err)
	}
	if _, err := svc.ResolvePath(nameA + "/a.txt"); err != nil {
		t.Fatalf("resolve a.txt: %v", err)
	}
	if _, err := svc.ResolvePath(nameB + "/b.txt"); err != nil {
		t.Fatalf("resolve b.txt: %v", err)
	}

	if err := os.Remove(fileA); err != nil {
		t.Fatalf("remove fileA: %v", err)
	}

	if err := applyDetectorBatch(svc, []detect.Event{{Type: detect.EventUnknown, Base: rootA, AbsPath: rootA, IsDir: true}}); err != nil {
		t.Fatalf("apply unknown: %v", err)
	}

	if _, err := svc.ResolvePath(nameA + "/a.txt"); err != domain.ErrNotFound {
		t.Fatalf("resolve a.txt err=%v, want ErrNotFound", err)
	}
	if _, err := svc.ResolvePath(nameB + "/b.txt"); err != nil {
		t.Fatalf("resolve b.txt after mountA unknown: %v", err)
	}
}

func TestConsumeDetectorEventsStressWithDuplicates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	const totalFiles = 160
	const deleteCount = 70

	paths := make([]string, 0, totalFiles)
	for i := 0; i < totalFiles; i++ {
		abs := filepath.Join(root, fmt.Sprintf("bulk-%03d.bin", i))
		if err := os.WriteFile(abs, []byte("seed"), 0o644); err != nil {
			t.Fatalf("write %q: %v", abs, err)
		}
		paths = append(paths, abs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan []detect.Event, 8192)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		consumeDetectorEvents(ctx, svc, ch, nil, 0)
	}()

	rnd := rand.New(rand.NewSource(42))
	for round := 0; round < 12; round++ {
		batch := make([]detect.Event, 0, totalFiles*2)
		for _, p := range paths {
			batch = append(batch, detect.Event{Type: detect.EventCreated, Base: root, AbsPath: p, IsDir: false})
			if rnd.Intn(2) == 0 {
				batch = append(batch, detect.Event{Type: detect.EventChanged, Base: root, AbsPath: p, IsDir: false})
			}
		}
		ch <- batch
	}

	for i := 0; i < deleteCount; i++ {
		if err := os.Remove(paths[i]); err != nil {
			t.Fatalf("remove %q: %v", paths[i], err)
		}
	}
	for round := 0; round < 12; round++ {
		batch := make([]detect.Event, 0, deleteCount*3)
		for i := 0; i < deleteCount; i++ {
			p := paths[i]
			batch = append(batch,
				detect.Event{Type: detect.EventDeleted, Base: root, AbsPath: p, IsDir: false},
				detect.Event{Type: detect.EventDeleted, Base: root, AbsPath: p, IsDir: false},
			)
			if round%3 == 0 {
				batch = append(batch, detect.Event{Type: detect.EventChanged, Base: root, AbsPath: p, IsDir: false})
			}
		}
		ch <- batch
	}

	waitUntil(t, 10*time.Second, func() bool {
		for i := 0; i < deleteCount; i++ {
			name := filepath.Base(paths[i])
			if _, err := svc.ResolvePath(rootName + "/" + name); err != domain.ErrNotFound {
				return false
			}
		}
		for i := deleteCount; i < totalFiles; i++ {
			name := filepath.Base(paths[i])
			if _, err := svc.ResolvePath(rootName + "/" + name); err != nil {
				return false
			}
		}
		return true
	})

	close(ch)
	select {
	case <-consumerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("detector consumer did not stop after its event channel closed")
	}
}

func TestCoalesceDetectorBatchesDrainsQueue(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan []detect.Event, 8)
	ch <- []detect.Event{{Type: detect.EventCreated, AbsPath: "/a"}}
	ch <- []detect.Event{{Type: detect.EventChanged, AbsPath: "/b"}}
	ch <- []detect.Event{{Type: detect.EventDeleted, AbsPath: "/c"}}

	out := coalesceDetectorBatches(ctx, ch, []detect.Event{{Type: detect.EventUnknown, AbsPath: "/z"}})
	if len(out) != 4 {
		t.Fatalf("len(out)=%d, want=4", len(out))
	}
	if len(ch) != 0 {
		t.Fatalf("queue not drained, remaining=%d", len(ch))
	}
}

func TestConsumeDetectorEventsSoak(t *testing.T) {
	if os.Getenv("FILEGATE_SOAK") != "1" {
		t.Skip("set FILEGATE_SOAK=1 to run soak test")
	}

	duration := durationFromEnv("FILEGATE_SOAK_DURATION", 45*time.Second)
	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller := detect.NewPoller([]string{root}, 20*time.Millisecond)
	poller.Start(ctx)
	defer poller.Close()

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		consumeDetectorEvents(ctx, svc, poller.Events(), nil, 0)
	}()
	defer func() {
		cancel()
		<-consumerDone
	}()

	type entry struct {
		exists bool
		size   int
	}
	truth := make(map[string]entry)
	var mu sync.Mutex

	start := time.Now()
	i := 0
	for time.Since(start) < duration {
		name := fmt.Sprintf("soak-%03d.dat", i%180)
		abs := filepath.Join(root, name)

		switch i % 3 {
		case 0, 1:
			payload := strings.Repeat("x", (i%512)+1)
			if err := os.WriteFile(abs, []byte(payload), 0o644); err != nil {
				t.Fatalf("write %q: %v", abs, err)
			}
			mu.Lock()
			truth[name] = entry{exists: true, size: len(payload)}
			mu.Unlock()
		default:
			_ = os.Remove(abs)
			mu.Lock()
			truth[name] = entry{exists: false, size: 0}
			mu.Unlock()
		}

		i++
		time.Sleep(5 * time.Millisecond)
	}

	if err := poller.ForceRescan(ctx); err != nil {
		t.Fatalf("force rescan: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for name, want := range truth {
			id, err := svc.ResolvePath(rootName + "/" + name)
			if !want.exists {
				if err != domain.ErrNotFound {
					return false
				}
				continue
			}
			if err != nil {
				return false
			}
			meta, err := svc.GetFile(id)
			if err != nil {
				return false
			}
			if meta.Size != int64(want.size) {
				return false
			}
		}
		return true
	})
}

func TestConsumeDetectorEventsChaos(t *testing.T) {
	if os.Getenv("FILEGATE_CHAOS") != "1" {
		t.Skip("set FILEGATE_CHAOS=1 to run chaos test")
	}

	duration := durationFromEnv("FILEGATE_CHAOS_DURATION", 60*time.Second)
	root := t.TempDir()
	svc, cleanup := newDetectorTestService(t, []string{root})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan []detect.Event, 16384)
	go consumeDetectorEvents(ctx, svc, ch, nil, 0)

	for i := 0; i < 1200; i++ {
		name := fmt.Sprintf("chaos-seed-%04d.bin", i)
		abs := filepath.Join(root, name)
		if err := os.WriteFile(abs, []byte("seed"), 0o644); err != nil {
			t.Fatalf("seed write %q: %v", abs, err)
		}
	}

	rnd := rand.New(rand.NewSource(1337))
	start := time.Now()
	ops := 0
	for time.Since(start) < duration {
		batch := make([]detect.Event, 0, 256)
		for j := 0; j < 120; j++ {
			name := fmt.Sprintf("chaos-%04d.bin", rnd.Intn(3000))
			abs := filepath.Join(root, name)
			mode := rnd.Intn(4)
			switch mode {
			case 0, 1:
				payload := strings.Repeat("c", rnd.Intn(2048)+1)
				if err := os.WriteFile(abs, []byte(payload), 0o644); err == nil {
					batch = append(batch, detect.Event{Type: detect.EventCreated, Base: root, AbsPath: abs, IsDir: false})
				}
			case 2:
				_ = os.Remove(abs)
				batch = append(batch, detect.Event{Type: detect.EventDeleted, Base: root, AbsPath: abs, IsDir: false})
			default:
				batch = append(batch, detect.Event{Type: detect.EventChanged, Base: root, AbsPath: abs, IsDir: false})
			}
			if rnd.Intn(5) == 0 {
				batch = append(batch, detect.Event{Type: detect.EventUnknown, Base: root, AbsPath: root, IsDir: true})
			}
		}
		ch <- batch
		ops += len(batch)
		time.Sleep(10 * time.Millisecond)
	}

	ch <- []detect.Event{{Type: detect.EventUnknown, Base: root, AbsPath: root, IsDir: true}}
	waitUntil(t, 20*time.Second, func() bool {
		return svc.RescanMount(root) == nil
	})

	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("chaos-seed-%04d.bin", i)
		if _, err := svc.ResolvePath(rootName + "/" + name); err != nil {
			t.Fatalf("seed file missing after chaos (%d ops): %s err=%v", ops, name, err)
		}
	}
	close(ch)
}

func TestConsumeDetectorEventsWithRealBTRFS(t *testing.T) {
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
	defer func() {
		_, _ = exec.Command("btrfs", "subvolume", "delete", subvol).CombinedOutput()
		_ = os.RemoveAll(subvol)
	}()

	svc, cleanup := newDetectorTestService(t, []string{subvol})
	defer cleanup()
	rootName := mustMountNameByPath(t, svc, subvol)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner, err := detect.New("btrfs", []string{subvol}, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("new btrfs detector: %v", err)
	}
	runner.Start(ctx)
	defer runner.Close()
	go consumeDetectorEvents(ctx, svc, runner.Events(), nil, 0)

	target := filepath.Join(subvol, "real-btrfs.txt")
	if err := os.WriteFile(target, []byte("one"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Under containerized loopback btrfs the detector initialization can race with
	// the first write. Keep generating real fs updates until the path appears.
	waitForResolveWithStimulus(t, 15*time.Second, 150*time.Millisecond, func() {
		_ = os.WriteFile(target, []byte("one"), 0o644)
	}, func() bool {
		_, err := svc.ResolvePath(rootName + "/real-btrfs.txt")
		return err == nil
	})

	if err := os.WriteFile(target, []byte("two-two"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		id, err := svc.ResolvePath(rootName + "/real-btrfs.txt")
		if err != nil {
			return false
		}
		meta, err := svc.GetFile(id)
		if err != nil {
			return false
		}
		return meta.Size == int64(len("two-two"))
	})

	if err := os.Remove(target); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, err := svc.ResolvePath(rootName + "/real-btrfs.txt")
		return err == domain.ErrNotFound
	})
}

func waitForResolveWithStimulus(t *testing.T, timeout, interval time.Duration, stimulate func(), cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		stimulate()
		time.Sleep(interval)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func newDetectorTestService(t *testing.T, roots []string) (*domain.Service, func()) {
	t.Helper()
	idx, err := indexpebble.Open(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	svc, err := domain.NewService(idx, filesystem.New(), eventbus.New(), roots, 20000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	return svc, func() { _ = idx.Close() }
}

func mustMountNameByPath(t *testing.T, svc *domain.Service, absPath string) string {
	t.Helper()
	want := filepath.Clean(absPath)
	for _, m := range svc.ListRoot() {
		resolved, err := svc.ResolveAbsPath(m.ID)
		if err != nil {
			continue
		}
		if filepath.Clean(resolved) == want {
			return m.Name
		}
	}
	t.Fatalf("mount path %q not found", want)
	return ""
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fn() {
		return
	}
	t.Fatalf("condition not met within %s", timeout)
}

func durationFromEnv(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}
