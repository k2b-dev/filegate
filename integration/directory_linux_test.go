//go:build linux

package integration_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

type directoryFiles struct {
	domain.Files
	beforeRename func(string, string) error
	failACL      error
	dropACL      bool
}

func (f *directoryFiles) Rename(a, b string, replace bool) error {
	if f.beforeRename != nil {
		if err := f.beforeRename(a, b); err != nil {
			return err
		}
	}
	return f.Files.Rename(a, b, replace)
}

func (f *directoryFiles) SetACL(file *os.File, scope domain.ACLScope, acl domain.ACL) error {
	if f.failACL != nil {
		return f.failACL
	}
	if f.dropACL {
		return nil
	}
	return f.Files.SetACL(file, scope, acl)
}

func TestDirectoryPublicationInstallsMetadataBeforeVisibility(t *testing.T) {
	for _, index := range []bool{false, true} {
		t.Run(fmt.Sprintf("index=%v", index), func(t *testing.T) {
			x := setup(t, index, false)
			uid, gid := os.Getuid(), os.Getgid()
			if uid == 0 {
				uid, gid = 12101, 12102
			}
			access, err := domain.NormalizeACL(namedACL(12103))
			if err != nil {
				t.Fatal(err)
			}
			defaults := sharedACL()
			published := false
			x.r.Files = &directoryFiles{Files: x.files, beforeRename: func(a, b string) error {
				if b != "group" {
					return fmt.Errorf("unexpected publication %s", b)
				}
				if _, err := os.Stat(filepath.Join(x.data, b)); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("destination visible before publication: %v", err)
				}
				f, err := x.files.Open(a, os.O_RDONLY, 0)
				if err != nil {
					return err
				}
				defer f.Close()
				st, err := f.Stat()
				if err != nil {
					return err
				}
				_, _, gotUID, gotGID, _ := x.files.Identity(st)
				if domain.UnixMode(st.Mode()) != 02770 || gotUID != uint32(uid) || gotGID != uint32(gid) {
					return fmt.Errorf("incomplete staged rights: %v %d:%d", st.Mode(), gotUID, gotGID)
				}
				got, err := x.files.GetACL(f, domain.DefaultACL)
				if err != nil || !reflect.DeepEqual(got, defaults) {
					return fmt.Errorf("incomplete staged default ACL: %+v %v", got, err)
				}
				published = true
				return nil
			}}
			n, err := x.r.Mkdir("group", domain.DirectoryOptions{
				Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "2770"},
				ACL:       &domain.DirectoryACLs{Access: &access, Default: &defaults},
			})
			if errors.Is(err, domain.ErrACLUnsupported) {
				t.Skip(err)
			}
			if err != nil || !published || !n.Directory || n.Size != 0 || n.Mode != "2770" || (n.ID != "") != index {
				t.Fatalf("publish directory: %+v %v, published=%v", n, err, published)
			}
			assertKernelRights(t, x, "group", 02770, uid, gid)
			wantAccess, err := x.r.GetACL("group", domain.AccessACL)
			if err != nil {
				t.Fatal(err)
			}
			for i := range access.Entries {
				if access.Entries[i].Tag == domain.ACLMask {
					access.Entries[i].Permissions = "rwx"
				}
			}
			if !reflect.DeepEqual(wantAccess, access) {
				t.Fatalf("explicit dirMode did not adjust mask while preserving named entries: %+v", wantAccess)
			}
			x.r.Files = x.files
			if index {
				resolved, err := x.r.Resolve(n.ID)
				if err != nil || resolved.Path != "group" {
					t.Fatalf("directory identity: %+v %v", resolved, err)
				}
			}
			if _, err := x.r.Mkdir("group/child", domain.DirectoryOptions{}); err != nil {
				t.Fatal(err)
			}
			assertKernelRights(t, x, "group/child", 02770, os.Getuid(), gid)
			assertACL(t, x.r, "group/child", domain.DefaultACL, defaults)
		})
	}
}

func TestDirectoryPublicationRejectsExistingAndMissingParents(t *testing.T) {
	x := setup(t, false, false)
	if _, err := x.r.Mkdir("existing", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "0700"}}); err != nil {
		t.Fatal(err)
	}
	acl := setACL(t, x.r, "existing", domain.DefaultACL, sharedACL())
	if _, err := x.r.Mkdir("existing", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2777"}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("existing directory accepted: %v", err)
	}
	assertKernelRights(t, x, "existing", 0700, os.Getuid(), os.Getgid())
	assertACL(t, x.r, "existing", domain.DefaultACL, acl)
	put(t, x.r, "file", "unchanged", domain.WriteOptions{})
	if _, err := x.r.Mkdir("file", domain.DirectoryOptions{}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("existing file accepted: %v", err)
	}
	if read(t, x.r, "file") != "unchanged" {
		t.Fatal("existing file changed")
	}
	if _, err := x.r.Mkdir("missing/child", domain.DirectoryOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.data, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("implicit ancestor created: %v", err)
	}
}

func TestDirectoryPublicationReplacesPrivateStagingInheritance(t *testing.T) {
	x := setup(t, false, false)
	f, err := x.files.Open(".filegate/staging", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = x.files.SetACL(f, domain.DefaultACL, namedACL(12501)); errors.Is(err, domain.ErrACLUnsupported) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.Mkdir("plain", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	assertACL(t, x.r, "plain", domain.AccessACL, domain.ACLFromMode(0755))
	assertACL(t, x.r, "plain", domain.DefaultACL, domain.ACL{Entries: []domain.ACLEntry{}})
	if _, err := x.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	inherited := setACL(t, x.r, "shared", domain.DefaultACL, sharedACL())
	if _, err := x.r.Mkdir("shared/child", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	assertACL(t, x.r, "shared/child", domain.AccessACL, sharedACL())
	assertACL(t, x.r, "shared/child", domain.DefaultACL, inherited)
}

func TestDirectoryPublicationFailureNeverExposesPartialMetadata(t *testing.T) {
	for _, failure := range []string{"set_acl", "readback", "cross_device"} {
		t.Run(failure, func(t *testing.T) {
			x := setup(t, false, false)
			f := &directoryFiles{Files: x.files}
			wantError := os.ErrPermission
			switch failure {
			case "set_acl":
				f.failACL = os.ErrPermission
			case "readback":
				f.dropACL = true
			case "cross_device":
				f.beforeRename = func(string, string) error { return syscall.EXDEV }
				wantError = domain.ErrCrossDevice
			}
			x.r.Files = f
			acl := sharedACL()
			if _, err := x.r.Mkdir("group", domain.DirectoryOptions{ACL: &domain.DirectoryACLs{Default: &acl}}); !errors.Is(err, wantError) {
				t.Fatalf("expected failure %v: %v", wantError, err)
			}
			if _, err := os.Stat(filepath.Join(x.data, "group")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial directory visible: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(x.data, ".filegate/staging"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging directory leaked: %+v %v", entries, err)
			}
		})
	}
}

func TestDirectoryPublicationRecoversAfterStateFailure(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			x := setup(t, true, false)
			state := &failingState{State: x.state, fail: true}
			x.r.State = state
			acl := sharedACL()
			if _, err := x.r.Mkdir("group", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "2770"}, ACL: &domain.DirectoryACLs{Default: &acl}}); err == nil {
				t.Fatal("state failure not injected")
			}
			state.fail = false
			if restart {
				reopen(t, x)
			}
			n, err := x.r.Stat("group")
			if err != nil || !n.Directory || n.ID == "" || n.Mode != "2770" {
				t.Fatalf("directory recovery: %+v %v", n, err)
			}
			assertACL(t, x.r, "group", domain.DefaultACL, acl)
			resolved, err := x.r.Resolve(n.ID)
			if err != nil || resolved.Path != "group" {
				t.Fatalf("recovered identity: %+v %v", resolved, err)
			}
			page, err := x.r.Search(ctx, "group", ".", domain.ListingOptions{Limit: 10, MaxEntries: 100})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != n.ID {
				t.Fatalf("recovered index: %+v %v", page, err)
			}
		})
	}
}
