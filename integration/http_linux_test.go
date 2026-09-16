//go:build linux

package integration_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	httpadapter "github.com/k2b-dev/filegate/v3/adapter/http"
	api "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
	sdk "github.com/k2b-dev/filegate/v3/sdk/filegate"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func server(t *testing.T) (*fixture, *httptest.Server, *sdk.Client) {
	x := setup(t, true, true)
	s := httptest.NewUnstartedServer(nil)
	s.Config.Handler = httpadapter.New([]*domain.Root{x.r}, httpadapter.Options{Token: strings.Repeat("t", 32), PublicURL: "http://" + s.Listener.Addr().String(), Origins: []string{"https://cloud.example"}})
	s.Start()
	t.Cleanup(s.Close)
	client, e := sdk.New(s.URL, strings.Repeat("t", 32))
	if e != nil {
		t.Fatal(e)
	}
	return x, s, client
}
func TestHTTPAndGoClientDirectOwnershipVersions(t *testing.T) {
	_, s, c := server(t)
	root := c.Root("test")
	uid, gid := os.Getuid(), os.Getgid()
	n, e := root.Put(ctx, "notes/a.txt", strings.NewReader("hello"), 5, domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0640", DirMode: "0750"}, Metadata: domain.Metadata{"message": "first"}})
	if e != nil || n.Mode != "0640" {
		t.Fatal(n, e)
	}
	if _, e = root.Put(ctx, "notes/a.txt", strings.NewReader("world"), 5, domain.WriteOptions{OnConflict: "overwrite", Metadata: domain.Metadata{"message": "second"}}); e != nil {
		t.Fatal(e)
	}
	vs, e := root.Versions(ctx, "notes/a.txt")
	if e != nil || len(vs) != 1 || vs[0].Metadata["message"] != "first" {
		t.Fatal(vs, e)
	}
	resp, e := http.Get(s.URL + "/v1/roots")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("missing auth accepted")
	}
	raw, e := root.ContentRaw(ctx, "missing")
	if e != nil {
		t.Fatal(e)
	}
	raw.Body.Close()
	if raw.StatusCode != 404 {
		t.Fatal("raw response changed")
	}
	d, e := root.DirectDownload(ctx, "notes/a.txt", 60)
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("GET", d.URL, nil)
	req.Header.Set("Range", "bytes=1-3")
	resp, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(b) != "orl" {
		t.Fatal(resp.Status, string(b))
	}
}
func TestDirectCapabilitiesBindSizeOwnershipAndMetadata(t *testing.T) {
	_, s, c := server(t)
	root := c.Root("test")
	u, e := root.DirectUpload(ctx, "a", 3, domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("PUT", u.URL+"?path=elsewhere&mode=0777", strings.NewReader("abc"))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatal(resp.Status)
	}
	n, e := root.Stat(ctx, "a")
	if e != nil || n.Mode != "0600" {
		t.Fatal(n, e)
	}
	u, e = root.DirectUpload(ctx, "short", 5, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = sdk.PutDirect(ctx, u.URL, strings.NewReader("abc"), 3); e == nil {
		t.Fatal("wrong size accepted")
	}
	u, e = root.DirectUpload(ctx, "tamper", 3, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	parts := strings.Split(u.URL, ".")
	_ = parts
	req, _ = http.NewRequest("PUT", u.URL+"x", strings.NewReader("abc"))
	resp, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("tampered token accepted", resp.Status)
	}
	body := `{"path":"large-meta","size":1,"metadata":{"message":"` + strings.Repeat("ä", 5000) + `"}}`
	req, _ = http.NewRequest("POST", s.URL+"/v1/roots/test/uploads/direct", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	resp, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatal("oversized metadata", resp.Status)
	}
}
func TestGoClientSessionRecoveryAndArchive(t *testing.T) {
	_, _, c := server(t)
	r := c.Root("test")
	created, e := r.CreateSession(ctx, "empty-dir/a", 5, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	session := sdk.DirectSession{URL: created.URL}
	if _, e = session.Put(ctx, 0, strings.NewReader("hello")); e != nil {
		t.Fatal(e)
	}
	n, e := session.Commit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	again, e := session.Commit(ctx)
	if e != nil || again.ID != n.ID {
		t.Fatal(again, e)
	}
	if _, e = r.Mkdir(ctx, "empty-dir/empty", nil); e != nil {
		t.Fatal(e)
	}
	resp, e := r.ArchiveRaw(ctx, "empty-dir")
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	tr := tar.NewReader(resp.Body)
	foundFile, foundDir := false, false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "a" {
			b, _ := io.ReadAll(tr)
			foundFile = string(b) == "hello"
		}
		if h.Name == "empty/" {
			foundDir = true
		}
	}
	if !foundFile || !foundDir {
		t.Fatal("archive incomplete", foundFile, foundDir)
	}
}
func TestHTTPRejectsUnknownFieldsAndKeepsRootBoundary(t *testing.T) {
	_, s, _ := server(t)
	for _, body := range []any{map[string]any{"path": "a", "size": 1, "legacy": true}, map[string]any{"path": "../outside", "size": 1}, map[string]any{"path": "a", "size": 1, "metadata": []string{"invalid"}}} {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", s.URL+"/v1/roots/test/uploads/direct", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(resp.Status, body)
		}
	}
	_ = api.Node{}
}
