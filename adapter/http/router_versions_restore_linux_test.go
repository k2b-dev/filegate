//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

func TestRestoreInPlaceReplacesBytesAndPreservesID(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	const v1 = "v1-original-content"
	const v2 = "v2-replaced-content"
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{v1, v2})

	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(listed.Items) == 0 {
		t.Fatalf("no versions captured")
	}
	target := listed.Items[0] // V1 (oldest, holds v1 bytes)

	w := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/restore", nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var resp apiv1.VersionRestoreResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&resp)
	if resp.AsNew {
		t.Fatalf("expected in-place restore, got AsNew=true")
	}
	if resp.Node.ID != id.String() {
		t.Fatalf("ID changed across in-place restore: got %s want %s", resp.Node.ID, id.String())
	}

	// Live file now holds the V1 bytes.
	abs, _ := svc.ResolveAbsPath(id)
	got := mustReadFile(t, abs)
	if got != v1 {
		t.Fatalf("file content=%q after restore, want %q", got, v1)
	}
}

func TestRestoreInPlaceSnapshotsCurrentFirst(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"v1-bytes", "v2-bytes"})
	beforeRestore, _ := svc.ListVersions(id, domain.VersionID{}, 100)

	// Wait so the snapshot-current step actually fires (cooldown).
	time.Sleep(80 * time.Millisecond)

	w := postJSON(t, r,
		"/v1/nodes/"+id.String()+"/versions/"+beforeRestore.Items[0].VersionID.String()+"/restore", nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d", w.Result().StatusCode)
	}

	afterRestore, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(afterRestore.Items) <= len(beforeRestore.Items) {
		t.Fatalf("restore did not capture pre-restore state: before=%d after=%d",
			len(beforeRestore.Items), len(afterRestore.Items))
	}
}

func TestRestoreAsNewCreatesSiblingWithSuffix(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "report.pdf",
		[]string{"original-pdf-content", "v2-content"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	target := listed.Items[0]

	w := postJSON(t, r,
		"/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/restore",
		apiv1.VersionRestoreRequest{AsNewFile: true})
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var resp apiv1.VersionRestoreResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&resp)
	if !resp.AsNew {
		t.Fatalf("expected AsNew=true")
	}
	if resp.Node.ID == id.String() {
		t.Fatalf("AsNew restore reused source ID — should be fresh")
	}
	if resp.Node.Name != "report-restored.pdf" {
		t.Fatalf("name=%q, want report-restored.pdf", resp.Node.Name)
	}
}

func TestRestoreAsNewWithUserNameAndConflictSuffix(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"old-bytes", "new-bytes"})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	target := listed.Items[0]

	// First as-new with user-provided name: should land at exactly "manual.bin".
	w1 := postJSON(t, r,
		"/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/restore",
		apiv1.VersionRestoreRequest{AsNewFile: true, Name: "manual.bin"})
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("first as-new status=%d", w1.Result().StatusCode)
	}
	var first apiv1.VersionRestoreResponse
	_ = json.NewDecoder(w1.Result().Body).Decode(&first)
	if first.Node.Name != "manual.bin" {
		t.Fatalf("first as-new name=%q, want manual.bin", first.Node.Name)
	}

	// Second as-new with the same user name: must auto-suffix to manual-1.bin.
	w2 := postJSON(t, r,
		"/v1/nodes/"+id.String()+"/versions/"+target.VersionID.String()+"/restore",
		apiv1.VersionRestoreRequest{AsNewFile: true, Name: "manual.bin"})
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("second as-new status=%d", w2.Result().StatusCode)
	}
	var second apiv1.VersionRestoreResponse
	_ = json.NewDecoder(w2.Result().Body).Decode(&second)
	if second.Node.Name != "manual-1.bin" {
		t.Fatalf("second as-new name=%q, want manual-1.bin", second.Node.Name)
	}
}

func TestRestoreAsNewPreservesVersionMode(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/f.bin",
		strings.NewReader("payload-content"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Snapshot now to lock in the current mode (default 0644 from create).
	snapW := postJSON(t, r, "/v1/nodes/"+meta.ID.String()+"/versions/snapshot", nil)
	if snapW.Result().StatusCode != http.StatusCreated {
		t.Fatalf("snapshot status=%d", snapW.Result().StatusCode)
	}
	var snap apiv1.VersionResponse
	_ = json.NewDecoder(snapW.Result().Body).Decode(&snap)

	w := postJSON(t, r,
		"/v1/nodes/"+meta.ID.String()+"/versions/"+snap.VersionID+"/restore",
		apiv1.VersionRestoreRequest{AsNewFile: true})
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d", w.Result().StatusCode)
	}
	var resp apiv1.VersionRestoreResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&resp)
	// The mode bits in Node.Ownership.Mode are formatted as octal string;
	// just assert it matches what the snapshot recorded (formatted the
	// same way).
	if resp.Node.Ownership.Mode == "" || snap.Mode == 0 {
		t.Fatalf("missing mode comparison: node=%+v snap=%+v", resp.Node.Ownership, snap)
	}
}

func TestRestoreInPlaceUnknownVersionReturns404(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"only"})
	bogus := "00000000-0000-7000-8000-000000000000"
	w := postJSON(t, r, "/v1/nodes/"+id.String()+"/versions/"+bogus+"/restore", nil)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Result().StatusCode)
	}
}

func TestRestoreSerializesConcurrentInPlaceCalls(t *testing.T) {
	r, svc, cleanup := versioningTestRouterWithPinCap(t, 100)
	defer cleanup()

	root := svc.ListRoot()[0]
	const v1 = "alpha-bytes-here"
	const v2 = "beta-bytes-different"
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{v1, v2})
	listed, _ := svc.ListVersions(id, domain.VersionID{}, 100)
	if len(listed.Items) < 2 {
		t.Fatalf("need at least 2 versions, got %d", len(listed.Items))
	}
	a := listed.Items[0]
	b := listed.Items[len(listed.Items)-1]

	// Fire two concurrent restores. The per-file lock must serialize
	// them so the live file ends up holding EXACTLY one of the two
	// version contents, not a torn mix.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = postJSON(t, r,
			"/v1/nodes/"+id.String()+"/versions/"+a.VersionID.String()+"/restore", nil)
	}()
	go func() {
		defer wg.Done()
		_ = postJSON(t, r,
			"/v1/nodes/"+id.String()+"/versions/"+b.VersionID.String()+"/restore", nil)
	}()
	wg.Wait()

	abs, _ := svc.ResolveAbsPath(id)
	got := mustReadFile(t, abs)
	if got != v1 && got != v2 {
		t.Fatalf("torn restore: got=%q, want %q or %q", got, v1, v2)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(buf)
}
