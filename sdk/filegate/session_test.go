package filegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionKeyAndReceiptPages(t *testing.T) {
	var backendPages, directPages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/roots/test/uploads/sessions":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["idempotencyKey"] != "retry" || request["expiresIn"] != float64(30) || r.Header.Get("Authorization") != "Bearer backend" {
				t.Errorf("wrong creation contract: %v", request)
			}
			_, _ = w.Write([]byte(`{"session":{"id":"session","state":"committed","uploadedSegments":1}}`))
		case "/v1/roots/test/uploads/sessions/session/segments":
			backendPages++
			if r.URL.Query().Get("after") != "7" || r.URL.Query().Get("limit") != "20" || r.Header.Get("Authorization") != "Bearer backend" {
				t.Error("backend page contract")
			}
			_, _ = w.Write([]byte(`{"items":[{"index":9,"hash":"sha256:example"}],"next":9}`))
		case "/direct":
			directPages++
			if r.URL.Query().Get("segments") != "1" || r.URL.Query().Get("after") != "-1" || r.URL.Query().Get("limit") != "10" || r.Header.Get("Authorization") != "" {
				t.Error("direct page contract")
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	root := client.Root("test")
	created, err := root.CreateSession(context.Background(), "file", 3, WriteOptions{}, SessionCreateOptions{ExpiresIn: 30, IdempotencyKey: "retry"})
	if err != nil || created.Lease != nil || created.Session.State != SessionCommitted {
		t.Fatalf("terminal replay: %+v %v", created, err)
	}
	page, err := root.SessionSegments(context.Background(), "session", 7, 20)
	if err != nil || page.Next == nil || *page.Next != 9 || len(page.Items) != 1 {
		t.Fatalf("backend page: %+v %v", page, err)
	}
	direct := DirectSession{URL: server.URL + "/direct"}
	page, err = direct.Segments(context.Background(), -1, 10)
	if err != nil || len(page.Items) != 0 || directPages != 1 || backendPages != 1 {
		t.Fatalf("direct page: %+v %v", page, err)
	}
}
