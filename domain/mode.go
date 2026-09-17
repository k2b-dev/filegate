package domain

import "os"

// UnixMode translates Go's special mode flags to their Unix octal positions.
func UnixMode(mode os.FileMode) uint32 {
	n := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		n |= 04000
	}
	if mode&os.ModeSetgid != 0 {
		n |= 02000
	}
	if mode&os.ModeSticky != 0 {
		n |= 01000
	}
	return n
}

// FileMode translates Unix permission bits, excluding the file type, to Go.
func FileMode(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0777)
	if mode&04000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&02000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&01000 != 0 {
		m |= os.ModeSticky
	}
	return m
}
