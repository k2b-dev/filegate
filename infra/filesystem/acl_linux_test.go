//go:build linux

package filesystem

import (
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
	"golang.org/x/sys/unix"
)

func TestACLCodec(t *testing.T) {
	// Linux UAPI version 2, followed by owner, named user 1234, owning group,
	// mask, other. Fixture is independent of the codec implementation.
	fixture, e := hex.DecodeString("0200000001000700ffffffff02000600d204000004000500ffffffff10000600ffffffff20000000ffffffff")
	if e != nil {
		t.Fatal(e)
	}
	id := uint32(1234)
	want := domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rwx"},
		{Tag: domain.ACLUser, ID: &id, Permissions: "rw-"},
		{Tag: domain.ACLOwningGroup, Permissions: "r-x"},
		{Tag: domain.ACLMask, Permissions: "rw-"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
	got, e := decodeACL(fixture)
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decode: %+v, %v", got, e)
	}
	encoded, e := encodeACL(want)
	if e != nil || !reflect.DeepEqual(encoded, fixture) {
		t.Fatalf("encode: %x, %v", encoded, e)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"short header":        func(b []byte) []byte { return b[:3] },
		"partial entry":       func(b []byte) []byte { return b[:len(b)-1] },
		"wrong version":       func(b []byte) []byte { b[0] = 3; return b },
		"invalid tag":         func(b []byte) []byte { b[4] = 3; return b },
		"invalid permissions": func(b []byte) []byte { b[6] = 8; return b },
		"invalid base id":     func(b []byte) []byte { b[8] = 0; return b },
		"invalid named id": func(b []byte) []byte {
			for i := 16; i < 20; i++ {
				b[i] = 255
			}
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, e := decodeACL(mutate(append([]byte{}, fixture...)))
			if !errors.Is(e, domain.ErrInvalidACL) {
				t.Fatalf("expected invalid ACL, got %v", e)
			}
		})
	}
}

func TestACLErrorClassification(t *testing.T) {
	if !errors.Is(aclError("read ACL", unix.ENOTSUP), domain.ErrACLUnsupported) {
		t.Fatal("unsupported ACL lost its error class")
	}
	for _, e := range []error{unix.EPERM, unix.EACCES} {
		if !errors.Is(aclError("set ACL", e), os.ErrPermission) {
			t.Fatal("ACL permission error lost its error class")
		}
	}
}

func TestACLFilesystemRoundTrip(t *testing.T) {
	root, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	if e = root.Mkdir("group", 0700); e != nil {
		t.Fatal(e)
	}
	dir, e := root.Open("group", os.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer dir.Close()
	id := uint32(os.Getuid())
	acl := domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOwner, Permissions: "rwx"},
		{Tag: domain.ACLUser, ID: &id, Permissions: "r-x"},
		{Tag: domain.ACLOwningGroup, Permissions: "rwx"},
		{Tag: domain.ACLMask, Permissions: "r-x"},
		{Tag: domain.ACLOther, Permissions: "---"},
	}}
	if e = root.SetACL(dir, domain.DefaultACL, acl); e != nil {
		t.Fatal(e)
	}
	st, e := dir.Stat()
	if e != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("default ACL changed directory access mode: %v %v", st, e)
	}
	got, e := root.GetACL(dir, domain.DefaultACL)
	if e != nil || !reflect.DeepEqual(got, acl) {
		t.Fatalf("default ACL round trip: %+v %v", got, e)
	}
	if e = root.SetACL(dir, domain.AccessACL, acl); e != nil {
		t.Fatal(e)
	}
	st, e = dir.Stat()
	if e != nil || st.Mode().Perm() != 0750 {
		t.Fatalf("access ACL mask not reflected in mode: %v %v", st, e)
	}
	minimal := domain.ACLFromMode(0710)
	if e = root.SetACL(dir, domain.AccessACL, minimal); e != nil {
		t.Fatal(e)
	}
	got, e = root.GetACL(dir, domain.AccessACL)
	if e != nil || !reflect.DeepEqual(got, minimal) {
		t.Fatalf("minimal access ACL replacement: %+v %v", got, e)
	}
	if _, e = unix.Fgetxattr(int(dir.Fd()), "system.posix_acl_access", nil); !errors.Is(e, unix.ENODATA) {
		t.Fatalf("minimal ACL did not remove extended access xattr: %v", e)
	}
	for i := 0; i < 2; i++ {
		if e = root.ClearDefaultACL(dir); e != nil {
			t.Fatalf("clear default ACL %d: %v", i, e)
		}
	}
	got, e = root.GetACL(dir, domain.DefaultACL)
	if e != nil || got.Entries == nil || len(got.Entries) != 0 {
		t.Fatalf("missing default ACL must be nonnil empty entries: %+v %v", got, e)
	}
}
