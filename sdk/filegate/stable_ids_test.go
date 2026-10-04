package filegate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestStableIDReadAndDownloadContracts(t *testing.T) {
	const fileID = "0199afb0-7c00-7000-8000-000000000001"
	const execution = `{"uid":1001,"gid":100,"groups":[200]}`
	type call struct {
		method, path string
		query        url.Values
		body         map[string]any
	}
	var calls []call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backend" || r.Header.Get("X-Filegate-Execution") != execution {
			t.Error("ID request lost backend execution scope")
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("download request lost JSON content type")
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.Query(), body})
		if r.Method == http.MethodGet {
			w.Header().Set("X-Filegate-Test", "preserved")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","message":"missing"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"url":"https://files.example/v1/direct/bound.signature","method":"GET","expires":"2030-01-01T00:00:00Z"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	root, err := client.Root("documents").WithExecution(ExecutionIdentity{UID: 1001, GID: 100, Groups: []uint32{200}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := root.ContentByIDRaw(context.Background(), fileID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusNotFound || response.Header.Get("X-Filegate-Test") != "preserved" || string(body) != `{"error":"not_found","message":"missing"}` {
		t.Fatalf("raw ID response changed: %v %s %v", response.Status, body, err)
	}
	lease, err := root.DirectDownloadByID(context.Background(), fileID, DownloadOptions{ExpiresIn: 30, FileName: "Grüße.txt"})
	if err != nil || lease.Method != http.MethodGet || lease.URL != "https://files.example/v1/direct/bound.signature" {
		t.Fatalf("ID lease changed: %+v %v", lease, err)
	}
	want := []call{
		{http.MethodGet, "/v1/roots/documents/content", url.Values{"fileId": {fileID}}, nil},
		{http.MethodPost, "/v1/roots/documents/downloads/direct", url.Values{}, map[string]any{"fileId": fileID, "expiresIn": float64(30), "fileName": "Grüße.txt"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("ID calls: %#v, want %#v", calls, want)
	}
}

func TestStableIDDownloadMintPreservesTypedErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"feature_disabled","message":"stable IDs disabled"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Root("documents").DirectDownloadByID(context.Background(), "0199afb0-7c00-7000-8000-000000000001", DownloadOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Status != http.StatusConflict || apiError.Code != "feature_disabled" || apiError.Message != "stable IDs disabled" {
		t.Fatalf("ID mint error changed: %v", err)
	}
}
