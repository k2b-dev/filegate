//go:build linux

package domain_test

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func newCountingService(t *testing.T) (*domain.Service, *countingIndex, func()) {
	t.Helper()

	baseDir := t.TempDir()
	idx, err := indexpebble.Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	counter := &countingIndex{Index: idx}
	svc, err := domain.NewService(counter, filesystem.New(), eventbus.New(), []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	return svc, counter, func() { _ = idx.Close() }
}

// countingIndex counts index write batches.
//
// Every batch commits with pebble.Sync, so the count is the number of durable
// writes -- the quantity that dominates commit cost and the one thing about it
// that does not move with machine load. Wall-clock on a shared machine cannot
// settle a question like "one write instead of eight"; a counter can.
type countingIndex struct {
	domain.Index
	batches atomic.Int64
}

func (c *countingIndex) Batch(fn func(domain.Batch) error) error {
	c.batches.Add(1)
	return c.Index.Batch(fn)
}

// Creating a directory chain costs one durable write, not one per level.
//
// The chain is created together and its levels are only reachable through each
// other, so indexing them in one batch is both cheaper and a stronger guarantee
// than a chain another reader can observe half-indexed. Committing an upload into
// a tree that does not exist yet -- what every file in a node-modules-shaped
// corpus does -- paid one synced write per level before.
func TestCreatingADirectoryChainCostsOneDurableWrite(t *testing.T) {
	for _, depth := range []int{1, 2, 5, 8} {
		t.Run(fmt.Sprintf("depth%d", depth), func(t *testing.T) {
			svc, counter, cleanup := newCountingService(t)
			defer cleanup()

			root := svc.ListRoot()[0]
			rel := ""
			for i := range depth {
				if rel != "" {
					rel += "/"
				}
				rel += fmt.Sprintf("level%d", i)
			}

			counter.batches.Store(0)
			if _, err := svc.MkdirRelative(root.ID, rel, true, nil, domain.ConflictError); err != nil {
				t.Fatalf("mkdir %s: %v", rel, err)
			}
			got := counter.batches.Load()

			// One write for the chain. Anything that scales with depth means the
			// per-level path was taken.
			if got != 1 {
				t.Errorf("depth %d took %d durable writes, want 1", depth, got)
			}

			// Cheap is worthless if the result is wrong: every level has to
			// resolve at its own depth.
			prefix := ""
			for i := range depth {
				if prefix != "" {
					prefix += "/"
				}
				prefix += fmt.Sprintf("level%d", i)
				if _, err := svc.ResolvePath(root.Name + "/" + prefix); err != nil {
					t.Errorf("%s does not resolve: %v", prefix, err)
				}
			}
		})
	}
}

// Reusing a directory costs no write at all.
//
// The second file into the same directory is the common case in any real upload,
// and re-creating the chain to discover it already exists is work with no result.
func TestReusingADirectoryChainCostsNoDurableWrite(t *testing.T) {
	svc, counter, cleanup := newCountingService(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	if _, err := svc.MkdirRelative(root.ID, "a/b/c", true, nil, domain.ConflictError); err != nil {
		t.Fatalf("first mkdir: %v", err)
	}

	counter.batches.Store(0)
	if _, err := svc.MkdirRelative(root.ID, "a/b/c", true, nil, domain.ConflictSkip); err != nil {
		t.Fatalf("second mkdir: %v", err)
	}
	if got := counter.batches.Load(); got != 0 {
		t.Errorf("re-creating an existing chain took %d durable writes, want 0", got)
	}
}

// Writing a file into a fresh chain stays proportional to the file, not the
// depth of the tree it lands in.
func TestWritingIntoAFreshChainDoesNotScaleWithDepth(t *testing.T) {
	counts := make(map[int]int64)
	for _, depth := range []int{2, 8} {
		svc, counter, cleanup := newCountingService(t)

		root := svc.ListRoot()[0]
		rel := ""
		for i := range depth {
			if rel != "" {
				rel += "/"
			}
			rel += fmt.Sprintf("d%d", i)
		}

		counter.batches.Store(0)
		if _, _, err := svc.WriteContentByVirtualPath(
			root.Name+"/"+rel+"/file.txt",
			bytes.NewReader([]byte("payload")),
			domain.ConflictOverwrite,
		); err != nil {
			cleanup()
			t.Fatalf("depth %d write: %v", depth, err)
		}
		counts[depth] = counter.batches.Load()
		cleanup()
	}

	// Four extra levels must not cost four extra durable writes. An exact match
	// is not required -- the file's own writes dominate -- but the difference
	// has to be flat rather than tracking depth.
	if diff := counts[8] - counts[2]; diff > 1 {
		t.Errorf("depth 2 took %d durable writes and depth 8 took %d; the chain is still written per level",
			counts[2], counts[8])
	}
}
