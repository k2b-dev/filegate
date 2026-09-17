package httpadapter

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
)

func TestACLErrorsAreDistinctAndDoNotExposeHostPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid", domain.ErrInvalidACL, 400, "invalid_acl"},
		{"unsupported", domain.ErrACLUnsupported, 501, "acl_not_supported"},
		{"permission", os.ErrPermission, 403, "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			fail(w, fmt.Errorf("/private/host/data: %w", tc.err))
			var body api.Error
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Error != tc.code || body.Message == "" || strings.Contains(body.Message, "/private/host") {
				t.Fatalf("unexpected error: status=%d body=%+v", w.Code, body)
			}
		})
	}
}

func TestACLHTTPValidationBeforeFilesystemAccess(t *testing.T) {
	// Invalid requests must be rejected before consulting any filesystem port.
	h := New([]*domain.Root{{Config: domain.RootConfig{Name: "freeipa"}}}, Options{Token: "secret"})
	for _, tc := range []struct {
		name, method, scope, body string
	}{
		{"missing read scope", "GET", "", ""},
		{"unknown read scope", "GET", "recursive", ""},
		{"access delete", "DELETE", "access", ""},
		{"missing delete scope", "DELETE", "", ""},
		{"unknown field", "PUT", "default", `{"entries":[],"recursive":true}`},
		{"negative ID", "PUT", "default", `{"entries":[{"tag":"user","id":-1,"permissions":"rwx"}]}`},
		{"overflow ID", "PUT", "default", `{"entries":[{"tag":"user","id":4294967296,"permissions":"rwx"}]}`},
		{"fractional ID", "PUT", "default", `{"entries":[{"tag":"user","id":1.5,"permissions":"rwx"}]}`},
		{"multiple JSON values", "PUT", "default", `{"entries":[]} {"entries":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/roots/freeipa/acl?path=groups&scope="+tc.scope, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var body api.Error
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 400 || body.Error != "invalid_acl" {
				t.Fatalf("status %d, body %+v", w.Code, body)
			}
		})
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		t.Run("unauthenticated "+method, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, "/v1/roots/freeipa/acl?path=groups&scope=default", nil))
			if w.Code != 401 {
				t.Fatalf("unauthenticated ACL request returned %d", w.Code)
			}
		})
	}
}

func TestACLHTTPRequiresExplicitPath(t *testing.T) {
	// Without a target, these requests must never read or modify the root ACL.
	h := New([]*domain.Root{{Config: domain.RootConfig{Name: "freeipa"}}}, Options{Token: "secret"})
	const acl = `{"entries":[{"tag":"owner","permissions":"rwx"},{"tag":"owningGroup","permissions":"rwx"},{"tag":"other","permissions":"---"}]}`
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		for _, query := range []string{"scope=default", "scope=default&path="} {
			t.Run(method+" "+query, func(t *testing.T) {
				req := httptest.NewRequest(method, "/v1/roots/freeipa/acl?"+query, strings.NewReader(acl))
				req.Header.Set("Authorization", "Bearer secret")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				var body api.Error
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if w.Code != 400 || body.Error != "invalid_argument" {
					t.Fatalf("status %d, body %+v", w.Code, body)
				}
			})
		}
	}
}
