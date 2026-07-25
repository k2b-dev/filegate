package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestStatsReportsPoolShape(t *testing.T) {
	s := New(2, 16)
	defer func() { _ = s.Close(context.Background()) }()

	got := s.Stats()
	if got.Workers != 2 {
		t.Errorf("workers = %d, want 2", got.Workers)
	}
	if got.QueueCapacity != 16 {
		t.Errorf("queue capacity = %d, want 16", got.QueueCapacity)
	}
	if got.Queued != 0 {
		t.Errorf("queued = %d, want 0 on an idle scheduler", got.Queued)
	}
}

func TestStatsCountsQueueFullRejections(t *testing.T) {
	// One worker, one queue slot: block the worker, fill the slot, and the next
	// submission has nowhere to go. That rejection is invisible today except to
	// the caller that hit it.
	s := New(1, 1)
	defer func() { _ = s.Close(context.Background()) }()

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = s.Do(context.Background(), "blocker", func(context.Context) (any, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started
	defer close(release)

	// Submit without waiting for results: Do would block on the busy worker.
	noop := func(context.Context) (any, error) { return nil, nil }
	var rejected bool
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; !rejected && time.Now().Before(deadline); i++ {
		_, _, err := s.getOrSubmit(fmt.Sprintf("overflow-%d", i), noop)
		rejected = errors.Is(err, ErrQueueFull)
	}
	if !rejected {
		t.Fatal("never observed ErrQueueFull")
	}

	if got := s.Stats().Rejected; got == 0 {
		t.Error("rejected counter stayed 0 after ErrQueueFull")
	}
}

func TestStatsCountsPanics(t *testing.T) {
	s := New(1, 4)
	defer func() { _ = s.Close(context.Background()) }()

	_, err := s.Do(context.Background(), "boom", func(context.Context) (any, error) {
		panic("boom")
	})
	if err == nil {
		t.Fatal("expected the panic to surface as an error")
	}
	if got := s.Stats().Panics; got != 1 {
		t.Errorf("panics = %d, want 1", got)
	}
}

func TestStatsOnNilScheduler(t *testing.T) {
	var s *Scheduler
	if got := s.Stats(); got.Workers != 0 || got.QueueCapacity != 0 {
		t.Errorf("nil scheduler stats = %+v, want zero value", got)
	}
}
