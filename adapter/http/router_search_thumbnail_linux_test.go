//go:build linux

package httpadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
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

func newTestRouterWithBasePaths(t *testing.T, basePaths []string) (http.Handler, *domain.Service, func()) {
	return newTestRouterWithBasePathsAndOptions(t, basePaths, RouterOptions{})
}

func newTestRouterWithBasePathsAndOptions(t *testing.T, basePaths []string, opts RouterOptions) (http.Handler, *domain.Service, func()) {
	t.Helper()

	indexDir := t.TempDir()

	idx, err := indexpebble.Open(indexDir, 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	store := filesystem.New()
	bus := eventbus.New()
	svc, err := domain.NewService(idx, store, bus, basePaths, 1000)
	if err != nil {
		_ = idx.Close()
		t.Fatalf("new service: %v", err)
	}

	if strings.TrimSpace(opts.BearerToken) == "" {
		opts.BearerToken = "test-token"
	}
	if opts.JobWorkers <= 0 {
		opts.JobWorkers = 2
	}
	if opts.JobQueueSize <= 0 {
		opts.JobQueueSize = 64
	}
	if opts.UploadExpiry <= 0 {
		opts.UploadExpiry = time.Hour
	}
	if opts.UploadCleanupInterval <= 0 {
		opts.UploadCleanupInterval = time.Hour
	}
	if opts.MaxChunkBytes <= 0 {
		opts.MaxChunkBytes = 10 << 20
	}
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = 100 << 20
	}
	opts.Rescan = svc.Rescan
	r := NewRouter(svc, opts)

	cleanup := func() {
		_ = idx.Close()
	}
	return r, svc, cleanup
}

func TestGlobSearchFairLimitAcrossPaths(t *testing.T) {
	baseA := t.TempDir()
	baseB := t.TempDir()
	r, _, cleanup := newTestRouterWithBasePaths(t, []string{baseA, baseB})
	defer cleanup()

	for i := 0; i < 4; i++ {
		if err := os.WriteFile(filepath.Join(baseA, fmt.Sprintf("a-%d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("write baseA file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(baseB, fmt.Sprintf("b-%d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("write baseB file: %v", err)
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/search/glob?pattern=**/*.txt&limit=4"))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d", w.Result().StatusCode)
	}

	var body struct {
		Results []struct {
			Path string `json:"path"`
		} `json:"results"`
		Errors []struct {
			Path string `json:"path"`
		} `json:"errors"`
		Paths []struct {
			Path     string `json:"path"`
			Returned int    `json:"returned"`
			HasMore  bool   `json:"hasMore"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", body.Errors)
	}
	if len(body.Results) != 4 {
		t.Fatalf("result count=%d, want 4", len(body.Results))
	}
	if len(body.Paths) != 2 {
		t.Fatalf("path stats count=%d, want 2", len(body.Paths))
	}
	for _, p := range body.Paths {
		if p.Returned != 2 {
			t.Fatalf("returned=%d for path=%q, want 2", p.Returned, p.Path)
		}
		if !p.HasMore {
			t.Fatalf("expected hasMore=true for path=%q", p.Path)
		}
	}

	mountA := filepath.Base(baseA)
	mountB := filepath.Base(baseB)
	countByMount := map[string]int{}
	for _, item := range body.Results {
		parts := strings.Split(strings.TrimPrefix(item.Path, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			t.Fatalf("invalid path: %q", item.Path)
		}
		countByMount[parts[0]]++
	}
	if countByMount[mountA] != 2 || countByMount[mountB] != 2 {
		t.Fatalf("unfair distribution: %v", countByMount)
	}
}

func TestGlobSearchReturnsPartialErrors(t *testing.T) {
	baseA := t.TempDir()
	baseB := t.TempDir()
	r, _, cleanup := newTestRouterWithBasePaths(t, []string{baseA, baseB})
	defer cleanup()

	if err := os.WriteFile(filepath.Join(baseA, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write baseA file: %v", err)
	}
	if err := os.RemoveAll(baseB); err != nil {
		t.Fatalf("remove baseB: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/search/glob?pattern=**/*.txt&limit=10"))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status=%d", w.Result().StatusCode)
	}

	var body struct {
		Results []map[string]any `json:"results"`
		Errors  []struct {
			Path  string `json:"path"`
			Cause string `json:"cause"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Results) == 0 {
		t.Fatalf("expected partial results")
	}
	if len(body.Errors) == 0 {
		t.Fatalf("expected path error")
	}
	if body.Errors[0].Path != "/"+filepath.Base(baseB) {
		t.Fatalf("error path=%q, want %q", body.Errors[0].Path, "/"+filepath.Base(baseB))
	}
}

func TestThumbnailEndpointFastPathAndValidation(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	imagePath := filepath.Join(baseAbs, "photo.jpg")
	if err := writeJPEGFile(imagePath, 800, 600); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	id, err := svc.ResolvePath(root.Name + "/photo.jpg")
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}

	first := httptest.NewRecorder()
	req1 := authedRequest(http.MethodGet, "/v1/nodes/"+id.String()+"/thumbnail?size=128")
	r.ServeHTTP(first, req1)
	if first.Result().StatusCode != http.StatusOK {
		t.Fatalf("thumbnail status=%d", first.Result().StatusCode)
	}
	if ct := first.Result().Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type=%q, want image/jpeg", ct)
	}
	etag := first.Result().Header.Get("ETag")
	if etag == "" {
		t.Fatalf("etag missing")
	}

	img, _, err := image.Decode(bytes.NewReader(first.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if img.Bounds().Dx() > 128 || img.Bounds().Dy() > 128 {
		t.Fatalf("thumb size=%dx%d, expected max 128", img.Bounds().Dx(), img.Bounds().Dy())
	}

	second := httptest.NewRecorder()
	req2 := authedRequest(http.MethodGet, "/v1/nodes/"+id.String()+"/thumbnail?size=128")
	req2.Header.Set("If-None-Match", etag)
	r.ServeHTTP(second, req2)
	if second.Result().StatusCode != http.StatusNotModified {
		t.Fatalf("if-none-match status=%d, want %d", second.Result().StatusCode, http.StatusNotModified)
	}

	invalidSize := httptest.NewRecorder()
	r.ServeHTTP(invalidSize, authedRequest(http.MethodGet, "/v1/nodes/"+id.String()+"/thumbnail?size=300"))
	if invalidSize.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid-size status=%d, want %d", invalidSize.Result().StatusCode, http.StatusBadRequest)
	}

	textPath := filepath.Join(baseAbs, "readme.txt")
	if err := os.WriteFile(textPath, []byte("not-image"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan text file: %v", err)
	}
	textID, err := svc.ResolvePath(root.Name + "/readme.txt")
	if err != nil {
		t.Fatalf("resolve text path: %v", err)
	}

	unsupported := httptest.NewRecorder()
	r.ServeHTTP(unsupported, authedRequest(http.MethodGet, "/v1/nodes/"+textID.String()+"/thumbnail"))
	if unsupported.Result().StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported status=%d, want %d", unsupported.Result().StatusCode, http.StatusUnsupportedMediaType)
	}
}

func TestThumbnailEndpointRejectsOversizedImageByPixelLimit(t *testing.T) {
	base := t.TempDir()
	r, svc, cleanup := newTestRouterWithBasePathsAndOptions(t, []string{base}, RouterOptions{
		ThumbnailMaxPixels: 10000,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	baseAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	imagePath := filepath.Join(baseAbs, "huge.jpg")
	if err := writeJPEGFile(imagePath, 800, 600); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	id, err := svc.ResolvePath(root.Name + "/huge.jpg")
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/v1/nodes/"+id.String()+"/thumbnail?size=128"))
	if w.Result().StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want=%d", w.Result().StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func writeJPEGFile(path string, width, height int) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill := color.RGBA{R: 31, G: 122, B: 231, A: 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, fill)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
}
