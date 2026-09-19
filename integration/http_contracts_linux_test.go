//go:build linux

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	httpadapter "github.com/k2b-dev/filegate/v5/adapter/http"
	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
	sdk "github.com/k2b-dev/filegate/v5/sdk/filegate"
)

func TestHTTPManagedPublicationETagAndBoundConditions(t *testing.T) {
	x, server, client := server(t)
	x.r.Config.Managed = true
	initial := put(t, x.r, "file", "old", domain.WriteOptions{})
	if initial.Revision == "" {
		t.Fatal("managed publication omitted revision")
	}
	response := executionRequest(t, "GET", server.URL+"/v1/roots/test/stat?path=file", "", true, nil)
	response.Body.Close()
	if response.Header.Get("ETag") != strconv.Quote(initial.Revision) {
		t.Fatal(response.Header)
	}
	opts := domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: initial.Revision}}
	first, err := client.Root("test").DirectUpload(ctx, "file", 3, opts, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Root("test").DirectUpload(ctx, "file", 3, opts, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("PUT", first.URL, strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("If-Match", strconv.Quote(initial.Revision))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var updated domain.Node
	if err := json.NewDecoder(response.Body).Decode(&updated); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 || updated.Revision == initial.Revision || response.Header.Get("ETag") != strconv.Quote(updated.Revision) {
		t.Fatalf("publication %s %+v %v", response.Status, updated, response.Header)
	}
	response = executionRequest(t, "PUT", second.URL, "bad", false, nil)
	var failure api.Error
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 412 || failure.Error != "precondition_failed" || read(t, x.r, "file") != "new" {
		t.Fatalf("condition failure %s %+v", response.Status, failure)
	}
	request, err = http.NewRequest("PUT", second.URL, strings.NewReader("bad"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("If-Match", strconv.Quote(updated.Revision))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("browser replaced bound condition", response.Status)
	}
	response = executionRequest(t, "GET", server.URL+"/v1/roots/test/content?path=file", "", true, nil)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Header.Get("ETag") != strconv.Quote(updated.Revision) {
		t.Fatal("content revision differs", response.Header)
	}
}

func TestHTTPManagedSessionCommitETagAndPrecondition(t *testing.T) {
	x, server, client := server(t)
	x.r.Config.Managed = true
	original := put(t, x.r, "file", "old", domain.WriteOptions{})
	created, err := client.Root("test").CreateSession(ctx, "file", 3, domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: original.Revision}}, sdk.SessionCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Lease == nil {
		t.Fatal("no session lease")
	}
	leaseRequest(t, "PUT", created.Lease.URL+"?segment=0", "new", false, 200, nil)
	put(t, x.r, "file", "other", domain.WriteOptions{OnConflict: "overwrite"})
	endpoint := server.URL + "/v1/roots/test/uploads/sessions/" + created.Session.ID + "/commit"
	response := executionRequest(t, "POST", endpoint, "", true, nil)
	response.Body.Close()
	if response.StatusCode != 412 {
		t.Fatal(response.Status)
	}
	status, err := x.r.Session(created.Session.ID)
	if err != nil || status.State != domain.SessionOpen {
		t.Fatal(status, err)
	}
	if read(t, x.r, "file") != "other" {
		t.Fatal("failed commit mutated live file")
	}
	empty, err := client.Root("test").CreateSession(ctx, "created", 0, domain.WriteOptions{Precondition: &domain.Precondition{IfNoneMatch: true}}, sdk.SessionCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	response = executionRequest(t, "POST", server.URL+"/v1/roots/test/uploads/sessions/"+empty.Session.ID+"/commit", "", true, nil)
	var node domain.Node
	if err := json.NewDecoder(response.Body).Decode(&node); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || node.Revision == "" || response.Header.Get("ETag") != strconv.Quote(node.Revision) {
		t.Fatal(response.Status, node, response.Header)
	}
}

func TestHTTPSignedDownloadNamesRangeAndCORS(t *testing.T) {
	x, server, client := server(t)
	x.r.Config.Managed = true
	put(t, x.r, "file.txt", "abcdef", domain.WriteOptions{})
	version, err := x.r.Snapshot("file.txt", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := client.Root("test")
	for _, historical := range []bool{false, true} {
		var lease sdk.DirectURL
		options := sdk.DownloadOptions{FileName: "Grüße.txt"}
		if historical {
			lease, err = root.DirectVersionDownload(ctx, "file.txt", version.ID, options)
		} else {
			lease, err = root.DirectDownload(ctx, "file.txt", options)
		}
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("GET", lease.URL+"?fileName=evil.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Range", "bytes=1-2")
		request.Header.Set("Origin", "https://cloud.example")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 206 || string(b) != "bc" {
			t.Fatalf("range: %s %q %v", response.Status, b, err)
		}
		disposition := response.Header.Get("Content-Disposition")
		if !strings.Contains(disposition, "filename=\"Gr__e.txt\"") || !strings.Contains(disposition, "filename*=UTF-8''Gr%C3%BC%C3%9Fe.txt") || strings.Contains(disposition, "evil") {
			t.Fatal(disposition)
		}
		exposed := response.Header.Get("Access-Control-Expose-Headers")
		for _, name := range []string{"Content-Disposition", "ETag", "Retry-After"} {
			if !strings.Contains(exposed, name) {
				t.Fatal(exposed)
			}
		}
	}
	for _, name := range []string{"../escape", "bad\r\nheader", "a/b", "a\\b"} {
		if _, err := root.DirectDownload(ctx, "file.txt", sdk.DownloadOptions{FileName: name}); err == nil {
			t.Fatal("unsafe name accepted", name)
		}
	}
	response := executionRequest(t, "GET", server.URL+"/v1/roots/test/content?path=file.txt&fileName="+url.QueryEscape("download.txt"), "", true, nil)
	response.Body.Close()
	if !strings.Contains(response.Header.Get("Content-Disposition"), "download.txt") {
		t.Fatal(response.Header)
	}
}

func TestHTTPListingOptionsCursorErrorsAndSubtreeStats(t *testing.T) {
	x, server, client := server(t)
	put(t, x.r, "folder/small", "a", domain.WriteOptions{})
	put(t, x.r, "folder/large", "abcd", domain.WriteOptions{})
	root := client.Root("test")
	page, err := root.List(ctx, "folder", sdk.ListingOptions{Sort: "size", Order: "desc", Type: "files", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "folder/large" || page.Next == "" {
		t.Fatal(page, err)
	}
	response := executionRequest(t, "GET", server.URL+"/v1/roots/test/entries?path=folder&sort=size&type=files&limit=1&after="+url.QueryEscape(page.Next), "", true, nil)
	var failure api.Error
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 409 || failure.Error != "cursor_invalid" {
		t.Fatal(response.Status, failure)
	}
	stats, err := root.RecursiveStats(ctx, "folder", 1)
	if err != nil || stats.Complete || stats.Path != "folder" || stats.Source != "filesystem" || stats.Freshness != "observed" {
		t.Fatal(stats, err)
	}
	stats, err = root.RecursiveStats(ctx, "folder", 10)
	if err != nil || !stats.Complete || stats.Files != 2 || stats.Bytes != 5 {
		t.Fatal(stats, err)
	}
}

func TestHTTPTransferResultsAndDestinationRecoveryRoutes(t *testing.T) {
	source, destination := transferRoots(t)
	put(t, source.r, "original", "bytes", domain.WriteOptions{})
	server := httptest.NewServer(httpadapter.New([]*domain.Root{source.r, destination.r}, httpadapter.Options{Token: strings.Repeat("t", 32)}))
	defer server.Close()
	source.r.Files = &transferDeniedRemove{Files: source.files, path: "original"}
	id := uuid.NewString()
	body := `{"path":"original","targetRoot":"destination","targetPath":"copy","move":true,"id":"` + id + `"}`
	var pending domain.TransferResult
	leaseRequest(t, "POST", server.URL+"/v1/roots/source/transfers", body, true, 202, &pending)
	if pending.ID != id || pending.State != domain.TransferSourcePending || pending.Node == nil || pending.Node.Path != "copy" {
		t.Fatal(pending)
	}
	endpoint := server.URL + "/v1/roots/destination/transfers/" + id
	var status domain.TransferResult
	leaseRequest(t, "GET", endpoint, "", true, 202, &status)
	if status.State != domain.TransferSourcePending || status.SourceRoot != "source" {
		t.Fatal(status)
	}
	source.r.Files = source.files
	leaseRequest(t, "POST", endpoint+"/resume", "", true, 200, &status)
	if status.State != domain.TransferCompleted {
		t.Fatal(status)
	}
	leaseRequest(t, "POST", server.URL+"/v1/roots/source/transfers", body, true, 200, &status)
	if status.State != domain.TransferCompleted {
		t.Fatal(status)
	}
	// Ordinary duplication returns the actual renamed node in the same result envelope.
	leaseRequest(t, "POST", server.URL+"/v1/roots/destination/transfers", `{"path":"copy","targetRoot":"destination","targetPath":"copy","onConflict":"rename"}`, true, 200, &status)
	if status.State != domain.TransferCompleted || status.Node == nil || status.Node.Path == "copy" {
		t.Fatal(status)
	}
}

func TestHTTPHistoricalCopyReturnsNewTarget(t *testing.T) {
	x, _, client := server(t)
	original := put(t, x.r, "original", "old", domain.WriteOptions{})
	version, err := x.r.Snapshot("original", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "original", "current", domain.WriteOptions{OnConflict: "overwrite"})
	result, err := client.Root("test").CopyVersion(ctx, version.ID, sdk.VersionCopyRequest{Path: "original", TargetRoot: "test", TargetPath: "copied"})
	if err != nil || result.Path != "copied" || result.ID == original.ID {
		t.Fatal(result, err)
	}
	if read(t, x.r, "original") != "current" || read(t, x.r, "copied") != "old" {
		t.Fatal("historical copy changed original or copied current content")
	}
	if _, err := client.Root("test").CopyVersion(ctx, version.ID, sdk.VersionCopyRequest{Path: "original", TargetRoot: "test", TargetPath: "copied"}); err == nil {
		t.Fatal("default conflict policy overwrote existing target")
	}
}
