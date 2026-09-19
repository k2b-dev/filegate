//go:build linux

package filesystem

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/k2b-dev/filegate/v6/domain"
	"golang.org/x/sys/unix"
)

const aclXattrVersion = 2

func aclName(scope domain.ACLScope) (string, error) {
	switch scope {
	case domain.AccessACL:
		return "system.posix_acl_access", nil
	case domain.DefaultACL:
		return "system.posix_acl_default", nil
	default:
		return "", fmt.Errorf("%w: scope must be access or default", domain.ErrInvalidACL)
	}
}

func aclError(operation string, e error) error {
	if errors.Is(e, unix.ENOTSUP) || errors.Is(e, unix.ENOSYS) {
		return fmt.Errorf("%s: %w", operation, domain.ErrACLUnsupported)
	}
	if errors.Is(e, unix.EPERM) || errors.Is(e, unix.EACCES) {
		return fmt.Errorf("%s: permission denied; check file ownership, process privileges and NFS export restrictions: %w", operation, e)
	}
	return fmt.Errorf("%s: %w", operation, e)
}

func checkACLFile(file *os.File, scope domain.ACLScope) error {
	if scope != domain.DefaultACL {
		return nil
	}
	st, e := file.Stat()
	if e != nil {
		return e
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: default ACLs require a directory", domain.ErrInvalidACL)
	}
	return nil
}

func (f *Files) GetACL(file *os.File, scope domain.ACLScope) (domain.ACL, error) {
	name, e := aclName(scope)
	if e != nil {
		return domain.ACL{}, e
	}
	if e = checkACLFile(file, scope); e != nil {
		return domain.ACL{}, e
	}
	// A fixed bounded buffer avoids a size/read race and covers every accepted ACL.
	buf := make([]byte, 4+8*domain.ACLMaxEntries)
	n, e := unix.Fgetxattr(int(file.Fd()), name, buf)
	if errors.Is(e, unix.ENODATA) {
		if scope == domain.DefaultACL {
			return domain.ACL{Entries: []domain.ACLEntry{}}, nil
		}
		st, e := file.Stat()
		if e != nil {
			return domain.ACL{}, e
		}
		return domain.ACLFromMode(st.Mode()), nil
	}
	if errors.Is(e, unix.ERANGE) {
		return domain.ACL{}, fmt.Errorf("read ACL: %w: ACL exceeds %d entries", domain.ErrLimit, domain.ACLMaxEntries)
	}
	if e != nil {
		return domain.ACL{}, aclError("read ACL", e)
	}
	return decodeACL(buf[:n])
}

func (f *Files) SetACL(file *os.File, scope domain.ACLScope, acl domain.ACL) error {
	name, e := aclName(scope)
	if e != nil {
		return e
	}
	if e = checkACLFile(file, scope); e != nil {
		return e
	}
	buf, e := encodeACL(acl)
	if e != nil {
		return e
	}
	if e = unix.Fsetxattr(int(file.Fd()), name, buf, 0); e != nil {
		return aclError("set ACL", e)
	}
	return nil
}

func (f *Files) ClearDefaultACL(file *os.File) error {
	if e := checkACLFile(file, domain.DefaultACL); e != nil {
		return e
	}
	e := unix.Fremovexattr(int(file.Fd()), "system.posix_acl_default")
	if e == nil || errors.Is(e, unix.ENODATA) {
		return nil
	}
	return aclError("clear default ACL", e)
}

var aclTags = map[domain.ACLTag]uint16{
	domain.ACLOwner: 0x01, domain.ACLUser: 0x02,
	domain.ACLOwningGroup: 0x04, domain.ACLGroup: 0x08,
	domain.ACLMask: 0x10, domain.ACLOther: 0x20,
}

func encodeACL(acl domain.ACL) ([]byte, error) {
	acl, e := domain.NormalizeACL(acl)
	if e != nil {
		return nil, e
	}
	buf := make([]byte, 4+8*len(acl.Entries))
	binary.LittleEndian.PutUint32(buf, aclXattrVersion)
	for i, entry := range acl.Entries {
		b := buf[4+8*i:]
		binary.LittleEndian.PutUint16(b, aclTags[entry.Tag])
		var perm uint16
		for j, bit := range []uint16{4, 2, 1} {
			if entry.Permissions[j] != '-' {
				perm |= bit
			}
		}
		binary.LittleEndian.PutUint16(b[2:], perm)
		id := ^uint32(0)
		if entry.ID != nil {
			id = *entry.ID
		}
		binary.LittleEndian.PutUint32(b[4:], id)
	}
	return buf, nil
}

func decodeACL(buf []byte) (domain.ACL, error) {
	if len(buf) < 4 || (len(buf)-4)%8 != 0 || binary.LittleEndian.Uint32(buf) != aclXattrVersion {
		return domain.ACL{}, fmt.Errorf("%w: malformed Linux ACL data", domain.ErrInvalidACL)
	}
	acl := domain.ACL{Entries: make([]domain.ACLEntry, 0, (len(buf)-4)/8)}
	for offset := 4; offset < len(buf); offset += 8 {
		b := buf[offset:]
		rawTag, perm, id := binary.LittleEndian.Uint16(b), binary.LittleEndian.Uint16(b[2:]), binary.LittleEndian.Uint32(b[4:])
		var tag domain.ACLTag
		for t, n := range aclTags {
			if n == rawTag {
				tag = t
				break
			}
		}
		if tag == "" || perm > 7 {
			return domain.ACL{}, fmt.Errorf("%w: malformed Linux ACL entry", domain.ErrInvalidACL)
		}
		entry := domain.ACLEntry{Tag: tag}
		if tag == domain.ACLUser || tag == domain.ACLGroup {
			entry.ID = &id
		} else if id != ^uint32(0) {
			return domain.ACL{}, fmt.Errorf("%w: unexpected ID in Linux ACL entry", domain.ErrInvalidACL)
		}
		p := []byte("---")
		for j, bit := range []uint16{4, 2, 1} {
			if perm&bit != 0 {
				p[j] = "rwx"[j]
			}
		}
		entry.Permissions = domain.ACLPermissions(p)
		acl.Entries = append(acl.Entries, entry)
	}
	return domain.NormalizeACL(acl)
}
