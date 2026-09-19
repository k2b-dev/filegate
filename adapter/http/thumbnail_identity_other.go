//go:build !linux

package httpadapter

import "os"

// Only Linux runs the daemon. Portable SDK/unit builds do not require ctime.
func thumbnailChangeTime(st os.FileInfo) int64 { return st.ModTime().UnixNano() }
