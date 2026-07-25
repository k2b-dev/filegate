package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v4"
)

var (
	ErrQueueFull    = errors.New("job queue full")
	ErrClosed       = errors.New("scheduler closed")
	ErrCloseTimeout = errors.New("scheduler close timed out")
)

// JobFunc is the signature for a background job executed by the Scheduler.
type JobFunc func(context.Context) (any, error)

// Stats is a point-in-time view of scheduler pressure.
//
// Queued against QueueCapacity is the signal that matters operationally: once
// the queue fills, submissions are rejected with ErrQueueFull, which callers
// surface as 503. Rejected is cumulative since process start.
type Stats struct {
	Workers       int
	Queued        int
	QueueCapacity int
	InFlight      int
	Rejected      uint64
	Panics        uint64
}

// Scheduler is a bounded worker pool with keyed job deduplication.
type Scheduler struct {
	ctx    context.Context
	cancel context.CancelFunc

	queue    chan *jobCall
	inFlight *xsync.Map[string, *jobCall]

	mu     sync.RWMutex
	closed bool

	// activeWorkers counts live worker goroutines. The last worker to exit
	// closes workersDone so Close can observe completion without spawning a
	// helper goroutine that would leak if a stuck worker prevents the
	// counter from reaching zero.
	activeWorkers atomic.Int32
	workersDone   chan struct{}

	// Cumulative counters for observability. Rejected tracks ErrQueueFull,
	// which is otherwise only visible to the caller that hit it.
	rejected atomic.Uint64
	panics   atomic.Uint64
}

type jobCall struct {
	key string
	fn  JobFunc

	done chan struct{}
	val  any
	err  error

	// joiners counts callers that attached to this in-flight call instead of
	// submitting their own. Used by tests to confirm all goroutines reached
	// LoadOrStore before the call's function is allowed to complete.
	joiners atomic.Int32
}

// New creates a Scheduler with the given number of workers and queue capacity.
func New(workers, queueSize int) *Scheduler {
	if workers <= 0 {
		workers = 4
	}
	if queueSize <= 0 {
		queueSize = 2048
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		ctx:         ctx,
		cancel:      cancel,
		queue:       make(chan *jobCall, queueSize),
		inFlight:    xsync.NewMap[string, *jobCall](),
		workersDone: make(chan struct{}),
	}
	s.activeWorkers.Store(int32(workers))
	for range workers {
		go s.worker()
	}
	return s
}

func (s *Scheduler) Do(ctx context.Context, key string, fn JobFunc) (any, error) {
	call, _, err := s.getOrSubmit(key, fn)
	if err != nil {
		return nil, err
	}
	return waitForResult(ctx, call)
}

// Close cancels in-flight context, drains the queue (failing each waiter with
// ErrClosed), and waits for workers to exit. The supplied context bounds the
// wait: if it expires before workers finish (because a job ignores the
// canceled context), Close returns ErrCloseTimeout. Workers that survive the
// timeout are leaked, but the caller is unblocked.
//
// Concurrent submitters are blocked from enqueueing new jobs once Close has
// flipped the closed flag — see getOrSubmit. This prevents the race where a
// submission would land in the queue after Close has already drained it.
func (s *Scheduler) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.cancel()

drain:
	for {
		select {
		case call := <-s.queue:
			call.err = ErrClosed
			close(call.done)
			s.inFlight.Delete(call.key)
		default:
			break drain
		}
	}

	select {
	case <-s.workersDone:
		return nil
	case <-ctx.Done():
		return ErrCloseTimeout
	}
}

func (s *Scheduler) worker() {
	defer func() {
		// The last worker to exit signals completion. Close observes this
		// via s.workersDone instead of spawning a per-call goroutine that
		// could outlive a timed-out close.
		if s.activeWorkers.Add(-1) == 0 {
			close(s.workersDone)
		}
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case call := <-s.queue:
			s.runCall(call)
		}
	}
}

func (s *Scheduler) runCall(call *jobCall) {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			call.err = fmt.Errorf("job panic: %v\n%s", r, string(debug.Stack()))
		}
		close(call.done)
		s.inFlight.Delete(call.key)
	}()

	call.val, call.err = call.fn(s.ctx)
}

func (s *Scheduler) getOrSubmit(key string, fn JobFunc) (*jobCall, bool, error) {
	if fn == nil {
		return nil, false, errors.New("job function is nil")
	}
	if key == "" {
		return nil, false, errors.New("job key is required")
	}

	// Hold RLock across the closed-check, inFlight registration, and queue
	// send. Close takes the write lock before flipping the flag and draining
	// the queue, so anything that successfully enters the queue inside this
	// critical section is guaranteed to be either drained by Close (with
	// ErrClosed) or picked up by a still-running worker — never silently
	// orphaned.
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, false, ErrClosed
	}

	call := &jobCall{
		key:  key,
		fn:   fn,
		done: make(chan struct{}),
	}
	actual, loaded := s.inFlight.LoadOrStore(key, call)
	if loaded {
		actual.joiners.Add(1)
		return actual, false, nil
	}
	select {
	case s.queue <- call:
		return call, true, nil
	default:
		s.rejected.Add(1)
		call.err = ErrQueueFull
		close(call.done)
		s.inFlight.Delete(key)
		return nil, false, ErrQueueFull
	}
}

// Stats reports current pressure and cumulative rejections. Safe on a nil
// scheduler, which reports zeroes.
func (s *Scheduler) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		Workers:       int(s.activeWorkers.Load()),
		Queued:        len(s.queue),
		QueueCapacity: cap(s.queue),
		InFlight:      s.inFlight.Size(),
		Rejected:      s.rejected.Load(),
		Panics:        s.panics.Load(),
	}
}

func waitForResult(ctx context.Context, call *jobCall) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.val, call.err
	}
}
