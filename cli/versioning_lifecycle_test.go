package cli

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
)

func TestSelectVersioning(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		mounts   []filesystem.MountHealth
		enabled  bool
		copyMode string
	}{
		{name: "auto reflink", mode: "auto", mounts: []filesystem.MountHealth{{ReflinkSupported: true}}, enabled: true, copyMode: "reflink"},
		{name: "auto byte copy", mode: "auto", mounts: []filesystem.MountHealth{{}}, enabled: false, copyMode: "disabled"},
		{name: "forced byte copy", mode: "on", mounts: []filesystem.MountHealth{{}}, enabled: true, copyMode: "byte-copy"},
		{name: "forced mixed", mode: "on", mounts: []filesystem.MountHealth{{ReflinkSupported: true}, {}}, enabled: true, copyMode: "mixed"},
		{name: "off", mode: "off", mounts: []filesystem.MountHealth{{ReflinkSupported: true}}, enabled: false, copyMode: "disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectVersioning(domain.VersioningConfig{Enabled: tt.mode}, tt.mounts)
			if got.Enabled != tt.enabled || got.CopyMode != tt.copyMode || got.Reason == "" {
				t.Fatalf("selection = %+v, want enabled=%v copyMode=%q and a reason", got, tt.enabled, tt.copyMode)
			}
		})
	}
}

// TestWarnOrphanVersionDirsLogsDetachedBlobs pins the operator-safety
// signal that fires when a Pebble format-version bump triggers a full
// index rebuild on a btrfs mount that already has captured version
// blobs. Without the warning the operator only finds out via "where
// did my disk space go" weeks later. The blobs themselves are NOT
// auto-removed (could contain recoverable data) — the warn is the
// whole signal.
func TestWarnOrphanVersionDirsLogsDetachedBlobs(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".fg-versions", "file-id"), 0o700); err != nil {
		t.Fatalf("seed orphan dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, ".fg-versions", "file-id", "blob.bin"),
		[]byte("payload"), 0o600); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	// Capture the standard logger's output for the duration of the call.
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	defer func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	}()
	var buf bytes.Buffer
	log.SetOutput(&buf)

	warnOrphanVersionDirs([]string{base})

	got := buf.String()
	if !strings.Contains(got, "WARNING") {
		t.Fatalf("warn missing WARNING prefix: %q", got)
	}
	if !strings.Contains(got, filepath.Join(base, ".fg-versions")) {
		t.Fatalf("warn missing path %q: %q", filepath.Join(base, ".fg-versions"), got)
	}
	if !strings.Contains(got, "version blob") {
		t.Fatalf("warn doesn't mention version blobs: %q", got)
	}
}

// TestWarnOrphanVersionDirsSilentWithoutOrphans pins that the warning
// fires only when there's actually something detached. A noisy WARN on
// every clean restart would train operators to ignore it.
func TestWarnOrphanVersionDirsSilentWithoutOrphans(t *testing.T) {
	base := t.TempDir()

	prevOutput := log.Writer()
	defer log.SetOutput(prevOutput)
	var buf bytes.Buffer
	log.SetOutput(&buf)

	warnOrphanVersionDirs([]string{base})

	if buf.Len() != 0 {
		t.Fatalf("warn fired with no orphans: %q", buf.String())
	}
}
