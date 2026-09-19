//go:build linux

package integration_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
	sdk "github.com/k2b-dev/filegate/v6/sdk/filegate"
	"golang.org/x/sys/unix"
)

func sharedACL() domain.ACL {
	return domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rwx"},
		{Tag: domain.ACLOwningGroup, Permissions: "rwx"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
}

func namedACL(id uint32) domain.ACL {
	return domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rwx"},
		{Tag: domain.ACLUser, ID: &id, Permissions: "rwx"},
		{Tag: domain.ACLOwningGroup, Permissions: "r-x"},
		{Tag: domain.ACLMask, Permissions: "r-x"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
}

func setACL(t *testing.T, r *domain.Root, p string, scope domain.ACLScope, acl domain.ACL) domain.ACL {
	t.Helper()
	got, err := r.SetACL(p, scope, acl)
	if errors.Is(err, domain.ErrACLUnsupported) {
		t.Skipf("test filesystem does not support POSIX ACLs: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertKernelRights(t *testing.T, x *fixture, p string, mode uint32, uid, gid int) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(x.data, p), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&07777 != mode || st.Uid != uint32(uid) || st.Gid != uint32(gid) {
		t.Fatalf("%s kernel rights: mode=%04o uid=%d gid=%d; want %04o %d:%d", p, st.Mode&07777, st.Uid, st.Gid, mode, uid, gid)
	}
	n, err := x.r.Stat(p)
	if err != nil || n.Mode != fmt.Sprintf("%04o", mode) || n.UID != st.Uid || n.GID != st.Gid {
		t.Fatalf("%s API rights differ from kernel: %+v, %v", p, n, err)
	}
}

func assertACL(t *testing.T, r *domain.Root, p string, scope domain.ACLScope, want domain.ACL) {
	t.Helper()
	got, err := r.GetACL(p, scope)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s %s ACL: got %+v (%v), want %+v", p, scope, got, err, want)
	}
}

func TestACLInheritanceAcrossCreationPaths(t *testing.T) {
	x := setup(t, false, false)
	uid, gid := os.Getuid(), os.Getgid()
	if _, err := x.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	inherited := setACL(t, x.r, "shared", domain.DefaultACL, sharedACL())
	assertKernelRights(t, x, "shared", 02770, uid, gid)
	put(t, x.r, "shared/direct", "direct", domain.WriteOptions{})
	session, err := x.r.CreateSession("shared/resumable", 3, domain.WriteOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.PutSegment(ctx, session.ID, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.CommitSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.Mkdir("shared/child", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "shared/implicit/deep/file", "implicit", domain.WriteOptions{})
	for _, p := range []string{"shared/direct", "shared/resumable", "shared/implicit/deep/file"} {
		assertKernelRights(t, x, p, 0660, uid, gid)
	}
	for _, p := range []string{"shared/child", "shared/implicit", "shared/implicit/deep"} {
		assertKernelRights(t, x, p, 02770, uid, gid)
		assertACL(t, x.r, p, domain.DefaultACL, inherited)
	}
	if _, err = x.r.SetOwnership("shared", &domain.Ownership{UID: &uid, GID: &gid}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "shared", 02770, uid, gid)
	if err = x.r.ClearDefaultACL("shared"); err != nil {
		t.Fatal(err)
	}
	got, err := x.r.GetACL("shared", domain.DefaultACL)
	if err != nil || len(got.Entries) != 0 {
		t.Fatalf("clear default ACL: %+v %v", got, err)
	}
	assertACL(t, x.r, "shared/child", domain.DefaultACL, inherited)
	assertKernelRights(t, x, "shared/direct", 0660, uid, gid)
}

func TestACLOverwriteRestoreMoveAndCopy(t *testing.T) {
	x := setup(t, true, true)
	uid, gid := os.Getuid(), os.Getgid()
	put(t, x.r, "program", "old", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0750"}})
	original := setACL(t, x.r, "program", domain.AccessACL, namedACL(uint32(uid+123)))
	version, err := x.r.Snapshot("program", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "program", "new", domain.WriteOptions{OnConflict: "overwrite"})
	assertACL(t, x.r, "program", domain.AccessACL, original)
	assertKernelRights(t, x, "program", 0750, uid, gid)
	currentACL := namedACL(uint32(uid + 456))
	current := setACL(t, x.r, "program", domain.AccessACL, currentACL)
	if _, err = x.r.Restore("program", version.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(t, x.r, "program"); got != "old" {
		t.Fatalf("restore content: %q", got)
	}
	assertACL(t, x.r, "program", domain.AccessACL, current)
	assertKernelRights(t, x, "program", 0750, uid, gid)
	if _, err = x.r.Mkdir("target", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	setACL(t, x.r, "target", domain.DefaultACL, sharedACL())
	if _, err = x.r.Move("program", "target/moved"); err != nil {
		t.Fatal(err)
	}
	assertACL(t, x.r, "target/moved", domain.AccessACL, current)
	assertKernelRights(t, x, "target/moved", 0750, uid, gid)
	if _, err = domain.Transfer(ctx, x.r, "target/moved", x.r, "target/copied", false, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "target/copied", 0660, uid, gid)
	copied, err := x.r.GetACL("target/copied", domain.AccessACL)
	if err != nil || len(copied.Entries) != 3 {
		t.Fatalf("copy retained source named ACL: %+v %v", copied, err)
	}
	put(t, x.r, "source/sub/file", "tree", domain.WriteOptions{})
	if _, err = domain.Transfer(ctx, x.r, "source", x.r, "target/tree", false, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "target/tree", 02770, uid, gid)
	assertKernelRights(t, x, "target/tree/sub", 02770, uid, gid)
	assertKernelRights(t, x, "target/tree/sub/file", 0660, uid, gid)
	// Explicit chmod changes the ACL mask, but must retain the named entries.
	if _, err = x.r.SetOwnership("target/moved", &domain.Ownership{Mode: "0640"}); err != nil {
		t.Fatal(err)
	}
	changed, err := x.r.GetACL("target/moved", domain.AccessACL)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range changed.Entries {
		if entry.Tag == domain.ACLUser && entry.ID != nil && *entry.ID == uint32(uid+456) {
			found = true
		}
	}
	if !found {
		t.Fatalf("chmod discarded named ACL: %+v", changed)
	}
	assertKernelRights(t, x, "target/moved", 0640, uid, gid)
}

func TestACLStagingDoesNotLeakIntoPublishedFile(t *testing.T) {
	x := setup(t, false, false)
	stage, err := x.files.Open(".filegate/staging", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if err = x.files.SetACL(stage, domain.DefaultACL, namedACL(uint32(os.Getuid()+123))); errors.Is(err, domain.ErrACLUnsupported) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "plain", "plain", domain.WriteOptions{})
	acl, err := x.r.GetACL("plain", domain.AccessACL)
	if err != nil || len(acl.Entries) != 3 {
		t.Fatalf("private staging ACL leaked: %+v %v", acl, err)
	}
	assertKernelRights(t, x, "plain", 0644, os.Getuid(), os.Getgid())
}

func TestACLHTTPDirectAndResumableOwnership(t *testing.T) {
	x, _, client := server(t)
	r := client.Root("test")
	uid, gid := os.Getuid(), os.Getgid()
	if _, err := r.Mkdir(ctx, "shared", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	acl, err := r.SetACL(ctx, "shared", sdk.DefaultACL, sharedACL())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetACL(ctx, "shared", sdk.DefaultACL)
	if err != nil || !reflect.DeepEqual(acl, got) {
		t.Fatalf("HTTP ACL round trip: %+v %v", got, err)
	}
	if os.Geteuid() == 0 {
		uid, gid = 41006, 42006
	}
	owner := &domain.Ownership{UID: &uid, GID: &gid, Mode: "0640", DirMode: "2770"}
	// Minted ownership and metadata must not be replaceable by upload URL parameters.
	upload, err := r.DirectUpload(ctx, "shared/direct", 3, domain.WriteOptions{Ownership: owner, Metadata: domain.Metadata{"message": "bound direct"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PUT", upload.URL+"?mode=0777&uid=12345&gid=12345", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("direct upload: %s %s", resp.Status, body)
	}
	session, err := r.CreateSession(ctx, "shared/resumable", 3, domain.WriteOptions{Ownership: owner, Metadata: domain.Metadata{"message": "bound session"}}, sdk.SessionCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	direct := sdk.DirectSession{URL: session.Lease.URL + "?mode=0777&uid=12345&gid=12345"}
	if _, err = direct.Put(ctx, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err = r.CommitSession(ctx, session.Session.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"shared/direct", "shared/resumable"} {
		assertKernelRights(t, x, p, 0640, uid, gid)
		if _, err = r.Put(ctx, p, strings.NewReader("new"), 3, domain.WriteOptions{OnConflict: "overwrite"}); err != nil {
			t.Fatal(err)
		}
		assertKernelRights(t, x, p, 0640, uid, gid)
		versions, err := r.Versions(ctx, p)
		message := "bound direct"
		if p == "shared/resumable" {
			message = "bound session"
		}
		if err != nil || len(versions) != 1 || versions[0].Metadata["message"] != message {
			t.Fatalf("bound metadata for %s: %+v %v", p, versions, err)
		}
	}
	if err = r.ClearDefaultACL(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	got, err = r.GetACL(ctx, "shared", sdk.DefaultACL)
	if err != nil || len(got.Entries) != 0 {
		t.Fatalf("HTTP clear default: %+v %v", got, err)
	}
}

// This child process runs under kernel credentials selected by the parent test.
func TestACLExternalProcess(t *testing.T) {
	action := os.Getenv("FILEGATE_ACL_TEST_ACTION")
	if action == "" {
		t.Skip("credential helper process")
	}
	p := os.Getenv("FILEGATE_ACL_TEST_PATH")
	unix.Umask(0077)
	var err error
	switch action {
	case "create":
		err = os.WriteFile(filepath.Join(p, "external"), []byte("external"), 0666)
		if err == nil {
			err = os.Mkdir(filepath.Join(p, "external-dir"), 0777)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(p, "external-dir", "child"), []byte("child"), 0666)
		}
	case "append":
		var f *os.File
		f, err = os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			_, err = f.WriteString("+member")
			f.Close()
		}
	default:
		os.Exit(20)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, os.ErrPermission) {
			os.Exit(10)
		}
		os.Exit(11)
	}
	os.Exit(0)
}

func TestACLExternalWritersAndSetgidInheritance(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to test distinct Unix users and groups")
	}
	x := setup(t, false, false)
	// Make only this fixture's ancestors traversable for the test identities.
	for _, p := range []string{filepath.Dir(filepath.Dir(x.data)), filepath.Dir(x.data), x.data} {
		if err := os.Chmod(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childBinary := filepath.Join(filepath.Dir(x.data), "acl-test")
	in, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(childBinary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(action, p string, uid, gid uint32, groups []uint32, denied bool) {
		t.Helper()
		cmd := exec.Command(childBinary, "-test.run=^TestACLExternalProcess$")
		cmd.Env = append(os.Environ(), "FILEGATE_ACL_TEST_ACTION="+action, "FILEGATE_ACL_TEST_PATH="+filepath.Join(x.data, p))
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
		output, err := cmd.CombinedOutput()
		if denied {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 10 {
				t.Fatalf("expected permission denial: %s %v", output, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("external %s: %s %v", action, output, err)
		}
	}
	owner, group := 41001, 42001
	if _, err = x.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &owner, GID: &group, DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	inherited := setACL(t, x.r, "shared", domain.DefaultACL, sharedACL())
	run("create", "shared", 41002, 42002, []uint32{uint32(group)}, false)
	assertKernelRights(t, x, "shared/external", 0660, 41002, group)
	assertKernelRights(t, x, "shared/external-dir", 02770, 41002, group)
	assertKernelRights(t, x, "shared/external-dir/child", 0660, 41002, group)
	assertACL(t, x.r, "shared/external-dir", domain.DefaultACL, inherited)
	put(t, x.r, "shared/filegate", "filegate", domain.WriteOptions{})
	assertKernelRights(t, x, "shared/filegate", 0660, os.Getuid(), group)
	explicitUID, explicitGID := 41004, 42004
	put(t, x.r, "shared/explicit", "explicit", domain.WriteOptions{Ownership: &domain.Ownership{UID: &explicitUID, GID: &explicitGID, Mode: "0640"}})
	assertKernelRights(t, x, "shared/explicit", 0640, explicitUID, explicitGID)
	for _, p := range []string{"shared/external", "shared/external-dir/child", "shared/filegate"} {
		run("append", p, 41003, 42003, []uint32{uint32(group)}, false)
		run("append", p, 41005, 42005, nil, true)
	}
	// A named user's effective rights are restricted by the mask, including
	// after atomic replacement of the inode during an upload.
	put(t, x.r, "named", "named", domain.WriteOptions{})
	namedID := uint32(41005)
	masked := domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rw-"},
		{Tag: domain.ACLUser, ID: &namedID, Permissions: "rw-"},
		{Tag: domain.ACLOwningGroup, Permissions: "---"},
		{Tag: domain.ACLMask, Permissions: "r--"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
	setACL(t, x.r, "named", domain.AccessACL, masked)
	run("append", "named", namedID, 42005, nil, true)
	masked.Entries[3].Permissions = "rw-"
	setACL(t, x.r, "named", domain.AccessACL, masked)
	run("append", "named", namedID, 42005, nil, false)
	put(t, x.r, "named", "replacement", domain.WriteOptions{OnConflict: "overwrite"})
	run("append", "named", namedID, 42005, nil, false)
	masked.Entries[3].Permissions = "r--"
	setACL(t, x.r, "named", domain.AccessACL, masked)
	put(t, x.r, "named", "replacement", domain.WriteOptions{OnConflict: "overwrite"})
	run("append", "named", namedID, 42005, nil, true)

	// Mode-only writes retain foreign ownership instead of reverting to the daemon.
	put(t, x.r, "shared/explicit", "mode change", domain.WriteOptions{OnConflict: "overwrite", Ownership: &domain.Ownership{Mode: "0600"}})
	assertKernelRights(t, x, "shared/explicit", 0600, explicitUID, explicitGID)
	// Access ACL updates on a parent do not rewrite executable children.
	put(t, x.r, "shared/program", "program", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0750"}})
	setACL(t, x.r, "shared", domain.AccessACL, sharedACL())
	assertKernelRights(t, x, "shared/program", 0750, os.Getuid(), group)
	access := namedACL(41005)
	for i := range access.Entries {
		if access.Entries[i].Tag == domain.ACLMask {
			access.Entries[i].Permissions = "rwx"
		}
	}
	access = setACL(t, x.r, "shared", domain.AccessACL, access)
	newOwner, newGroup := 41007, 42007
	if _, err = x.r.SetOwnership("shared", &domain.Ownership{UID: &newOwner, GID: &newGroup}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "shared", 02770, newOwner, newGroup)
	assertACL(t, x.r, "shared", domain.AccessACL, access)
	assertACL(t, x.r, "shared", domain.DefaultACL, inherited)
	assertKernelRights(t, x, "shared/program", 0750, os.Getuid(), group)
}

type failingACLFiles struct {
	domain.Files
	failure error
}

func (f failingACLFiles) SetACL(file *os.File, scope domain.ACLScope, acl domain.ACL) error {
	return f.failure
}

func TestACLPublishFailurePreservesExistingFile(t *testing.T) {
	for _, failure := range []error{domain.ErrACLUnsupported, os.ErrPermission} {
		t.Run(failure.Error(), func(t *testing.T) {
			x := setup(t, false, false)
			put(t, x.r, "original", "keep", domain.WriteOptions{})
			original := setACL(t, x.r, "original", domain.AccessACL, namedACL(uint32(os.Getuid()+123)))
			x.r.Files = failingACLFiles{Files: x.files, failure: failure}
			if _, err := x.r.Put(ctx, "original", strings.NewReader("replacement"), domain.WriteOptions{OnConflict: "overwrite"}); !errors.Is(err, failure) {
				t.Fatalf("ACL failure was ignored: %v", err)
			}
			x.r.Files = x.files
			if got := read(t, x.r, "original"); got != "keep" {
				t.Fatalf("failed ACL publication replaced content: %q", got)
			}
			assertACL(t, x.r, "original", domain.AccessACL, original)
		})
	}
}

func TestACLReplacementDropsFilePrivilegeBits(t *testing.T) {
	for _, special := range []uint32{04755, 02755} {
		t.Run(fmt.Sprintf("%04o", special), func(t *testing.T) {
			x := setup(t, true, true)
			put(t, x.r, "program", "old", domain.WriteOptions{})
			setACL(t, x.r, "program", domain.AccessACL, namedACL(uint32(os.Getuid()+123)))
			full := filepath.Join(x.data, "program")
			if err := unix.Chmod(full, special); err != nil {
				t.Fatal(err)
			}
			assertKernelRights(t, x, "program", special, os.Getuid(), os.Getgid())
			original, err := x.r.GetACL("program", domain.AccessACL)
			if err != nil {
				t.Fatal(err)
			}
			version, err := x.r.Snapshot("program", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			put(t, x.r, "program", "new", domain.WriteOptions{OnConflict: "overwrite"})
			assertKernelRights(t, x, "program", 0755, os.Getuid(), os.Getgid())
			assertACL(t, x.r, "program", domain.AccessACL, original)
			if err = unix.Chmod(full, special); err != nil {
				t.Fatal(err)
			}
			if _, err = x.r.Restore("program", version.ID); err != nil {
				t.Fatal(err)
			}
			assertKernelRights(t, x, "program", 0755, os.Getuid(), os.Getgid())
			assertACL(t, x.r, "program", domain.AccessACL, original)
			if got := read(t, x.r, "program"); got != "old" {
				t.Fatalf("restore content: %q", got)
			}
		})
	}
}

type unavailableACLFiles struct {
	domain.Files
	failure error
}

func (f unavailableACLFiles) GetACL(*os.File, domain.ACLScope) (domain.ACL, error) {
	return domain.ACL{}, f.failure
}
func (f unavailableACLFiles) SetACL(*os.File, domain.ACLScope, domain.ACL) error { return f.failure }
func (f unavailableACLFiles) ClearDefaultACL(*os.File) error                     { return f.failure }

func TestACLUnsupportedAllowsOnlyOrdinaryFileOperations(t *testing.T) {
	x := setup(t, false, false)
	x.r.Files = unavailableACLFiles{Files: x.files, failure: domain.ErrACLUnsupported}
	put(t, x.r, "nested/file", "plain", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0640", DirMode: "0700"}})
	assertKernelRights(t, x, "nested/file", 0640, os.Getuid(), os.Getgid())
	assertKernelRights(t, x, "nested", 0700, os.Getuid(), os.Getgid())
	put(t, x.r, "nested/file", "replacement", domain.WriteOptions{OnConflict: "overwrite"})
	assertKernelRights(t, x, "nested/file", 0640, os.Getuid(), os.Getgid())
	if _, err := x.r.Mkdir("directory", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, "directory", 02770, os.Getuid(), os.Getgid())
	if _, err := x.r.GetACL("directory", domain.DefaultACL); !errors.Is(err, domain.ErrACLUnsupported) {
		t.Fatalf("explicit ACL read: %v", err)
	}
	if _, err := x.r.SetACL("directory", domain.DefaultACL, sharedACL()); !errors.Is(err, domain.ErrACLUnsupported) {
		t.Fatalf("explicit ACL write: %v", err)
	}
	if err := x.r.ClearDefaultACL("directory"); !errors.Is(err, domain.ErrACLUnsupported) {
		t.Fatalf("explicit ACL clear: %v", err)
	}
}

func TestACLReadPermissionFailureDoesNotFallBackToModes(t *testing.T) {
	x := setup(t, false, false)
	put(t, x.r, "existing", "keep", domain.WriteOptions{})
	x.r.Files = unavailableACLFiles{Files: x.files, failure: os.ErrPermission}
	for _, p := range []string{"new", "existing"} {
		if _, err := x.r.Put(ctx, p, strings.NewReader("replacement"), domain.WriteOptions{OnConflict: "overwrite"}); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("ACL read permission error for %s: %v", p, err)
		}
	}
	if _, err := x.r.Mkdir("new-dir", domain.DirectoryOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("parent ACL read permission error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published new file after ACL read failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "new-dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created directory after ACL read failure: %v", err)
	}
	if got := read(t, x.r, "existing"); got != "keep" {
		t.Fatalf("changed existing file after ACL read failure: %q", got)
	}
}

type recordingMkdirFiles struct {
	domain.Files
	modes map[string]os.FileMode
}

func (f recordingMkdirFiles) Mkdir(p string, mode os.FileMode) error {
	f.modes[p] = mode
	return f.Files.Mkdir(p, mode)
}

func TestACLPrivateDirectoriesAreCreatedPrivate(t *testing.T) {
	x := setup(t, false, false)
	setACL(t, x.r, ".", domain.DefaultACL, sharedACL())
	modes := map[string]os.FileMode{}
	x.r.Files = recordingMkdirFiles{Files: x.files, modes: modes}
	if _, err := x.r.Mkdir("private", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "0700"}}); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "implicit/deep/file", "private", domain.WriteOptions{Ownership: &domain.Ownership{DirMode: "0700"}})
	staged := 0
	for p, mode := range modes {
		if !strings.HasPrefix(p, ".filegate/staging/") {
			t.Fatalf("directory created outside private preparation: %s", p)
		}
		staged++
		if mode.Perm() != 0700 {
			t.Fatalf("staged directory was not initially private: %04o", mode.Perm())
		}
	}
	if staged != 3 {
		t.Fatalf("expected three privately prepared directories, got %d", staged)
	}
	for _, p := range []string{"private", "implicit", "implicit/deep"} {
		assertKernelRights(t, x, p, 0700, os.Getuid(), os.Getgid())
	}
}

// Model a filesystem that maps ownership at inode creation. This is not an NFS
// emulator; it verifies that Filegate respects the resulting inode identity.
type mappedCreationFiles struct {
	domain.Files
	uid, gid int
}

func (f mappedCreationFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	file, err := f.Files.Open(p, flags, mode)
	if err != nil {
		return nil, err
	}
	if flags&os.O_CREATE != 0 && strings.HasPrefix(p, ".filegate/staging/") {
		if err = file.Chown(f.uid, f.gid); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}

func TestACLMappedCreationOwnershipIsRespected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to model filesystem ownership mapping")
	}
	x := setup(t, false, false)
	mappedUID, mappedGID := 41007, 42007
	x.r.Files = mappedCreationFiles{Files: x.files, uid: mappedUID, gid: mappedGID}
	put(t, x.r, "mapped", "mapped", domain.WriteOptions{})
	assertKernelRights(t, x, "mapped", 0644, mappedUID, mappedGID)
	explicitUID, explicitGID := 41008, 42008
	put(t, x.r, "explicit", "explicit", domain.WriteOptions{Ownership: &domain.Ownership{UID: &explicitUID, GID: &explicitGID}})
	assertKernelRights(t, x, "explicit", 0644, explicitUID, explicitGID)
	// A setgid destination changes only the default group, not the mapped owner.
	if _, err := x.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "shared/mapped", "mapped", domain.WriteOptions{})
	assertKernelRights(t, x, "shared/mapped", 0644, mappedUID, os.Getgid())
}
