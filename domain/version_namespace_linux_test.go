//go:build linux

package domain_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/k2b-dev/filegate/v3/adapter/http"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// versioningNamespaceTestService boots a Service with versioning enabled
// and returns the service + the on-disk mount path. Mirrors the helpers
// in service_version_capture_linux_test.go but split out so namespace
// tests don't get tangled with capture-specific scaffolding.
func versioningNamespaceTestService(t *testing.T) (*domain.Service, string, func()) {
	t.Helper()
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	bus := eventbus.New()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    2048,
		MaxPinnedPerFile: 100,
	}, true)
	return svc, baseDir, func() {
		bus.Close()
		_ = idx.Close()
	}
}

// TestSyncAbsPathIgnoresVersionsNamespaceBlob pins that detector-driven
// SyncAbsPath calls for files inside .fg-versions are no-ops. Without
// this, btrfs find-new (or poll detector) would index every newly-
// captured version blob as a regular file, exposing it through the
// public path API and letting users delete their own version history
// via DELETE /v1/paths/.../.fg-versions/...
func TestSyncAbsPathIgnoresVersionsNamespaceBlob(t *testing.T) {
	svc, baseDir, cleanup := versioningNamespaceTestService(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootName := root.Name

	// Capture a real version by writing twice (overwriting outside cooldown).
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/source.bin",
		strings.NewReader("v1-bytes"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := svc.WriteContent(meta.ID, strings.NewReader("v2-bytes")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	listed, _ := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("no versions captured for blob path test")
	}
	blobPath := filepath.Join(baseDir, ".fg-versions",
		meta.ID.String(), listed.Items[0].VersionID.String()+".bin")
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob missing on disk: %v", err)
	}

	// SyncAbsPath of the blob path should silently no-op — not return
	// an error AND not index anything new.
	if err := svc.SyncAbsPath(blobPath); err != nil {
		t.Fatalf("SyncAbsPath of blob returned err: %v", err)
	}

	// Mount root listing must NOT contain .fg-versions.
	listing, err := svc.ListNodeChildren(root.ID, "", 100, false)
	if err != nil {
		t.Fatalf("ListNodeChildren: %v", err)
	}
	for _, item := range listing.Items {
		if item.Name == ".fg-versions" {
			t.Fatalf(".fg-versions is exposed in mount listing: %#v", item)
		}
	}

	// Path resolution to the namespace must be forbidden.
	if _, err := svc.ResolvePath(rootName + "/.fg-versions"); err == nil {
		t.Fatalf("ResolvePath of .fg-versions did not error")
	}
}

// TestReconcileAndRescanDoNotIndexVersionsNamespace pins that both the
// directory-reconcile pass and a full rescan walk skip the namespace
// without removing the actual blobs from disk. A regression here would
// either expose blobs (indexing them) or destroy version history (the
// rescan's prune treating them as stale).
func TestReconcileAndRescanDoNotIndexVersionsNamespace(t *testing.T) {
	svc, baseDir, cleanup := versioningNamespaceTestService(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootName := root.Name

	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/x.bin",
		strings.NewReader("seed"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := svc.WriteContent(meta.ID, strings.NewReader("seed-v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	versionsDir := filepath.Join(baseDir, ".fg-versions", meta.ID.String())
	preEntries, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("versions dir missing: %v", err)
	}
	if len(preEntries) == 0 {
		t.Fatalf("expected version blob on disk")
	}

	if err := svc.ReconcileDirectory(baseDir); err != nil {
		t.Fatalf("ReconcileDirectory: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("Rescan: %v", err)
	}

	// 1. .fg-versions must NOT appear in the public listing.
	listing, _ := svc.ListNodeChildren(root.ID, "", 100, false)
	for _, item := range listing.Items {
		if item.Name == ".fg-versions" {
			t.Fatalf(".fg-versions leaked into mount listing after reconcile/rescan")
		}
	}

	// 2. The blobs MUST still be on disk — rescan's prune must not
	//    treat them as stale and remove them.
	postEntries, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("versions dir disappeared after rescan: %v", err)
	}
	if len(postEntries) != len(preEntries) {
		t.Fatalf("blob count changed across rescan: pre=%d post=%d",
			len(preEntries), len(postEntries))
	}

	// 3. The version is still listable through the versioning API.
	listed, err := svc.ListVersions(meta.ID, domain.VersionID{}, 100)
	if err != nil {
		t.Fatalf("ListVersions after rescan: %v", err)
	}
	if len(listed.Items) == 0 {
		t.Fatalf("versions disappeared after rescan")
	}
}

// TestPathAPIsRejectVersionsNamespaceSegment pins the user-facing
// rejection. The reserved name shows up at the public-API surface as
// 403 Forbidden — never as a silent success that would let a user
// shadow the internal namespace.
func TestPathAPIsRejectVersionsNamespaceSegment(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()
	bus := eventbus.New()
	defer bus.Close()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown: 50 * time.Millisecond, MaxLabelBytes: 2048, MaxPinnedPerFile: 100,
	}, true)
	root := svc.ListRoot()[0]

	router := httpadapter.NewRouter(svc, httpadapter.RouterOptions{
		BearerToken:           "ns-token",
		JobWorkers:            2,
		JobQueueSize:          16,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         1 << 20,
		MaxUploadBytes:        4 << 20,
	})

	// PUT to a path naming .fg-versions at the root → forbidden.
	{
		req := httptest.NewRequest(http.MethodPut,
			"/v1/paths/"+root.Name+"/.fg-versions/pwn.bin",
			strings.NewReader("malicious"))
		req.Header.Set("Authorization", "Bearer ns-token")
		req.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("PUT root/.fg-versions/... status=%d, want 403", w.Code)
		}
	}

	// PUT nested .fg-versions → also forbidden (we reject the segment
	// anywhere in the path, not just at the root).
	{
		req := httptest.NewRequest(http.MethodPut,
			"/v1/paths/"+root.Name+"/safe/.fg-versions/nested.bin",
			strings.NewReader("malicious"))
		req.Header.Set("Authorization", "Bearer ns-token")
		req.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("PUT root/safe/.fg-versions/... status=%d, want 403", w.Code)
		}
	}

	// MkdirRelative direct service call — same rejection at the domain layer.
	if _, err := svc.MkdirRelative(root.ID, "subdir/.fg-versions/leaf", true, nil, domain.ConflictSkip); err == nil {
		t.Fatalf("MkdirRelative with .fg-versions segment did not error")
	}
}
