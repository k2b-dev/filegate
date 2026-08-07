//go:build linux

package filesystem

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/k2b-dev/filegate/v3/domain"
)

func testFileID(t *testing.T) domain.FileID {
	t.Helper()
	value, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("new UUID: %v", err)
	}
	return domain.FileID(value)
}

func corruptIDForTest(t *testing.T, path string) {
	t.Helper()
	if err := unix.Setxattr(path, domain.XAttrIDKey(), []byte("not-a-filegate-id"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("filesystem does not support user xattrs")
		}
		t.Fatalf("corrupt xattr: %v", err)
	}
}

func TestSetIDIfAbsentRepairsMalformedXattr(t *testing.T) {
	path := t.TempDir() + "/file"
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	corruptIDForTest(t, path)

	want := testFileID(t)
	got, wrote, err := setIDIfAbsent(path, want)
	if err != nil {
		t.Fatalf("set ID: %v", err)
	}
	if !wrote || got != want {
		t.Fatalf("got ID=%s wrote=%t, want ID=%s wrote=true", got, wrote, want)
	}
	stored, err := getID(path)
	if err != nil || stored != want {
		t.Fatalf("stored ID=%s err=%v, want %s", stored, err, want)
	}
}

func TestSetIDIfAbsentSerializesMalformedXattrRepair(t *testing.T) {
	path := t.TempDir() + "/file"
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	corruptIDForTest(t, path)

	const workers = 16
	type result struct {
		id    domain.FileID
		wrote bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		candidate := testFileID(t)
		go func() {
			ready.Done()
			<-start
			id, wrote, err := setIDIfAbsent(path, candidate)
			results <- result{id: id, wrote: wrote, err: err}
		}()
	}
	ready.Wait()
	close(start)

	stored, storedErr := domain.FileID{}, error(nil)
	writes := 0
	for range workers {
		result := <-results
		if result.err != nil {
			t.Fatalf("set ID: %v", result.err)
		}
		if result.wrote {
			writes++
		}
		if stored.IsZero() {
			stored = result.id
		} else if result.id != stored {
			t.Fatalf("workers settled on different IDs: %s and %s", stored, result.id)
		}
	}
	if writes != 1 {
		t.Fatalf("writers=%d, want 1", writes)
	}
	if onDisk, err := getID(path); err != nil {
		storedErr = err
	} else if onDisk != stored {
		t.Fatalf("stored ID=%s, worker ID=%s", onDisk, stored)
	}
	if storedErr != nil {
		t.Fatalf("read stored ID: %v", storedErr)
	}
}
