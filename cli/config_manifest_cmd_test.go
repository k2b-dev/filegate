package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

func TestReadConfigManifestFlattensVersionedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filegate.manifest.yaml")
	if err := os.WriteFile(path, []byte(`
version: 1
config:
  server:
    access_log_enabled: true
  upload:
    expiry: 2h
    max_upload_bytes: 4096
`), 0o600); err != nil {
		t.Fatal(err)
	}

	values, err := readConfigManifest(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if values["server.access_log_enabled"] != true || values["upload.expiry"] != "2h" || values["upload.max_upload_bytes"] != 4096 {
		t.Errorf("values = %#v", values)
	}
}

func TestReadConfigManifestRejectsUnknownEnvelopeAndWrongVersion(t *testing.T) {
	for name, body := range map[string]string{
		"unknown": "version: 1\nconfig: {}\nchanges: {}\n",
		"version": "version: 2\nconfig: {}\n",
		"missing": "version: 1\n",
		"dotted":  "version: 1\nconfig:\n  upload.expiry: 2h\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readConfigManifest(path); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestConfigPlanAndApplyUseAuthenticatedHTTPAndRevision(t *testing.T) {
	var mu sync.Mutex
	var planCalls, applyCalls int
	var applied apiv1.ConfigManifestApplyRequest
	var actor string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer remote-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		actor = r.Header.Get("X-Filegate-Actor")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/config/plan":
			mu.Lock()
			planCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(apiv1.ConfigManifestPlanResponse{
				CurrentRevision:  "old-revision",
				ProposedRevision: "new-revision",
				Changes: []apiv1.ConfigManifestChange{{
					Path: "upload.max_upload_bytes", Operation: "add", Activation: "runtime", To: float64(4096),
				}},
			})
		case "/v1/config/apply":
			mu.Lock()
			applyCalls++
			mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&applied); err != nil {
				t.Errorf("decode apply: %v", err)
			}
			_ = json.NewEncoder(w).Encode(apiv1.ConfigManifestApplyResponse{
				Manifest: apiv1.ConfigManifestStatus{Revision: "new-revision", AppliedBy: actor},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("version: 1\nconfig:\n  upload:\n    max_upload_bytes: 4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("remote-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeCLI("config", "apply", "-f", manifest, "--host", server.URL, "--token-file", tokenFile, "--actor", "alice")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if planCalls != 1 || applyCalls != 1 {
		t.Fatalf("calls: plan=%d apply=%d", planCalls, applyCalls)
	}
	if applied.ExpectedRevision != "old-revision" || applied.Values["upload.max_upload_bytes"] != float64(4096) {
		t.Errorf("apply request = %+v", applied)
	}
	if actor != "alice" || !strings.Contains(out, "Applied manifest") {
		t.Errorf("actor=%q output=%q", actor, out)
	}
}

func TestConfigPlanSupportsRemoteEnvironmentCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer env-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(apiv1.ConfigManifestPlanResponse{ProposedRevision: "new"})
	}))
	defer server.Close()
	t.Setenv("FILEGATE_HOST", server.URL)
	t.Setenv("FILEGATE_TOKEN", "env-token")

	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("version: 1\nconfig: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := executeCLI("config", "plan", "-f", manifest); err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
}
