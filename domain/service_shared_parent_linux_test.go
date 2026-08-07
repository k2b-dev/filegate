//go:build linux

package domain_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// Concurrent writes that create sibling directories under a shared parent must
// all resolve afterwards.
//
// They did not, because indexing a path took the parent's xattr as proof that
// the parent was in the index. An ID is claimed on the file and written to the
// index as two separate steps, so a sibling request landing between them read
// the ID, skipped indexing the parent, and anchored its child to an entity
// that did not exist yet. Walking that parent chain answered not-found, which
// surfaced as a 404 from PUT /v1/paths for a directory the caller was creating
// itself -- 22 to 95 failures per 5000 files at 128 requests in flight.
func TestConcurrentWritesUnderAFreshSharedParent(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	mount := svc.ListRoot()[0].Name
	const workers = 32

	for round := range 25 {
		// A parent that does not exist yet, so every worker races to index it.
		parent := fmt.Sprintf("round-%d/shared", round)

		errs := make([]error, workers)
		paths := make([]string, workers)
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(workers)

		for i := range workers {
			paths[i] = fmt.Sprintf("%s/%s/leaf-%d/file.txt", mount, parent, i)
			go func() {
				defer done.Done()
				start.Wait()
				_, _, errs[i] = svc.WriteContentByVirtualPath(paths[i], bytes.NewReader([]byte("x")), domain.ConflictOverwrite)
			}()
		}
		start.Done()
		done.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, worker %d writing %s: %v", round, i, paths[i], err)
			}
		}

		// Writing is not enough: the parent's identity has to be consistent
		// afterwards, which is what the index edges depend on.
		parentID, err := svc.ResolvePath(mount + "/" + parent)
		if err != nil {
			t.Fatalf("round %d: shared parent does not resolve: %v", round, err)
		}
		for _, path := range paths {
			if _, err := svc.ResolvePath(path); err != nil {
				t.Fatalf("round %d: %s written but does not resolve: %v", round, path, err)
			}
		}

		listed, err := svc.ListNodeChildren(parentID, "", 1000, false)
		if err != nil {
			t.Fatalf("round %d: list children: %v", round, err)
		}
		if len(listed.Items) != workers {
			t.Fatalf("round %d: parent has %d children, want %d; edges were written under different parent ids", round, len(listed.Items), workers)
		}
	}
}

// A directory chain with a hole in it must not be chained across the hole.
//
// MkdirRelative reports the levels it created, and under concurrency that list
// can skip a middle level: another request creates it between this one's lstat
// and its mkdir. Indexing such a list as one chain anchors the deepest directory
// to its grandparent, which silently drops a path component -- writes then land
// at the wrong place rather than failing. The fix keeps only the trailing
// contiguous run, so this asserts the resulting paths, which is what a lost
// component would change.
func TestDirectoriesResolveWhenAnIntermediateLevelIsCreatedConcurrently(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	mount := svc.ListRoot()[0].Name

	// Two workers race into the same three-level chain from opposite ends of the
	// middle level, which is the shape that produced a skipped level.
	for round := range 40 {
		top := fmt.Sprintf("hole-%d", round)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, errs[0] = svc.WriteContentByVirtualPath(
				fmt.Sprintf("%s/%s/mid/leaf-a/file.txt", mount, top),
				bytes.NewReader([]byte("a")), domain.ConflictOverwrite)
		}()
		go func() {
			defer wg.Done()
			_, _, errs[1] = svc.WriteContentByVirtualPath(
				fmt.Sprintf("%s/%s/mid/leaf-b/file.txt", mount, top),
				bytes.NewReader([]byte("b")), domain.ConflictOverwrite)
		}()
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d worker %d: %v", round, i, err)
			}
		}

		// Every level has to resolve at its real depth. A chain written across a
		// hole would leave leaf-a or leaf-b hanging directly off top.
		for _, rel := range []string{
			top, top + "/mid", top + "/mid/leaf-a", top + "/mid/leaf-b",
			top + "/mid/leaf-a/file.txt", top + "/mid/leaf-b/file.txt",
		} {
			if _, err := svc.ResolvePath(mount + "/" + rel); err != nil {
				t.Fatalf("round %d: %s does not resolve: %v", round, rel, err)
			}
		}
		if _, err := svc.ResolvePath(mount + "/" + top + "/leaf-a"); err == nil {
			t.Fatalf("round %d: leaf-a resolves directly under %s, so a path component was dropped", round, top)
		}
	}
}
