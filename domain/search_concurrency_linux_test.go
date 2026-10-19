//go:build linux

package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v7/domain"
	"github.com/k2b-dev/filegate/v7/infra/filesystem"
	"github.com/k2b-dev/filegate/v7/infra/pebble"
)

type searchState struct {
	domain.State
	block  func()
	closed atomic.Int32
	visits atomic.Int32
}

func (s *searchState) Snapshot() (domain.StateSnapshot, error) {
	reader, err := s.State.Snapshot()
	if err != nil {
		return nil, err
	}
	return &searchSnapshot{StateSnapshot: reader, state: s}, nil
}

type searchSnapshot struct {
	domain.StateSnapshot
	state *searchState
}

func (s *searchSnapshot) visit(fn func(string, []byte) error) func(string, []byte) error {
	count := 0
	return func(k string, b []byte) error {
		count++
		s.state.visits.Add(1)
		if count == 2 && s.state.block != nil {
			s.state.block()
		}
		return fn(k, b)
	}
}
func (s *searchSnapshot) ScanAfter(prefix, after string, fn func(string, []byte) error) error {
	return s.StateSnapshot.ScanAfter(prefix, after, s.visit(fn))
}
func (s *searchSnapshot) ScanBefore(prefix, before string, fn func(string, []byte) error) error {
	return s.StateSnapshot.ScanBefore(prefix, before, s.visit(fn))
}
func (s *searchSnapshot) Close() error {
	s.state.closed.Add(1)
	return s.StateSnapshot.Close()
}

func newSearchRoot(t *testing.T) (*domain.Root, *searchState) {
	t.Helper()
	dir := t.TempDir()
	files, err := filesystem.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	state, err := pebble.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	s := &searchState{State: state}
	r, err := domain.NewRoot(domain.RootConfig{Name: "test", Path: dir, Index: true}, files, s, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"folder", "folder/a", "folder/b", "folder/c"} {
		if _, err := r.Mkdir(p, domain.DirectoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return r, s
}

type searchResult struct {
	page domain.Page
	err  error
}

func startSearch(r *domain.Root, ctx context.Context, base string, opts domain.ListingOptions) <-chan searchResult {
	done := make(chan searchResult, 1)
	go func() {
		defer close(done)
		page, err := r.Search(ctx, "", base, opts)
		done <- searchResult{page, err}
	}()
	return done
}

// Timeouts only bound failed tests; all ordering is established by channels.
func awaitSearchValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("search operation did not complete")
		var zero T
		return zero
	}
}

// Exercise both slots after failures. One leftover slot would still allow
// sequential queries, but would prevent these two requests from scanning.
// List and Search must also share the root's admission limit.
func assertSearchSlotsReusable(t *testing.T, r *domain.Root, state *searchState) {
	t.Helper()
	blocked, release := make(chan struct{}, 2), make(chan struct{})
	state.block = func() { blocked <- struct{}{}; <-release }
	unblock := sync.OnceFunc(func() { close(release) })
	first := startSearch(r, context.Background(), ".", domain.ListingOptions{})
	second := make(chan searchResult, 1)
	go func() {
		defer close(second)
		page, err := r.List(context.Background(), "folder", domain.ListingOptions{})
		second <- searchResult{page, err}
	}()
	t.Cleanup(func() { unblock(); awaitSearchValue(t, first); awaitSearchValue(t, second) })
	awaitSearchValue(t, blocked)
	awaitSearchValue(t, blocked)
	busy := startSearch(r, context.Background(), ".", domain.ListingOptions{})
	t.Cleanup(func() { unblock(); awaitSearchValue(t, busy) })
	if result := awaitSearchValue(t, busy); !errors.Is(result.err, domain.ErrSearchBusy) {
		t.Fatalf("List and Search did not share capacity: %+v", result)
	}
	unblock()
	if a, b := awaitSearchValue(t, first), awaitSearchValue(t, second); a.err != nil || b.err != nil {
		t.Fatalf("slots were not reusable: %v %v", a.err, b.err)
	}
}

func TestSearchScanAllowsWrites(t *testing.T) {
	for _, tc := range []struct{ base, sort, order string }{
		{".", "path", "asc"},
		{".", "size", "desc"},
		{"folder", "name", "asc"},
	} {
		t.Run(tc.base+"/"+tc.sort, func(t *testing.T) {
			r, state := newSearchRoot(t)
			blocked, release := make(chan struct{}), make(chan struct{})
			state.block = func() { close(blocked); <-release }
			unblock := sync.OnceFunc(func() { close(release) })
			done := startSearch(r, context.Background(), tc.base, domain.ListingOptions{Sort: tc.sort, Order: tc.order, Limit: 1})
			// Always unblock and join the search before closing its database,
			// including when a write times out and fails the test.
			t.Cleanup(func() { unblock(); awaitSearchValue(t, done) })
			awaitSearchValue(t, blocked)
			written := make(chan error, 1)
			go func() {
				defer close(written)
				_, err := r.Mkdir("probe", domain.DirectoryOptions{})
				written <- err
			}()
			t.Cleanup(func() { unblock(); awaitSearchValue(t, written) })
			if err := awaitSearchValue(t, written); err != nil {
				t.Fatal(err)
			}
			unblock()
			result := awaitSearchValue(t, done)
			if !errors.Is(result.err, domain.ErrCursorInvalid) || len(result.page.Items) != 0 || result.page.Next != "" || state.closed.Load() != 1 {
				t.Fatalf("stale observation served or leaked: %+v closed=%d", result, state.closed.Load())
			}
			assertSearchSlotsReusable(t, r, state)
		})
	}
}

func TestSearchScanCancellation(t *testing.T) {
	for _, tc := range []struct{ base, sort, order string }{
		{".", "path", "asc"},
		{".", "size", "desc"},
		{"folder", "name", "asc"},
	} {
		t.Run(tc.base+"/"+tc.sort, func(t *testing.T) {
			r, state := newSearchRoot(t)
			blocked, release := make(chan struct{}), make(chan struct{})
			state.block = func() { close(blocked); <-release }
			unblock := sync.OnceFunc(func() { close(release) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := startSearch(r, ctx, tc.base, domain.ListingOptions{Sort: tc.sort, Order: tc.order})
			t.Cleanup(func() { cancel(); unblock(); awaitSearchValue(t, done) })
			awaitSearchValue(t, blocked)
			cancel()
			unblock()
			result := awaitSearchValue(t, done)
			if !errors.Is(result.err, context.Canceled) || len(result.page.Items) != 0 || result.page.Next != "" || state.visits.Load() != 2 || state.closed.Load() != 1 {
				t.Fatalf("scan ignored cancellation or leaked: %+v visits=%d closed=%d", result, state.visits.Load(), state.closed.Load())
			}
			assertSearchSlotsReusable(t, r, state)
		})
	}
}

func TestSearchScanRejectsRebuild(t *testing.T) {
	r, state := newSearchRoot(t)
	blocked, release := make(chan struct{}), make(chan struct{})
	state.block = func() { close(blocked); <-release }
	unblock := sync.OnceFunc(func() { close(release) })
	done := startSearch(r, context.Background(), "folder", domain.ListingOptions{Sort: "name", Limit: 1})
	t.Cleanup(func() { unblock(); awaitSearchValue(t, done) })
	awaitSearchValue(t, blocked)
	rebuilt := make(chan error, 1)
	go func() {
		defer close(rebuilt)
		rebuilt <- r.Rebuild(context.Background())
	}()
	t.Cleanup(func() { unblock(); awaitSearchValue(t, rebuilt) })
	if err := awaitSearchValue(t, rebuilt); err != nil {
		t.Fatal(err)
	}
	unblock()
	result := awaitSearchValue(t, done)
	if !errors.Is(result.err, domain.ErrCursorInvalid) || len(result.page.Items) != 0 || result.page.Next != "" || state.closed.Load() != 1 {
		t.Fatalf("rebuilt observation served or leaked: %+v closed=%d", result, state.closed.Load())
	}
	assertSearchSlotsReusable(t, r, state)
}

func TestSearchConcurrencyBusy(t *testing.T) {
	r, state := newSearchRoot(t)
	blocked, release := make(chan struct{}, 2), make(chan struct{})
	state.block = func() { blocked <- struct{}{}; <-release }
	unblock := sync.OnceFunc(func() { close(release) })
	first := startSearch(r, context.Background(), ".", domain.ListingOptions{})
	second := startSearch(r, context.Background(), "folder", domain.ListingOptions{Sort: "name"})
	t.Cleanup(func() { unblock(); awaitSearchValue(t, first); awaitSearchValue(t, second) })
	awaitSearchValue(t, blocked)
	awaitSearchValue(t, blocked)
	third := startSearch(r, context.Background(), ".", domain.ListingOptions{})
	t.Cleanup(func() { unblock(); awaitSearchValue(t, third) })
	busy := awaitSearchValue(t, third)
	unblock()
	a, b := awaitSearchValue(t, first), awaitSearchValue(t, second)
	if !errors.Is(busy.err, domain.ErrSearchBusy) || len(busy.page.Items) != 0 || busy.page.Next != "" || a.err != nil || b.err != nil || state.closed.Load() != 2 {
		t.Fatalf("admission failed: busy=%+v first=%v second=%v closed=%d", busy, a.err, b.err, state.closed.Load())
	}
	assertSearchSlotsReusable(t, r, state)
}

func TestSearchStateSnapshotConsistent(t *testing.T) {
	_, state := newSearchRoot(t)
	for _, key := range []string{"test/a", "test/b"} {
		if err := state.Put(key, "old"); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := state.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := state.Delete("test/a"); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("test/b", "new"); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("test/c", "new"); err != nil {
		t.Fatal(err)
	}
	for _, scan := range []func(string, string, func(string, []byte) error) error{snapshot.ScanAfter, snapshot.ScanBefore} {
		count := 0
		if err := scan("test/", "", func(k string, b []byte) error {
			count++
			var indexed, primary string
			if err := json.Unmarshal(b, &indexed); err != nil {
				return err
			}
			if err := snapshot.Get(k, &primary); err != nil {
				return err
			}
			if indexed != "old" || primary != indexed {
				t.Fatalf("mixed snapshot observations: key=%s index=%q primary=%q", k, indexed, primary)
			}
			return nil
		}); err != nil || count != 2 {
			t.Fatalf("snapshot scan: count=%d err=%v", count, err)
		}
	}
	var missing string
	if err := snapshot.Get("test/c", &missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new row leaked into snapshot: %v", err)
	}
}
