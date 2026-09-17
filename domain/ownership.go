package domain

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
)

func validateOwnership(o *Ownership) error {
	if o == nil {
		return nil
	}
	if (o.UID == nil) != (o.GID == nil) {
		return ErrInvalid
	}
	if o.UID != nil && (*o.UID < 0 || *o.GID < 0 || uint64(*o.UID) >= 1<<32-1 || uint64(*o.GID) >= 1<<32-1) {
		return ErrInvalid
	}
	for _, v := range []struct {
		mode    string
		allowed uint64
	}{{o.Mode, 0777}, {o.DirMode, 02777}} {
		if v.mode != "" {
			n, e := strconv.ParseUint(v.mode, 8, 32)
			if e != nil || n & ^v.allowed != 0 {
				return ErrInvalid
			}
		}
	}
	return nil
}

func chmod(f *os.File, mode os.FileMode) error {
	if e := f.Chmod(mode); e != nil {
		return e
	}
	st, e := f.Stat()
	if e != nil {
		return e
	}
	// Linux can silently clear setgid when the caller lacks group membership.
	if UnixMode(st.Mode()) != UnixMode(mode) {
		return fmt.Errorf("requested mode was not applied; check group membership and process privileges: %w", os.ErrPermission)
	}
	return nil
}

func (r *Root) chown(f *os.File, uid, gid uint32) error {
	st, e := f.Stat()
	if e != nil {
		return e
	}
	_, _, oldUID, oldGID, _ := r.Files.Identity(st)
	if oldUID == uid && oldGID == gid {
		return nil
	}
	if e = f.Chown(int(uid), int(gid)); e != nil {
		return e
	}
	st, e = f.Stat()
	if e != nil {
		return e
	}
	_, _, actualUID, actualGID, _ := r.Files.Identity(st)
	if actualUID != uid || actualGID != gid {
		return fmt.Errorf("requested ownership was not applied; check process privileges and NFS export policy: %w", os.ErrPermission)
	}
	return nil
}

func (r *Root) applyOwner(f *os.File, o *Ownership) error {
	if o == nil {
		return nil
	}
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if o.UID != nil {
		if e = r.chown(f, uint32(*o.UID), uint32(*o.GID)); e != nil {
			return e
		}
	}
	m := o.Mode
	if st.IsDir() {
		m = o.DirMode
	}
	if m != "" {
		n, _ := strconv.ParseUint(m, 8, 32)
		return chmod(f, FileMode(uint32(n)))
	}
	if st.IsDir() && o.UID != nil {
		// Chown can clear special bits. An ownership-only update retains the
		// directory's mode and its ACL mask, including an inherited setgid bit.
		return chmod(f, st.Mode())
	}
	return nil
}

func (r *Root) parentPermissions(p string) (os.FileInfo, ACL, error) {
	f, e := r.Files.Open(path.Dir(p), os.O_RDONLY, 0)
	if e != nil {
		return nil, ACL{}, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, ACL{}, e
	}
	a, e := r.Files.GetACL(f, DefaultACL)
	return st, a, e
}

// makeDirectory is shared by implicit parents and directory copies.
// Kernel creation inherits the parent's default ACL and setgid group.
func (r *Root) makeDirectory(p string, o *Ownership) error {
	if e := r.guard(); e != nil {
		return e
	}
	if _, e := r.Files.Stat(p); e == nil {
		return os.ErrExist
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	_, acl, e := r.parentPermissions(p)
	if e != nil && !errors.Is(e, ErrACLUnsupported) {
		return e
	}
	mode := os.FileMode(0755)
	if len(acl.Entries) != 0 {
		mode = 0777
	}
	if o != nil && o.DirMode != "" {
		n, _ := strconv.ParseUint(o.DirMode, 8, 32)
		// Apply the creation ceiling immediately, especially for private 0700
		// provisioning. Ownership and final special bits follow on the inode.
		mode = FileMode(uint32(n))
	}
	if e = r.Files.Mkdir(p, mode); e != nil {
		return e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	if len(acl.Entries) == 0 && (o == nil || o.DirMode == "") {
		st, e := f.Stat()
		if e != nil {
			return e
		}
		// Filegate's default directory mode is explicit, like its default file
		// mode, independent of the daemon umask. Retain kernel-inherited setgid.
		if e = chmod(f, 0755|st.Mode()&os.ModeSetgid); e != nil {
			return e
		}
	}
	if e = r.applyOwner(f, o); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	return r.Files.Sync(path.Dir(p))
}

// preparePublication replaces staging metadata with destination metadata before
// rename. Renaming an inode does not trigger directory ACL or group inheritance.
func (r *Root) preparePublication(p string, f *os.File, exists bool, o *Ownership) error {
	staged, e := f.Stat()
	if e != nil {
		return e
	}
	// Keep the actual creating identity unless destination inheritance or an
	// explicit override applies. Remote filesystems can map process identities.
	_, _, uid, gid, _ := r.Files.Identity(staged)
	mode := os.FileMode(0644)
	acl := ACLFromMode(mode)
	aclUnsupported := false
	if exists {
		old, e := r.Files.Open(p, os.O_RDONLY, 0)
		if e != nil {
			return e
		}
		defer old.Close()
		st, e := old.Stat()
		if e != nil {
			return e
		}
		_, _, uid, gid, _ = r.Files.Identity(st)
		// Replacing contents retains execute permissions, but must not restore
		// setuid/setgid privilege bits that an ordinary Unix write would clear.
		mode = st.Mode() &^ (os.ModeSetuid | os.ModeSetgid)
		acl, e = r.Files.GetACL(old, AccessACL)
		if errors.Is(e, ErrACLUnsupported) {
			aclUnsupported = true
			acl = ACLFromMode(mode)
		} else if e != nil {
			return e
		}
	} else {
		parent, inherited, e := r.parentPermissions(p)
		if errors.Is(e, ErrACLUnsupported) {
			aclUnsupported = true
		} else if e != nil {
			return e
		}
		if parent.Mode()&os.ModeSetgid != 0 {
			_, _, _, gid, _ = r.Files.Identity(parent)
		}
		if len(inherited.Entries) != 0 {
			acl = inheritACL(inherited, 0666)
			mode = aclMode(acl)
		}
	}
	if o != nil && o.UID != nil {
		uid, gid = uint32(*o.UID), uint32(*o.GID)
	}
	if e := r.chown(f, uid, gid); e != nil {
		return e
	}
	// Install even a minimal ACL: otherwise named entries inherited from the
	// private staging directory could become effective after chmod.
	if e := r.Files.SetACL(f, AccessACL, acl); e != nil {
		// Permit mode-only filesystems, but never silently discard an ACL
		// obtained from an ACL-capable destination.
		if !aclUnsupported || !errors.Is(e, ErrACLUnsupported) {
			return e
		}
	}
	if o != nil && o.Mode != "" {
		n, _ := strconv.ParseUint(o.Mode, 8, 32)
		mode = FileMode(uint32(n))
	}
	return chmod(f, mode)
}

func inheritACL(a ACL, mode os.FileMode) ACL {
	out := ACL{Entries: append([]ACLEntry(nil), a.Entries...)}
	hasMask := false
	for _, entry := range out.Entries {
		hasMask = hasMask || entry.Tag == ACLMask
	}
	for i := range out.Entries {
		entry := &out.Entries[i]
		bits := mode.Perm()
		switch entry.Tag {
		case ACLOwner:
			bits >>= 6
		case ACLMask:
			bits >>= 3
		case ACLOwningGroup:
			if hasMask {
				continue
			}
			bits >>= 3
		case ACLOther:
		default:
			continue
		}
		entry.Permissions = permissions(aclBits(entry.Permissions) & uint32(bits&7))
	}
	return out
}

func aclBits(p ACLPermissions) uint32 {
	var bits uint32
	for i, c := range p {
		if c != '-' {
			bits |= 1 << (2 - i)
		}
	}
	return bits
}

func permissions(bits uint32) ACLPermissions {
	p := []byte("---")
	for i, c := range []byte("rwx") {
		if bits&(1<<(2-i)) != 0 {
			p[i] = c
		}
	}
	return ACLPermissions(p)
}

func aclMode(a ACL) os.FileMode {
	var owner, group, mask, other uint32
	hasMask := false
	for _, entry := range a.Entries {
		switch entry.Tag {
		case ACLOwner:
			owner = aclBits(entry.Permissions)
		case ACLOwningGroup:
			group = aclBits(entry.Permissions)
		case ACLMask:
			mask, hasMask = aclBits(entry.Permissions), true
		case ACLOther:
			other = aclBits(entry.Permissions)
		}
	}
	if hasMask {
		group = mask
	}
	return os.FileMode(owner<<6 | group<<3 | other)
}

func (r *Root) SetOwnership(p string, o *Ownership) (Node, error) {
	p, e := CleanPath(p)
	if e != nil {
		return Node{}, e
	}
	if e = validateOwnership(o); e != nil {
		return Node{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e = r.guard(); e != nil {
		return Node{}, e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, e
	}
	defer f.Close()
	if e = r.applyOwner(f, o); e != nil {
		return Node{}, e
	}
	if e = f.Sync(); e != nil {
		return Node{}, e
	}
	n, e := r.node(p, true)
	if e == nil {
		e = r.indexNode(n)
	}
	return n, e
}
