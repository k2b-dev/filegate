//go:build linux

package integration_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/k2b-dev/filegate/v6/api/v1"
	"github.com/k2b-dev/filegate/v6/domain"
)

func executionRequest(t *testing.T, method, url, body string, backend bool, identity *domain.ExecutionIdentity) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if backend {
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	}
	if identity != nil {
		encoded, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Filegate-Execution", string(encoded))
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestExecutionDownloadLeaseBindsIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	x, s, _ := server(t)
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0777); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "download", "private bytes", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	actor := &domain.ExecutionIdentity{UID: executionUID, GID: executionGID, Groups: []uint32{}}
	resp := executionRequest(t, "POST", s.URL+"/v1/roots/test/downloads/direct", `{"path":"download"}`, true, actor)
	var lease api.DirectURL
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("mint: %s", resp.Status)
	}
	resp = executionRequest(t, "GET", lease.URL, "", false, nil)
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || string(b) != "private bytes" {
		t.Fatalf("download %s %q %v", resp.Status, b, err)
	}
	if err := os.Chmod(filepath.Join(x.data, "download"), 0600); err != nil {
		t.Fatal(err)
	}
	// A browser cannot upgrade the signed identity through a new header or query.
	resp = executionRequest(t, "GET", lease.URL+"?uid=0&gid=0", "", false, &domain.ExecutionIdentity{UID: 0, GID: 0})
	b, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 400 || bytes.Contains(b, []byte("private bytes")) {
		t.Fatalf("lease bypass %s %q %v", resp.Status, b, err)
	}
	resp = executionRequest(t, "GET", lease.URL, "", false, nil)
	b, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 403 || bytes.Contains(b, []byte("private bytes")) {
		t.Fatalf("current permission bypass %s %q %v", resp.Status, b, err)
	}
}

func TestExecutionArchiveLeaseDoesNotReturnUnreadableBytes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	x, s, _ := server(t)
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "source"), 0755); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "source/a", "allowed", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	put(t, x.r, "source/b", "forbidden secret", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	actor := &domain.ExecutionIdentity{UID: executionUID, GID: executionGID, Groups: []uint32{}}
	resp := executionRequest(t, "POST", s.URL+"/v1/downloads/archives", `{"items":[{"root":"test","path":"source","archivePath":"source"}]}`, true, actor)
	var lease api.ArchiveLease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("mint archive: %s", resp.Status)
	}
	requestArchive := func() (*http.Response, error) {
		request, err := http.NewRequest("POST", lease.URL, strings.NewReader(url.Values{"manifest": {lease.Manifest}}.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return http.DefaultClient.Do(request)
	}
	resp, err := requestArchive()
	if err != nil {
		t.Fatal(err)
	}
	complete, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("readable ZIP: %s %v", resp.Status, err)
	}
	if _, err := zip.NewReader(bytes.NewReader(complete), int64(len(complete))); err != nil {
		t.Fatalf("readable ZIP malformed: %v", err)
	}
	if err := os.Chmod(filepath.Join(x.data, "source/b"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, err = requestArchive()
	// A denied leaf may abort the HTTP stream after earlier entries. It must
	// never return a valid, silently incomplete ZIP or the unreadable content.
	if err != nil {
		return
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("forbidden secret")) {
		t.Fatal("archive leaked denied content")
	}
	if resp.StatusCode >= 400 {
		return
	}
	archive, zipErr := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if readErr == nil && zipErr == nil {
		names := make([]string, len(archive.File))
		for i, file := range archive.File {
			names[i] = file.Name
		}
		t.Fatalf("permission denial returned successful ZIP: %v", names)
	}
}

func TestExecutionUploadLeasePreservesOwnershipAndTargetRights(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	x, s, _ := server(t)
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "locked"), 0755); err != nil {
		t.Fatal(err)
	}
	actor := &domain.ExecutionIdentity{UID: executionUID, GID: executionGID, Groups: []uint32{}}
	for _, test := range []struct {
		path   string
		status int
	}{{"upload", 201}, {"locked/upload", 403}} {
		t.Run(test.path, func(t *testing.T) {
			body := fmt.Sprintf(`{"path":%q,"size":3,"ownership":{"uid":22001,"gid":22002,"mode":"0640"}}`, test.path)
			resp := executionRequest(t, "POST", s.URL+"/v1/roots/test/uploads/direct", body, true, actor)
			var lease api.DirectURL
			if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
				resp.Body.Close()
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Fatalf("mint upload: %s", resp.Status)
			}
			resp = executionRequest(t, "PUT", lease.URL, "abc", false, nil)
			response, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != test.status {
				t.Fatalf("upload %s %s %v", resp.Status, response, err)
			}
			if test.status == 201 {
				assertKernelRights(t, x, test.path, 0640, 22001, 22002)
			}
		})
	}
}
