package domain

import (
	"fmt"
	"os"
	"sort"
)

type ACLScope string

const (
	AccessACL  ACLScope = "access"
	DefaultACL ACLScope = "default"
)

type ACLTag string

const (
	ACLOwner       ACLTag = "owner"
	ACLUser        ACLTag = "user"
	ACLOwningGroup ACLTag = "owningGroup"
	ACLGroup       ACLTag = "group"
	ACLMask        ACLTag = "mask"
	ACLOther       ACLTag = "other"
)

type ACLPermissions string

type ACLEntry struct {
	Tag         ACLTag         `json:"tag"`
	ID          *uint32        `json:"id,omitempty"`
	Permissions ACLPermissions `json:"permissions"`
}

type ACL struct {
	Entries []ACLEntry `json:"entries"`
}

const ACLMaxEntries = 256

// NormalizeACL validates a complete ACL and returns entries in POSIX order.
// Empty default ACLs are represented by reads, but removed through ClearDefaultACL.
func NormalizeACL(acl ACL) (ACL, error) {
	if len(acl.Entries) < 3 || len(acl.Entries) > ACLMaxEntries {
		return ACL{}, fmt.Errorf("%w: expected 3 to %d entries", ErrInvalidACL, ACLMaxEntries)
	}
	result := ACL{Entries: make([]ACLEntry, len(acl.Entries))}
	seen := make(map[string]bool)
	base := make(map[ACLTag]bool)
	named := false
	for i, entry := range acl.Entries {
		switch entry.Tag {
		case ACLUser, ACLGroup:
			if entry.ID == nil || *entry.ID == ^uint32(0) {
				return ACL{}, fmt.Errorf("%w: named entries require a valid numeric ID", ErrInvalidACL)
			}
			id := *entry.ID
			entry.ID = &id
			named = true
		case ACLOwner, ACLOwningGroup, ACLMask, ACLOther:
			if entry.ID != nil {
				return ACL{}, fmt.Errorf("%w: only named entries accept an ID", ErrInvalidACL)
			}
			base[entry.Tag] = true
		default:
			return ACL{}, fmt.Errorf("%w: unknown entry tag", ErrInvalidACL)
		}
		p := entry.Permissions
		if len(p) != 3 || (p[0] != 'r' && p[0] != '-') || (p[1] != 'w' && p[1] != '-') || (p[2] != 'x' && p[2] != '-') {
			return ACL{}, fmt.Errorf("%w: permissions must use rwx notation", ErrInvalidACL)
		}
		key := string(entry.Tag)
		if entry.ID != nil {
			key += fmt.Sprintf(":%d", *entry.ID)
		}
		if seen[key] {
			return ACL{}, fmt.Errorf("%w: duplicate entry", ErrInvalidACL)
		}
		seen[key] = true
		result.Entries[i] = entry
	}
	if !base[ACLOwner] || !base[ACLOwningGroup] || !base[ACLOther] || named && !base[ACLMask] {
		return ACL{}, fmt.Errorf("%w: owner, owningGroup and other are required; named entries also require a mask", ErrInvalidACL)
	}
	order := map[ACLTag]int{ACLOwner: 0, ACLUser: 1, ACLOwningGroup: 2, ACLGroup: 3, ACLMask: 4, ACLOther: 5}
	sort.Slice(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		if a.Tag != b.Tag {
			return order[a.Tag] < order[b.Tag]
		}
		return a.ID != nil && b.ID != nil && *a.ID < *b.ID
	})
	return result, nil
}

// ACLFromMode returns the minimal access ACL represented by permission bits.
func ACLFromMode(mode os.FileMode) ACL {
	permissions := func(n uint32) ACLPermissions {
		p := []byte("---")
		for i, bit := range []uint32{4, 2, 1} {
			if n&bit != 0 {
				p[i] = "rwx"[i]
			}
		}
		return ACLPermissions(p)
	}
	return ACL{Entries: []ACLEntry{
		{Tag: ACLOwner, Permissions: permissions(uint32(mode.Perm()) >> 6)},
		{Tag: ACLOwningGroup, Permissions: permissions(uint32(mode.Perm()) >> 3)},
		{Tag: ACLOther, Permissions: permissions(uint32(mode.Perm()))},
	}}
}

func (r *Root) aclFile(p string, scope ACLScope) (*os.File, string, error) {
	if scope != AccessACL && scope != DefaultACL {
		return nil, "", fmt.Errorf("%w: scope must be access or default", ErrInvalidACL)
	}
	p, e := CleanPath(p)
	if e != nil {
		return nil, "", e
	}
	if e = r.guard(); e != nil {
		return nil, "", e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return nil, "", e
	}
	if scope == DefaultACL {
		st, e := f.Stat()
		if e != nil || !st.IsDir() {
			f.Close()
			if e == nil {
				e = fmt.Errorf("%w: default ACLs require a directory", ErrInvalidACL)
			}
			return nil, "", e
		}
	}
	return f, p, nil
}

func (r *Root) GetACL(p string, scope ACLScope) (ACL, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, _, e := r.aclFile(p, scope)
	if e != nil {
		return ACL{}, e
	}
	defer f.Close()
	return r.Files.GetACL(f, scope)
}

func (r *Root) SetACL(p string, scope ACLScope, acl ACL) (ACL, error) {
	acl, e := NormalizeACL(acl)
	if e != nil {
		return ACL{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, p, e := r.aclFile(p, scope)
	if e != nil {
		return ACL{}, e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return ACL{}, e
	}
	if e = r.Files.SetACL(f, scope, acl); e != nil {
		return ACL{}, e
	}
	if scope == AccessACL && before.IsDir() {
		// Setting an access ACL can clear setgid. Preserve directory special
		// bits without restoring the previous permission bits or ACL mask.
		special := before.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		if special != 0 {
			after, e := f.Stat()
			if e != nil {
				return ACL{}, e
			}
			if e = chmod(f, after.Mode().Perm()|special); e != nil {
				return ACL{}, e
			}
		}
	}
	if e = f.Sync(); e != nil {
		return ACL{}, e
	}
	if scope == AccessACL {
		n, e := r.node(p, true)
		if e != nil {
			return ACL{}, e
		}
		if e = r.indexNode(n); e != nil {
			return ACL{}, e
		}
	}
	return r.Files.GetACL(f, scope)
}

func (r *Root) ClearDefaultACL(p string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, _, e := r.aclFile(p, DefaultACL)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = r.Files.ClearDefaultACL(f); e != nil {
		return e
	}
	return f.Sync()
}
