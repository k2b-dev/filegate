//go:build linux

package httpadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v3/domain"
)

func TestCORSDisabledByDefault(t *testing.T) {
	r, _, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://app.example.test")
	r.ServeHTTP(w, req)

	if w.Result().Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS header present while disabled: %s", w.Result().Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	r, _, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken: "test-token",
		CORS: domain.CORSConfig{
			AllowedOrigins: []string{"https://app.example.test"},
			AllowedMethods: []string{http.MethodPut},
			AllowedHeaders: []string{"Content-Type"},
			ExposedHeaders: []string{"X-Node-Id"},
			MaxAge:         10 * time.Minute,
		},
	})
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/uploads/direct/token", nil)
	req.Header.Set("Origin", "https://app.example.test")
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	r.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want %d", w.Result().StatusCode, http.StatusNoContent)
	}
	if got := w.Result().Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.test" {
		t.Fatalf("allow-origin=%q", got)
	}
	if got := w.Result().Header.Get("Access-Control-Allow-Methods"); got != http.MethodPut {
		t.Fatalf("allow-methods=%q", got)
	}
	if got := w.Result().Header.Get("Access-Control-Allow-Headers"); got != "Content-Type" {
		t.Fatalf("allow-headers=%q", got)
	}
	if got := w.Result().Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("max-age=%q", got)
	}
	if got := w.Result().Header.Get("Access-Control-Expose-Headers"); got != "X-Node-Id" {
		t.Fatalf("expose-headers=%q", got)
	}
	if !strings.Contains(w.Result().Header.Get("Vary"), "Origin") {
		t.Fatalf("Vary missing Origin: %q", w.Result().Header.Get("Vary"))
	}
}

func TestCORSDisallowedOriginFallsThrough(t *testing.T) {
	r, _, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken: "test-token",
		CORS: domain.CORSConfig{
			AllowedOrigins: []string{"https://app.example.test"},
		},
	})
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/uploads/direct/token", nil)
	req.Header.Set("Origin", "https://evil.example.test")
	r.ServeHTTP(w, req)

	if w.Result().Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS header present for disallowed origin: %s", w.Result().Header.Get("Access-Control-Allow-Origin"))
	}
	if w.Result().StatusCode == http.StatusNoContent {
		t.Fatalf("disallowed preflight should not be handled as allowed CORS")
	}
}

func TestCORSActualRequestAllowedOrigin(t *testing.T) {
	r, _, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken: "test-token",
		CORS: domain.CORSConfig{
			AllowedOrigins:   []string{"*"},
			ExposedHeaders:   []string{"X-Node-Id"},
			AllowCredentials: false,
		},
	})
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://app.example.test")
	r.ServeHTTP(w, req)

	if got := w.Result().Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin=%q", got)
	}
	if got := w.Result().Header.Get("Access-Control-Expose-Headers"); got != "X-Node-Id" {
		t.Fatalf("expose-headers=%q", got)
	}
}
