package domain

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"syscall"
)

var ErrCrossDevice = errors.New("root staging and target must be on the same filesystem")

type DirectoryACLs struct {
	Access  *ACL `json:"access,omitempty"`
	Default *ACL `json:"default,omitempty"`
}

type DirectoryOptions struct {
	Ownership *Ownership     `json:"ownership,omitempty"`
	ACL       *DirectoryACLs `json:"acl,omitempty"`
}

// Mkdir publishes one fully configured directory. Its parent must already exist;
// existing destinations are never modified, including when their type differs.
func (r *Root) Mkdir(p string, o DirectoryOptions) (Node, error) {
	p, e := validWrite(p)
	if e != nil {
		return Node{}, e
	}
	if e = validateOwnership(o.Ownership); e != nil {
		return Node{}, e
	}
	if o.ACL != nil {
		acls := *o.ACL
		o.ACL = &acls
		for _, target := range []**ACL{&acls.Access, &acls.Default} {
			if *target != nil {
				a, e := NormalizeACL(**target)
				if e != nil {
					return Node{}, e
				}
				*target = &a
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e = r.guard(); e != nil {
		return Node{}, e
	}
	if _, e = r.Files.Stat(p); e == nil {
		return Node{}, ErrConflict
	} else if !errors.Is(e, os.ErrNotExist) {
		return Node{}, e
	}
	parent, inherited, e := r.parentPermissions(p)
	aclUnsupported := errors.Is(e, ErrACLUnsupported)
	if e != nil && !aclUnsupported {
		return Node{}, e
	}
	if parent == nil || !parent.IsDir() {
		return Node{}, fmt.Errorf("%w: parent must be an existing directory", ErrInvalid)
	}
	if aclUnsupported && o.ACL != nil && (o.ACL.Access != nil || o.ACL.Default != nil) {
		return Node{}, ErrACLUnsupported
	}
	temp := ".filegate/staging/" + newID()
	if e = r.Files.Mkdir(temp, 0700); e != nil {
		return Node{}, e
	}
	// The private ancestor protects the directory even after its final ownership
	// and mode are installed. Cleanup is deliberately nonrecursive.
	defer r.Files.Remove(temp, false)
	f, e := r.Files.Open(temp, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, e
	}
	defer f.Close()
	if e = r.prepareDirectory(f, parent, inherited, aclUnsupported, o); e != nil {
		return Node{}, e
	}
	id := ""
	if r.Config.Index {
		id = newID()
		if e = r.Files.SetID(f, id); e != nil {
			return Node{}, e
		}
	}
	if e = f.Sync(); e != nil {
		return Node{}, e
	}
	st, e := f.Stat()
	if e != nil {
		return Node{}, e
	}
	dev, ino, uid, gid, _ := r.Files.Identity(st)
	n := Node{Root: r.Config.Name, Path: p, ID: id, Directory: true,
		Modified: st.ModTime().UTC(), Mode: fmt.Sprintf("%04o", UnixMode(st.Mode())), UID: uid, GID: gid}
	rec := publication{Path: p, Temp: temp, Node: n, Claim: claim{Device: dev, Inode: ino, Path: p}}
	key := "pending/" + newID()
	if e = r.State.Put(key, rec); e != nil {
		return Node{}, e
	}
	r.needsRecovery = true
	if e = r.Files.Rename(temp, p, false); e != nil {
		if errors.Is(e, syscall.EXDEV) {
			return Node{}, ErrCrossDevice
		}
		return Node{}, e
	}
	if e = r.finishPublication(key, rec); e != nil {
		return Node{}, e
	}
	r.needsRecovery = false
	r.invalidateStats()
	return n, nil
}

func (r *Root) prepareDirectory(f *os.File, parent os.FileInfo, inherited ACL, aclUnsupported bool, o DirectoryOptions) error {
	st, e := f.Stat()
	if e != nil {
		return e
	}
	_, _, uid, gid, _ := r.Files.Identity(st)
	mode := os.FileMode(0755)
	access := ACLFromMode(mode)
	defaults := inherited
	if len(inherited.Entries) != 0 {
		access = inheritACL(inherited, 0777)
		mode = aclMode(access)
	}
	if o.ACL != nil {
		if o.ACL.Access != nil {
			access = *o.ACL.Access
			mode = aclMode(access)
		}
		if o.ACL.Default != nil {
			defaults = *o.ACL.Default
		}
	}
	if parent.Mode()&os.ModeSetgid != 0 {
		_, _, _, gid, _ = r.Files.Identity(parent)
		mode |= os.ModeSetgid
	}
	if o.Ownership != nil {
		if o.Ownership.UID != nil {
			uid, gid = uint32(*o.Ownership.UID), uint32(*o.Ownership.GID)
		}
		if o.Ownership.DirMode != "" {
			n, _ := strconv.ParseUint(o.Ownership.DirMode, 8, 32)
			mode = FileMode(uint32(n))
		}
	}
	if e = r.chown(f, uid, gid); e != nil {
		return e
	}
	allowUnsupported := func(e error) error {
		if aclUnsupported && errors.Is(e, ErrACLUnsupported) {
			return nil
		}
		return e
	}
	// Always replace staging ACLs, including a default ACL inherited from the
	// private staging directory. No staging policy may leak into the target.
	if e = allowUnsupported(r.Files.SetACL(f, AccessACL, access)); e != nil {
		return e
	}
	if len(defaults.Entries) != 0 {
		e = r.Files.SetACL(f, DefaultACL, defaults)
	} else {
		e = r.Files.ClearDefaultACL(f)
	}
	if e = allowUnsupported(e); e != nil {
		return e
	}
	if e = chmod(f, mode); e != nil {
		return e
	}
	if aclUnsupported {
		return nil
	}
	// chmod changes only the owner, mask (or owning group), and other entries.
	want := aclWithMode(access, mode)
	for _, check := range []struct {
		scope ACLScope
		want  ACL
	}{{AccessACL, want}, {DefaultACL, defaults}} {
		got, e := r.Files.GetACL(f, check.scope)
		if e != nil {
			return e
		}
		if len(got.Entries) == 0 && len(check.want.Entries) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, check.want) {
			return fmt.Errorf("requested %s ACL was not applied: %w", check.scope, os.ErrPermission)
		}
	}
	return nil
}

func aclWithMode(a ACL, mode os.FileMode) ACL {
	a.Entries = append([]ACLEntry(nil), a.Entries...)
	hasMask := false
	for _, entry := range a.Entries {
		hasMask = hasMask || entry.Tag == ACLMask
	}
	for i := range a.Entries {
		entry := &a.Entries[i]
		switch entry.Tag {
		case ACLOwner:
			entry.Permissions = permissions(uint32(mode.Perm()) >> 6 & 7)
		case ACLMask:
			entry.Permissions = permissions(uint32(mode.Perm()) >> 3 & 7)
		case ACLOwningGroup:
			if !hasMask {
				entry.Permissions = permissions(uint32(mode.Perm()) >> 3 & 7)
			}
		case ACLOther:
			entry.Permissions = permissions(uint32(mode.Perm()) & 7)
		}
	}
	return a
}
