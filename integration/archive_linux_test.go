//go:build linux

package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	httpadapter "github.com/k2b-dev/filegate/v5/adapter/http"
	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
)

func archiveServer(t *testing.T, roots ...*domain.Root) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(nil)
	s.Config.Handler = httpadapter.New(roots, httpadapter.Options{Token: strings.Repeat("t", 32), PublicURL: "http://" + s.Listener.Addr().String()})
	s.Start()
	t.Cleanup(s.Close)
	return s
}
func archiveMint(t *testing.T, s *httptest.Server, items []api.ArchiveItem) api.ArchiveLease {
	t.Helper()
	b, _ := json.Marshal(api.ArchiveRequest{Items: items})
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/downloads/archives", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	var lease api.ArchiveLease
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint: %s %s", resp.Status, b)
	}
	if e = json.NewDecoder(resp.Body).Decode(&lease); e != nil {
		t.Fatal(e)
	}
	return lease
}
func archivePost(t *testing.T, lease api.ArchiveLease, manifest string) *http.Response {
	t.Helper()
	resp, e := http.PostForm(lease.URL, url.Values{"manifest": {manifest}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestArchiveMultiRootZIPAndBoundManifest(t *testing.T) {
	a, b := setup(t, false, false), setup(t, false, false)
	b.r.Config.Name = "second"
	put(t, a.r, "folder/a.txt", "alpha", domain.WriteOptions{})
	put(t, b.r, "b.txt", "beta", domain.WriteOptions{})
	s := archiveServer(t, a.r, b.r)
	lease := archiveMint(t, s, []api.ArchiveItem{{Root: "test", Path: "folder", ArchivePath: "first"}, {Root: "second", Path: "b.txt", ArchivePath: "second.txt"}})
	resp := archivePost(t, lease, lease.Manifest)
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	body, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if e != nil {
		t.Fatal(e)
	}
	got := map[string]string{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		got[f.Name] = string(b)
	}
	if len(got) != 3 || got["first/a.txt"] != "alpha" || got["second.txt"] != "beta" {
		t.Fatal(got)
	}
	resp = archivePost(t, lease, strings.Replace(lease.Manifest, "b.txt", "secret.txt", 1))
	if resp.StatusCode != 403 {
		t.Fatal("manifest tampering accepted", resp.Status)
	}
	resp = archivePost(t, lease, lease.Manifest+" ")
	if resp.StatusCode != 403 {
		t.Fatal("manifest hash is not exact", resp.Status)
	}
}

func TestArchiveSelectedFileCannotExpandToDirectory(t *testing.T) {
	x := setup(t, false, false)
	put(t, x.r, "item", "a", domain.WriteOptions{})
	s := archiveServer(t, x.r)
	lease := archiveMint(t, s, []api.ArchiveItem{{Root: "test", Path: "item", ArchivePath: "item"}})
	if e := os.Remove(filepath.Join(x.data, "item")); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(x.data, "item"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(x.data, "item", "secret"), []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	resp := archivePost(t, lease, lease.Manifest)
	if resp.StatusCode != 409 {
		t.Fatal("file selection widened", resp.Status)
	}
}

func TestArchivePrivatePathsSymlinksCollisionsAndSize(t *testing.T) {
	x := setup(t, false, false)
	put(t, x.r, "a", "a", domain.WriteOptions{})
	s := archiveServer(t, x.r)
	lease := archiveMint(t, s, []api.ArchiveItem{{Root: "test", Path: ".", ArchivePath: "root"}})
	resp := archivePost(t, lease, lease.Manifest)
	body, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if e != nil {
		t.Fatal(e)
	}
	if len(z.File) != 2 {
		t.Fatal("private entries leaked", len(z.File))
	}
	if e := os.Symlink("a", filepath.Join(x.data, "link")); e != nil {
		t.Fatal(e)
	}
	resp = archivePost(t, lease, lease.Manifest)
	if resp.StatusCode == 200 {
		t.Fatal("symlink did not fail closed")
	}
	if e := os.Remove(filepath.Join(x.data, "link")); e != nil {
		t.Fatal(e)
	}
	put(t, x.r, "A", "b", domain.WriteOptions{})
	resp = archivePost(t, lease, lease.Manifest)
	if resp.StatusCode != 409 {
		t.Fatal("case collision accepted", resp.Status)
	}
	if e := os.Remove(filepath.Join(x.data, "A")); e != nil {
		t.Fatal(e)
	}
	if e := os.Truncate(filepath.Join(x.data, "a"), (100<<30)+1); e != nil {
		t.Fatal(e)
	}
	resp = archivePost(t, lease, lease.Manifest)
	if resp.StatusCode != 413 {
		t.Fatal("archive byte limit ignored", resp.Status)
	}
}

func TestArchiveTraversalCancellationAndDepth(t *testing.T) {
	x := setup(t, false, false)
	c, cancel := context.WithCancel(ctx)
	cancel()
	budget := 20000
	e := x.r.WalkArchive(c, ".", true, 64, &budget, func(string, os.FileInfo) error { t.Fatal("visited after cancellation"); return nil })
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(x.data, "a", "b", "c"), 0700); e != nil {
		t.Fatal(e)
	}
	e = x.r.WalkArchive(ctx, ".", true, 1, &budget, func(string, os.FileInfo) error { return nil })
	if !errors.Is(e, domain.ErrLimit) {
		t.Fatal("depth limit ignored", e)
	}
}

func TestArchivePrivateEntriesConsumeTraversalBudget(t *testing.T) {
	x := setup(t, false, false)
	for i := 0; i < 5; i++ {
		if e := os.WriteFile(filepath.Join(x.data, fmt.Sprintf(".fg-%d", i)), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	budget := 3
	visited := 0
	e := x.r.WalkArchive(ctx, ".", true, 64, &budget, func(string, os.FileInfo) error { visited++; return nil })
	if !errors.Is(e, domain.ErrLimit) || visited != 1 {
		t.Fatal("hidden entries bypassed scan budget", e, visited)
	}
}

func TestArchiveExpandedEntryLimit(t *testing.T) {
	x := setup(t, false, false)
	for i := 0; i < 10000; i++ {
		if e := os.WriteFile(filepath.Join(x.data, fmt.Sprint(i)), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	s := archiveServer(t, x.r)
	lease := archiveMint(t, s, []api.ArchiveItem{{Root: "test", Path: ".", ArchivePath: "root"}})
	resp := archivePost(t, lease, lease.Manifest)
	if resp.StatusCode != 413 {
		t.Fatal("expanded entry limit ignored", resp.Status)
	}
}
