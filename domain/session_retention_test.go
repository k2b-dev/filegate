package domain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type slowCommitFiles struct {
	*versionTestFiles
	advance func()
}

func (f *slowCommitFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	if flags&os.O_CREATE != 0 && f.advance != nil {
		f.advance()
		f.advance = nil
	}
	return f.versionTestFiles.Open(p, flags, mode)
}
func (f *slowCommitFiles) Stat(p string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(f.directory, p))
}
func (f *slowCommitFiles) Rename(from, to string, _ bool) error {
	return os.Rename(filepath.Join(f.directory, from), filepath.Join(f.directory, to))
}
func (f *slowCommitFiles) GetACL(*os.File, ACLScope) (ACL, error) {
	return ACL{}, ErrACLUnsupported
}
func (f *slowCommitFiles) SetACL(*os.File, ACLScope, ACL) error {
	return ErrACLUnsupported
}

func TestSessionOpenKeySurvivesCommitFinishingAfterExpiry(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "direct-replay"
		if cleanup {
			name = "after-maintenance"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.MkdirAll(filepath.Join(directory, ".filegate/staging"), 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			state := newVersionState()
			files := &slowCommitFiles{versionTestFiles: &versionTestFiles{directory: directory}}
			root := &Root{Config: RootConfig{Name: "test"}, State: state, Files: files, MaxBytes: 1, rootShared: &rootShared{now: func() time.Time { return now }}}
			opened, err := root.CreateSession("file", 0, WriteOptions{}, "retry-key")
			if err != nil {
				t.Fatal(err)
			}
			// The commit is admitted while open, but assembly/publication runs beyond
			// the upload deadline. Its result still needs the full terminal retention.
			now = opened.Expires.Add(-time.Minute)
			files.advance = func() { now = opened.Expires.Add(time.Hour) }
			published, err := root.CommitSession(context.Background(), opened.ID)
			if err != nil {
				t.Fatal(err)
			}
			now = opened.Expires.Add(SessionRetention + time.Minute)
			if cleanup {
				if err := root.CleanupSessions(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			replayed, err := root.CreateSession("file", 0, WriteOptions{}, "retry-key")
			if err != nil || replayed.ID != opened.ID || replayed.State != SessionCommitted || replayed.Result == nil || *replayed.Result != published {
				t.Fatalf("retained result lost on open retry: %+v %v", replayed, err)
			}
			now = opened.Expires.Add(time.Hour + SessionRetention)
			if err := root.CleanupSessions(context.Background()); err != nil {
				t.Fatal(err)
			}
			replacement, err := root.CreateSession("file", 0, WriteOptions{}, "retry-key")
			if err != nil || replacement.ID == opened.ID {
				t.Fatalf("expired result prevented key reuse: %+v %v", replacement, err)
			}
		})
	}
}
