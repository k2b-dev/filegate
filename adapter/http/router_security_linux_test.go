//go:build linux

package httpadapter

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/eventbus"
	"github.com/valentinkolb/filegate/infra/filesystem"
	indexpebble "github.com/valentinkolb/filegate/infra/pebble"
)

func TestStatusFromErrMapsInsufficientStorage(t *testing.T) {
	w := httptest.NewRecorder()
	statusFromErr(w, domain.ErrInsufficientStorage)
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusInsufficientStorage)
	}
}

func newTestRouter(t *testing.T) (http.Handler, *domain.Service, func()) {
	return newTestRouterWithToken(t, "test-token")
}

func newTestRouterWithToken(t *testing.T, token string) (http.Handler, *domain.Service, func()) {
	return newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           token,
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        100 << 20,
	})
}

// newTestRouterWithCustomLimits wires up a router with caller-controlled
// upload/job/thumbnail limits. This is the building block for adversarial
// tests that need to drive the server into a specific resource boundary.
// If opts.Rescan is nil the service's own Rescan is used.
func newTestRouterWithCustomLimits(t *testing.T, baseDir, indexDir string, opts RouterOptions) (http.Handler, *domain.Service, func()) {
	t.Helper()

	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	store := filesystem.New()
	bus := eventbus.New()
	svc, err := domain.NewService(idx, store, bus, []string{baseDir}, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}

	if opts.Rescan == nil {
		opts.Rescan = svc.Rescan
	}
	r := NewRouter(svc, opts)

	closeRouter := func() {}
	if closer, ok := r.(interface{ Close() error }); ok {
		closeRouter = func() {
			_ = closer.Close()
		}
	}

	cleanup := func() {
		closeRouter()
		_ = idx.Close()
	}
	return r, svc, cleanup
}

func authedRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func TestSecureHeadersPresent(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/paths/"))

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", w.Result().StatusCode)
	}
	for k, want := range map[string]string{
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "strict-origin-when-cross-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := w.Result().Header.Get(k); got != want {
			t.Fatalf("header %s = %q, want %q", k, got, want)
		}
	}
}

func TestAuthMiddlewareFailsClosedWhenTokenNotConfigured(t *testing.T) {
	r, _, cleanup := newTestRouterWithToken(t, "")
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/paths/"))

	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d, want %d", w.Result().StatusCode, http.StatusUnauthorized)
	}
}

func TestConfigRoutesExposeManifestWorkflowOnly(t *testing.T) {
	stub := &configServiceStub{}
	r, _, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		JobWorkers:            1,
		JobQueueSize:          8,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         1 << 20,
		MaxUploadBytes:        10 << 20,
		ConfigService:         stub,
	})
	defer cleanup()

	plan := authedRequest(http.MethodPost, "/v1/config/plan")
	plan.Body = io.NopCloser(strings.NewReader(`{"values":{}}`))
	plan.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	r.ServeHTTP(out, plan)
	if out.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", out.Code, out.Body.String())
	}

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/v1/config"},
		{http.MethodPost, "/v1/config/validate"},
		{http.MethodPost, "/v1/config/reload"},
	} {
		req := authedRequest(route.method, route.path)
		req.Body = io.NopCloser(strings.NewReader(`{"changes":{"upload.expiry":"1h"}}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status=%d, want removed route", route.method, route.path, rec.Code)
		}
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/paths/%2e%2e/%2e%2e/etc"))
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusBadRequest)
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	mount := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(mount.ID)
	if err != nil {
		t.Fatalf("resolve mount path: %v", err)
	}

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	linkPath := filepath.Join(baseAbs, "leak")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	target := "/v1/paths/" + mount.Name + "/leak"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, target))
	status := w.Result().StatusCode
	if status != http.StatusForbidden && status != http.StatusNotFound {
		var body map[string]any
		_ = json.NewDecoder(w.Result().Body).Decode(&body)
		t.Fatalf("status = %d, want %d or %d, body=%v", status, http.StatusForbidden, http.StatusNotFound, body)
	}
}

func TestRescanDoesNotWriteIDsToSymlinkTargetsOutsideMount(t *testing.T) {
	_, svc, cleanup := newTestRouter(t)
	defer cleanup()

	mount := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(mount.ID)
	if err != nil {
		t.Fatalf("resolve mount path: %v", err)
	}

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	linkPath := filepath.Join(baseAbs, "outside-link")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	store := filesystem.New()
	if _, err := store.GetID(outsideFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file should not receive filegate id via symlink scan, err=%v", err)
	}
}

func TestDirectoryTarSkipsSymlinkEntries(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	mount := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(mount.ID)
	if err != nil {
		t.Fatalf("resolve mount path: %v", err)
	}

	dirPath := filepath.Join(baseAbs, "bundle")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write ok file: %v", err)
	}
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(dirPath, "leak")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	dirID, err := svc.ResolvePath(mount.Name + "/bundle")
	if err != nil {
		t.Fatalf("resolve dir id: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/nodes/"+dirID.String()+"/content"))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want=%d", w.Result().StatusCode, http.StatusOK)
	}

	tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
	foundRegular := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if strings.Contains(hdr.Name, "leak") {
			t.Fatalf("tar should not contain symlink entry leak, got %q", hdr.Name)
		}
		if strings.Contains(hdr.Name, "ok.txt") {
			foundRegular = true
		}
	}
	if !foundRegular {
		t.Fatalf("expected regular file entry in tar")
	}
}

func TestDirectoryTarPreflightEnforcesBudgets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deep", "child"), 0o755); err != nil {
		t.Fatalf("mkdir deep child: %v", err)
	}

	if err := preflightDirectoryTarWithBudget(root, directoryTarBudget{
		MaxEntries: 10,
		MaxBytes:   10,
		MaxDepth:   3,
	}); err != nil {
		t.Fatalf("small tree rejected: %v", err)
	}

	cases := []struct {
		name   string
		budget directoryTarBudget
	}{
		{
			name: "entries",
			budget: directoryTarBudget{
				MaxEntries: 1,
				MaxBytes:   10,
				MaxDepth:   3,
			},
		},
		{
			name: "bytes",
			budget: directoryTarBudget{
				MaxEntries: 10,
				MaxBytes:   3,
				MaxDepth:   3,
			},
		},
		{
			name: "depth",
			budget: directoryTarBudget{
				MaxEntries: 10,
				MaxBytes:   10,
				MaxDepth:   1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightDirectoryTarWithBudget(root, tc.budget)
			if err == nil {
				t.Fatalf("expected budget error")
			}
			var budgetErr directoryTarBudgetError
			if !errors.As(err, &budgetErr) {
				t.Fatalf("err=%T %v, want directoryTarBudgetError", err, err)
			}
		})
	}
}

func TestDirectoryTarRouteRejectsOverBudget(t *testing.T) {
	oldBudget := defaultDirectoryTarBudget
	defaultDirectoryTarBudget = directoryTarBudget{
		MaxEntries: 10,
		MaxBytes:   3,
		MaxDepth:   3,
	}
	t.Cleanup(func() {
		defaultDirectoryTarBudget = oldBudget
	})

	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	mount := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(mount.ID)
	if err != nil {
		t.Fatalf("resolve mount path: %v", err)
	}
	dirPath := filepath.Join(baseAbs, "too-big")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir too-big: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, "a.txt"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	dirID, err := svc.ResolvePath(mount.Name + "/too-big")
	if err != nil {
		t.Fatalf("resolve dir id: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/nodes/"+dirID.String()+"/content"))
	if w.Result().StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want=%d body=%s", w.Result().StatusCode, http.StatusRequestEntityTooLarge, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "directory tar exceeds max content bytes") {
		t.Fatalf("body missing budget reason: %s", w.Body.String())
	}
}

func TestDownloadHeaderSanitizesFilename(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	fileMeta, err := svc.CreateChild(root.ID, "bad\nname.txt", false, nil)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := svc.WriteContent(fileMeta.ID, strings.NewReader("x")); err != nil {
		t.Fatalf("write content: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/nodes/"+fileMeta.ID.String()+"/content"))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want=%d", w.Result().StatusCode, http.StatusOK)
	}
	cd := w.Result().Header.Get("Content-Disposition")
	if strings.ContainsRune(cd, '\n') || strings.ContainsRune(cd, '\r') {
		t.Fatalf("content-disposition contains control chars: %q", cd)
	}
	if !strings.Contains(strings.ToLower(cd), "filename=") {
		t.Fatalf("content-disposition missing filename parameter: %q", cd)
	}
}

func TestUploadSessionEndpointsRejectInvalidSessionID(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, authedRequest(http.MethodGet, "/v1/uploads/sessions/not-valid"))
	if w1.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w1.Result().StatusCode, http.StatusBadRequest)
	}

	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/uploads/sessions/not-valid/segments/0",
		bytes.NewReader([]byte("x")),
	)
	req.Header.Set("Authorization", "Bearer test-token")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w2.Result().StatusCode, http.StatusBadRequest)
	}
}
