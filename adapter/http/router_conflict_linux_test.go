//go:build linux

package httpadapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

// putPath is a tiny helper that issues PUT /v1/paths with optional onConflict
// and returns the recorder so tests can inspect status, headers, and body.
func putPath(t *testing.T, r http.Handler, vp string, body []byte, onConflict string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/paths/" + strings.TrimPrefix(vp, "/")
	if onConflict != "" {
		target += "?onConflict=" + onConflict
	}
	req := httptest.NewRequest(http.MethodPut, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) apiv1.ErrorResponse {
	t.Helper()
	var out apiv1.ErrorResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return out
}

func decodeNode(t *testing.T, w *httptest.ResponseRecorder) apiv1.Node {
	t.Helper()
	var out apiv1.Node
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	return out
}

// ============================================================================
// PUT /v1/paths/{path}  — file uploads
// ============================================================================

func TestPathPutConflictDefaultIsErrorWhenFileExists(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	// First PUT creates the file.
	if w := putPath(t, r, root.Name+"/dup.txt", []byte("v1"), ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("first put status=%d", w.Result().StatusCode)
	}

	// Second PUT (no onConflict) must 409, NOT silently overwrite.
	w := putPath(t, r, root.Name+"/dup.txt", []byte("v2"), "")
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second put status=%d, want=%d", w.Result().StatusCode, http.StatusConflict)
	}

	// 409 body must include diagnostic fields.
	body := decodeError(t, w)
	if body.Error == "" {
		t.Fatalf("error message empty")
	}
	if body.ExistingID == "" || body.ExistingPath == "" {
		t.Fatalf("missing diagnostic fields: id=%q path=%q", body.ExistingID, body.ExistingPath)
	}

	// File content must remain "v1".
	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	got, err := os.ReadFile(filepath.Join(rootAbs, "dup.txt"))
	if err != nil {
		t.Fatalf("read on-disk file: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("file mutated despite default-error mode: got=%q", got)
	}
}

func TestPathPutConflictOverwriteReplacesContent(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	if w := putPath(t, r, root.Name+"/dup.txt", []byte("v1"), ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("first put status=%d", w.Result().StatusCode)
	}
	first := decodeNode(t, putPath(t, r, root.Name+"/dup.txt", []byte("v1-redo"), "overwrite"))
	if first.ID == "" {
		t.Fatalf("missing id")
	}

	// Subsequent overwrite preserves the same node id.
	w := putPath(t, r, root.Name+"/dup.txt", []byte("v2"), "overwrite")
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("overwrite status=%d, want=%d", w.Result().StatusCode, http.StatusOK)
	}
	second := decodeNode(t, w)
	if second.ID != first.ID {
		t.Fatalf("overwrite changed id: %s vs %s", second.ID, first.ID)
	}

	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	got, _ := os.ReadFile(filepath.Join(rootAbs, "dup.txt"))
	if string(got) != "v2" {
		t.Fatalf("content=%q, want v2", got)
	}
}

func TestPathPutConflictRenameProducesNewPath(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	if w := putPath(t, r, root.Name+"/photo.jpg", []byte("a"), ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("first put status=%d", w.Result().StatusCode)
	}

	w := putPath(t, r, root.Name+"/photo.jpg", []byte("b"), "rename")
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("rename put status=%d, want=%d", w.Result().StatusCode, http.StatusCreated)
	}
	node := decodeNode(t, w)
	if node.Name == "photo.jpg" {
		t.Fatalf("rename mode produced same name: %q", node.Name)
	}
	if !strings.HasPrefix(node.Name, "photo-") || !strings.HasSuffix(node.Name, ".jpg") {
		t.Fatalf("renamed name %q does not match scheme photo-NN.jpg", node.Name)
	}

	// Both files must exist on disk.
	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	if _, err := os.Stat(filepath.Join(rootAbs, "photo.jpg")); err != nil {
		t.Fatalf("original gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootAbs, node.Name)); err != nil {
		t.Fatalf("renamed gone: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(rootAbs, "photo.jpg"))
	b, _ := os.ReadFile(filepath.Join(rootAbs, node.Name))
	if string(a) != "a" || string(b) != "b" {
		t.Fatalf("contents diverged: a=%q b=%q", a, b)
	}
}

func TestPathPutConflictWithDirectoryAlwaysFails(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	// Create directory with the target name.
	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	if err := os.MkdirAll(filepath.Join(rootAbs, "thing"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	// PUT to that path must fail with 409 even with overwrite — we never
	// silently nuke a directory subtree from a single file PUT.
	for _, mode := range []string{"", "overwrite", "rename"} {
		w := putPath(t, r, root.Name+"/thing", []byte("x"), mode)
		switch mode {
		case "rename":
			// rename-mode falls through to createAndWriteContent which now
			// targets a unique sibling path; should succeed with Created.
			if w.Result().StatusCode != http.StatusCreated {
				t.Errorf("mode=rename: status=%d, want=Created", w.Result().StatusCode)
			}
		default:
			if w.Result().StatusCode != http.StatusConflict {
				t.Errorf("mode=%q: status=%d, want=Conflict", mode, w.Result().StatusCode)
			}
		}
	}

	// The directory must still be intact.
	info, err := os.Stat(filepath.Join(rootAbs, "thing"))
	if err != nil {
		t.Fatalf("stat thing: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("thing is no longer a directory: mode=%v", info.Mode())
	}
}

func TestPathPutConflictRejectsSkip(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := putPath(t, r, "mount/x.txt", []byte("x"), "skip")
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=BadRequest (skip is mkdir-only)", w.Result().StatusCode)
	}
}

// ============================================================================
// POST /v1/nodes/{id}/mkdir
// ============================================================================

func mkdirReq(t *testing.T, r http.Handler, parentID, path, onConflict string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"path": path}
	if onConflict != "" {
		body["onConflict"] = onConflict
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/nodes/"+parentID+"/mkdir", raw))
	return w
}

func TestMkdirConflictDefaultIsErrorWhenDirExists(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	if w := mkdirReq(t, r, root.ID.String(), "stuff", ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("first mkdir status=%d", w.Result().StatusCode)
	}
	w := mkdirReq(t, r, root.ID.String(), "stuff", "")
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("second mkdir status=%d, want=Conflict", w.Result().StatusCode)
	}
	body := decodeError(t, w)
	if body.ExistingID == "" || body.ExistingPath == "" {
		t.Fatalf("missing diagnostic fields: %+v", body)
	}
}

func TestMkdirConflictSkipReturnsExisting(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	first := decodeNode(t, mkdirReq(t, r, root.ID.String(), "shared", ""))
	if first.ID == "" {
		t.Fatalf("missing id from first mkdir")
	}

	w := mkdirReq(t, r, root.ID.String(), "shared", "skip")
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("skip mkdir status=%d", w.Result().StatusCode)
	}
	second := decodeNode(t, w)
	if second.ID != first.ID {
		t.Fatalf("skip mode returned different id: %s vs %s", second.ID, first.ID)
	}
}

func TestMkdirConflictRenameProducesUniqueDir(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	first := decodeNode(t, mkdirReq(t, r, root.ID.String(), "uploads", ""))
	w := mkdirReq(t, r, root.ID.String(), "uploads", "rename")
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("rename mkdir status=%d", w.Result().StatusCode)
	}
	renamed := decodeNode(t, w)
	if renamed.Name == "uploads" {
		t.Fatalf("rename produced original name")
	}
	if renamed.ID == first.ID {
		t.Fatalf("rename returned the existing id")
	}

	// Both must exist on disk.
	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	if _, err := os.Stat(filepath.Join(rootAbs, "uploads")); err != nil {
		t.Fatalf("original gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootAbs, renamed.Name)); err != nil {
		t.Fatalf("renamed gone: %v", err)
	}
}

func TestMkdirConflictRejectsOverwrite(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	if w := mkdirReq(t, r, root.ID.String(), "x", ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("seed mkdir status=%d", w.Result().StatusCode)
	}
	w := mkdirReq(t, r, root.ID.String(), "x", "overwrite")
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=BadRequest (overwrite is mkdir-forbidden)", w.Result().StatusCode)
	}
}

func TestMkdirConflictWhenFileExistsAlwaysFails(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	rootAbs, _ := svc.ResolveAbsPath(root.ID)
	if err := os.WriteFile(filepath.Join(rootAbs, "shadow"), []byte("file"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	for _, mode := range []string{"", "skip"} {
		w := mkdirReq(t, r, root.ID.String(), "shadow", mode)
		if w.Result().StatusCode != http.StatusConflict {
			t.Errorf("mode=%q: status=%d, want=Conflict (cannot skip a file with mkdir)", mode, w.Result().StatusCode)
		}
	}

	// rename should succeed because the conflict resolves to a fresh name.
	w := mkdirReq(t, r, root.ID.String(), "shadow", "rename")
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("mode=rename: status=%d, want=Created", w.Result().StatusCode)
	}
	renamed := decodeNode(t, w)
	if renamed.Name == "shadow" {
		t.Fatalf("rename produced original name despite conflict with file")
	}
}

func TestMkdirIntermediateSegmentsAlwaysReused(t *testing.T) {
	// The user's onConflict only governs the LEAF — intermediate segments
	// must always be reused-if-dir, otherwise mkdir -p style use
	// (consumer is WriteContentByVirtualPath) would 409 the second time.
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	if w := mkdirReq(t, r, root.ID.String(), "a/b/c", ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("first mkdir status=%d", w.Result().StatusCode)
	}
	// Re-create with leaf=error: leaf c exists, must 409. Intermediates
	// a, b must be reused silently — failure here would mean we 409 on the
	// intermediates instead.
	w := mkdirReq(t, r, root.ID.String(), "a/b/c", "")
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want=Conflict on leaf c", w.Result().StatusCode)
	}
	// New leaf under existing intermediates must succeed with default error.
	if w := mkdirReq(t, r, root.ID.String(), "a/b/d", ""); w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("leaf d under existing a/b: status=%d", w.Result().StatusCode)
	}
}

// ============================================================================
// Cross-cutting: ParseConflictMode validation surfaces correctly
// ============================================================================

func TestPathPutConflictRejectsUnknownMode(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()
	w := putPath(t, r, "mount/x.txt", []byte("x"), "merge-please")
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=BadRequest", w.Result().StatusCode)
	}
}

func TestMkdirConflictRejectsUnknownMode(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]
	w := mkdirReq(t, r, root.ID.String(), "x", "merge-please")
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=BadRequest", w.Result().StatusCode)
	}
}

// Compile-time guard: prevent silent breakage if domain renames the modes.
var _ = []domain.ConflictMode{
	domain.ConflictError, domain.ConflictOverwrite, domain.ConflictRename, domain.ConflictSkip,
}

// TestTransferConflictReturnsDiagnosticFields pins the iter3 fix: Transfer
// 409 responses now include existingId/existingPath via writeConflict, not
// the bare {"error":"conflict"} envelope from statusFromErr.
func TestTransferConflictReturnsDiagnosticFields(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	src, err := svc.CreateChild(root.ID, "src.bin", false, nil)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if err := svc.WriteContent(src.ID, strings.NewReader("hello")); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst, err := svc.CreateChild(root.ID, "dst.bin", false, nil)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if err := svc.WriteContent(dst.ID, strings.NewReader("victim")); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	body := map[string]any{
		"op":             "copy",
		"sourceId":       src.ID.String(),
		"targetParentId": root.ID.String(),
		"targetName":     "dst.bin",
		"onConflict":     "error",
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/transfers", raw))
	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want=Conflict", w.Result().StatusCode)
	}
	resp := decodeError(t, w)
	if resp.ExistingID == "" || resp.ExistingPath == "" {
		t.Fatalf("transfer 409 missing diagnostic fields: %+v", resp)
	}
	if resp.ExistingID != dst.ID.String() {
		t.Fatalf("existingId=%q, want=%q", resp.ExistingID, dst.ID.String())
	}
}

// TestTransferConflictRejectsUnknownMode pins the iter3 fix: Transfer now
// validates the onConflict mode through ParseConflictMode, so unknown values
// surface as 400 instead of being silently treated as "error".
func TestTransferConflictRejectsUnknownMode(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()
	root := svc.ListRoot()[0]

	src, err := svc.CreateChild(root.ID, "x.bin", false, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := map[string]any{
		"op":             "copy",
		"sourceId":       src.ID.String(),
		"targetParentId": root.ID.String(),
		"targetName":     "y.bin",
		"onConflict":     "merge-please",
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedJSONRequest(http.MethodPost, "/v1/transfers", raw))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=BadRequest", w.Result().StatusCode)
	}
}
