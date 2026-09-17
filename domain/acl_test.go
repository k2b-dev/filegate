package domain

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func aclID(id uint32) *uint32 { return &id }

func TestNormalizeACL(t *testing.T) {
	input := ACL{Entries: []ACLEntry{
		{Tag: ACLOther, Permissions: "---"},
		{Tag: ACLGroup, ID: aclID(90), Permissions: "r-x"},
		{Tag: ACLOwningGroup, Permissions: "rwx"},
		{Tag: ACLMask, Permissions: "rw-"},
		{Tag: ACLUser, ID: aclID(101), Permissions: "r--"},
		{Tag: ACLOwner, Permissions: "rwx"},
		{Tag: ACLUser, ID: aclID(0), Permissions: "rwx"},
	}}
	got, e := NormalizeACL(input)
	if e != nil {
		t.Fatal(e)
	}
	wantTags := []ACLTag{ACLOwner, ACLUser, ACLUser, ACLOwningGroup, ACLGroup, ACLMask, ACLOther}
	for i, tag := range wantTags {
		if got.Entries[i].Tag != tag {
			t.Fatalf("entry %d: got %v, want %s", i, got.Entries[i], tag)
		}
	}
	if *got.Entries[1].ID != 0 || *got.Entries[2].ID != 101 {
		t.Fatalf("named users are not ordered by numeric ID: %+v", got)
	}
	*input.Entries[4].ID = 999
	if *got.Entries[2].ID != 101 {
		t.Fatal("normalized ACL aliases caller IDs")
	}
	if input.Entries[0].Tag != ACLOther {
		t.Fatal("normalization reordered caller input")
	}
}

func TestInvalidACL(t *testing.T) {
	base := ACLFromMode(0770)
	tests := map[string]ACL{
		"empty":              {},
		"missing base":       {Entries: base.Entries[:2]},
		"duplicate":          {Entries: append(append([]ACLEntry{}, base.Entries...), base.Entries[0])},
		"named without mask": {Entries: append(append([]ACLEntry{}, base.Entries...), ACLEntry{Tag: ACLGroup, ID: aclID(100), Permissions: "rwx"})},
	}
	for name, bad := range map[string]ACLEntry{
		"unknown tag":       {Tag: "everyone", Permissions: "rwx"},
		"id on owner":       {Tag: ACLOwner, ID: aclID(42), Permissions: "rwx"},
		"no named id":       {Tag: ACLUser, Permissions: "rwx"},
		"reserved id":       {Tag: ACLUser, ID: aclID(^uint32(0)), Permissions: "rwx"},
		"short permissions": {Tag: ACLOwner, Permissions: "rw"},
		"bad permissions":   {Tag: ACLOwner, Permissions: "xwr"},
	} {
		entries := append([]ACLEntry{}, base.Entries...)
		entries[0] = bad
		tests[name] = ACL{Entries: entries}
	}
	for name, acl := range tests {
		t.Run(name, func(t *testing.T) {
			_, e := NormalizeACL(acl)
			if !errors.Is(e, ErrInvalidACL) || !errors.Is(e, ErrInvalid) {
				t.Fatalf("expected invalid ACL, got %v", e)
			}
		})
	}
	tooLarge := ACL{Entries: make([]ACLEntry, ACLMaxEntries+1)}
	if _, e := NormalizeACL(tooLarge); !errors.Is(e, ErrInvalidACL) {
		t.Fatalf("unbounded ACL: %v", e)
	}
}

func TestACLFromMode(t *testing.T) {
	got := ACLFromMode(os.ModeSetgid | 0751)
	want := ACL{Entries: []ACLEntry{
		{Tag: ACLOwner, Permissions: "rwx"},
		{Tag: ACLOwningGroup, Permissions: "r-x"},
		{Tag: ACLOther, Permissions: "--x"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
