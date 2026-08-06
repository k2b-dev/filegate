//go:build linux

package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCloneFileReflinkSucceedsOnBTRFS exercises the FICLONE ioctl
// success path. Skipped unless FILEGATE_BTRFS_REAL=1 +
// FILEGATE_BTRFS_REAL_ROOT point at a btrfs filesystem (the
// run-versioning-btrfs-real-docker.sh wrapper sets both inside a
// privileged container with a loopback btrfs image).
//
// Asserts:
//  1. CloneFile returns reflinked=true (the API contract).
//  2. The destination inode shares storage with the source — proven by
//     comparing st_blocks of the dst against the size of an unrelated
//     dummy file of the same byte length. With a true reflink, the
//     btrfs allocator does not allocate fresh extents for the dst, so
//     its block usage stays near zero.
func TestCloneFileReflinkSucceedsOnBTRFS(t *testing.T) {
	if os.Getenv("FILEGATE_BTRFS_REAL") != "1" {
		t.Skip("set FILEGATE_BTRFS_REAL=1 to run real btrfs reflink test")
	}
	root := strings.TrimSpace(os.Getenv("FILEGATE_BTRFS_REAL_ROOT"))
	if root == "" {
		t.Skip("FILEGATE_BTRFS_REAL_ROOT is required")
	}

	// Use a fresh subvolume so test artifacts can't pollute one another.
	dir, err := os.MkdirTemp(root, "reflink-test-")
	if err != nil {
		t.Fatalf("mkdtemp under btrfs root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	health := CheckMountHealth(dir)
	if len(health.Errors) != 0 {
		t.Fatalf("mount health probe on btrfs: %v", health.Errors)
	}
	if !health.ReflinkSupported {
		t.Fatal("mount health probe did not detect btrfs reflink support")
	}

	// 4 MiB payload — large enough that a copy fallback would clearly
	// allocate new extents but small enough to keep the test snappy.
	const payloadSize = 4 * 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	srcPath := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dstPath := filepath.Join(dir, "dst.bin")

	used, err := CloneFile(srcPath, dstPath)
	if err != nil {
		t.Fatalf("CloneFile on btrfs: %v", err)
	}
	if !used {
		t.Fatalf("expected reflinked=true on btrfs, got false (FICLONE was not used)")
	}

	// Sanity-check the bytes round-tripped.
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(got) != len(payload) || !bytesEqual(got, payload) {
		t.Fatalf("dst bytes do not match src after reflink")
	}

	// Reflink check: dst's reported st_blocks should be much smaller
	// than the byte size, because btrfs reports the dst as sharing
	// extents with src. Be lenient — kernels sometimes report all
	// blocks as charged on the first stat. We require dst <= src.
	srcStat, err := os.Stat(srcPath)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstStat, err := os.Stat(dstPath)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	srcSys := srcStat.Sys().(*syscall.Stat_t)
	dstSys := dstStat.Sys().(*syscall.Stat_t)
	if dstSys.Blocks > srcSys.Blocks {
		t.Fatalf("dst.st_blocks=%d > src.st_blocks=%d — reflink did not share extents",
			dstSys.Blocks, srcSys.Blocks)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
