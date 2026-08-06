package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/valentinkolb/filegate/domain"
)

// MountHealth is the per-mount probe result. Operators see a
// summary line per mount on startup and the system API exposes the
// full struct.
type MountHealth struct {
	Path             string
	Exists           bool
	Writable         bool
	XAttrSupported   bool
	ReflinkSupported bool
	FreeBytes        uint64 // 0 when unknown (statfs failed)
	TotalBytes       uint64 // 0 when unknown
	// Errors collected during the probe. An empty slice means the
	// mount is healthy. A non-empty slice with FailFast=true at
	// the call site causes startup to abort.
	Errors []string
}

// CheckMountHealth runs the full probe sequence on one mount path:
//
//  1. Stat — does the mount exist and is it a directory?
//  2. Touch — can we create a tiny test file in a sibling
//     `.fg-healthcheck` dir? Catches read-only mounts and missing
//     write permission.
//  3. xattr — can we set + read back a 16-byte user.filegate.id?
//     The whole filegate identity model depends on xattr support;
//     a mount without it can't host filegate data correctly.
//  4. Reflink — can FICLONE create a cheap copy on this mount?
//  5. Free space — disk usage via statfs. Surfaced for the log
//     and used by callers that want to early-warn at low free.
//
// The test artifacts are removed at the end of each probe — even
// on partial failure — so we don't leave debris behind.
//
// CheckMountHealth never panics. Every error is captured in the
// returned MountHealth.Errors slice and the caller decides the
// policy (warn vs hard-fail).
func CheckMountHealth(path string) MountHealth {
	h := MountHealth{Path: path}

	info, err := os.Stat(path)
	if err != nil {
		h.Errors = append(h.Errors, fmt.Sprintf("stat: %s", err))
		return h
	}
	h.Exists = true
	if !info.IsDir() {
		h.Errors = append(h.Errors, "mount path is not a directory")
		return h
	}

	// Probe writability + xattr in a unique-named temp dir so we
	// never touch user data. CRITICAL: do NOT use a fixed name
	// like ".fg-healthcheck" — if the user happens to have a
	// directory by that name (it's not reserved by filegate), the
	// deferred RemoveAll would wipe their data on every restart.
	// MkdirTemp gives us a unique path the probe creates and owns.
	probeDir, err := os.MkdirTemp(path, ".fg-healthcheck-*")
	if err != nil {
		h.Errors = append(h.Errors, fmt.Sprintf("mkdir probe dir: %s", err))
		// can't continue without the probe dir — but try to
		// surface free space anyway.
		fillFreeSpace(&h, path)
		return h
	}
	defer func() {
		// Best-effort cleanup. We created probeDir and only the
		// probe writes inside it, so RemoveAll on this exact path
		// only touches our artifacts.
		_ = os.RemoveAll(probeDir)
	}()

	// Writability + xattr via a single throwaway file.
	probeFile := filepath.Join(probeDir, "probe")
	f, err := os.OpenFile(probeFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		h.Errors = append(h.Errors, fmt.Sprintf("write probe: %s", err))
		fillFreeSpace(&h, path)
		return h
	}
	if _, werr := f.Write([]byte("x")); werr != nil {
		_ = f.Close()
		h.Errors = append(h.Errors, fmt.Sprintf("write probe data: %s", werr))
		fillFreeSpace(&h, path)
		return h
	}
	if cerr := f.Close(); cerr != nil {
		h.Errors = append(h.Errors, fmt.Sprintf("close probe: %s", cerr))
	}
	h.Writable = true

	// xattr round-trip with a known-shape FileID.
	var probeID domain.FileID
	for i := range probeID {
		probeID[i] = byte(i)
	}
	if err := setID(probeFile, probeID); err != nil {
		h.Errors = append(h.Errors, fmt.Sprintf("xattr setxattr (mount likely missing user_xattr support): %s", err))
		fillFreeSpace(&h, path)
		return h
	}
	got, err := getID(probeFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			h.Errors = append(h.Errors, "xattr getxattr returned not-exist after setxattr — mount likely strips xattrs")
		} else {
			h.Errors = append(h.Errors, fmt.Sprintf("xattr getxattr: %s", err))
		}
		fillFreeSpace(&h, path)
		return h
	}
	if got != probeID {
		h.Errors = append(h.Errors, fmt.Sprintf("xattr round-trip mismatch: set=%x got=%x", probeID, got))
		fillFreeSpace(&h, path)
		return h
	}
	h.XAttrSupported = true

	// CloneFile performs the real FICLONE call and falls back to a byte copy
	// when the filesystem does not support it. The boolean therefore reports
	// the effective behavior versioning and same-mount S3 copies will get,
	// without relying on a filesystem-name check.
	clonePath := filepath.Join(probeDir, "reflink-probe")
	if reflinked, cloneErr := CloneFile(probeFile, clonePath); cloneErr == nil {
		h.ReflinkSupported = reflinked
	}

	fillFreeSpace(&h, path)
	return h
}

// CheckMountsHealth probes every path. Errors aggregate; the
// returned slice is index-aligned with `paths`. The caller
// decides whether any error counts as fatal.
func CheckMountsHealth(paths []string) []MountHealth {
	out := make([]MountHealth, len(paths))
	for i, p := range paths {
		out[i] = CheckMountHealth(p)
	}
	return out
}
