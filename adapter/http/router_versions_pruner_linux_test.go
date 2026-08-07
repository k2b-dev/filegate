//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

// versioningTestRouterWithRetention wires the standard router and turns
// versioning on with a tunable retention policy so each prune scenario
// can drive the pruner deterministically.
func versioningTestRouterWithRetention(t *testing.T, cfg domain.VersioningConfig) (http.Handler, *domain.Service, func()) {
	t.Helper()
	r, svc, cleanup := newTestRouter(t)
	if cfg.MaxLabelBytes == 0 {
		cfg.MaxLabelBytes = 2048
	}
	if cfg.MaxPinnedPerFile == 0 {
		cfg.MaxPinnedPerFile = 100
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 50 * time.Millisecond
	}
	svc.EnableVersioning(cfg, true)
	return r, svc, cleanup
}

func TestPrunerRemovesUnreferencedVersionsOutsideBuckets(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
		// No buckets defined → without explicit retention we'd keep
		// everything. Add a very narrow one to force pruning of older
		// versions.
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: 30 * 24 * time.Hour, MaxCount: 1},
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"v1", "v2", "v3", "v4", "v5"})

	before, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(before.Items) < 4 {
		t.Fatalf("expected >= 4 versions before prune, got %d", len(before.Items))
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if stats.VersionsDeleted == 0 {
		t.Fatalf("expected the pruner to delete some versions, got %#v", stats)
	}

	after, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	// At most 1 (MaxCount=1) for the live, unpinned set.
	if len(after.Items) > 1 {
		t.Fatalf("after prune expected 1 version, got %d: %#v", len(after.Items), after.Items)
	}
}

func TestPrunerKeepsPinnedRegardlessOfBuckets(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: time.Hour, MaxCount: 1},
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"a", "b", "c"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	first := listed.Items[0]

	// Pin the oldest version explicitly via API.
	postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+first.VersionID.String()+"/pin", nil)

	if _, err := svc.PruneVersions(); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	after, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	foundPinned := false
	for _, v := range after.Items {
		if v.VersionID == first.VersionID {
			foundPinned = true
			break
		}
	}
	if !foundPinned {
		t.Fatalf("pruner removed a pinned version; survivors: %v", after.Items)
	}
}

func TestPrunerOrphanGraceWindow(t *testing.T) {
	// 0-second grace = pruner deletes orphans on the very first pass.
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1:       0,
		PinnedGraceAfterDelete: 0,
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: time.Hour, MaxCount: -1},
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "doomed.bin",
		[]string{"v1-bytes", "v2-bytes"})
	before, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(before.Items) == 0 {
		t.Fatalf("no versions captured")
	}

	if err := svc.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if stats.OrphansPurged == 0 {
		t.Fatalf("expected pruner to purge orphans, got stats=%#v", stats)
	}
}

func TestPrunerOrphanGraceKeepsRecentlyDeleted(t *testing.T) {
	// Long grace = nothing should be pruned even after delete.
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1:       0,
		PinnedGraceAfterDelete: 24 * time.Hour,
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: time.Hour, MaxCount: -1},
		},
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"a", "b"})
	if err := svc.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if stats.OrphansPurged != 0 {
		t.Fatalf("expected 0 orphans purged within grace, got %d", stats.OrphansPurged)
	}
}

func TestDeleteVersionEndpoint(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"a", "b"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("no versions captured")
	}
	target := listed.Items[0]

	req := httptest.NewRequest(http.MethodDelete,
		"/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String(), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", w.Result().StatusCode)
	}

	// Subsequent GET on the deleted version returns 404.
	getReq := httptest.NewRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/content", nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, getReq)
	if w2.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("post-delete GET status=%d, want 404", w2.Result().StatusCode)
	}
}

func TestDeleteVersionWorksOnPinned(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithRetention(t, domain.VersioningConfig{
		MinSizeForAutoV1: 0,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/f.bin",
		strings.NewReader("payload"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Snapshot pinned.
	w := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot", nil)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("snapshot status=%d", w.Result().StatusCode)
	}
	var snap apiv1.VersionResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&snap)
	if !snap.Pinned {
		t.Fatalf("snapshot not pinned: %#v", snap)
	}

	// Manual delete of the pinned version must still succeed (operator override).
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/nodes/"+meta.ID.String()+"/versions/"+snap.VersionID, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	wDel := httptest.NewRecorder()
	r.ServeHTTP(wDel, req)
	if wDel.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("delete pinned status=%d, want 204", wDel.Result().StatusCode)
	}
}
