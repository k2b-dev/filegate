package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
)

const ChunkSize int64 = 8 << 20
const MaxSegments = 10000
const SessionLifetime = 24 * time.Hour
const SessionRetention = 7 * 24 * time.Hour

type SessionState string

const (
	SessionOpen      SessionState = "open"
	SessionCommitted SessionState = "committed"
	SessionAborted   SessionState = "aborted"
	SessionExpired   SessionState = "expired"
)

var (
	ErrSessionCommitted = errors.New("session already committed")
	ErrSessionAborted   = errors.New("session aborted")
	ErrSessionExpired   = errors.New("session expired")
)

type Session struct {
	State       SessionState   `json:"state"`
	TerminalAt  *time.Time     `json:"terminalAt,omitempty"`
	RetainUntil *time.Time     `json:"retainUntil,omitempty"`
	ID          string         `json:"id"`
	Root        string         `json:"root"`
	Path        string         `json:"path"`
	Size        int64          `json:"size"`
	ChunkSize   int64          `json:"chunkSize"`
	Expires     time.Time      `json:"expires"`
	Options     WriteOptions   `json:"options"`
	Segments    map[int]string `json:"segments"`
	Received    int64          `json:"received"`
	Result      *Node          `json:"result,omitempty"`
}

// sessionReceipt stores a compact terminal result and a retryable cleanup flag.
// The flag is internal; Session is the public representation.
type sessionReceipt struct {
	Session
	CleanupPending bool `json:"cleanupPending,omitempty"`
}

func (r *Root) CreateSession(p string, size int64, o WriteOptions) (Session, error) {
	p, e := validWrite(p)
	if e != nil {
		return Session{}, e
	}
	if size < 0 || size > r.MaxBytes || size > ChunkSize*MaxSegments {
		return Session{}, ErrLimit
	}
	if e = ValidateOptions(o); e != nil {
		return Session{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return Session{}, e
	}
	s := Session{State: SessionOpen, ID: newID(), Root: r.Config.Name, Path: p, Size: size, ChunkSize: ChunkSize, Expires: r.now().UTC().Add(SessionLifetime), Options: o, Segments: map[int]string{}}
	return s, r.State.Put("session/"+s.ID, s)
}
func validSessionID(id string) bool {
	parsed, e := uuid.Parse(id)
	return e == nil && parsed.String() == id
}

func terminalSession(s Session, state SessionState, at time.Time, result *Node) Session {
	at = at.UTC()
	retain := at.Add(SessionRetention)
	s.State, s.TerminalAt, s.RetainUntil, s.Result = state, &at, &retain, result
	s.Segments = map[int]string{}
	if result != nil {
		s.Received = result.Size
	}
	return s
}

func (r *Root) saveTerminal(s Session) error {
	c, e := encoded("done/"+s.ID, sessionReceipt{Session: s, CleanupPending: true})
	if e != nil {
		return e
	}
	return r.State.Batch([]Change{c, {Key: "session/" + s.ID, Delete: true}})
}

func (r *Root) session(id string) (Session, error) {
	if !validSessionID(id) {
		return Session{}, ErrInvalid
	}
	if e := r.guard(); e != nil {
		return Session{}, e
	}
	var s Session
	if e := r.State.Get("done/"+id, &s); e == nil {
		if s.RetainUntil == nil || !s.RetainUntil.After(r.now()) {
			return Session{}, os.ErrNotExist
		}
		return s, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return Session{}, e
	}
	if e := r.State.Get("session/"+id, &s); e != nil {
		return Session{}, e
	}
	if !s.Expires.After(r.now()) {
		s = terminalSession(s, SessionExpired, s.Expires, nil)
		if e := r.saveTerminal(s); e != nil {
			return Session{}, e
		}
		if !s.RetainUntil.After(r.now()) {
			return Session{}, os.ErrNotExist
		}
	}
	return s, nil
}

func sessionOpen(s Session) error {
	switch s.State {
	case SessionOpen:
		return nil
	case SessionCommitted:
		return ErrSessionCommitted
	case SessionAborted:
		return ErrSessionAborted
	case SessionExpired:
		return ErrSessionExpired
	default:
		return ErrInvalid
	}
}
func (r *Root) Session(id string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session(id)
}
func (r *Root) PutSegment(ctx context.Context, id string, index int, body io.Reader) (Session, error) {
	r.mu.Lock()
	s, e := r.session(id)
	r.mu.Unlock()
	if e != nil {
		return s, e
	}
	if e = sessionOpen(s); e != nil {
		return s, e
	}
	count := (s.Size + s.ChunkSize - 1) / s.ChunkSize
	if index < 0 || int64(index) >= count {
		return s, ErrInvalid
	}
	size := s.ChunkSize
	if int64(index) == count-1 {
		size = s.Size - int64(index)*s.ChunkSize
	}
	temp := ".filegate/staging/" + newID()
	f, e := r.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return s, e
	}
	defer func() { f.Close(); r.Files.Remove(temp, false) }()
	h := sha256.New()
	n, e := copyStream(io.MultiWriter(f, h), &contextReader{ctx, body}, size)
	if e != nil {
		return s, e
	}
	if n != size {
		return s, ErrInvalid
	}
	hash := "sha256:" + hex.EncodeToString(h.Sum(nil))
	r.mu.Lock()
	defer r.mu.Unlock()
	s, e = r.session(id)
	if e != nil {
		return s, e
	}
	if e = sessionOpen(s); e != nil {
		return s, e
	}
	if previous, ok := s.Segments[index]; ok {
		if previous != hash {
			return s, ErrConflict
		}
		return s, nil
	}
	if e = f.Sync(); e != nil {
		return s, e
	}
	if e = r.Files.Rename(temp, segmentPath(id, index), true); e != nil {
		return s, e
	}
	s.Segments[index] = hash
	s.Received += n
	return s, r.State.Put("session/"+id, s)
}
func segmentPath(id string, index int) string {
	return fmt.Sprintf(".filegate/staging/%s-%d", id, index)
}
func (r *Root) CommitSession(ctx context.Context, id string) (Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, e := r.session(id)
	if e != nil {
		return Node{}, e
	}
	if s.State == SessionCommitted && s.Result != nil {
		return *s.Result, nil
	}
	if e = sessionOpen(s); e != nil {
		return Node{}, e
	}
	count := int((s.Size + s.ChunkSize - 1) / s.ChunkSize)
	if len(s.Segments) != count {
		return Node{}, ErrConflict
	}
	temp := ".filegate/staging/" + newID()
	dst, e := r.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return Node{}, e
	}
	defer func() { dst.Close(); r.Files.Remove(temp, false) }()
	for i := 0; i < count; i++ {
		if e = ctx.Err(); e != nil {
			return Node{}, e
		}
		f, e := r.Files.Open(segmentPath(id, i), os.O_RDONLY, 0)
		if e != nil {
			return Node{}, e
		}
		h := sha256.New()
		n, ce := io.Copy(io.MultiWriter(dst, h), f)
		e = ce
		f.Close()
		expected := s.ChunkSize
		if i == count-1 {
			expected = s.Size - int64(i)*s.ChunkSize
		}
		if e == nil && (n != expected || "sha256:"+hex.EncodeToString(h.Sum(nil)) != s.Segments[i]) {
			e = fmt.Errorf("segment integrity check failed")
		}
		if e != nil {
			return Node{}, e
		}
	}
	if e = dst.Sync(); e != nil {
		return Node{}, e
	}
	n, e := r.publish(s.Path, temp, dst, s.Options, false, "done/"+id)
	if e != nil {
		return Node{}, e
	}
	// The receipt is durable. Cleanup is retried by maintenance and at startup;
	// its failure must not turn a committed operation into an ambiguous failure.
	var receipt sessionReceipt
	if r.State.Get("done/"+id, &receipt) == nil {
		_ = r.cleanupReceipt(receipt)
	}
	return n, nil
}
func (r *Root) AbortSession(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, e := r.session(id)
	if e != nil {
		return e
	}
	if s.State == SessionAborted {
		return nil
	}
	if e = sessionOpen(s); e != nil {
		return e
	}
	s = terminalSession(s, SessionAborted, r.now(), nil)
	if e = r.saveTerminal(s); e != nil {
		return e
	}
	_ = r.cleanupReceipt(sessionReceipt{Session: s, CleanupPending: true})
	return nil
}

func (r *Root) cleanupSessionSegments(s Session) error {
	count := int((s.Size + s.ChunkSize - 1) / s.ChunkSize)
	for i := 0; i < count; i++ {
		if e := r.Files.Remove(segmentPath(s.ID, i), false); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return nil
}

func (r *Root) cleanupReceipt(receipt sessionReceipt) error {
	if !receipt.CleanupPending {
		return nil
	}
	if e := r.cleanupSessionSegments(receipt.Session); e != nil {
		return e
	}
	receipt.CleanupPending = false
	return r.State.Put("done/"+receipt.ID, receipt)
}

func (r *Root) cleanupSessions(ctx context.Context) error {
	if e := r.State.Scan("session/", func(_ string, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		var s Session
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if !s.Expires.After(r.now()) {
			return r.saveTerminal(terminalSession(s, SessionExpired, s.Expires, nil))
		}
		return nil
	}); e != nil {
		return e
	}
	return r.State.Scan("done/", func(k string, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		var s sessionReceipt
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if e := r.cleanupReceipt(s); e != nil {
			return e
		}
		if s.RetainUntil != nil && !s.RetainUntil.After(r.now()) {
			return r.State.Delete(k)
		}
		return nil
	})
}

func (r *Root) CleanupSessions(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return e
	}
	return r.cleanupSessions(ctx)
}
