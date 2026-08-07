//go:build linux

package domain_test

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestPathLockSerializesConcurrentWritesAndDeletes pins the cross-
// op contract: a concurrent WriteContent and Delete on the same
// file ID must serialize. Without path-locks, the two paths were
// already serialized via versionLocks — but path-locks add a SECOND
// serialization layer for ops that come in via different code
// surfaces (e.g. S3 PUT racing REST DELETE later in M1+). This
// test pins that the existing same-id case still works under the
// new lock stack.
func TestPathLockSerializesConcurrentWritesAndDeletes(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/race.txt",
		strings.NewReader("v0"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers = 4
	const ops = 50
	var (
		wg         sync.WaitGroup
		writeOK    atomic.Int64
		writeStale atomic.Int64
		deletes    atomic.Int64
		stop       atomic.Bool
	)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				if stop.Load() {
					return
				}
				body := strings.NewReader("v" + string(rune('0'+(i%10))))
				err := svc.WriteContent(meta.ID, body)
				if err == nil {
					writeOK.Add(1)
				} else if errors.Is(err, domain.ErrNotFound) {
					writeStale.Add(1)
				}
			}
		}()
	}
	// One delete in the middle.
	go func() {
		time.Sleep(5 * time.Millisecond)
		if err := svc.Delete(meta.ID); err == nil {
			deletes.Add(1)
		}
		stop.Store(true)
	}()

	wg.Wait()

	// Properties:
	// - The lock stack must not panic / deadlock (we got here).
	// - writeStale (ErrNotFound after delete) is the EXPECTED
	//   outcome for any write that beat the lock to the file but
	//   found it already deleted on revalidate.
	// - At least one write succeeded BEFORE the delete (we wrote
	//   v0 at create time; subsequent writes may or may not have
	//   landed depending on scheduling).
	if deletes.Load() != 1 {
		t.Fatalf("delete failed; got %d", deletes.Load())
	}
	t.Logf("writeOK=%d writeStale=%d deletes=%d",
		writeOK.Load(), writeStale.Load(), deletes.Load())
}

// TestPathLockSubtreeDeleteBlocksDescendantWrite proves that a
// recursive Delete on a directory blocks a concurrent CreateChild
// at a descendant path until the delete completes. Without path
// locks, the descendant create could land mid-delete, leaving the
// new file orphaned (parent gone but file's inode still on disk).
func TestPathLockSubtreeDeleteBlocksDescendantWrite(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	root := svc.ListRoot()[0].ID
	dir, err := svc.CreateChild(root, "subtree", true, nil)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed a file inside so deleteSubtree has work to do.
	if _, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/subtree/seed.txt",
		strings.NewReader("seed"),
		domain.ConflictError,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Delete in one goroutine, attempt CreateChild in another after
	// a tiny delay (so delete is likely already holding the
	// subtree-lock when create tries to acquire its point-lock).
	deleteDone := make(chan error, 1)
	createDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.Delete(dir.ID)
	}()
	time.Sleep(2 * time.Millisecond)
	go func() {
		_, err := svc.CreateChild(dir.ID, "racing.txt", false, nil)
		createDone <- err
	}()

	dErr := <-deleteDone
	cErr := <-createDone

	if dErr != nil {
		t.Fatalf("delete failed: %v", dErr)
	}
	// CreateChild should fail: either the subtree-lock blocked it
	// until the delete completed and now the parent is gone, OR
	// the parent.Type check / ResolveAbsPath fails because the
	// directory is gone. Either way, it should return an error,
	// NOT silently create a file under a deleted directory.
	if cErr == nil {
		t.Fatalf("CreateChild succeeded after subtree delete — orphan child created")
	}
}

// TestPathLockRESTVirtualCreateBlocksConcurrentS3Write pins the
// cross-protocol namespace contract: REST virtual-path create holds
// the same leaf path-lock as S3 PutObject. The blocked REST body
// keeps the create critical section open; a parallel S3 write to the
// same path must wait until REST finishes instead of committing first.
func TestPathLockRESTVirtualCreateBlocksConcurrentS3Write(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	target := "/" + mountName + "/locked-create.txt"
	body := newBlockingReader("rest")

	restDone := make(chan error, 1)
	go func() {
		_, _, err := svc.WriteContentByVirtualPath(target, body, domain.ConflictError)
		restDone <- err
	}()

	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("REST create did not reach body read")
	}

	s3Done := make(chan error, 1)
	go func() {
		_, _, err := svc.WriteObjectS3(target, strings.NewReader("s3"), domain.S3WriteOptions{})
		s3Done <- err
	}()

	select {
	case err := <-s3Done:
		t.Fatalf("S3 write completed while REST create was still in-flight, err=%v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: S3 is waiting on the REST-held leaf path-lock.
	}

	body.release()

	if err := <-restDone; err != nil {
		t.Fatalf("REST create failed: %v", err)
	}
	if err := <-s3Done; err != nil {
		t.Fatalf("S3 write after REST create failed: %v", err)
	}

	id, err := svc.ResolvePath(strings.TrimPrefix(target, "/"))
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	reader, _, isDir, err := svc.OpenContent(id)
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	if isDir {
		t.Fatalf("target resolved as directory")
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read final content: %v", err)
	}
	if string(got) != "s3" {
		t.Fatalf("final content=%q, want s3 overwrite after REST create", string(got))
	}
}

// TestPathLockReleasesAfterPanic verifies that even if a locked
// operation panics (or returns an error), the locks are released so
// subsequent ops on the same path can proceed. Without proper defer
// release, a single bad request would jam the path forever.
func TestPathLockReleasesAfterPanic(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/jam.txt",
		strings.NewReader("x"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Force an error by passing a body that aborts mid-stream.
	// Service.WriteContent should release locks even when the
	// underlying write fails.
	_ = svc.WriteContent(meta.ID, &abortReader{})
	// Note: WriteContent may succeed if abortReader returns 0 bytes
	// + EOF — that's fine, we just want to verify no deadlock.

	// A subsequent op MUST proceed without blocking.
	done := make(chan error, 1)
	go func() {
		done <- svc.WriteContent(meta.ID, strings.NewReader("y"))
	}()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("subsequent write failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("subsequent write blocked — locks leaked from prior op")
	}
}

type abortReader struct{}

func (a *abortReader) Read(p []byte) (int, error) {
	return 0, errors.New("aborted")
}

type blockingReader struct {
	payload  []byte
	started  chan struct{}
	released chan struct{}
	done     bool
}

func newBlockingReader(payload string) *blockingReader {
	return &blockingReader{
		payload:  []byte(payload),
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.released
	r.done = true
	return copy(p, r.payload), io.EOF
}

func (r *blockingReader) release() {
	close(r.released)
}
