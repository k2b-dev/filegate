//go:build linux

package filesystem

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/valentinkolb/filegate/domain"
)

func setID(path string, id domain.FileID) error {
	if err := unix.Setxattr(path, domain.XAttrIDKey(), id[:], 0); err != nil {
		return fmt.Errorf("setxattr %s: %w", path, err)
	}
	return nil
}

// setIDIfAbsent writes the ID only when the path has none, and reports whether
// this call was the one that wrote it.
//
// XATTR_CREATE makes that an atomic test-and-set in the kernel, which is what
// first-time indexing needs: two goroutines reaching an unindexed directory at
// the same moment would otherwise each mint a fresh ID, both write it, and each
// continue with its own value. The index then carries a child edge pointing at
// an ID the xattr no longer holds, and resolving that path answers not-found.
//
// A lock would also work but not here: MkdirRelative already holds the path
// lock for the leaf it is creating, and this runs underneath it.
func setIDIfAbsent(path string, id domain.FileID) (domain.FileID, bool, error) {
	err := unix.Setxattr(path, domain.XAttrIDKey(), id[:], unix.XATTR_CREATE)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return domain.FileID{}, false, fmt.Errorf("setxattr %s: %w", path, err)
	}

	// Someone else won. Their value is the one on disk, so adopt it rather
	// than returning an ID nothing else will ever agree with.
	existing, getErr := getID(path)
	if getErr != nil {
		return domain.FileID{}, false, fmt.Errorf("setxattr %s raced and the winning value is unreadable: %w", path, getErr)
	}
	return existing, false, nil
}

func getID(path string) (domain.FileID, error) {
	var id domain.FileID
	buf := make([]byte, 16)
	n, err := unix.Getxattr(path, domain.XAttrIDKey(), buf)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			return id, os.ErrNotExist
		}
		if errors.Is(err, unix.ENOENT) {
			return id, os.ErrNotExist
		}
		// ERANGE means the xattr exists but doesn't fit in our 16-byte
		// buffer (i.e. an admin or backup tool wrote a non-UUID value
		// over user.filegate.id). Treat it as missing so syncSingle
		// reissues a fresh ID and clobbers the malformed payload.
		// Without this the entire sync fails and the path is never
		// re-indexed.
		if errors.Is(err, unix.ERANGE) {
			return id, os.ErrNotExist
		}
		return id, fmt.Errorf("getxattr %s: %w", path, err)
	}
	if n != 16 {
		return id, os.ErrNotExist
	}
	copy(id[:], buf[:16])
	return id, nil
}
