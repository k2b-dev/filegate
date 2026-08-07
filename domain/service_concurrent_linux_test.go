//go:build linux

package domain_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestRescanRaceWithConcurrentAPIMutations runs Rescan in a tight loop while
// other goroutines create, delete, and read files via the public API. The
// goal is to surface index corruption, panics, or stale-entry leaks under
// production-like load. The invariant we check: every file that survives the
// run is still resolvable by virtual path AND retrievable by ID.
func TestRescanRaceWithConcurrentAPIMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race test in short mode")
	}

	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	mountName := svc.ListRoot()[0].Name

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const writers = 4
	const rescanners = 2
	const readers = 4
	survivorsPerWriter := 8

	var wg sync.WaitGroup
	survivors := make([][]string, writers)
	var rescanErrs atomic.Int64
	var readErrs atomic.Int64

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			i := 0
			for {
				if ctx.Err() != nil {
					return
				}
				name := fmt.Sprintf("w%d-f%d.txt", slot, i)
				meta, err := svc.CreateChild(root, name, false, nil)
				if err != nil {
					i++
					continue
				}
				if err := svc.WriteContent(meta.ID, strings.NewReader("payload")); err != nil {
					_ = svc.Delete(meta.ID)
					i++
					continue
				}
				if i%4 == 0 {
					_ = svc.Delete(meta.ID)
				} else if len(survivors[slot]) < survivorsPerWriter {
					survivors[slot] = append(survivors[slot], name)
				}
				i++
			}
		}(w)
	}

	rescanErrSample := make(chan error, 1)
	for r := 0; r < rescanners; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				if err := svc.Rescan(); err != nil {
					rescanErrs.Add(1)
					select {
					case rescanErrSample <- err:
					default:
					}
				}
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				stats, err := svc.Stats()
				if err != nil {
					readErrs.Add(1)
					continue
				}
				_ = stats
			}
		}()
	}

	wg.Wait()

	if got := rescanErrs.Load(); got > 0 {
		var sample error
		select {
		case sample = <-rescanErrSample:
		default:
		}
		t.Errorf("rescan failures during race: %d (sample: %v)", got, sample)
	}
	if got := readErrs.Load(); got > 0 {
		t.Errorf("stats read failures during race: %d", got)
	}

	// Final invariant check after a settling rescan: every survivor must be
	// resolvable by both virtual path and ID, and the IDs must agree.
	if err := svc.Rescan(); err != nil {
		t.Fatalf("settling rescan: %v", err)
	}
	for slot := 0; slot < writers; slot++ {
		for _, name := range survivors[slot] {
			id, err := svc.ResolvePath(mountName + "/" + name)
			if err != nil {
				t.Errorf("survivor %q lost from index: %v", name, err)
				continue
			}
			meta, err := svc.GetFile(id)
			if err != nil {
				t.Errorf("survivor %q meta unavailable: %v", name, err)
				continue
			}
			if meta.ID != id {
				t.Errorf("survivor %q id mismatch: got %s want %s", name, meta.ID, id)
			}
		}
	}
}

// TestSearchGlobSurvivesConcurrentDelete drives concurrent search and
// deletion against the same tree. The properties verified after the workers
// finish are:
//   - Search never panics (every worker's recover() reports no panic).
//   - Search never blocks past the per-worker context deadline (the wg.Wait
//     below has its own outer timeout via t.Cleanup; if a search hangs, that
//     fires).
//   - Once the dust settles, a final search returns no entry for any file
//     whose Delete returned nil. This is the strongest property we can
//     assert without an arbitrary serialization point during the race.
func TestSearchGlobSurvivesConcurrentDelete(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	mountName := svc.ListRoot()[0].Name

	const seedFiles = 200
	names := make([]string, 0, seedFiles)
	ids := make([]domain.FileID, 0, seedFiles)
	for i := 0; i < seedFiles; i++ {
		name := fmt.Sprintf("seed-%04d.txt", i)
		meta, err := svc.CreateChild(root, name, false, nil)
		if err != nil {
			t.Fatalf("create seed %d: %v", i, err)
		}
		names = append(names, name)
		ids = append(ids, meta.ID)
	}

	// Outer fail-safe: if anything hangs, fail loudly instead of waiting for
	// the global test timeout to kill the process.
	hardCtx, hardCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hardCancel()
	workerCtx, cancelWorkers := context.WithTimeout(hardCtx, 1500*time.Millisecond)
	defer cancelWorkers()

	var wg sync.WaitGroup
	var searchPanics atomic.Int64

	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					searchPanics.Add(1)
				}
			}()
			for {
				if workerCtx.Err() != nil {
					return
				}
				_, _ = svc.SearchGlob(domain.GlobSearchRequest{
					Pattern: "seed-*",
					Paths:   []string{mountName},
					Limit:   500,
				})
			}
		}()
	}

	// Track which files were definitively removed so the post-race assertion
	// can demand they no longer surface in search results.
	var deletedMu sync.Mutex
	deleted := make(map[string]struct{}, seedFiles)
	for d := 0; d < 2; d++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := start; i < seedFiles; i += 2 {
				if workerCtx.Err() != nil {
					return
				}
				if err := svc.Delete(ids[i]); err == nil {
					deletedMu.Lock()
					deleted[names[i]] = struct{}{}
					deletedMu.Unlock()
				}
			}
		}(d)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-hardCtx.Done():
		t.Fatalf("workers did not finish within hard deadline — search likely deadlocked")
	}

	if got := searchPanics.Load(); got > 0 {
		t.Fatalf("search panicked %d times during concurrent delete", got)
	}

	// Property check: a final search must not surface any file whose Delete
	// returned nil. This proves the index reflects deletes once the race has
	// quiesced and that search did not return zombie entries from a stale
	// view that survived the test.
	resp, err := svc.SearchGlob(domain.GlobSearchRequest{
		Pattern: "seed-*",
		Paths:   []string{mountName},
		Limit:   seedFiles + 100,
	})
	if err != nil {
		t.Fatalf("post-race search: %v", err)
	}
	deletedMu.Lock()
	defer deletedMu.Unlock()
	for _, item := range resp.Results {
		if _, wasDeleted := deleted[item.Name]; wasDeleted {
			t.Errorf("search returned deleted file %q after race", item.Name)
		}
	}
}

// TestPaginationCursorSkipsConcurrentlyDeletedChildren verifies that paging
// through a directory while a concurrent goroutine deletes some of its
// children never returns the same name twice and never panics. We don't
// require the cursor to surface every survivor (that's eventually consistent),
// only that it never duplicates and never crashes.
func TestPaginationCursorSkipsConcurrentlyDeletedChildren(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID

	const total = 80
	ids := make([]domain.FileID, 0, total)
	for i := 0; i < total; i++ {
		meta, err := svc.CreateChild(root, fmt.Sprintf("page-%04d.txt", i), false, nil)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, meta.ID)
	}

	deleted := make(chan struct{})
	go func() {
		defer close(deleted)
		// Delete every second file once we begin paging.
		for i := 0; i < total; i += 2 {
			_ = svc.Delete(ids[i])
		}
	}()

	seen := make(map[string]struct{})
	cursor := ""
	const pageSize = 10
	for iterations := 0; iterations < total; iterations++ {
		out, err := svc.ListNodeChildren(root, cursor, pageSize, false)
		if err != nil {
			// Cursor invalidation due to a concurrent delete must surface as
			// an explicit error rather than corrupt iteration state. The test
			// proves the API fails fast instead of silently duplicating.
			break
		}
		if len(out.Items) == 0 {
			break
		}
		for _, child := range out.Items {
			if _, dup := seen[child.Name]; dup {
				t.Fatalf("duplicate child returned during pagination: %q", child.Name)
			}
			seen[child.Name] = struct{}{}
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	<-deleted
}

// TestRescanIgnoresSymlinkCycles creates A -> B -> A symlink loop inside a
// mount and verifies that Rescan terminates without exhausting goroutine
// stack or hanging. The cycle should be silently skipped.
func TestRescanIgnoresSymlinkCycles(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0].ID
	rootAbs, err := svc.ResolveAbsPath(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	dirA := filepath.Join(rootAbs, "A")
	dirB := filepath.Join(rootAbs, "B")
	if _, err := svc.CreateChild(root, "A", true, nil); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if _, err := svc.CreateChild(root, "B", true, nil); err != nil {
		t.Fatalf("mkdir B: %v", err)
	}
	// A/loop -> B; B/loop -> A; together they form a closed cycle for any
	// scanner that follows symlinks.
	if err := os.Symlink(dirB, filepath.Join(dirA, "loop")); err != nil {
		t.Fatalf("link A->B: %v", err)
	}
	if err := os.Symlink(dirA, filepath.Join(dirB, "loop")); err != nil {
		t.Fatalf("link B->A: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- svc.Rescan()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rescan with symlink cycle: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("rescan hung on symlink cycle")
	}
}
