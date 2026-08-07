package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/k2b-dev/filegate/v3/domain"
)

// Store implements domain.Store using OS filesystem operations.
type Store struct{}

// New returns a ready-to-use filesystem Store.
func New() *Store {
	return &Store{}
}

func (s *Store) Abs(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	if rp, err := filepath.EvalSymlinks(abs); err == nil {
		return rp, nil
	}
	return abs, nil
}

func (s *Store) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (s *Store) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (s *Store) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (s *Store) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (s *Store) Remove(path string) error {
	return os.Remove(path)
}

func (s *Store) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (s *Store) RenameNoReplace(oldPath, newPath string) error {
	return renameNoReplace(oldPath, newPath)
}

func (s *Store) OpenRead(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (s *Store) OpenWrite(path string, perm os.FileMode) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if perm == 0 {
		perm = 0o644
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
}

func (s *Store) SetID(path string, id domain.FileID) error {
	return setID(path, id)
}

// SetIDIfAbsent assigns an ID only if the path has none yet, returning the ID
// that ended up on disk and whether this call wrote it.
func (s *Store) SetIDIfAbsent(path string, id domain.FileID) (domain.FileID, bool, error) {
	return setIDIfAbsent(path, id)
}

func (s *Store) GetID(path string) (domain.FileID, error) {
	return getID(path)
}

func (s *Store) CloneFile(srcPath, dstPath string) (bool, error) {
	return CloneFile(srcPath, dstPath)
}
