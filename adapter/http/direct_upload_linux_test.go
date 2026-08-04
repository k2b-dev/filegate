//go:build linux

package httpadapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

func TestDirectUploadURLWritesWithoutBearer(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		PublicURL:             "https://files.example.test",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        1024,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	body := []byte(`{"path":"` + root.Name + `/direct/hello.txt","contentType":"text/plain","maxBytes":64,"expiresInSeconds":60}`)
	create := httptest.NewRecorder()
	r.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body))
	if create.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d, want %d", create.Result().StatusCode, http.StatusCreated)
	}
	var created apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(create.Result().Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(created.UploadURL, "https://files.example.test/v1/uploads/direct/") {
		t.Fatalf("uploadUrl=%q does not use public URL", created.UploadURL)
	}
	if created.Method != http.MethodPut || created.MaxBytes != 64 {
		t.Fatalf("unexpected direct upload response: %#v", created)
	}

	put := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, created.UploadURL, bytes.NewReader([]byte("hello direct")))
	req.Header.Set("Content-Type", "text/plain")
	r.ServeHTTP(put, req)
	if put.Result().StatusCode != http.StatusCreated {
		t.Fatalf("put status=%d, want %d", put.Result().StatusCode, http.StatusCreated)
	}
	if put.Result().Header.Get("X-Node-Id") == "" {
		t.Fatalf("missing X-Node-Id")
	}
	if _, err := svc.ResolvePath(root.Name + "/direct/hello.txt"); err != nil {
		t.Fatalf("resolve uploaded file: %v", err)
	}
}

func TestDirectUploadURLIgnoresUntrustedForwardedHost(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        1024,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	body := []byte(`{"path":"` + root.Name + `/direct-host.txt","maxBytes":64,"expiresInSeconds":60}`)
	req := authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body)
	req.Host = "files.local"
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var out apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(out.UploadURL, "http://files.local/v1/uploads/direct/") {
		t.Fatalf("uploadUrl=%q", out.UploadURL)
	}
}

func TestDirectUploadURLHonorsTrustedForwardedHost(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		TrustedProxies:        []netip.Prefix{netip.MustParsePrefix("203.0.113.10/32")},
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        1024,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	body := []byte(`{"path":"` + root.Name + `/direct-host.txt","maxBytes":64,"expiresInSeconds":60}`)
	req := authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body)
	req.Host = "files.local"
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-Host", "public.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var out apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(out.UploadURL, "https://public.example/v1/uploads/direct/") {
		t.Fatalf("uploadUrl=%q", out.UploadURL)
	}
}

func TestDirectUploadCreateRequiresBearer(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads/direct", strings.NewReader(`{"path":"`+root.Name+`/blocked.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", w.Result().StatusCode, http.StatusUnauthorized)
	}
}

func TestDirectUploadRejectsTamperedToken(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	body := []byte(`{"path":"` + root.Name + `/tampered.txt","expiresInSeconds":60}`)
	create := httptest.NewRecorder()
	r.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body))
	if create.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", create.Result().StatusCode)
	}
	var created apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(create.Result().Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	tampered := created.UploadURL + "x"
	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, tampered, strings.NewReader("x")))
	if put.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", put.Result().StatusCode, http.StatusUnauthorized)
	}
}

func TestDirectUploadRejectsExpiredToken(t *testing.T) {
	_, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	direct := newDirectUploadManager(svc, "test-token", newLiveConfig(RouterOptions{MaxUploadBytes: 1024}))
	token, err := direct.sign(directUploadToken{
		Version:    1,
		Path:       root.Name + "/expired.txt",
		ExpiresAt:  time.Now().Add(-time.Second).Unix(),
		OnConflict: "error",
		MaxBytes:   64,
		Nonce:      "test",
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/uploads/direct/"+token, strings.NewReader("x"))
	req.SetPathValue("token", token)
	direct.handlePut(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", w.Result().StatusCode, http.StatusUnauthorized)
	}
}

func TestDirectUploadRejectsTooLargeBody(t *testing.T) {
	r, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:           "test-token",
		JobWorkers:            2,
		JobQueueSize:          64,
		UploadExpiry:          time.Hour,
		UploadCleanupInterval: time.Hour,
		MaxChunkBytes:         10 << 20,
		MaxUploadBytes:        1024,
	})
	defer cleanup()

	root := svc.ListRoot()[0]
	create := httptest.NewRecorder()
	body := []byte(`{"path":"` + root.Name + `/too-large.txt","maxBytes":5,"expiresInSeconds":60}`)
	r.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body))
	if create.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", create.Result().StatusCode)
	}
	var created apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(create.Result().Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, created.UploadURL, strings.NewReader("123456")))
	if w.Result().StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", w.Result().StatusCode, http.StatusRequestEntityTooLarge)
	}
}
