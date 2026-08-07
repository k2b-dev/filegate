//go:build linux

package httpadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

// versioningTestRouter wires the standard test router but turns the
// versioning subsystem on with a 0-byte size floor so even small test
// payloads produce a V1.
func versioningTestRouter(t *testing.T) (http.Handler, *domain.Service, func()) {
	t.Helper()
	r, svc, cleanup := newTestRouter(t)
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    2048,
	}, true)
	return r, svc, cleanup
}

// seedFileWithMultipleVersions creates a file via PUT and then issues
// further overwrites separated by enough sleep that each one is outside
// the cooldown window. Returns the file ID.
func seedFileWithMultipleVersions(t *testing.T, r http.Handler, svc *domain.Service, mountName, name string, payloads []string) domain.FileID {
	t.Helper()
	if len(payloads) == 0 {
		t.Fatalf("seedFileWithMultipleVersions: payloads must not be empty")
	}
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+mountName+"/"+name,
		strings.NewReader(payloads[0]),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	for i := 1; i < len(payloads); i++ {
		// 80ms is comfortably > the 50ms cooldown but still tolerable
		// in CI. Test runs stay under 1s for typical 3-version setups.
		time.Sleep(80 * time.Millisecond)
		if err := svc.WriteContent(meta.ID, strings.NewReader(payloads[i])); err != nil {
			t.Fatalf("overwrite %d: %v", i, err)
		}
	}
	return meta.ID
}

func TestListVersionsReturnsCapturedVersions(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"v1-bytes", "v2-bytes", "v3-bytes"})

	req := authedRequest(http.MethodGet, "/v1/nodes/"+id.String()+"/versions")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Result().StatusCode, w.Body.String())
	}
	var resp apiv1.ListVersionsResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 3 writes total: V1 from create, plus 2 pre-overwrite captures.
	if len(resp.Items) < 3 {
		t.Fatalf("want >= 3 versions, got %d: %#v", len(resp.Items), resp.Items)
	}
	for i, v := range resp.Items {
		if v.VersionID == "" || v.FileID != id.String() {
			t.Fatalf("item %d: bad ids %#v", i, v)
		}
		if v.Size <= 0 {
			t.Fatalf("item %d: empty size", i)
		}
	}
}

func TestListVersionsCursorPagination(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin",
		[]string{"a-bytes", "b-bytes", "c-bytes", "d-bytes"})

	// First page: limit=2 should return exactly 2 + a NextCursor.
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions?limit=2"))
	if w1.Result().StatusCode != http.StatusOK {
		t.Fatalf("page1 status=%d", w1.Result().StatusCode)
	}
	var page1 apiv1.ListVersionsResponse
	_ = json.NewDecoder(w1.Result().Body).Decode(&page1)
	if len(page1.Items) != 2 {
		t.Fatalf("page1 size=%d, want 2", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatalf("page1 missing NextCursor with more items available")
	}

	// Second page: cursor advances past page1.
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions?limit=2&cursor="+page1.NextCursor))
	if w2.Result().StatusCode != http.StatusOK {
		t.Fatalf("page2 status=%d", w2.Result().StatusCode)
	}
	var page2 apiv1.ListVersionsResponse
	_ = json.NewDecoder(w2.Result().Body).Decode(&page2)
	if len(page2.Items) == 0 {
		t.Fatalf("page2 empty")
	}
	for _, p2 := range page2.Items {
		for _, p1 := range page1.Items {
			if p2.VersionID == p1.VersionID {
				t.Fatalf("page2 includes page1 item %s — cursor did not advance", p2.VersionID)
			}
		}
	}
}

func TestListVersionsForUnknownIDReturns404(t *testing.T) {
	r, _, cleanup := versioningTestRouter(t)
	defer cleanup()

	bogus := "00000000-0000-7000-8000-000000000000"
	req := authedRequest(http.MethodGet, "/v1/nodes/"+bogus+"/versions")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Result().StatusCode)
	}
}

func TestListVersionsRejectsInvalidCursor(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"only-version"})

	req := authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions?cursor=not-a-uuid")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Result().StatusCode)
	}
}

func TestListVersionsRejectsInvalidLimit(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"only"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions?limit=0"))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Result().StatusCode)
	}
}

func TestVersionContentReturnsCapturedBytes(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	const v1Bytes = "first-payload-content"
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "v.bin",
		[]string{v1Bytes, "second-payload"})

	listed, err := svc.ListVersions(id, domain.VersionID{}, 100)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(listed.Items) == 0 {
		t.Fatalf("expected at least 1 captured version")
	}
	// The first item is V1 (oldest, ascending order). It holds the
	// initial content because the first overwrite captured it pre-write.
	v1 := listed.Items[0]

	req := authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions/"+v1.VersionID.String()+"/content")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Result().StatusCode, w.Body.String())
	}
	body, _ := io.ReadAll(w.Result().Body)
	if string(body) != v1Bytes {
		t.Fatalf("body=%q, want %q", body, v1Bytes)
	}
	if got := w.Result().Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type=%q", got)
	}
}

func TestVersionContentForUnknownVersionReturns404(t *testing.T) {
	r, svc, cleanup := versioningTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	id := seedFileWithMultipleVersions(t, r, svc, root.Name, "f.bin", []string{"only"})

	bogusVID := "00000000-0000-7000-8000-000000000000"
	req := authedRequest(http.MethodGet,
		"/v1/nodes/"+id.String()+"/versions/"+bogusVID+"/content")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Result().StatusCode)
	}
}

func TestVersioningDisabledReturns404OnList(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	// Versioning intentionally NOT enabled.

	root := svc.ListRoot()[0]
	meta, err := svc.CreateChild(root.ID, "x.bin", false, nil)
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	req := authedRequest(http.MethodGet, "/v1/nodes/"+meta.ID.String()+"/versions")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (versioning disabled)", w.Result().StatusCode)
	}
}
