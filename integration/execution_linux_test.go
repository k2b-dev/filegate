//go:build linux

package integration_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
	"github.com/k2b-dev/filegate/v5/infra/filesystem"
)

func TestMain(m *testing.M) {
	if handled, err := filesystem.RunExecutionWorker(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const executionUID uint32 = 21001
const executionGID uint32 = 21002
const executionGroup uint32 = 21003

func executionFixture(t *testing.T, index, versions bool) *fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	x := setup(t, index, versions)
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0777); err != nil {
		t.Fatal(err)
	}
	return x
}

func executionView(t *testing.T, x *fixture, groups ...uint32) *domain.Root {
	t.Helper()
	r, close, err := x.r.WithExecution(ctx, &domain.ExecutionIdentity{UID: executionUID, GID: executionGID, Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	return r
}

func executionFile(t *testing.T, x *fixture, p string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(x.data, p), []byte("private bytes"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.data, p), mode); err != nil {
		t.Fatal(err)
	}
}

func requireExecutionDenied(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected kernel permission error, got %v", err)
	}
}

func TestExecutionReadTraversalAndLeafPermissions(t *testing.T) {
	x := executionFixture(t, false, false)
	executionFile(t, x, "private", 0600)
	if err := os.Mkdir(filepath.Join(x.data, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	executionFile(t, x, "blocked/leaf", 0644)
	if err := os.Mkdir(filepath.Join(x.data, "search"), 0711); err != nil {
		t.Fatal(err)
	}
	executionFile(t, x, "search/leaf", 0644)
	r := executionView(t, x)
	for _, p := range []string{"private", "blocked/leaf"} {
		f, err := r.Open(p)
		if f != nil {
			f.Close()
		}
		requireExecutionDenied(t, err)
	}
	if got := read(t, r, "search/leaf"); got != "private bytes" {
		t.Fatal(got)
	}
	if _, err := r.List(ctx, "search", domain.ListingOptions{Limit: 10}); err == nil {
		t.Fatal("execute-only directory was listed")
	} else {
		requireExecutionDenied(t, err)
	}
	if n, err := r.Stat("private"); err != nil || n.Size != int64(len("private bytes")) {
		t.Fatalf("stat should not require content read: %+v %v", n, err)
	}
}

func TestExecutionSupplementaryGroupACLAndMask(t *testing.T) {
	x := executionFixture(t, false, false)
	executionFile(t, x, "group", 0600)
	group := executionGroup
	acl := domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rw-"},
		{Tag: domain.ACLOwningGroup, Permissions: "---"},
		{Tag: domain.ACLGroup, ID: &group, Permissions: "r--"},
		{Tag: domain.ACLMask, Permissions: "r--"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
	setACL(t, x.r, "group", domain.AccessACL, acl)
	allowed := executionView(t, x, group)
	denied := executionView(t, x)
	if got := read(t, allowed, "group"); got != "private bytes" {
		t.Fatal(got)
	}
	f, err := denied.Open("group")
	if f != nil {
		f.Close()
	}
	requireExecutionDenied(t, err)
	acl.Entries[3].Permissions = "---"
	setACL(t, x.r, "group", domain.AccessACL, acl)
	f, err = allowed.Open("group")
	if f != nil {
		f.Close()
	}
	requireExecutionDenied(t, err)
}

func TestExecutionPublicationPermissionsAndInheritance(t *testing.T) {
	x := executionFixture(t, false, false)
	if err := os.Mkdir(filepath.Join(x.data, "denied"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "drop"), 0733); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.data, "drop"), 0733); err != nil {
		t.Fatal(err)
	}
	r := executionView(t, x, executionGroup)
	_, err := r.Put(ctx, "denied/file", strings.NewReader("denied"), domain.WriteOptions{})
	requireExecutionDenied(t, err)
	if _, err := os.Stat(filepath.Join(x.data, "denied/file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed write published a file: %v", err)
	}
	n := put(t, r, "drop/file", "written", domain.WriteOptions{})
	if n.UID != executionUID || n.GID != executionGID {
		t.Fatalf("default ownership: %+v", n)
	}
	uid, gid := 0, int(executionGroup)
	if _, err := x.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	setACL(t, x.r, "shared", domain.DefaultACL, sharedACL())
	put(t, r, "shared/file", "shared", domain.WriteOptions{})
	assertKernelRights(t, x, "shared/file", 0660, int(executionUID), gid)
	if _, err := r.Mkdir("shared/child", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "shared/child", 02770, int(executionUID), gid)
	assertACL(t, x.r, "shared/child", domain.DefaultACL, sharedACL())
}

func TestExecutionStickyReplacementAndPublicMetadata(t *testing.T) {
	x := executionFixture(t, false, false)
	if err := os.Mkdir(filepath.Join(x.data, "sticky"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.data, "sticky"), os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	executionFile(t, x, "sticky/other", 0644)
	executionFile(t, x, "metadata", 0666)
	r := executionView(t, x)
	_, err := r.Put(ctx, "sticky/other", strings.NewReader("replace"), domain.WriteOptions{OnConflict: "overwrite"})
	requireExecutionDenied(t, err)
	if got := read(t, x.r, "sticky/other"); got != "private bytes" {
		t.Fatal("failed sticky replacement changed content")
	}
	_, err = r.SetOwnership("metadata", &domain.Ownership{Mode: "0600"})
	requireExecutionDenied(t, err)
	_, err = r.SetACL("metadata", domain.AccessACL, sharedACL())
	requireExecutionDenied(t, err)
	n, err := x.r.Stat("metadata")
	if err != nil || n.Mode != "0666" {
		t.Fatalf("failed metadata mutation changed mode: %+v %v", n, err)
	}
}

func TestExecutionSessionRetainsIdentityAndExplicitOwnership(t *testing.T) {
	x := executionFixture(t, false, false)
	r := executionView(t, x)
	uid, gid := 22001, 22002
	opts := domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0640"}}
	put(t, r, "direct", "abc", opts)
	assertKernelRights(t, x, "direct", 0640, uid, gid)
	s, err := r.CreateSession("session", 3, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Execution == nil || s.Execution.UID != executionUID {
		t.Fatalf("session did not retain identity: %+v", s.Execution)
	}
	if _, err := x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.CommitSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "session", 0640, uid, gid)
	if err := os.Mkdir(filepath.Join(x.data, "locked"), 0755); err != nil {
		t.Fatal(err)
	}
	s, err = r.CreateSession("locked/denied", 3, domain.WriteOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	_, err = x.r.CommitSession(ctx, s.ID)
	requireExecutionDenied(t, err)
	if _, err := os.Stat(filepath.Join(x.data, "locked/denied")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("privileged commit bypassed stored identity: %v", err)
	}
}

func TestExecutionHistoricalReadUsesCurrentPermissions(t *testing.T) {
	x := executionFixture(t, true, true)
	put(t, x.r, "history", "old", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	v, err := x.r.Snapshot("history", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "history", "new", domain.WriteOptions{OnConflict: "overwrite"})
	r := executionView(t, x)
	f, err := r.OpenVersion("history", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(b) != "old" {
		t.Fatalf("historical content %q %v", b, err)
	}
	if err := os.Chmod(filepath.Join(x.data, "history"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Versions("history"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("version metadata bypassed current read permission: %v", err)
	}
	f, err = r.OpenVersion("history", v.ID)
	if f != nil {
		f.Close()
	}
	requireExecutionDenied(t, err)
}

func TestExecutionRecursiveCopyDoesNotReadDeniedLeaf(t *testing.T) {
	x := executionFixture(t, false, false)
	if err := os.Mkdir(filepath.Join(x.data, "source"), 0755); err != nil {
		t.Fatal(err)
	}
	executionFile(t, x, "source/a-readable", 0644)
	executionFile(t, x, "source/b-private", 0600)
	r := executionView(t, x)
	_, err := domain.Transfer(ctx, r, "source", r, "copy", false, domain.WriteOptions{})
	requireExecutionDenied(t, err)
	if _, err := os.Stat(filepath.Join(x.data, "copy/b-private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy exposed denied leaf: %v", err)
	}
}

func TestExecutionRecursiveRemoveUsesNamespaceQuarantineRights(t *testing.T) {
	x := executionFixture(t, false, false)
	if err := os.Mkdir(filepath.Join(x.data, "tree"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(x.data, "tree"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "tree/protected"), 0755); err != nil {
		t.Fatal(err)
	}
	executionFile(t, x, "tree/protected/file", 0644)
	r := executionView(t, x)
	if err := r.Remove("tree", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "tree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authorized namespace removal failed: %v", err)
	}
}

func TestExecutionReplaceUsesNativeDirectoryPermissions(t *testing.T) {
	x := executionFixture(t, false, false)
	executionFile(t, x, "readonly", 0444)
	r := executionView(t, x)
	// Linux permits replacement of a read-only inode through a writable parent.
	// The replacement preserves ownership and permissions of the existing file.
	put(t, r, "readonly", "replacement", domain.WriteOptions{OnConflict: "overwrite"})
	if got := read(t, r, "readonly"); got != "replacement" {
		t.Fatal(got)
	}
	assertKernelRights(t, x, "readonly", 0444, 0, 0)
	if err := os.Mkdir(filepath.Join(x.data, "locked"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := r.Move("readonly", "locked/moved")
	requireExecutionDenied(t, err)
	if got := read(t, x.r, "readonly"); got != "replacement" {
		t.Fatal("failed move changed source")
	}
}

func TestExecutionVersionReadRecoversPublishedIdentity(t *testing.T) {
	x := executionFixture(t, true, true)
	original := put(t, x.r, "history", "old", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	v, err := x.r.Snapshot("history", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := &failingState{State: x.state, fail: true}
	x.r.State = state
	r := executionView(t, x)
	_, err = r.Put(ctx, "history", strings.NewReader("new"), domain.WriteOptions{OnConflict: "overwrite"})
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("publication failure not injected: %v", err)
	}
	state.fail = false
	f, err := r.OpenVersion("history", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(b) != "old" {
		t.Fatalf("history after recovery: %q %v", b, err)
	}
	n, err := r.Stat("history")
	if err != nil || n.ID != original.ID {
		t.Fatalf("recovery lost identity: %+v %v", n, err)
	}
	if got := read(t, r, "history"); got != "new" {
		t.Fatalf("recovery changed publication: %q", got)
	}
}

func TestExecutionExplicitOwnershipForImplicitAndCopiedDirectories(t *testing.T) {
	x := executionFixture(t, false, false)
	r := executionView(t, x, executionGroup)
	// A different owner with an actor-accessible group exercises privileged
	// provisioning of private preparation plus actor-checked publication.
	uid, gid := 22001, int(executionGroup)
	opts := domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0660", DirMode: "2770"}}
	put(t, r, "implicit/nested/file", "abc", opts)
	assertKernelRights(t, x, "implicit", 02770, uid, gid)
	assertKernelRights(t, x, "implicit/nested", 02770, uid, gid)
	assertKernelRights(t, x, "implicit/nested/file", 0660, uid, gid)
	if _, err := domain.Transfer(ctx, r, "implicit", r, "copied", false, opts); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "copied", 02770, uid, gid)
	assertKernelRights(t, x, "copied/nested", 02770, uid, gid)
	assertKernelRights(t, x, "copied/nested/file", 0660, uid, gid)
}

func TestExecutionHistoricalReadRejectsReplacementIdentity(t *testing.T) {
	x := executionFixture(t, true, true)
	original := put(t, x.r, "history", "original", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	version, err := x.r.Snapshot("history", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := x.files.Open("replacement", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("replacement"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	// Copying the visible identity xattr to a different inode must not grant its
	// history. The durable identity claim binds the original device and inode.
	if err := x.files.SetID(f, original.ID); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(x.data, "replacement"), filepath.Join(x.data, "history")); err != nil {
		t.Fatal(err)
	}
	r := executionView(t, x)
	f, err = r.OpenVersion("history", version.ID)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement inherited unrelated history: %v", err)
	}
}
