//go:build linux

// Package filesystem implements descriptor-relative Linux filesystem access.
package filesystem

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v5/domain"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

type Files struct {
	lock       *os.File
	root       *os.File
	private    *os.File
	privateDev uint64
	privateIno uint64
}

func Open(p string) (*Files, error) {
	fd, e := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return &Files{root: os.NewFile(uintptr(fd), p)}, nil
}
func (f *Files) Close() error {
	if f.lock != nil {
		f.lock.Close()
	}
	if f.private != nil {
		f.private.Close()
	}
	return f.root.Close()
}
func (f *Files) parent(p string) (*os.File, string, error) {
	if p == "" || strings.HasPrefix(p, "/") {
		return nil, "", os.ErrInvalid
	}
	parts := strings.Split(p, "/")
	base := f.root
	if f.private != nil && strings.HasPrefix(p, ".filegate/") {
		base = f.private
		parts = parts[1:]
	}
	fd, e := unix.Dup(int(base.Fd()))
	if e != nil {
		return nil, "", e
	}
	for _, c := range parts[:len(parts)-1] {
		if c == ".." || c == "" {
			unix.Close(fd)
			return nil, "", os.ErrInvalid
		}
		if c == "." {
			continue
		}
		next, e := unix.Openat(fd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, "", e
		}
		if f.private != nil && !strings.HasPrefix(p, ".filegate/") && p != ".filegate" {
			var st unix.Stat_t
			if e := unix.Fstat(next, &st); e != nil {
				unix.Close(next)
				return nil, "", e
			}
			if uint64(st.Dev) == f.privateDev && st.Ino == f.privateIno {
				unix.Close(next)
				return nil, "", os.ErrPermission
			}
		}
		fd = next
	}
	last := parts[len(parts)-1]
	if last == ".." || last == "" {
		unix.Close(fd)
		return nil, "", os.ErrInvalid
	}
	return os.NewFile(uintptr(fd), path.Dir(p)), last, nil
}
func (f *Files) Open(p string, flag int, mode os.FileMode) (*os.File, error) {
	dir, name, e := f.parent(p)
	if e != nil {
		return nil, e
	}
	defer dir.Close()
	fd, e := unix.Openat(int(dir.Fd()), name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, domain.UnixMode(mode))
	if e != nil {
		return nil, e
	}
	o := os.NewFile(uintptr(fd), p)
	st, e := o.Stat()
	if e != nil || (!st.Mode().IsRegular() && !st.IsDir()) {
		o.Close()
		if e == nil {
			e = os.ErrInvalid
		}
		return nil, e
	}
	if f.private != nil && !strings.HasPrefix(p, ".filegate/") && p != ".filegate" {
		dev, ino, _, _, _ := f.Identity(st)
		if dev == f.privateDev && ino == f.privateIno {
			o.Close()
			return nil, os.ErrPermission
		}
	}
	return o, nil
}
func (f *Files) Stat(p string) (os.FileInfo, error) {
	o, e := f.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	defer o.Close()
	return o.Stat()
}
func (f *Files) Mkdir(p string, mode os.FileMode) error {
	d, n, e := f.parent(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return unix.Mkdirat(int(d.Fd()), n, domain.UnixMode(mode))
}
func (f *Files) Rename(a, b string, replace bool) error {
	ad, an, e := f.parent(a)
	if e != nil {
		return e
	}
	defer ad.Close()
	bd, bn, e := f.parent(b)
	if e != nil {
		return e
	}
	defer bd.Close()
	flags := uint(0)
	if !replace {
		flags = unix.RENAME_NOREPLACE
	}
	if e = unix.Renameat2(int(ad.Fd()), an, int(bd.Fd()), bn, flags); e != nil {
		return e
	}
	if e = bd.Sync(); e != nil {
		return e
	}
	return ad.Sync()
}
func (f *Files) Remove(p string, recursive bool) error {
	d, n, e := f.parent(p)
	if e != nil {
		return e
	}
	defer d.Close()
	var st unix.Stat_t
	if e = unix.Fstatat(int(d.Fd()), n, &st, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return e
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if recursive {
			fd, e := unix.Openat(int(d.Fd()), n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if e != nil {
				return e
			}
			child := &Files{root: os.NewFile(uintptr(fd), p)}
			names, e := child.root.Readdirnames(-1)
			if e == nil {
				for _, name := range names {
					if e = child.Remove(name, true); e != nil {
						break
					}
				}
			}
			child.Close()
			if e != nil {
				return e
			}
		}
		e = unix.Unlinkat(int(d.Fd()), n, unix.AT_REMOVEDIR)
	} else {
		e = unix.Unlinkat(int(d.Fd()), n, 0)
	}
	if e != nil {
		return e
	}
	return d.Sync()
}
func (f *Files) Sync(p string) error {
	o, e := f.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	defer o.Close()
	return o.Sync()
}
func (f *Files) ID(o *os.File) (string, error) {
	b := make([]byte, 16)
	n, e := unix.Fgetxattr(int(o.Fd()), "user.filegate.id", b)
	if errors.Is(e, unix.ENODATA) || errors.Is(e, unix.ERANGE) || n != 16 && e == nil {
		return "", os.ErrNotExist
	}
	if e != nil {
		return "", e
	}
	u, e := uuid.FromBytes(b)
	if e != nil || u == uuid.Nil {
		return "", os.ErrNotExist
	}
	return u.String(), nil
}
func (f *Files) SetID(o *os.File, id string) error {
	u, e := uuid.Parse(id)
	if e != nil {
		return e
	}
	return unix.Fsetxattr(int(o.Fd()), "user.filegate.id", u[:], 0)
}
func (f *Files) Identity(st os.FileInfo) (uint64, uint64, uint32, uint32, uint64) {
	s := st.Sys().(*syscall.Stat_t)
	return uint64(s.Dev), s.Ino, s.Uid, s.Gid, uint64(s.Nlink)
}
func (f *Files) Clone(src, dst *os.File) (bool, error) {
	e := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	if e == nil {
		return true, nil
	}
	if !errors.Is(e, unix.EOPNOTSUPP) && !errors.Is(e, unix.EXDEV) && !errors.Is(e, unix.EINVAL) && !errors.Is(e, unix.ENOTTY) {
		return false, e
	}
	_, e = io.Copy(dst, src)
	return false, e
}
func (f *Files) Capacity() (string, uint64, uint64, error) {
	var s unix.Statfs_t
	e := unix.Fstatfs(int(f.root.Fd()), &s)
	if e != nil {
		return "", 0, 0, e
	}
	st, e := f.root.Stat()
	if e != nil {
		return "", 0, 0, e
	}
	dev, _, _, _, _ := f.Identity(st)
	return fmt.Sprint(dev), s.Blocks * uint64(s.Bsize), s.Bavail * uint64(s.Bsize), nil
}

func (f *Files) SecurePrivate() error {
	file, e := f.Open(".filegate", os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	st, e := file.Stat()
	if e != nil {
		file.Close()
		return e
	}
	_, _, uid, _, _ := f.Identity(st)
	if !st.IsDir() || uid != uint32(os.Geteuid()) || st.Mode().Perm() != 0700 {
		file.Close()
		return fmt.Errorf(".filegate must be private and daemon-owned")
	}
	f.private = file
	f.privateDev, f.privateIno, _, _, _ = f.Identity(st)
	// NFS exclusive flock requires a writable regular file, not a directory fd.
	lock, e := f.Open(".filegate/LOCK", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		lock.Close()
		return fmt.Errorf("root already in use: %w", e)
	}
	f.lock = lock
	return nil
}
