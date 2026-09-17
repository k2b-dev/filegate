package filegate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSessionBackendAndDirectoryContracts(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]any
	}
	var calls []call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backend" {
			t.Error("missing backend bearer")
		}
		var body map[string]any
		if r.Body != nil {
			err := json.NewDecoder(r.Body).Decode(&body)
			if err != nil && err != io.EOF {
				t.Error(err)
			}
		}
		calls = append(calls, call{r.Method, r.URL.Path, body})
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	root := c.Root("cloud")
	ctx := context.Background()
	if _, err = root.CreateSession(ctx, "inbox/new", 0, WriteOptions{OnConflict: "error"}, SessionLeaseRequest{ExpiresIn: 30, AllowAbort: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = root.Session(ctx, "session-id"); err != nil {
		t.Fatal(err)
	}
	if _, err = root.SessionLease(ctx, "session-id", SessionLeaseRequest{ExpiresIn: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err = root.CommitSession(ctx, "session-id"); err != nil {
		t.Fatal(err)
	}
	if err = root.AbortSession(ctx, "session-id"); err != nil {
		t.Fatal(err)
	}
	if _, err = root.Mkdir(ctx, "groups/editors", DirectoryOptions{Ownership: &Ownership{DirMode: "2770"}, ACL: &DirectoryACLs{Default: &ACL{Entries: []ACLEntry{}}}}); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"POST", "/v1/roots/cloud/uploads/sessions", map[string]any{"path": "inbox/new", "size": float64(0), "onConflict": "error", "expiresIn": float64(30), "allowAbort": true}},
		{"GET", "/v1/roots/cloud/uploads/sessions/session-id", nil},
		{"POST", "/v1/roots/cloud/uploads/sessions/session-id/lease", map[string]any{"expiresIn": float64(60)}},
		{"POST", "/v1/roots/cloud/uploads/sessions/session-id/commit", nil},
		{"DELETE", "/v1/roots/cloud/uploads/sessions/session-id", nil},
		{"POST", "/v1/roots/cloud/directories", map[string]any{"path": "groups/editors", "ownership": map[string]any{"dirMode": "2770"}, "acl": map[string]any{"default": map[string]any{"entries": []any{}}}}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls: %#v, want %#v", calls, want)
	}
}

func TestArchiveLeaseDownloadDoesNotSendBackendToken(t *testing.T) {
	ctx := context.Background()
	manifest := `{"items":[{"path":"a & b"}]}`
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("backend token leaked")
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("wrong archive request")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if len(r.PostForm) != 1 || r.PostForm.Get("manifest") != manifest {
			t.Errorf("manifest altered: %v", r.PostForm)
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"lease_expired"}`))
	}))
	defer download.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/downloads/archives" || r.Header.Get("Authorization") != "Bearer backend" {
			t.Error("invalid archive mint")
		}
		var body struct {
			Items     []ArchiveItem `json:"items"`
			ExpiresIn int           `json:"expiresIn"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Items) != 1 || body.Items[0].ArchivePath != "files/a & b" || body.ExpiresIn != 30 {
			t.Errorf("bad mint body: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(ArchiveLease{URL: download.URL, Method: "POST", Manifest: manifest})
	}))
	defer backend.Close()
	c, err := New(backend.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.ArchiveLease(ctx, []ArchiveItem{{Root: "cloud", Path: "a & b", ArchivePath: "files/a & b"}}, 30)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.ArchiveRaw(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("raw response status lost: %d", resp.StatusCode)
	}
}

func TestDirectSessionHasNoCommitAndSendsNoBearer(t *testing.T) {
	if _, ok := reflect.TypeOf(DirectSession{}).MethodByName("Commit"); ok {
		t.Fatal("browser commit must not exist")
	}
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "" {
			t.Error("backend bearer in direct request")
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if r.Method == "PUT" && r.URL.Query().Get("segment") != "0" {
			t.Error("segment lost")
		}
		_, _ = w.Write([]byte(`{"state":"open","size":0,"segments":{},"received":0}`))
	}))
	defer server.Close()
	ctx := context.Background()
	s := DirectSession{URL: server.URL + "?token=lease"}
	if _, err := s.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"GET", "PUT", "DELETE"}) {
		t.Fatal(methods)
	}
}

func TestDirectUploadCustomLeaseLifetime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/roots/cloud/uploads/direct" || r.Header.Get("Authorization") != "Bearer backend" {
			t.Errorf("unexpected mint request: %s %s", r.Method, r.URL)
		}
		var body struct {
			Path      string     `json:"path"`
			Size      int64      `json:"size"`
			ExpiresIn int        `json:"expiresIn"`
			Ownership *Ownership `json:"ownership"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Path != "new.txt" || body.Size != 5 || body.ExpiresIn != 30 || body.Ownership == nil || body.Ownership.Mode != "0640" {
			t.Errorf("direct lease options lost: %+v", body)
		}
		_, _ = w.Write([]byte(`{"url":"https://files.example/v1/direct/lease","method":"PUT"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Root("cloud").DirectUpload(context.Background(), "new.txt", 5, WriteOptions{Ownership: &Ownership{Mode: "0640"}}, 30)
	if err != nil || lease.Method != "PUT" {
		t.Fatalf("mint: %+v, %v", lease, err)
	}
}
