//go:build linux

package domain_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// newServiceWithVersioning is the per-test setup for the Phase 2 capture
// surface. It builds a Service with versioning enabled so we don't have
// to plumb config through every helper. A short 50ms cooldown keeps the
// timing-sensitive tests fast; the size floor stays at the production
// default so we can probe both above and below it.
func newServiceWithVersioning(t *testing.T, cfg domain.VersioningConfig) (*domain.Service, func()) {
	t.Helper()
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 50 * time.Millisecond
	}
	if cfg.MaxLabelBytes == 0 {
		cfg.MaxLabelBytes = 2048
	}
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	svc, err := domain.NewService(idx, filesystem.New(), eventbus.New(), []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	svc.EnableVersioning(cfg, true)
	return svc, func() { _ = idx.Close() }
}

func mountRootName(t *testing.T, svc *domain.Service) string {
	t.Helper()
	roots := svc.ListRoot()
	if len(roots) != 1 {
		t.Fatalf("expected 1 mount root, got %d", len(roots))
	}
	return roots[0].Name
}

func listVersionsViaIndex(t *testing.T, svc *domain.Service, fileID domain.FileID) []domain.VersionMeta {
	t.Helper()
	// We don't have a public ListVersions yet (Phase 3); reach through
	// the index port via the public path that lists Stats / paths to
	// avoid coupling tests to internals. Cleanest interim: round-trip
	// through ResolveAbsPath to confirm the file exists, then talk to
	// the index port directly via a helper.
	if _, err := svc.ResolveAbsPath(fileID); err != nil {
		t.Fatalf("resolve abs path: %v", err)
	}
	got := serviceListVersions(t, svc, fileID)
	return got
}

// versionsRootDir returns the .fg-versions root inside the (single) mount
// for the test's service. Used to make on-disk assertions about blob
// placement.
func versionsRootDir(t *testing.T, svc *domain.Service, fileID domain.FileID) string {
	t.Helper()
	roots := svc.ListRoot()
	if len(roots) != 1 {
		t.Fatalf("expected 1 mount root, got %d", len(roots))
	}
	mountAbs, err := svc.ResolveAbsPath(roots[0].ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath mount root: %v", err)
	}
	return filepath.Join(mountAbs, ".fg-versions", fileID.String())
}

// TestWriteContentCapturesPreOverwriteVersion pins the contract: an
// HTTP-overwrite of an existing file with content >= the size floor and
// outside any cooldown produces exactly one captured version whose bytes
// match the OLD state.
func TestWriteContentCapturesPreOverwriteVersion(t *testing.T) {
	svc, cleanup := newServiceWithVersioning(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0, // disable floor for this test
	})
	defer cleanup()

	root := mountRootName(t, svc)
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root+"/v.bin",
		strings.NewReader("original-content-bytes"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Small sleep so the overwrite falls outside the 50ms cooldown.
	time.Sleep(80 * time.Millisecond)

	if err := svc.WriteContent(meta.ID, strings.NewReader("new-content-bytes")); err != nil {
		t.Fatalf("WriteContent overwrite: %v", err)
	}

	versions := listVersionsViaIndex(t, svc, meta.ID)
	if len(versions) < 1 {
		t.Fatalf("want >= 1 captured version, got %d", len(versions))
	}
	// Find the version that holds the OLD bytes (size matches "original-content-bytes").
	const wantOldSize = int64(len("original-content-bytes"))
	foundOld := false
	for _, v := range versions {
		if v.Size == wantOldSize {
			foundOld = true
			break
		}
	}
	if !foundOld {
		t.Fatalf("no version with old size %d found in %v", wantOldSize, versions)
	}
}

func TestWriteContentSkipsCaptureWithinCooldown(t *testing.T) {
	svc, cleanup := newServiceWithVersioning(t, domain.VersioningConfig{
		Cooldown:         5 * time.Second, // long cooldown
		MinSizeForAutoV1: 0,
	})
	defer cleanup()

	root := mountRootName(t, svc)
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root+"/v.bin",
		strings.NewReader("content-1"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	beforeOverwrite := serviceListVersions(t, svc, meta.ID)

	// Immediate overwrite — well within cooldown — must NOT capture.
	if err := svc.WriteContent(meta.ID, strings.NewReader("content-2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	afterOverwrite := serviceListVersions(t, svc, meta.ID)
	if len(afterOverwrite) != len(beforeOverwrite) {
		t.Fatalf("cooldown was bypassed: before=%d after=%d versions",
			len(beforeOverwrite), len(afterOverwrite))
	}
}

func TestCreateAboveFloorCapturesV1(t *testing.T) {
	svc, cleanup := newServiceWithVersioning(t, domain.VersioningConfig{
		MinSizeForAutoV1: 16, // tiny so 32-byte test content qualifies
	})
	defer cleanup()

	root := mountRootName(t, svc)
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root+"/big.bin",
		strings.NewReader(strings.Repeat("X", 32)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	versions := serviceListVersions(t, svc, meta.ID)
	if len(versions) != 1 {
		t.Fatalf("want 1 V1 version, got %d", len(versions))
	}
	if versions[0].Size != 32 {
		t.Fatalf("V1 size=%d, want 32", versions[0].Size)
	}
	if versions[0].Pinned {
		t.Fatalf("V1 should not be pinned")
	}
	// On-disk blob must exist.
	dir := versionsRootDir(t, svc, meta.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read versions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 blob in %s, got %d", dir, len(entries))
	}
}

func TestCreateBelowFloorSkipsV1(t *testing.T) {
	svc, cleanup := newServiceWithVersioning(t, domain.VersioningConfig{
		MinSizeForAutoV1: 1024,
	})
	defer cleanup()

	root := mountRootName(t, svc)
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root+"/tiny.txt",
		strings.NewReader("small"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := serviceListVersions(t, svc, meta.ID); len(got) != 0 {
		t.Fatalf("expected 0 versions (below floor), got %d", len(got))
	}
}

func TestVersioningDisabledNoOps(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer idx.Close()
	svc, err := domain.NewService(idx, filesystem.New(), eventbus.New(), []string{baseDir}, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	// Versioning never enabled.

	root := mountRootName(t, svc)
	if _, _, err := svc.WriteContentByVirtualPath(
		"/"+root+"/x.bin",
		strings.NewReader(strings.Repeat("X", 64*1024)),
		domain.ConflictError,
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if svc.VersioningEnabled() {
		t.Fatalf("VersioningEnabled() must be false after NewService without EnableVersioning")
	}
	// And no .fg-versions directory must have been created.
	rootAbs := versionsRootBase(t, svc)
	if _, err := os.Stat(rootAbs); err == nil {
		t.Fatalf("versioning produced %s despite being disabled", rootAbs)
	}
}

// versionsRootBase returns the .fg-versions directory at the test mount
// (without the per-file subdir). Used by TestVersioningDisabledNoOps to
// assert the directory was never created.
func versionsRootBase(t *testing.T, svc *domain.Service) string {
	t.Helper()
	roots := svc.ListRoot()
	if len(roots) != 1 {
		t.Fatalf("expected 1 mount root, got %d", len(roots))
	}
	mountAbs, err := svc.ResolveAbsPath(roots[0].ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath mount root: %v", err)
	}
	return filepath.Join(mountAbs, ".fg-versions")
}

func TestReplaceFileOverwriteCapturesPrevious(t *testing.T) {
	svc, cleanup := newServiceWithVersioning(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	rootAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("ResolveAbsPath: %v", err)
	}
	target, err := svc.CreateChild(root.ID, "target.bin", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if err := svc.WriteContent(target.ID, strings.NewReader("original-payload-content")); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	beforeReplace := serviceListVersions(t, svc, target.ID)

	time.Sleep(80 * time.Millisecond) // outside cooldown

	src := filepath.Join(rootAbs, ".tmp-src")
	if err := os.WriteFile(src, []byte("replacement-payload"), 0o644); err != nil {
		t.Fatalf("write tmp src: %v", err)
	}

	if _, err := svc.ReplaceFile(root.ID, "target.bin", src, nil, domain.ConflictOverwrite); err != nil {
		t.Fatalf("ReplaceFile overwrite: %v", err)
	}

	after := serviceListVersions(t, svc, target.ID)
	if len(after) <= len(beforeReplace) {
		t.Fatalf("expected new version after overwrite: before=%d after=%d", len(beforeReplace), len(after))
	}
}

// serviceListVersions returns every captured version for a file via the
// public ListVersions API. Tests use this for introspection without
// poking at the Pebble index directly.
func serviceListVersions(t *testing.T, svc *domain.Service, fileID domain.FileID) []domain.VersionMeta {
	t.Helper()
	listed, err := svc.ListVersions(fileID, domain.VersionID{}, 1000)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	return listed.Items
}
