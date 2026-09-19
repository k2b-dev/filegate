//go:build linux

package httpadapter

import (
	"os"
	"syscall"
)

func thumbnailChangeTime(st os.FileInfo) int64 {
	if info, ok := st.Sys().(*syscall.Stat_t); ok {
		return info.Ctim.Sec*1e9 + info.Ctim.Nsec
	}
	return 0
}
