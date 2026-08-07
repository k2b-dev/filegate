//go:build linux

package httpadapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

func TestDirectDownloadURLReadsWithoutBearerAndSupportsRange(t *testing.T) {
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
	body := []byte("hello direct download")
	meta, _, err := svc.WriteContentByVirtualPath(root.Name+"/direct.txt", bytes.NewReader(body), domain.ConflictError)
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}

	createBody, _ := json.Marshal(apiv1.DirectDownloadURLRequest{
		Path:             root.Name + "/direct.txt",
		ExpiresInSeconds: 60,
	})
	create := httptest.NewRecorder()
	r.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/downloads/direct", createBody))
	if create.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Result().StatusCode, create.Body.String())
	}
	var created apiv1.DirectDownloadURLResponse
	if err := json.NewDecoder(create.Result().Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(created.DownloadURL, "https://files.example.test/v1/downloads/direct/") {
		t.Fatalf("downloadUrl=%q does not use public URL", created.DownloadURL)
	}
	if created.Method != http.MethodGet || created.Node.ID != meta.ID.String() {
		t.Fatalf("unexpected direct download response: %#v", created)
	}
	if tokenSHA := directDownloadTokenSHA256(t, created.DownloadURL); tokenSHA != meta.SHA256 {
		t.Fatalf("token SHA256=%q, want %q", tokenSHA, meta.SHA256)
	}

	get := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, created.DownloadURL, nil)
	req.Header.Set("Range", "bytes=0-4")
	r.ServeHTTP(get, req)
	if get.Result().StatusCode != http.StatusPartialContent {
		t.Fatalf("range status=%d body=%s", get.Result().StatusCode, get.Body.String())
	}
	if get.Body.String() != "hello" {
		t.Fatalf("range body=%q", get.Body.String())
	}
	if got := get.Result().Header.Get("Content-Range"); got != "bytes 0-4/21" {
		t.Fatalf("Content-Range=%q", got)
	}

	head := httptest.NewRecorder()
	r.ServeHTTP(head, httptest.NewRequest(http.MethodHead, created.DownloadURL, nil))
	if head.Result().StatusCode != http.StatusOK {
		t.Fatalf("head status=%d", head.Result().StatusCode)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD wrote body")
	}
}

func TestDirectDownloadURLRejectsChangedFile(t *testing.T) {
	r, svc, cleanup := newTestRouter(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(root.Name+"/stale.txt", strings.NewReader("old"), domain.ConflictError)
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	createBody, _ := json.Marshal(apiv1.DirectDownloadURLRequest{NodeID: meta.ID.String()})
	create := httptest.NewRecorder()
	r.ServeHTTP(create, authedJSONRequest(http.MethodPost, "/v1/downloads/direct", createBody))
	if create.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Result().StatusCode, create.Body.String())
	}
	var created apiv1.DirectDownloadURLResponse
	if err := json.NewDecoder(create.Result().Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if err := svc.WriteContent(meta.ID, strings.NewReader("new bytes")); err != nil {
		t.Fatalf("replace file: %v", err)
	}

	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, created.DownloadURL, nil))
	if get.Result().StatusCode != http.StatusConflict {
		t.Fatalf("status=%d want=%d body=%s", get.Result().StatusCode, http.StatusConflict, get.Body.String())
	}
}

func directDownloadTokenSHA256(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse download URL: %v", err)
	}
	token := strings.TrimPrefix(parsed.Path, "/v1/downloads/direct/")
	payload, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("download URL token missing signature: %q", token)
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode token payload: %v", err)
	}
	var out directDownloadToken
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal token payload: %v", err)
	}
	return out.SHA256
}
