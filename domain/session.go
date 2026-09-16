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
)

const ChunkSize int64 = 8 << 20
const MaxSegments = 10000

type Session struct {
	ID        string         `json:"id"`
	Root      string         `json:"root"`
	Path      string         `json:"path"`
	Size      int64          `json:"size"`
	ChunkSize int64          `json:"chunkSize"`
	Expires   time.Time      `json:"expires"`
	Options   WriteOptions   `json:"options"`
	Segments  map[int]string `json:"segments"`
	Received  int64          `json:"received"`
	Result    *Node          `json:"result,omitempty"`
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
	s := Session{ID: newID(), Root: r.Config.Name, Path: p, Size: size, ChunkSize: ChunkSize, Expires: r.now().Add(24 * time.Hour), Options: o, Segments: map[int]string{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	return s, r.State.Put("session/"+s.ID, s)
}
func (r *Root) session(id string) (Session, error) {
	if e := r.guard(); e != nil {
		return Session{}, e
	}
	var s Session
	if e := r.State.Get("session/"+id, &s); e != nil {
		return s, e
	}
	if !s.Expires.After(r.now()) {
		return s, os.ErrNotExist
	}
	var n Node
	if e := r.State.Get("done/"+id, &n); e == nil {
		s.Result = &n
	} else if !errors.Is(e, os.ErrNotExist) {
		return s, e
	}
	return s, nil
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
	if s.Result != nil {
		return s, ErrConflict
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
	if s.Result != nil {
		return s, ErrConflict
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
	if s.Result != nil {
		return *s.Result, nil
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
	for i := 0; i < count; i++ {
		if e = r.Files.Remove(segmentPath(id, i), false); e != nil && !errors.Is(e, os.ErrNotExist) {
			return n, e
		}
	}
	return n, nil
}
func (r *Root) AbortSession(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return e
	}
	var s Session
	if e := r.State.Get("session/"+id, &s); e != nil {
		return e
	}
	return r.deleteSession(s)
}
func (r *Root) deleteSession(s Session) error {
	count := int((s.Size + s.ChunkSize - 1) / s.ChunkSize)
	for i := 0; i < count; i++ {
		e := r.Files.Remove(segmentPath(s.ID, i), false)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return r.State.Batch([]Change{{Key: "session/" + s.ID, Delete: true}, {Key: "done/" + s.ID, Delete: true}})
}
func (r *Root) CleanupSessions(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.guard(); e != nil {
		return e
	}
	return r.State.Scan("session/", func(_ string, b []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		var s Session
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if !s.Expires.After(r.now()) {
			return r.deleteSession(s)
		}
		return nil
	})
}
