//go:build linux

package integration_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
	"github.com/k2b-dev/filegate/v6/infra/filesystem"
	"github.com/k2b-dev/filegate/v6/infra/pebble"
)

func TestTreeCopyPublishesCompleteTargetWithFreshIdentities(t *testing.T) {
	x := setup(t, true, true)
	original := put(t, x.r, "source/nested/file", "source bytes", domain.WriteOptions{})
	if _, err := x.r.Snapshot(original.Path, true, domain.Metadata{"message": "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(x.data, original.Path))
	if err != nil {
		t.Fatal(err)
	}
	result, err := domain.Transfer(ctx, x.r, "source", x.r, "source", false, domain.WriteOptions{OnConflict: "rename"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path == "source" || !result.Directory || !strings.HasPrefix(result.Path, "source-") {
		t.Fatalf("actual destination: %+v", result)
	}
	copied, err := x.r.Stat(result.Path + "/nested/file")
	if err != nil {
		t.Fatal(err)
	}
	if copied.ID == original.ID || copied.ID == "" || read(t, x.r, copied.Path) != "source bytes" {
		t.Fatalf("copy identity/content: %+v", copied)
	}
	versions, err := x.r.Versions(original.Path)
	if err != nil || len(versions) != 1 {
		t.Fatalf("source history %+v %v", versions, err)
	}
	after, err := os.Stat(filepath.Join(x.data, original.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) {
		t.Fatal("source inode or mtime changed")
	}
	page, err := x.r.Search(ctx, "file", result.Path, domain.ListingOptions{Sort: "size"})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != copied.ID {
		t.Fatalf("tree not indexed %+v %v", page, err)
	}
	if err := x.r.State.Scan("tree/", func(string, []byte) error { t.Error("completed tree left manifest"); return nil }); err != nil {
		t.Fatal(err)
	}
}

type failingTreeSource struct {
	domain.Files
	target   string
	leaves   int
	observed bool
}

func (f *failingTreeSource) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	if strings.HasPrefix(p, "source/") {
		if _, err := os.Stat(f.target); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("partial target became visible")
		}
		f.leaves++
		if f.leaves == 2 {
			f.observed = true
			return nil, os.ErrPermission
		}
	}
	return f.Files.Open(p, flags, mode)
}
func TestTreeCopyFailureLeavesNoVisibleDestination(t *testing.T) {
	x := setup(t, true, false)
	put(t, x.r, "source/a", "A", domain.WriteOptions{})
	put(t, x.r, "source/b", "B", domain.WriteOptions{})
	wrapped := &failingTreeSource{Files: x.r.Files, target: filepath.Join(x.data, "destination")}
	x.r.Files = wrapped
	if _, err := domain.Transfer(ctx, x.r, "source", x.r, "destination", false, domain.WriteOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected source failure: %v", err)
	}
	if !wrapped.observed {
		t.Fatal("failure was not injected after initial copy work")
	}
	if _, err := os.Stat(wrapped.target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial tree published: %v", err)
	}
	if err := x.r.State.Scan("tree/", func(string, []byte) error { t.Error("failed preparation left manifest"); return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(x.data, ".filegate/staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preparation left staging: %v %v", entries, err)
	}
}

func TestTreeCopyRejectsDescendantAndSymlink(t *testing.T) {
	x := setup(t, true, false)
	put(t, x.r, "source/a", "A", domain.WriteOptions{})
	if _, err := domain.Transfer(ctx, x.r, "source", x.r, "source/sub/copy", false, domain.WriteOptions{OnConflict: "rename"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("descendant accepted: %v", err)
	}
	if err := os.Symlink("a", filepath.Join(x.data, "source/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.Transfer(ctx, x.r, "source", x.r, "copy", false, domain.WriteOptions{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("symlink copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported tree partially published")
	}
}

func TestTreeCopyExecutionRightsAndExplicitOwnership(t *testing.T) {
	x := executionFixture(t, true, false)
	put(t, x.r, "source/readable", "A", domain.WriteOptions{})
	actor := executionView(t, x)
	uid, gid := 32001, int(executionGID)
	node, err := domain.Transfer(ctx, actor, "source", actor, "copy", false, domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0640", DirMode: "2770"}})
	if err != nil {
		t.Fatal(err)
	}
	if node.UID != uint32(uid) || node.GID != uint32(gid) || node.Mode != "2770" {
		t.Fatalf("root ownership %+v", node)
	}
	child, err := x.r.Stat(node.Path + "/readable")
	if err != nil || child.UID != uint32(uid) || child.GID != uint32(gid) || child.Mode != "0640" {
		t.Fatalf("child ownership %+v %v", child, err)
	}
	if err := os.WriteFile(filepath.Join(x.data, "source/private"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.Transfer(ctx, actor, "source", actor, "denied", false, domain.WriteOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable bytes copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "denied")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied tree published")
	}
}

// The unprivileged daemon can publish a prepared inaccessible tree because
// completion consumes the manifest rather than reopening the destination.
func TestTreeCopyUnprivilegedChild(t *testing.T) {
	if os.Getenv("FILEGATE_TREE_CHILD") != "1" {
		t.Skip("credential helper")
	}
	base := os.Getenv("FILEGATE_TREE_BASE")
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	files, err := filesystem.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	state, err := pebble.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := domain.NewRoot(domain.RootConfig{Name: "files", Path: data, Index: true, Managed: true}, files, state, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "source/sub/file", "data", domain.WriteOptions{})
	result, err := domain.Transfer(ctx, root, "source", root, "copy", false, domain.WriteOptions{Ownership: &domain.Ownership{DirMode: "0700", Mode: "0000"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "0700" {
		t.Fatal(result)
	}
	state.Close()
	files.Close()
	files, err = filesystem.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	state, err = pebble.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	root, err = domain.NewRoot(domain.RootConfig{Name: "files", Path: data, Index: true, Managed: true}, files, state, 1<<20)
	if err != nil {
		t.Fatalf("reopen inaccessible copied tree: %v", err)
	}
	page, err := root.Search(ctx, "file", "copy", domain.ListingOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Revision == "" {
		t.Fatalf("manifest metadata unavailable %+v %v", page, err)
	}
	// Leave an inaccessible, unpublished private tree to exercise startup cleanup.
	if err := os.Mkdir(filepath.Join(data, ".filegate/staging/orphan"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(data, ".filegate/staging/orphan/sub"), 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(data, ".filegate/staging/orphan"), 0000); err != nil {
		t.Fatal(err)
	}
	if err := root.Files.Remove(".filegate/staging/orphan", true); err != nil {
		t.Fatalf("private inaccessible cleanup: %v", err)
	}
	// Native rename cannot move an unwritable directory between parents.
	if _, err := domain.Transfer(ctx, root, "source", root, "denied", false, domain.WriteOptions{Ownership: &domain.Ownership{DirMode: "0000"}}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unwritable-directory publication: %v", err)
	}
	if _, err := root.Stat("source"); err != nil {
		t.Fatalf("failed publication did not recover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "denied")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied target was published")
	}
	if err := state.Scan("tree/", func(string, []byte) error { t.Error("failed tree retained manifest"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestTreeCopyUnprivilegedPublicationAndCleanup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch isolated unprivileged daemon")
	}
	base, err := os.MkdirTemp("", "filegate-tree-unprivileged-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	const uid, gid = 33001, 33002
	if err := os.Chown(base, uid, gid); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	childBinary := filepath.Join(base, "test-binary")
	output, err := os.OpenFile(childBinary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		t.Fatal(err)
	}
	output.Close()
	cmd := exec.Command(childBinary, "-test.run=^TestTreeCopyUnprivilegedChild$", "-test.v")
	cmd.Env = append(os.Environ(), "FILEGATE_TREE_CHILD=1", "FILEGATE_TREE_BASE="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged tree: %v\n%s", err, out)
	}
}

type failTreeFinalization struct {
	domain.State
	fail bool
}

func (s *failTreeFinalization) Batch(changes []domain.Change) error {
	if s.fail {
		for _, change := range changes {
			if change.Delete && strings.HasPrefix(change.Key, "tree/") {
				s.fail = false
				return errors.New("injected tree completion failure")
			}
		}
	}
	return s.State.Batch(changes)
}
func TestTreeCopyRecoversPublishedManifestWithoutChangingIDs(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			x := setup(t, true, false)
			put(t, x.r, "source/a", "A", domain.WriteOptions{})
			put(t, x.r, "source/sub/b", "B", domain.WriteOptions{})
			failure := &failTreeFinalization{State: x.r.State, fail: true}
			x.r.State = failure
			if _, err := domain.Transfer(ctx, x.r, "source", x.r, "copy", false, domain.WriteOptions{}); err == nil || !strings.Contains(err.Error(), "injected tree completion") {
				t.Fatalf("completion failure not injected: %v", err)
			}
			// Namespace publication is already complete, including every child.
			f, err := x.files.Open("copy/sub/b", os.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			id, err := x.files.ID(f)
			f.Close()
			if err != nil || id == "" {
				t.Fatal(id, err)
			}
			if restart {
				reopen(t, x)
			}
			recovered, err := x.r.Stat("copy/sub/b")
			if err != nil || recovered.ID != id {
				t.Fatalf("recovery changed child identity: %+v %v", recovered, err)
			}
			page, err := x.r.Search(ctx, "", "copy", domain.ListingOptions{Sort: "size", Type: "files"})
			if err != nil || len(page.Items) != 2 {
				t.Fatalf("recovered index: %+v %v", page, err)
			}
			for _, prefix := range []string{"pending/", "tree/"} {
				if err := x.r.State.Scan(prefix, func(string, []byte) error { t.Errorf("recovery left %s rows", prefix); return nil }); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
