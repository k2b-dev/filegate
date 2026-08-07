package directuploads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/sdk/filegate"
)

func TestSegmentsPutUsesSessionToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/uploads/sessions/upl_abc/segments/2" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Filegate-Upload-Session"); got != "session-token" {
			t.Fatalf("session token=%q", got)
		}
		if got := r.Header.Get("X-Segment-Checksum"); got != "sha256:abc" {
			t.Fatalf("checksum=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(filegate.UploadSegmentResponse{
			SessionID:        "upl_abc",
			Index:            2,
			UploadedSegments: []int{2},
		})
	}))
	defer server.Close()

	out, err := Segments.Put(context.Background(), PutSegmentRequest{
		Direct: filegate.UploadSessionDirect{
			BaseURL: server.URL + "/v1/uploads/sessions/upl_abc",
			Token:   "session-token",
		},
		Index:    2,
		Body:     strings.NewReader("x"),
		Checksum: "sha256:abc",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if out.SessionID != "upl_abc" || out.Index != 2 {
		t.Fatalf("unexpected response: %#v", out)
	}
}

func TestCommitUsesSessionToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/uploads/sessions/upl_abc/commit" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Filegate-Upload-Session"); got != "session-token" {
			t.Fatalf("session token=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(filegate.UploadSessionCommitResponse{
			Node:     filegate.Node{ID: "node-1", Type: "file", Name: "x", Path: "/m/x"},
			Checksum: "sha256:abc",
		})
	}))
	defer server.Close()

	out, err := Commit(context.Background(), SessionRequest{
		Direct: filegate.UploadSessionDirect{
			BaseURL: server.URL + "/v1/uploads/sessions/upl_abc",
			Token:   "session-token",
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if out.Node.ID != "node-1" {
		t.Fatalf("unexpected response: %#v", out)
	}
}
