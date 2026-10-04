//go:build linux

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	httpadapter "github.com/k2b-dev/filegate/v7/adapter/http"
	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
	sdk "github.com/k2b-dev/filegate/v7/sdk/filegate"
)

func stableIDServer(t *testing.T, roots ...*domain.Root) (*httptest.Server, *sdk.Client) {
	t.Helper()
	s := httptest.NewUnstartedServer(nil)
	s.Config.Handler = httpadapter.New(roots, httpadapter.Options{Token: strings.Repeat("t", 32), PublicURL: "http://" + s.Listener.Addr().String()})
	s.Start()
	t.Cleanup(s.Close)
	client, err := sdk.New(s.URL, strings.Repeat("t", 32))
	if err != nil {
		t.Fatal(err)
	}
	return s, client
}

func TestHTTPStableIDsProjectPublicResultsWithoutChangingHistory(t *testing.T) {
	for _, index := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%v/managed=%v", index, managed), func(t *testing.T) {
				x := setup(t, index, index)
				x.r.Config.Managed = managed
				s, client := stableIDServer(t, x.r)
				root := client.Root("test")
				stable := index && managed
				info, err := root.Info(ctx)
				if err != nil || info.StableIDs != stable {
					t.Fatalf("capability: %+v %v", info, err)
				}
				n, err := root.Put(ctx, "file", strings.NewReader("old"), 3, domain.WriteOptions{})
				if err != nil || (n.ID != "") != stable {
					t.Fatalf("upload identity: %+v %v", n, err)
				}
				internal, err := x.r.Stat("file")
				if err != nil || (internal.ID != "") != index {
					t.Fatalf("public projection modified internal identity: %+v %v", internal, err)
				}
				id := internal.ID
				if id == "" {
					id = uuid.Must(uuid.NewV7()).String()
				}
				status := http.StatusConflict
				if stable {
					status = http.StatusOK
				}
				leaseRequest(t, "GET", s.URL+"/v1/roots/test/resolve?id="+id, "", true, status, nil)
				stat, err := root.Stat(ctx, "file")
				if err != nil || stat.ID != n.ID {
					t.Fatalf("stat identity: %+v %v", stat, err)
				}
				page, err := root.List(ctx, ".", sdk.ListingOptions{})
				if err != nil || len(page.Items) != 1 || page.Items[0].ID != n.ID {
					t.Fatalf("list identity: %+v %v", page, err)
				}
				page, err = root.Search(ctx, "file", ".", sdk.ListingOptions{})
				if err != nil || len(page.Items) != 1 || page.Items[0].ID != n.ID {
					t.Fatalf("search identity: %+v %v", page, err)
				}
				directory, err := root.Mkdir(ctx, "directory", sdk.DirectoryOptions{})
				if err != nil || !directory.Directory || (directory.ID != "") != stable {
					t.Fatalf("directory identity: %+v %v", directory, err)
				}
				created, err := root.CreateSession(ctx, "empty", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "empty"})
				if err != nil {
					t.Fatal(err)
				}
				committed, err := root.CommitSession(ctx, created.Session.ID)
				if err != nil || (committed.ID != "") != stable {
					t.Fatalf("commit identity: %+v %v", committed, err)
				}
				receipt, err := root.Session(ctx, created.Session.ID)
				if err != nil || receipt.Result == nil || receipt.Result.ID != committed.ID {
					t.Fatalf("receipt identity: %+v %v", receipt, err)
				}
				replayed, err := root.CreateSession(ctx, "empty", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "empty"})
				if err != nil || replayed.Session.Result == nil || replayed.Session.Result.ID != committed.ID {
					t.Fatalf("replayed receipt identity: %+v %v", replayed, err)
				}
				internalReceipt, err := x.r.Session(created.Session.ID)
				if err != nil || internalReceipt.Result == nil || (internalReceipt.Result.ID != "") != index {
					t.Fatalf("public receipt modified persisted identity: %+v %v", internalReceipt, err)
				}
				if !index {
					return
				}
				version, err := root.Snapshot(ctx, "file", api.VersionRequest{})
				if err != nil || (version.FileID != "") != stable {
					t.Fatalf("snapshot identity: %+v %v", version, err)
				}
				put(t, x.r, "file", "new", domain.WriteOptions{OnConflict: "overwrite"})
				versions, err := root.Versions(ctx, "file")
				if err != nil || len(versions) != 1 || versions[0].FileID != version.FileID {
					t.Fatalf("version projection: %+v %v", versions, err)
				}
				internalVersions, err := x.r.Versions("file")
				if err != nil || len(internalVersions) != 1 || internalVersions[0].FileID != internal.ID {
					t.Fatalf("public versions modified history: %+v %v", internalVersions, err)
				}
				restored, err := root.Restore(ctx, "file", version.ID)
				if err != nil || restored.ID != n.ID || read(t, x.r, "file") != "old" {
					t.Fatalf("restore after public projection: %+v %v", restored, err)
				}
			})
		}
	}
}

func TestHTTPStableIDContentAndLeasesUseExactIdentity(t *testing.T) {
	x := setup(t, true, false)
	x.r.Config.Managed = true
	s, client := stableIDServer(t, x.r)
	root := client.Root("test")
	original := put(t, x.r, "original.txt", "original", domain.WriteOptions{})
	response, err := root.ContentByIDRaw(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(body) != "original" || response.Header.Get("ETag") == "" {
		t.Fatalf("content by ID: %s %q %v", response.Status, body, err)
	}
	lease, err := root.DirectDownloadByID(ctx, original.ID, sdk.DownloadOptions{FileName: "chosen.txt"})
	if err != nil {
		t.Fatal(err)
	}
	response, body = downloadResponse(t, "GET", lease.URL+"?fileId="+uuid.NewString()+"&path=other", "bytes=1-3")
	if response.StatusCode != 206 || string(body) != "rig" || !strings.Contains(response.Header.Get("Content-Disposition"), "chosen.txt") {
		t.Fatalf("signed ID/range/name: %s %q %v", response.Status, body, response.Header)
	}
	response, body = downloadResponse(t, "HEAD", lease.URL, "")
	if response.StatusCode != 200 || len(body) != 0 || response.Header.Get("Content-Length") != "8" {
		t.Fatalf("ID HEAD: %s %q %v", response.Status, body, response.Header)
	}
	parsed, err := url.Parse(lease.URL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/v1/direct/"), ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var signed map[string]any
	if err = json.Unmarshal(payload, &signed); err != nil || signed["fileId"] != original.ID || signed["path"] != original.Path {
		t.Fatalf("unsigned identity: %s %v", payload, err)
	}
	signed["fileId"] = uuid.NewString()
	payload, err = json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	tampered := s.URL + "/v1/direct/" + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
	leaseRequest(t, "GET", tampered, "", false, 401, nil)
	put(t, x.r, "original.txt", "updated", domain.WriteOptions{OnConflict: "overwrite"})
	response, body = downloadResponse(t, "GET", lease.URL, "")
	if response.StatusCode != 200 || string(body) != "updated" {
		t.Fatalf("same-identity overwrite: %s %q", response.Status, body)
	}
	if _, err = x.r.Move("original.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", lease.URL, "", false, 404, nil)
	put(t, x.r, "original.txt", "replacement", domain.WriteOptions{})
	leaseRequest(t, "GET", lease.URL, "", false, 404, nil)
	newLease, err := root.DirectDownloadByID(ctx, original.ID, sdk.DownloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	response, body = downloadResponse(t, "GET", newLease.URL, "")
	if response.StatusCode != 200 || string(body) != "updated" || !strings.Contains(response.Header.Get("Content-Disposition"), "renamed.txt") {
		t.Fatalf("reissued moved ID: %s %q %v", response.Status, body, response.Header)
	}
	if err = x.r.Remove("renamed.txt", false); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "renamed.txt", "new identity", domain.WriteOptions{})
	leaseRequest(t, "GET", newLease.URL, "", false, 404, nil)
	response, err = root.ContentByIDRaw(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("removed ID response changed: %s", response.Status)
	}
}

func TestHTTPStableIDSelectorsAndErrors(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	s, _ := stableIDServer(t, x.r)
	n := put(t, x.r, "file", "data", domain.WriteOptions{})
	base := s.URL + "/v1/roots/test"
	for _, query := range []string{"", "?fileId=", "?fileId=invalid", "?path=file&fileId=" + n.ID, "?path=&fileId=" + n.ID, "?fileId=" + n.ID + "&fileId=" + n.ID, "?path=file&path=file"} {
		var result api.Error
		leaseRequest(t, "GET", base+"/content"+query, "", true, 400, &result)
		if result.Error != "invalid_argument" {
			t.Fatal(result)
		}
	}
	for _, body := range []string{`{}`, `{"fileId":""}`, `{"fileId":"invalid"}`, `{"path":"file","fileId":"` + n.ID + `"}`, `{"fileId":"` + n.ID + `","unknown":1}`} {
		leaseRequest(t, "POST", base+"/downloads/direct", body, true, 400, nil)
	}
	leaseRequest(t, "GET", base+"/resolve?id=invalid", "", true, 400, nil)
	leaseRequest(t, "GET", base+"/content?fileId="+uuid.NewString(), "", true, 404, nil)
	leaseRequest(t, "GET", base+"/content?fileId="+n.ID, "", false, 401, nil)
	directory, err := x.r.Mkdir("directory", domain.DirectoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", base+"/resolve?id="+directory.ID, "", true, 200, nil)
	leaseRequest(t, "GET", base+"/content?fileId="+directory.ID, "", true, 400, nil)
	version, err := x.r.Snapshot("file", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "POST", base+"/versions/"+version.ID+"/downloads/direct", `{"path":"file","fileId":"`+n.ID+`"}`, true, 400, nil)
	plain := setup(t, true, false)
	plain.r.Config.Managed = false
	plainServer, _ := stableIDServer(t, plain.r)
	for _, endpoint := range []string{"/content?fileId=" + n.ID, "/resolve?id=" + n.ID} {
		var result api.Error
		leaseRequest(t, "GET", plainServer.URL+"/v1/roots/test"+endpoint, "", true, 409, &result)
		if result.Error != "feature_disabled" {
			t.Fatal(result)
		}
	}
	leaseRequest(t, "POST", plainServer.URL+"/v1/roots/test/downloads/direct", `{"fileId":"`+n.ID+`"}`, true, 409, nil)
}

func TestHTTPStableIDTransfersUseDestinationCapability(t *testing.T) {
	for _, sourceManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("sourceManaged=%v", sourceManaged), func(t *testing.T) {
			source := setup(t, true, true)
			source.r.Config.Managed = sourceManaged
			destination := setup(t, true, false)
			destination.r.Config.Name = "destination"
			destination.r.Config.Managed = !sourceManaged
			_, client := stableIDServer(t, source.r, destination.r)
			put(t, source.r, "source", "content", domain.WriteOptions{})
			result, err := client.Root("test").Transfer(ctx, sdk.TransferRequest{Path: "source", TargetRoot: "destination", TargetPath: "copied"})
			if err != nil || result.Node == nil || (result.Node.ID != "") != destination.r.StableIDs() {
				t.Fatalf("transfer used source identity policy: %+v %v", result, err)
			}
			internal, err := destination.r.Stat("copied")
			if err != nil || internal.ID == "" {
				t.Fatalf("transfer projection changed destination identity: %+v %v", internal, err)
			}
			version, err := source.r.Snapshot("source", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			copied, err := client.Root("test").CopyVersion(ctx, version.ID, sdk.VersionCopyRequest{Path: "source", TargetRoot: "destination", TargetPath: "historical"})
			if err != nil || (copied.ID != "") != destination.r.StableIDs() {
				t.Fatalf("historical copy used source identity policy: %+v %v", copied, err)
			}
		})
	}
}

func TestHTTPStableIDReadsAndLeasesEnforceExecutionRights(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	x := setup(t, true, false)
	x.r.Config.Managed = true
	x.r.Config.Execution = true
	if err := os.Chmod(x.data, 0777); err != nil {
		t.Fatal(err)
	}
	n := put(t, x.r, "file", "private", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	s, _ := stableIDServer(t, x.r)
	actor := &domain.ExecutionIdentity{UID: 32051, GID: 32051}
	base := s.URL + "/v1/roots/test"
	response := executionRequest(t, "GET", base+"/content?fileId="+n.ID, "", true, actor)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("ID read elevated Unix execution: %s", response.Status)
	}
	if err := os.Chmod(filepath.Join(x.data, "file"), 0644); err != nil {
		t.Fatal(err)
	}
	response = executionRequest(t, "POST", base+"/downloads/direct", `{"fileId":"`+n.ID+`"}`, true, actor)
	var lease api.DirectURL
	err := json.NewDecoder(response.Body).Decode(&lease)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 {
		t.Fatalf("scoped ID lease: %s %+v %v", response.Status, lease, err)
	}
	leaseRequest(t, "GET", lease.URL, "", false, 200, nil)
	if err := os.Chmod(filepath.Join(x.data, "file"), 0600); err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", lease.URL, "", false, 403, nil)
}

func TestHTTPStableIDReadsDuringMovesAndPathReuse(t *testing.T) {
	x := setup(t, true, false)
	x.r.Config.Managed = true
	_, client := stableIDServer(t, x.r)
	original := put(t, x.r, "current", "object content", domain.WriteOptions{})
	start := make(chan struct{})
	reads, moves := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		for range 20 {
			response, err := client.Root("test").ContentByIDRaw(context.Background(), original.ID)
			if err != nil {
				reads <- err
				return
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != "object content" {
				reads <- fmt.Errorf("ID read during move: %s %q %v", response.Status, body, err)
				return
			}
		}
		reads <- nil
	}()
	go func() {
		<-start
		for range 20 {
			if _, err := x.r.Move("current", "moved"); err != nil {
				moves <- err
				return
			}
			if _, err := x.r.Put(ctx, "current", strings.NewReader("foreign content"), domain.WriteOptions{}); err != nil {
				moves <- err
				return
			}
			if err := x.r.Remove("current", false); err != nil {
				moves <- err
				return
			}
			if _, err := x.r.Move("moved", "current"); err != nil {
				moves <- err
				return
			}
		}
		moves <- nil
	}()
	close(start)
	readErr, moveErr := <-reads, <-moves
	if readErr != nil || moveErr != nil {
		t.Fatalf("concurrent ID read/move: %v / %v", readErr, moveErr)
	}
}
