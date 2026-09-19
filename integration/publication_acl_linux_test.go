//go:build linux

package integration_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestPublicationExplicitAccessACL(t *testing.T) {
	id := uint32(21991)
	acl := domain.ACL{Entries: []domain.ACLEntry{
		{Tag: domain.ACLOther, Permissions: "---"},
		{Tag: domain.ACLOwner, Permissions: "rw-"},
		{Tag: domain.ACLUser, ID: &id, Permissions: "r--"},
		{Tag: domain.ACLOwningGroup, Permissions: "---"},
		{Tag: domain.ACLMask, Permissions: "r--"},
	}}
	expected, err := domain.NormalizeACL(acl)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "session"}[session], func(t *testing.T) {
			x := setup(t, false, false)
			opts := domain.WriteOptions{AccessACL: &acl}
			var n domain.Node
			if session {
				s, e := x.r.CreateSession("file", 3, opts, "")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("new")); e != nil {
					t.Fatal(e)
				}
				n, err = x.r.CommitSession(ctx, s.ID)
			} else {
				n, err = x.r.Put(ctx, "file", strings.NewReader("new"), opts)
			}
			if errors.Is(err, domain.ErrACLUnsupported) {
				t.Skip(err)
			}
			if err != nil || n.Mode != "0640" {
				t.Fatal(n, err)
			}
			got, e := x.r.GetACL("file", domain.AccessACL)
			if e != nil || !reflect.DeepEqual(got, expected) {
				t.Fatal(got, e)
			}
			// An explicit mode is applied last: named entries remain, with its group
			// bits defining their effective ACL mask.
			opts.OnConflict = "overwrite"
			opts.Ownership = &domain.Ownership{Mode: "0600"}
			n, e = x.r.Put(ctx, "file", strings.NewReader("replacement"), opts)
			if e != nil || n.Mode != "0600" {
				t.Fatal(n, e)
			}
			got, e = x.r.GetACL("file", domain.AccessACL)
			if e != nil {
				t.Fatal(e)
			}
			want := expected
			want.Entries = append([]domain.ACLEntry(nil), expected.Entries...)
			for i := range want.Entries {
				if want.Entries[i].Tag == domain.ACLMask {
					want.Entries[i].Permissions = "---"
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal(got, want)
			}
		})
	}
}
