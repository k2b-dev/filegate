//go:build linux

package httpadapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

// versioningTestRouterWithPinCap is a per-test router with a tunable
// pinned-version cap so the cap-enforcement test can drive the boundary
// without inflating runtime.
func versioningTestRouterWithPinCap(t *testing.T, cap int) (http.Handler, *domain.Service, func()) {
	t.Helper()
	r, svc, cleanup := newTestRouter(t)
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    2048,
		MaxPinnedPerFile: cap,
	}, true)
	return r, svc, cleanup
}

func postJSON(t *testing.T, r http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Authorization", "Bearer test-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSnapshotCreatesPinnedVersionImmediately(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/file.bin",
		strings.NewReader("payload"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}

	w := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot",
		apiv1.VersionSnapshotRequest{Label: "my-checkpoint"})
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, body=%s", w.Result().StatusCode, w.Body.String())
	}
	var resp apiv1.VersionResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Pinned {
		t.Fatalf("snapshot not pinned: %#v", resp)
	}
	if resp.Label != "my-checkpoint" {
		t.Fatalf("label=%q, want %q", resp.Label, "my-checkpoint")
	}
}

func TestSnapshotIgnoresCooldown(t *testing.T) {
	// 5-second cooldown — manual snapshot should still succeed.
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         5 * time.Second,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    2048,
		MaxPinnedPerFile: 100,
	}, true)

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/x.bin",
		strings.NewReader("body"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Immediate snapshot must succeed even though we're well within cooldown.
	w := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot",
		apiv1.VersionSnapshotRequest{Label: "now"})
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("status=%d (cooldown blocked snapshot?)", w.Result().StatusCode)
	}
}

func TestSnapshotAtPinnedCapReturns409(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 2)
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/f.bin",
		strings.NewReader("hello"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 2; i++ {
		w := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot", nil)
		if w.Result().StatusCode != http.StatusCreated {
			t.Fatalf("snapshot %d status=%d", i, w.Result().StatusCode)
		}
	}
	w3 := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot", nil)
	if w3.Result().StatusCode != http.StatusConflict {
		t.Fatalf("3rd snapshot status=%d, want 409", w3.Result().StatusCode)
	}
}

func TestSnapshotRejectsOversizedLabel(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    16, // tiny cap to make the boundary cheap to hit
		MaxPinnedPerFile: 100,
	}, true)

	root := svc.ListRoot()[0]
	meta, _, _ := svc.WriteContentByVirtualPath("/"+root.Name+"/f.bin",
		strings.NewReader("bytes"), domain.ConflictError)

	w := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot",
		apiv1.VersionSnapshotRequest{Label: strings.Repeat("X", 17)})
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (label too long)", w.Result().StatusCode)
	}
}

func TestPinUnpinFlow(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "v.bin",
		[]string{"a", "b"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("no captured versions")
	}
	target := listed.Items[0]

	// Pin with label.
	label := "my-saved-state"
	pinReq := apiv1.VersionPinRequest{Label: &label}
	w := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/pin", pinReq)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("pin status=%d", w.Result().StatusCode)
	}
	var pinned apiv1.VersionResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&pinned)
	if !pinned.Pinned || pinned.Label != label {
		t.Fatalf("pin response not as expected: %#v", pinned)
	}

	// Unpin.
	w2 := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/unpin", nil)
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("unpin status=%d", w2.Result().StatusCode)
	}
	var unpinned apiv1.VersionResponse
	_ = json.NewDecoder(w2.Result().Body).Decode(&unpinned)
	if unpinned.Pinned {
		t.Fatalf("unpin still pinned: %#v", unpinned)
	}
	// Label is preserved across unpin (UnpinVersion only flips the flag).
	if unpinned.Label != label {
		t.Fatalf("label lost on unpin: got %q, want %q", unpinned.Label, label)
	}
}

func TestRePinIsIdempotentAndUpdatesLabel(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "v.bin", []string{"only"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	target := listed.Items[0]

	first := "v1"
	w1 := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/pin",
		apiv1.VersionPinRequest{Label: &first})
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("first pin status=%d", w1.Result().StatusCode)
	}

	// Re-pin with a new label — should succeed and update.
	second := "v2-updated"
	w2 := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/pin",
		apiv1.VersionPinRequest{Label: &second})
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("re-pin status=%d", w2.Result().StatusCode)
	}
	var resp apiv1.VersionResponse
	_ = json.NewDecoder(w2.Result().Body).Decode(&resp)
	if !resp.Pinned || resp.Label != second {
		t.Fatalf("re-pin did not update label: %#v", resp)
	}
}

func TestPinAtCapButRePinAllowed(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 1)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"v1-content", "v2-content"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(listed.Items) < 2 {
		t.Fatalf("need >= 2 versions, got %d", len(listed.Items))
	}
	first, second := listed.Items[0], listed.Items[1]

	// Pin first one — fills the cap (1).
	w1 := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+first.VersionID.String()+"/pin", nil)
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("first pin status=%d", w1.Result().StatusCode)
	}

	// Pin second one — would exceed cap. Expect 409.
	w2 := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+second.VersionID.String()+"/pin", nil)
	if w2.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second pin status=%d, want 409 (cap exceeded)", w2.Result().StatusCode)
	}

	// Re-pinning the first one (already pinned) is fine even at cap.
	wRe := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+first.VersionID.String()+"/pin", nil)
	if wRe.Result().StatusCode != http.StatusOK {
		t.Fatalf("re-pin at cap status=%d, want 200", wRe.Result().StatusCode)
	}
}

func TestPinUnknownVersionReturns404(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "x.bin", []string{"only"})
	bogus := "00000000-0000-7000-8000-000000000000"
	w := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+bogus+"/pin", nil)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Result().StatusCode)
	}
}

func TestUnpinUnpinnedIsIdempotent(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "x.bin", []string{"only"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	target := listed.Items[0]

	w := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/unpin", nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (unpin on unpinned should noop)", w.Result().StatusCode)
	}
}
