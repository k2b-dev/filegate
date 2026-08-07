package eventbus

import (
	"log"
	"sync"

	"github.com/k2b-dev/filegate/v3/domain"
)

// InMemory is an in-memory event bus with bounded asynchronous handler dispatch.
type InMemory struct {
	mu     sync.RWMutex
	subs   map[domain.EventType][]func(domain.Event)
	closed bool

	parallelism chan struct{}

	// wg tracks in-flight handlers (async and inline). Close observes it
	// via wg.Wait to drain cleanly.
	wg sync.WaitGroup
}

// New creates an InMemory event bus ready for use.
func New() *InMemory {
	return &InMemory{
		subs:        make(map[domain.EventType][]func(domain.Event)),
		parallelism: make(chan struct{}, 64),
	}
}

// Publish dispatches event to all registered handlers. After Close has been
// called Publish becomes a no-op and is safe to call (callers do not need
// to coordinate with shutdown).
func (b *InMemory) Publish(event domain.Event) {
	// The closed-check and wg.Add must both happen inside the read-locked
	// section: Close flips the flag under the write lock, so a Publish
	// that passed the check has finished its Add before Close starts
	// wg.Wait. Checking the flag outside the lock allowed handlers to be
	// spawned after Close had already returned.
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	handlers := append([]func(domain.Event){}, b.subs[event.Type]...)
	handlers = append(handlers, b.subs["*"]...)
	b.wg.Add(len(handlers))
	b.mu.RUnlock()

	for _, h := range handlers {
		select {
		case b.parallelism <- struct{}{}:
			go func(handler func(domain.Event)) {
				defer b.wg.Done()
				defer func() { <-b.parallelism }()
				safeInvoke(event, handler)
			}(h)
		default:
			// Backpressure fallback: run inline when the async budget is saturated.
			safeInvoke(event, h)
			b.wg.Done()
		}
	}
}

func safeInvoke(event domain.Event, handler func(domain.Event)) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[filegate] event handler panic type=%s: %v", event.Type, rec)
		}
	}()
	handler(event)
}

// Subscribe registers handler for events of the given type. Use the "*"
// sentinel to subscribe to all events.
func (b *InMemory) Subscribe(eventType domain.EventType, handler func(domain.Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], handler)
}

// Close marks the bus as closed (Publish becomes a no-op) and waits for
// any in-flight handlers to finish. Idempotent.
func (b *InMemory) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	b.wg.Wait()
}
