//go:build linux

package httpadapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

func TestRuntimeUploadConfigReachesManagersAndOperationalEndpoints(t *testing.T) {
	initial := domain.Config{
		Server: domain.ServerConfig{PublicURL: "https://old.example.test"},
		Upload: domain.UploadConfig{
			MaxChunkBytes:         1024,
			MaxUploadBytes:        2048,
			MaxSessionUploadBytes: 4096,
			MinFreeBytes:          0,
		},
	}
	holder := domain.NewConfigHolder(initial)
	router, svc, cleanup := newTestRouterWithCustomLimits(t, t.TempDir(), t.TempDir(), RouterOptions{
		BearerToken:                "test-token",
		Config:                     holder,
		PublicURL:                  initial.Server.PublicURL,
		JobWorkers:                 2,
		JobQueueSize:               64,
		UploadExpiry:               time.Hour,
		UploadCleanupInterval:      time.Hour,
		MaxChunkBytes:              initial.Upload.MaxChunkBytes,
		MaxUploadBytes:             initial.Upload.MaxUploadBytes,
		MaxSessionUploadBytes:      initial.Upload.MaxSessionUploadBytes,
		MaxConcurrentSegmentWrites: 4,
	})
	defer cleanup()

	updated := initial
	updated.Server.PublicURL = "https://new.example.test/"
	updated.Upload.MaxChunkBytes = 2048
	updated.Upload.MaxUploadBytes = 8192
	updated.Upload.MaxSessionUploadBytes = 16384
	updated.Upload.MinFreeBytes = 1
	holder.Set(updated)

	capabilities := httptest.NewRecorder()
	router.ServeHTTP(capabilities, authedRequest(http.MethodGet, "/v1/capabilities"))
	if capabilities.Code != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", capabilities.Code, capabilities.Body.String())
	}
	var caps apiv1.CapabilitiesResponse
	if err := json.NewDecoder(capabilities.Body).Decode(&caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if caps.Uploads.MaxChunkBytes != 2048 || caps.Uploads.MaxUploadBytes != 8192 || caps.Uploads.MaxSessionUploadBytes != 16384 {
		t.Fatalf("capabilities still expose startup limits: %#v", caps.Uploads)
	}

	infoResponse := httptest.NewRecorder()
	router.ServeHTTP(infoResponse, authedRequest(http.MethodGet, "/v1/system/info"))
	if infoResponse.Code != http.StatusOK {
		t.Fatalf("system info status=%d body=%s", infoResponse.Code, infoResponse.Body.String())
	}
	var info apiv1.SystemInfoResponse
	if err := json.NewDecoder(infoResponse.Body).Decode(&info); err != nil {
		t.Fatalf("decode system info: %v", err)
	}
	if info.Limits.UploadMinFreeBytes != 1 {
		t.Fatalf("upload min free bytes=%d, want 1", info.Limits.UploadMinFreeBytes)
	}

	root := svc.ListRoot()[0]
	create := httptest.NewRecorder()
	body := []byte(`{"path":"` + root.Name + `/runtime.bin","maxBytes":4096,"expiresInSeconds":60}`)
	router.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/uploads/direct", body))
	if create.Code != http.StatusCreated {
		t.Fatalf("direct upload status=%d body=%s", create.Code, create.Body.String())
	}
	var direct apiv1.DirectUploadURLResponse
	if err := json.NewDecoder(create.Body).Decode(&direct); err != nil {
		t.Fatalf("decode direct upload: %v", err)
	}
	if !strings.HasPrefix(direct.UploadURL, "https://new.example.test/v1/uploads/direct/") {
		t.Fatalf("direct upload URL=%q", direct.UploadURL)
	}
}
