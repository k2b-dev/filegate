//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v6/domain"
)

func createFilledSession(t *testing.T, x *fixture, name, content string) domain.Session {
	t.Helper()
	s, e := x.r.CreateSession(name, int64(len(content)), domain.WriteOptions{}, "")
	if e != nil {
		t.Fatal(e)
	}
	if content != "" {
		s, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader(content))
		if e != nil {
			t.Fatal(e)
		}
	}
	return s
}

func TestSessionReceiptSurvivesFileChangesAndOriginalExpiry(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "filesystem", true: "indexed"}[indexed], func(t *testing.T) {
			x := setup(t, indexed, false)
			s := createFilledSession(t, x, "original", "hello")
			result, e := x.r.CommitSession(ctx, s.ID)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = x.r.Move("original", "moved"); e != nil {
				t.Fatal(e)
			}
			if e = x.r.Remove("moved", false); e != nil {
				t.Fatal(e)
			}
			receipt, e := x.r.Session(s.ID)
			if e != nil {
				t.Fatal(e)
			}
			receipt.Expires = time.Now().Add(-time.Hour)
			if e = x.state.Put("done/"+s.ID, receipt); e != nil {
				t.Fatal(e)
			}
			reopen(t, x)
			again, e := x.r.CommitSession(ctx, s.ID)
			if e != nil || !reflect.DeepEqual(result, again) {
				t.Fatalf("frozen result changed: %+v %+v %v", result, again, e)
			}
			receipt, e = x.r.Session(s.ID)
			if e != nil || receipt.State != domain.SessionCommitted || receipt.Received != 5 || receipt.UploadedSegments != 1 {
				t.Fatalf("receipt %+v %v", receipt, e)
			}
			if receipt.TerminalAt == nil || receipt.RetainUntil == nil || receipt.RetainUntil.Sub(*receipt.TerminalAt) != domain.SessionRetention {
				t.Fatal("retention missing", receipt)
			}
			if e = x.r.AbortSession(s.ID); !errors.Is(e, domain.ErrSessionCommitted) {
				t.Fatal("abort erased commit", e)
			}
		})
	}
}

func TestSessionExpiryAndRetention(t *testing.T) {
	x := setup(t, false, false)
	s := createFilledSession(t, x, "expired", "abc")
	if time.Until(s.Expires) > domain.SessionLifetime || time.Until(s.Expires) < domain.SessionLifetime-time.Minute {
		t.Fatal("wrong lifetime")
	}
	s.Expires = time.Now().UTC().Add(-time.Minute)
	if e := x.state.Put("session/"+s.ID, s); e != nil {
		t.Fatal(e)
	}
	got, e := x.r.Session(s.ID)
	if e != nil || got.State != domain.SessionExpired || got.TerminalAt == nil || !got.TerminalAt.Equal(s.Expires) {
		t.Fatalf("expired %+v %v", got, e)
	}
	if _, e = x.r.CommitSession(ctx, s.ID); !errors.Is(e, domain.ErrSessionExpired) {
		t.Fatal(e)
	}
	if e = x.r.AbortSession(s.ID); !errors.Is(e, domain.ErrSessionExpired) {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); !errors.Is(e, domain.ErrSessionExpired) {
		t.Fatal(e)
	}
	if e = x.r.CleanupSessions(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(x.data, ".filegate/staging", s.ID+"-0")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("expired segment retained", e)
	}
	reopen(t, x)
	if got, e = x.r.Session(s.ID); e != nil || got.State != domain.SessionExpired {
		t.Fatal(got, e)
	}
	past := time.Now().Add(-time.Minute)
	got.RetainUntil = &past
	if e = x.state.Put("done/"+s.ID, got); e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.Session(s.ID); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("past retention remains queryable", e)
	}
	if e = x.r.CleanupSessions(ctx); e != nil {
		t.Fatal(e)
	}
	if e = x.state.Get("done/"+s.ID, &got); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("receipt not collected", e)
	}
}

func TestEmptyAndSmallSessions(t *testing.T) {
	for _, content := range []string{"", "a"} {
		t.Run(map[bool]string{true: "empty", false: "small"}[content == ""], func(t *testing.T) {
			x := setup(t, false, false)
			s := createFilledSession(t, x, "file", content)
			n, e := x.r.CommitSession(ctx, s.ID)
			if e != nil || n.Size != int64(len(content)) || read(t, x.r, "file") != content {
				t.Fatal(n, e)
			}
			got, e := x.r.Session(s.ID)
			if e != nil || got.Result == nil || got.Result.Size != n.Size || got.Received != n.Size {
				t.Fatal(got, e)
			}
		})
	}
}

type sessionCleanupFailure struct {
	domain.Files
	fail    bool
	session string
}

func (f *sessionCleanupFailure) Remove(p string, recursive bool) error {
	if f.fail && strings.HasPrefix(p, ".filegate/staging/"+f.session+"-") {
		return os.ErrPermission
	}
	return f.Files.Remove(p, recursive)
}

func TestTerminalSessionSurvivesCleanupFailure(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "abort"}[commit], func(t *testing.T) {
			x := setup(t, false, false)
			s := createFilledSession(t, x, "file", "abc")
			f := &sessionCleanupFailure{Files: x.files, fail: true, session: s.ID}
			x.r.Files = f
			if commit {
				if _, e := x.r.CommitSession(ctx, s.ID); e != nil {
					t.Fatal(e)
				}
			} else if e := x.r.AbortSession(s.ID); e != nil {
				t.Fatal(e)
			}
			got, e := x.r.Session(s.ID)
			expected := domain.SessionAborted
			if commit {
				expected = domain.SessionCommitted
			}
			if e != nil || got.State != expected {
				t.Fatal(got, e)
			}
			if e = x.r.CleanupSessions(ctx); !errors.Is(e, os.ErrPermission) {
				t.Fatal("expected cleanup failure", e)
			}
			if !commit {
				if e = x.r.AbortSession(s.ID); e != nil {
					t.Fatal("abort retry", e)
				}
				if _, e = x.r.CommitSession(ctx, s.ID); !errors.Is(e, domain.ErrSessionAborted) {
					t.Fatal(e)
				}
			}
			// Startup retries cleanup with the real filesystem and retains the receipt.
			reopen(t, x)
			if got, e = x.r.Session(s.ID); e != nil || got.State != expected {
				t.Fatal(got, e)
			}
			if _, e = os.Stat(filepath.Join(x.data, ".filegate/staging", s.ID+"-0")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("segment not retried", e)
			}
		})
	}
}

func TestSessionReceiptRecoveredWithPublication(t *testing.T) {
	x := setup(t, true, true)
	s := createFilledSession(t, x, "file", "abc")
	x.r.State = &failingState{State: x.state, fail: true}
	if _, e := x.r.CommitSession(ctx, s.ID); e == nil {
		t.Fatal("missing injected failure")
	}
	var pending struct{ Receipt *domain.Session }
	if e := x.state.Scan("pending/", func(_ string, b []byte) error { return json.Unmarshal(b, &pending) }); e != nil {
		t.Fatal(e)
	}
	if pending.Receipt == nil || pending.Receipt.TerminalAt == nil {
		t.Fatal("journal lacks frozen receipt")
	}
	reopen(t, x)
	got, e := x.r.Session(s.ID)
	if e != nil || got.State != domain.SessionCommitted || !reflect.DeepEqual(got, *pending.Receipt) {
		t.Fatalf("receipt recovery %+v %v", got, e)
	}
	if n, e := x.r.CommitSession(ctx, s.ID); e != nil || n.Size != 3 {
		t.Fatal(n, e)
	}
}

type terminalGateState struct {
	domain.State
	entered chan struct{}
	release chan struct{}
}

func (s *terminalGateState) Batch(cs []domain.Change) error {
	for _, c := range cs {
		if !c.Delete && strings.HasPrefix(c.Key, "done/") {
			close(s.entered)
			<-s.release
			break
		}
	}
	return s.State.Batch(cs)
}

func TestCommitAbortRaceHasOneDurableWinner(t *testing.T) {
	for _, commitFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit-wins", false: "abort-wins"}[commitFirst], func(t *testing.T) {
			x := setup(t, false, false)
			s := createFilledSession(t, x, "file", "abc")
			gate := &terminalGateState{State: x.state, entered: make(chan struct{}), release: make(chan struct{})}
			x.r.State = gate
			commits, aborts := make(chan error, 1), make(chan error, 1)
			commit := func() { _, e := x.r.CommitSession(ctx, s.ID); commits <- e }
			abort := func() { aborts <- x.r.AbortSession(s.ID) }
			if commitFirst {
				go commit()
			} else {
				go abort()
			}
			<-gate.entered
			if commitFirst {
				go abort()
			} else {
				go commit()
			}
			close(gate.release)
			ce, ae := <-commits, <-aborts
			if commitFirst {
				if ce != nil || !errors.Is(ae, domain.ErrSessionCommitted) {
					t.Fatal(ce, ae)
				}
			} else if ae != nil || !errors.Is(ce, domain.ErrSessionAborted) {
				t.Fatal(ce, ae)
			}
			reopen(t, x)
			got, e := x.r.Session(s.ID)
			expected := domain.SessionAborted
			if commitFirst {
				expected = domain.SessionCommitted
			}
			if e != nil || got.State != expected {
				t.Fatal(got, e)
			}
			_, e = os.Stat(filepath.Join(x.data, "file"))
			if commitFirst && e != nil || !commitFirst && !errors.Is(e, os.ErrNotExist) {
				t.Fatal("wrong publication", e)
			}
		})
	}
}

func TestInFlightSegmentCannotReopenTerminalSession(t *testing.T) {
	for _, expire := range []bool{true, false} {
		t.Run(map[bool]string{true: "expiry", false: "abort"}[expire], func(t *testing.T) {
			x := setup(t, false, false)
			s, e := x.r.CreateSession("file", 3, domain.WriteOptions{}, "")
			if e != nil {
				t.Fatal(e)
			}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			started, done := make(chan struct{}), make(chan error, 1)
			go func() { _, e := x.r.PutSegment(ctx, s.ID, 0, &signaledReader{reader, started}); done <- e }()
			<-started
			wantErr := domain.ErrSessionAborted
			if expire {
				s.Expires = time.Now().Add(-time.Minute)
				if e = x.state.Put("session/"+s.ID, s); e != nil {
					t.Fatal(e)
				}
				wantErr = domain.ErrSessionExpired
			} else if e = x.r.AbortSession(s.ID); e != nil {
				t.Fatal(e)
			}
			if _, e = writer.Write([]byte("abc")); e != nil {
				t.Fatal(e)
			}
			writer.Close()
			if e = <-done; !errors.Is(e, wantErr) {
				t.Fatal("accepted segment after terminal state", e)
			}
			got, e := x.r.Session(s.ID)
			if e != nil || got.Received != 0 || got.UploadedSegments != 0 {
				t.Fatal(got, e)
			}
			if _, e = os.Stat(filepath.Join(x.data, ".filegate/staging", s.ID+"-0")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("published terminal segment", e)
			}
		})
	}
}

func TestSessionIDsRejectTraversalAndNonCanonicalUUID(t *testing.T) {
	x := setup(t, false, false)
	for _, id := range []string{"../other", "", "../done/key", "00000000000000000000000000000000", "{00000000-0000-0000-0000-000000000000}"} {
		if _, e := x.r.Session(id); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal(id, e)
		}
		if e := x.r.AbortSession(id); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal(id, e)
		}
		if _, e := x.r.CommitSession(ctx, id); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal(id, e)
		}
		if _, e := x.r.PutSegment(ctx, id, 0, strings.NewReader("")); !errors.Is(e, domain.ErrInvalid) {
			t.Fatal(id, e)
		}
	}
}

type terminalWriteFailure struct {
	domain.State
	fail bool
}

func (s *terminalWriteFailure) Batch(cs []domain.Change) error {
	for _, c := range cs {
		if s.fail && !c.Delete && strings.HasPrefix(c.Key, "done/") {
			return errors.New("injected terminal receipt failure")
		}
	}
	return s.State.Batch(cs)
}

func TestAbortPersistsReceiptBeforeDeletingSegments(t *testing.T) {
	x := setup(t, false, false)
	session := createFilledSession(t, x, "file", "abc")
	state := &terminalWriteFailure{State: x.state, fail: true}
	x.r.State = state
	if e := x.r.AbortSession(session.ID); e == nil {
		t.Fatal("missing receipt failure")
	}
	got, e := x.r.Session(session.ID)
	if e != nil || got.State != domain.SessionOpen || got.Received != 3 {
		t.Fatal(got, e)
	}
	b, e := os.ReadFile(filepath.Join(x.data, ".filegate/staging", session.ID+"-0"))
	if e != nil || string(b) != "abc" {
		t.Fatal("cleanup ran before durable receipt", e)
	}
	state.fail = false
	if e = x.r.AbortSession(session.ID); e != nil {
		t.Fatal(e)
	}
	reopen(t, x)
	got, e = x.r.Session(session.ID)
	if e != nil || got.State != domain.SessionAborted {
		t.Fatal(got, e)
	}
}
