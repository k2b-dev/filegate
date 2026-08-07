//go:build linux

package domain_test

import (
	"io"
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

func versioningOrphanService(t *testing.T, cfg domain.VersioningConfig) (*domain.Service, string, func()) {
	t.Helper()
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 50 * time.Millisecond
	}
	if cfg.MaxLabelBytes == 0 {
		cfg.MaxLabelBytes = 2048
	}
	if cfg.MaxPinnedPerFile == 0 {
		cfg.MaxPinnedPerFile = 100
	}
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("svc: %v", err)
	}
	svc.EnableVersioning(cfg, true)
	return svc, baseDir, func() {
		bus.Close()
		_ = idx.Close()
	}
}

// TestOrphanVersionsRemainListableAndContentReadableAfterDelete pins
// the headline grace-period contract: after a file is deleted, its
// captured versions are still listable AND their bytes are still
// fetchable through the public API for the duration of
// pinned_grace_after_delete. Without this, the "30 day recovery from
// accidental delete" promise collapses to "you have until the next
// pruner tick".
func TestOrphanVersionsRemainListableAndContentReadableAfterDelete(t *testing.T) {
	svc, _, cleanup := versioningOrphanService(t, domain.VersioningConfig{
		PinnedGraceAfterDelete: 24 * time.Hour, // generous grace
		MinSizeForAutoV1:       0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	const oldBytes = "original-content-here"
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/doomed.bin",
		strings.NewReader(oldBytes),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force a V2 capture so we have something distinct.
	time.Sleep(80 * time.Millisecond)
	if err := svc.WriteContent(meta.ID, strings.NewReader("new-content")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	preDelete, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(preDelete.Items) == 0 {
		t.Fatalf("no versions captured")
	}
	// V1 holds oldBytes (auto-V1 from create).
	v1 := preDelete.Items[0]

	// Delete the file. Versions should become orphans (DeletedAt != 0)
	// but stay reachable.
	if err := svc.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	postDelete, err := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if err != nil {
		t.Fatalf("ListVersions after delete: %v", err)
	}
	if len(postDelete.Items) == 0 {
		t.Fatalf("orphan versions not listable after delete")
	}
	for _, v := range postDelete.Items {
		if v.DeletedAt == 0 {
			t.Fatalf("orphan version %s missing DeletedAt: %#v", v.VersionID, v)
		}
	}

	// Content of the V1 orphan must still be fetchable.
	rc, vmeta, err := svc.OpenVersionContent(meta.ID, v1.VersionID)
	if err != nil {
		t.Fatalf("OpenVersionContent on orphan: %v", err)
	}
	defer rc.Close()
	if vmeta.DeletedAt == 0 {
		t.Fatalf("orphan content meta missing DeletedAt")
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read orphan content: %v", err)
	}
	if string(got) != oldBytes {
		t.Fatalf("orphan content=%q, want %q", got, oldBytes)
	}
}

// TestPrunerDeletesOrphanBlobAfterEntityGone pins the storage-leak
// fix: when the source entity is gone, the pruner uses
// VersionMeta.MountName to locate the blob and remove it. Without
// this, every deleted file's blobs would linger forever.
func TestPrunerDeletesOrphanBlobAfterEntityGone(t *testing.T) {
	svc, baseDir, cleanup := versioningOrphanService(t, domain.VersioningConfig{
		PinnedGraceAfterDelete: 0, // immediate purge after delete
		MinSizeForAutoV1:       0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/doomed.bin",
		strings.NewReader("v1"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := svc.WriteContent(meta.ID, strings.NewReader("v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	listed, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("no versions captured")
	}
	versionsDir := filepath.Join(baseDir, ".fg-versions", meta.ID.String())
	preBlobs, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("read versions dir: %v", err)
	}
	if len(preBlobs) == 0 {
		t.Fatalf("expected blobs on disk before delete")
	}

	if err := svc.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if stats.OrphansPurged != len(preBlobs) {
		t.Fatalf("OrphansPurged=%d, want %d", stats.OrphansPurged, len(preBlobs))
	}

	postBlobs, _ := os.ReadDir(versionsDir)
	if len(postBlobs) != 0 {
		t.Fatalf("orphan blobs still on disk after prune: %d remaining", len(postBlobs))
	}
}
