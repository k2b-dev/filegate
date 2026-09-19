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
	"strings"
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
	Execution        *ExecutionIdentity `json:"execution,omitempty"`
	State            SessionState       `json:"state"`
	TerminalAt       *time.Time         `json:"terminalAt,omitempty"`
	RetainUntil      *time.Time         `json:"retainUntil,omitempty"`
	ID               string             `json:"id"`
	Root             string             `json:"root"`
	Path             string             `json:"path"`
	Size             int64              `json:"size"`
	ChunkSize        int64              `json:"chunkSize"`
	Expires          time.Time          `json:"expires"`
	Options          WriteOptions       `json:"options"`
	UploadedSegments int                `json:"uploadedSegments"`
	Received         int64              `json:"received"`
	Result           *Node              `json:"result,omitempty"`
}

// sessionReceipt stores a compact terminal result and a retryable cleanup flag.
// The flag is internal; Session is the public representation.
type sessionReceipt struct {
	Session
	CleanupPending bool `json:"cleanupPending,omitempty"`
}

func (r *Root) CreateSession(p string, size int64, o WriteOptions, idempotencyKey string) (Session, error) {
	if len(idempotencyKey) > 128 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Session{}, ErrInvalid
	}
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
	if o.Precondition != nil && !r.Config.Managed {
		return Session{}, ErrDisabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return Session{}, e
	}
	fingerprintBytes, e := json.Marshal(struct {
		Path      string
		Size      int64
		Options   WriteOptions
		Execution *ExecutionIdentity
	}{p, size, o, r.Execution()})
	if e != nil {
		return Session{}, e
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintBytes))
	key := ""
	if idempotencyKey != "" {
		key = fmt.Sprintf("session-key/%x", sha256.Sum256([]byte(idempotencyKey)))
		var previous sessionKey
		if e := r.State.Get(key, &previous); e == nil {
			existing, err := r.session(previous.SessionID)
			if err == nil {
				if previous.Fingerprint != fingerprint {
					return Session{}, ErrConflict
				}
				return existing, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return Session{}, err
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return Session{}, e
		}
	}
	s := Session{Execution: r.Execution(), State: SessionOpen, ID: newID(), Root: r.Config.Name, Path: p, Size: size, ChunkSize: ChunkSize, Expires: r.now().UTC().Add(SessionLifetime), Options: o}
	change, e := encoded("session/"+s.ID, s)
	if e != nil {
		return Session{}, e
	}
	changes := []Change{change}
	if key != "" {
		c, err := encoded(key, sessionKey{SessionID: s.ID, Fingerprint: fingerprint})
		if err != nil {
			return Session{}, err
		}
		changes = append(changes, c)
	}
	return s, r.State.Batch(changes)
}
func validSessionID(id string) bool {
	parsed, e := uuid.Parse(id)
	return e == nil && parsed.String() == id
}

func terminalSession(s Session, state SessionState, at time.Time, result *Node) Session {
	at = at.UTC()
	retain := at.Add(SessionRetention)
	s.State, s.TerminalAt, s.RetainUntil, s.Result = state, &at, &retain, result
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
	record, e := r.loadSessionRecord("done/" + id)
	s := record.Session
	if e == nil {
		if s.RetainUntil == nil || !s.RetainUntil.After(r.now()) {
			return Session{}, os.ErrNotExist
		}
		return s, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return Session{}, e
	}
	record, e = r.loadSessionRecord("session/" + id)
	if e != nil {
		return Session{}, e
	}
	s = record.Session
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
	var previous SessionSegment
	if e = r.State.Get(segmentKey(id, index), &previous); e == nil {
		if previous.Hash != hash {
			return s, ErrConflict
		}
		return s, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return s, e
	}
	if e = f.Sync(); e != nil {
		return s, e
	}
	if e = r.Files.Rename(temp, segmentPath(id, index), true); e != nil {
		return s, e
	}
	s.UploadedSegments++
	s.Received += n
	receipt, e := encoded(segmentKey(id, index), SessionSegment{Index: index, Hash: hash})
	if e != nil {
		return s, e
	}
	compact, e := encoded("session/"+id, s)
	if e != nil {
		return s, e
	}
	return s, r.State.Batch([]Change{receipt, compact})
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
	// A commit always uses the identity captured when the session was opened.
	// Backend retries without an execution header must never publish as daemon.
	if !sameExecution(r.execution, s.Execution) {
		if r.execution != nil {
			return Node{}, ErrConflict
		}
		view, close, err := r.WithExecution(ctx, s.Execution)
		if err != nil {
			return Node{}, err
		}
		defer close()
		r = view
	}
	count := int((s.Size + s.ChunkSize - 1) / s.ChunkSize)
	if s.UploadedSegments != count {
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
		var receipt SessionSegment
		if e := r.State.Get(segmentKey(id, i), &receipt); e != nil {
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
		if e == nil && (n != expected || "sha256:"+hex.EncodeToString(h.Sum(nil)) != receipt.Hash) {
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
	changes := make([]Change, 0, count)
	for i := 0; i < count; i++ {
		changes = append(changes, Change{Key: segmentKey(s.ID, i), Delete: true})
		if e := r.Files.Remove(segmentPath(s.ID, i), false); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if len(changes) > 0 {
		return r.State.Batch(changes)
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
	if e := r.State.Scan("session/", func(key string, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		record, e := r.decodeSessionRecord(key, b)
		if e != nil {
			return e
		}
		s := record.Session
		if !s.Expires.After(r.now()) {
			return r.saveTerminal(terminalSession(s, SessionExpired, s.Expires, nil))
		}
		return nil
	}); e != nil {
		return e
	}
	if err := r.State.Scan("done/", func(k string, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		s, e := r.decodeSessionRecord(k, b)
		if e != nil {
			return e
		}
		if e := r.cleanupReceipt(s); e != nil {
			return e
		}
		if s.RetainUntil != nil && !s.RetainUntil.After(r.now()) {
			return r.State.Delete(k)
		}
		return nil
	}); err != nil {
		return err
	}
	return r.State.Scan("session-key/", func(k string, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var key sessionKey
		if err := json.Unmarshal(b, &key); err != nil {
			return err
		}
		_, err := r.session(key.SessionID)
		if errors.Is(err, os.ErrNotExist) {
			return r.State.Delete(k)
		}
		return err
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

type sessionKey struct {
	SessionID   string `json:"sessionId"`
	Fingerprint string `json:"fingerprint"`
}

type SessionSegment struct {
	Index int    `json:"index"`
	Hash  string `json:"hash"`
}
type SessionSegmentPage struct {
	Items []SessionSegment `json:"items"`
	Next  *int             `json:"next,omitempty"`
}

func segmentKey(id string, index int) string { return fmt.Sprintf("segment/%s/%05d", id, index) }

func (r *Root) SessionSegments(id string, after, limit int) (SessionSegmentPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	page := SessionSegmentPage{Items: []SessionSegment{}}
	if after < -1 || after >= MaxSegments || limit < 1 || limit > 1000 {
		return page, ErrInvalid
	}
	s, err := r.session(id)
	if err != nil {
		return page, err
	}
	if s.State != SessionOpen {
		return page, nil
	}
	prefix := "segment/" + id + "/"
	cursor := ""
	if after >= 0 {
		cursor = segmentKey(id, after)
	}
	stop := errors.New("page complete")
	err = r.State.ScanAfter(prefix, cursor, func(_ string, b []byte) error {
		if len(page.Items) == limit {
			next := page.Items[len(page.Items)-1].Index
			page.Next = &next
			return stop
		}
		var segment SessionSegment
		if err := json.Unmarshal(b, &segment); err != nil {
			return err
		}
		page.Items = append(page.Items, segment)
		return nil
	})
	if errors.Is(err, stop) {
		err = nil
	}
	return page, err
}

// Existing durable sessions are converted once, atomically. This preserves
// acknowledged chunks and retained commit results without a legacy wire API.
func (r *Root) decodeSessionRecord(key string, b []byte) (sessionReceipt, error) {
	var stored struct {
		sessionReceipt
		Segments map[int]string `json:"segments"`
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return sessionReceipt{}, err
	}
	if stored.Segments == nil {
		return stored.sessionReceipt, nil
	}
	s := &stored.Session
	if s.ChunkSize <= 0 || s.Size < 0 || s.Size > s.ChunkSize*MaxSegments {
		return sessionReceipt{}, ErrInvalid
	}
	changes := make([]Change, 0, len(stored.Segments)+1)
	if s.State == SessionOpen {
		var received int64
		for index, hash := range stored.Segments {
			count := int((s.Size + s.ChunkSize - 1) / s.ChunkSize)
			if index < 0 || index >= count || len(hash) != 71 || !strings.HasPrefix(hash, "sha256:") {
				return sessionReceipt{}, ErrInvalid
			}
			if _, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:")); err != nil {
				return sessionReceipt{}, ErrInvalid
			}
			bytes := s.ChunkSize
			if index == count-1 {
				bytes = s.Size - int64(index)*s.ChunkSize
			}
			received += bytes
			change, err := encoded(segmentKey(s.ID, index), SessionSegment{Index: index, Hash: hash})
			if err != nil {
				return sessionReceipt{}, err
			}
			changes = append(changes, change)
		}
		if s.Received != received {
			return sessionReceipt{}, ErrInvalid
		}
		s.UploadedSegments = len(stored.Segments)
	} else {
		if s.Received < 0 || s.Received > s.Size {
			return sessionReceipt{}, ErrInvalid
		}
		s.UploadedSegments = int((s.Received + s.ChunkSize - 1) / s.ChunkSize)
	}
	change, err := encoded(key, stored.sessionReceipt)
	if err != nil {
		return sessionReceipt{}, err
	}
	changes = append(changes, change)
	if err := r.State.Batch(changes); err != nil {
		return sessionReceipt{}, err
	}
	return stored.sessionReceipt, nil
}
func (r *Root) loadSessionRecord(key string) (sessionReceipt, error) {
	var raw json.RawMessage
	if err := r.State.Get(key, &raw); err != nil {
		return sessionReceipt{}, err
	}
	return r.decodeSessionRecord(key, raw)
}
