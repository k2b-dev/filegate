package domain

import (
	"sync/atomic"
	"testing"
	"time"
)

// Recovery can wait on filesystem/state I/O while holding the root mutex.
// Its duration must not reduce the lifetime of a session created afterward.
func TestCreateSessionLifetimeStartsAfterRecovery(t *testing.T) {
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	state := &sessionRecoveryGate{entered: make(chan struct{}), release: make(chan struct{})}
	root := &Root{
		Config:   RootConfig{Name: "test"},
		State:    state,
		MaxBytes: 1024,
		rootShared: &rootShared{needsRecovery: true,
			now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }},
	}
	type outcome struct {
		session Session
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		session, err := root.CreateSession("file", 1, WriteOptions{}, "")
		done <- outcome{session, err}
	}()
	<-state.entered
	resumed := start.Add(2 * time.Hour)
	clock.Store(resumed.UnixNano())
	close(state.release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !result.session.Expires.Equal(resumed.Add(SessionLifetime)) {
		t.Fatalf("recovery consumed session lifetime: expires %s, want %s", result.session.Expires, resumed.Add(SessionLifetime))
	}
}

type sessionRecoveryGate struct {
	State
	entered chan struct{}
	release chan struct{}
}

func (s *sessionRecoveryGate) Scan(prefix string, _ func(string, []byte) error) error {
	if prefix == "pending/" {
		close(s.entered)
		<-s.release
	}
	return nil
}
func (s *sessionRecoveryGate) Put(string, any) error { return nil }
func (s *sessionRecoveryGate) Delete(string) error   { return nil }

func (s *sessionRecoveryGate) Batch([]Change) error { return nil }
