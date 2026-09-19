//go:build linux

package integration_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/k2b-dev/filegate/v5/adapter/http"
	api "github.com/k2b-dev/filegate/v5/api/v1"
	"github.com/k2b-dev/filegate/v5/domain"
)

func downloadResponse(t *testing.T, method, target, byteRange string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://cloud.example")
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, b
}

func putPNG(t *testing.T, root *domain.Root, p string, width, height int) {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Put(ctx, p, &b, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestVersionDownloadLease(t *testing.T) {
	x, s, client := server(t)
	root := client.Root("test")
	put(t, x.r, "notes/a.txt", "historical", domain.WriteOptions{})
	v, err := x.r.Snapshot("notes/a.txt", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "notes/a.txt", "current", domain.WriteOptions{OnConflict: "overwrite"})
	put(t, x.r, "other.txt", "other", domain.WriteOptions{})
	other, err := x.r.Snapshot("other.txt", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	lease, err := root.DirectVersionDownload(ctx, "notes/a.txt", v.ID, api.DownloadOptions{ExpiresIn: 0})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Method != "GET" || lease.Expires.Unix() < before+60 || lease.Expires.Unix() > time.Now().Unix()+60 {
		t.Fatal(lease)
	}
	res, b := downloadResponse(t, "GET", lease.URL+"?root=other&path=other.txt&version="+other.ID+"&purpose=download", "")
	if res.StatusCode != 200 || string(b) != "historical" {
		t.Fatalf("%s: %s", res.Status, b)
	}
	if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") || res.Header.Get("Access-Control-Allow-Origin") != "https://cloud.example" {
		t.Fatal(res.Header)
	}
	raw, err := root.VersionContentRaw(ctx, "notes/a.txt", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, err := io.ReadAll(raw.Body)
	raw.Body.Close()
	if err != nil || raw.StatusCode != 200 || !bytes.Equal(rawBody, b) || raw.Header.Get("Content-Disposition") != res.Header.Get("Content-Disposition") {
		t.Fatal(raw.Status, err)
	}
	res, b = downloadResponse(t, "GET", lease.URL, "bytes=1-3")
	if res.StatusCode != 206 || string(b) != "ist" || res.Header.Get("Content-Range") != "bytes 1-3/10" || !strings.Contains(res.Header.Get("Access-Control-Expose-Headers"), "Content-Range") {
		t.Fatal(res.Status, string(b), res.Header)
	}
	res, b = downloadResponse(t, "HEAD", lease.URL, "")
	if res.StatusCode != 200 || len(b) != 0 || res.ContentLength != 10 {
		t.Fatal(res.Status, len(b), res.ContentLength)
	}
	res, _ = downloadResponse(t, "GET", lease.URL, "bytes=100-200")
	if res.StatusCode != 416 {
		t.Fatal(res.Status)
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		leaseRequest(t, method, lease.URL, "", false, 403, nil)
	}
	mint := s.URL + "/v1/roots/test/versions/" + v.ID + "/downloads/direct"
	leaseRequest(t, "POST", mint, `{"path":"notes/a.txt"}`, false, 401, nil)
	leaseRequest(t, "POST", mint, `{"path":"other.txt"}`, true, 404, nil)
	leaseRequest(t, "POST", mint, `{"path":"notes/a.txt","expiresIn":301}`, true, 400, nil)
	// The signed path cannot follow a moved identity or acquire a replacement's history.
	if err := os.Rename(filepath.Join(x.data, "notes/a.txt"), filepath.Join(x.data, "moved.txt")); err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", lease.URL, "", false, 404, nil)
	put(t, x.r, "notes/a.txt", "replacement", domain.WriteOptions{})
	leaseRequest(t, "GET", lease.URL, "", false, 404, nil)
	// Mint a new lease at the new path, then delete the historical content.
	moved, err := root.DirectVersionDownload(ctx, "moved.txt", v.ID, api.DownloadOptions{ExpiresIn: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.r.DeleteVersion("moved.txt", v.ID); err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", moved.URL, "", false, 404, nil)
}

func TestThumbnailDownloadLease(t *testing.T) {
	x, s, client := server(t)
	root := client.Root("test")
	putPNG(t, x.r, "photo.png", 80, 40)
	putPNG(t, x.r, "other.png", 30, 90)
	lease, err := root.DirectThumbnail(ctx, "photo.png", 32, 32, 30)
	if err != nil {
		t.Fatal(err)
	}
	target := lease.URL + "?root=other&path=other.png&width=2048&height=2048&format=png&quality=1"
	res, b := downloadResponse(t, "GET", target, "bytes=0-9")
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 32 || cfg.Height != 16 || format != "jpeg" {
		t.Fatal(cfg, format, err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/jpeg" || res.Header.Get("Content-Disposition") != "" || res.Header.Get("Content-Range") != "" || res.Header.Get("Access-Control-Allow-Origin") != "https://cloud.example" {
		t.Fatal(res.Status, res.Header)
	}
	raw, err := root.ThumbnailRaw(ctx, "photo.png", 32, 32)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, err := io.ReadAll(raw.Body)
	raw.Body.Close()
	if err != nil || raw.StatusCode != 200 || !bytes.Equal(rawBody, b) {
		t.Fatal(raw.Status, err)
	}
	res, b = downloadResponse(t, "HEAD", lease.URL, "")
	if res.StatusCode != 200 || len(b) != 0 || res.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatal(res.Status, res.Header, len(b))
	}
	res, _ = downloadResponse(t, "OPTIONS", lease.URL, "")
	if res.StatusCode != 204 || !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Range") || !strings.Contains(res.Header.Get("Access-Control-Allow-Methods"), "HEAD") {
		t.Fatal(res.Status, res.Header)
	}
	mint := s.URL + "/v1/roots/test/thumbnail/direct"
	leaseRequest(t, "POST", mint, `{"path":"photo.png"}`, false, 401, nil)
	for _, body := range []string{`{"path":"photo.png","width":0}`, `{"path":"photo.png","height":2049}`, `{"path":"photo.png","width":1.5}`, `{"path":"photo.png","expiresIn":-1}`, `{"path":"photo.png","format":"png"}`} {
		leaseRequest(t, "POST", mint, body, true, 400, nil)
	}
	var defaults api.DirectURL
	leaseRequest(t, "POST", mint, `{"path":"photo.png"}`, true, 201, &defaults)
	_, b = downloadResponse(t, "GET", defaults.URL, "")
	cfg, _, err = image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 80 || cfg.Height != 40 {
		t.Fatal(cfg, err)
	}
	for _, method := range []string{"PUT", "POST", "DELETE"} {
		leaseRequest(t, method, lease.URL, "", false, 403, nil)
	}
	if err := x.r.Remove("photo.png", false); err != nil {
		t.Fatal(err)
	}
	leaseRequest(t, "GET", lease.URL, "", false, 404, nil)
	// A path-bound preview reads the new content; its requested output size stays fixed.
	putPNG(t, x.r, "photo.png", 40, 80)
	_, b = downloadResponse(t, "GET", lease.URL, "")
	cfg, _, err = image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 16 || cfg.Height != 32 {
		t.Fatal(cfg, err)
	}
}

func TestDerivedDownloadRootIsolationAndErrors(t *testing.T) {
	x, y, disabled := setup(t, true, true), setup(t, true, true), setup(t, false, false)
	y.r.Config.Name, disabled.r.Config.Name = "other", "plain"
	put(t, x.r, "file", "original", domain.WriteOptions{})
	put(t, y.r, "file", "foreign", domain.WriteOptions{})
	v, err := x.r.Snapshot("file", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := y.r.Snapshot("file", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	putPNG(t, disabled.r, "image.png", 10, 20)
	s := httptest.NewUnstartedServer(nil)
	s.Config.Handler = httpadapter.New([]*domain.Root{x.r, y.r, disabled.r}, httpadapter.Options{Token: strings.Repeat("t", 32), PublicURL: "http://" + s.Listener.Addr().String()})
	s.Start()
	defer s.Close()
	base := s.URL + "/v1/roots/"
	var versionLease, thumbLease api.DirectURL
	leaseRequest(t, "POST", base+"test/versions/"+v.ID+"/downloads/direct", `{"path":"file"}`, true, 201, &versionLease)
	// Thumbnails work on roots without an index or history.
	leaseRequest(t, "POST", base+"plain/thumbnail/direct", `{"path":"image.png"}`, true, 201, &thumbLease)
	leaseRequest(t, "GET", thumbLease.URL, "", false, 200, nil)
	for _, tc := range []struct {
		root, p, id string
		status      int
	}{
		{"other", "file", v.ID, 404}, {"test", "file", other.ID, 404}, {"plain", "image.png", v.ID, 409}, {"test", "missing", v.ID, 404}, {"test", "file", "missing", 404},
	} {
		var direct, backend api.Error
		leaseRequest(t, "POST", base+tc.root+"/versions/"+tc.id+"/downloads/direct", `{"path":"`+tc.p+`"}`, true, tc.status, &direct)
		leaseRequest(t, "GET", base+tc.root+"/versions/"+tc.id+"/content?path="+tc.p, "", true, tc.status, &backend)
		if direct.Error != backend.Error {
			t.Fatal(direct, backend)
		}
	}
	for _, p := range []string{"missing", "file", "../escape", ".filegate/versions/" + v.ID} {
		status := 400
		if p == "missing" {
			status = 404
		}
		if strings.HasPrefix(p, ".filegate") {
			status = 400
		}
		var direct, backend api.Error
		leaseRequest(t, "POST", base+"test/thumbnail/direct", `{"path":"`+p+`"}`, true, status, &direct)
		leaseRequest(t, "GET", base+"test/thumbnail?path="+url.QueryEscape(p), "", true, status, &backend)
		if direct.Error != backend.Error {
			t.Fatal(direct, backend)
		}
	}
	// Alter every scoped field without the signing key: no changed payload is accepted.
	for _, lease := range []api.DirectURL{versionLease, thumbLease} {
		for _, change := range []struct {
			key   string
			value any
		}{
			{"root", "other"}, {"path", "file"}, {"version", other.ID}, {"purpose", "download"}, {"thumbnail", map[string]int{"width": 2048, "height": 2048}}, {"expires", time.Now().Add(time.Hour).Unix()},
		} {
			parts := strings.Split(strings.TrimPrefix(lease.URL, s.URL+"/v1/direct/"), ".")
			payload, err := base64.RawURLEncoding.DecodeString(parts[0])
			if err != nil {
				t.Fatal(err)
			}
			var claims map[string]any
			if err = json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			if change.key == "path" {
				change.value = "different"
			}
			claims[change.key] = change.value
			payload, err = json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			changed := s.URL + "/v1/direct/" + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
			leaseRequest(t, "GET", changed, "", false, 401, nil)
		}
	}
}
