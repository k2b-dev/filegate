package filegate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInternalTransferOriginAndRawErrors(t *testing.T) {
	requests := 0
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/direct/payload.signature" || r.Header.Get("Authorization") != "" {
			t.Error("unsafe internal transfer", r.URL, r.Header)
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte("expired"))
	}))
	defer internal.Close()
	client, err := New("https://api.example", "backend")
	if err != nil {
		t.Fatal(err)
	}
	client, err = client.WithTransferBaseURL(internal.URL)
	if err != nil {
		t.Fatal(err)
	}
	lease := DirectURL{URL: "https://public.example/v1/direct/payload.signature", Method: "GET"}
	response, err := client.DownloadRaw(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 403 || string(body) != "expired" || requests != 1 {
		t.Fatalf("raw response lost: %v %s %v", response.Status, body, err)
	}
	if lease.URL != "https://public.example/v1/direct/payload.signature" {
		t.Fatal("public lease rewritten")
	}
	session, err := client.DirectSession(SessionLease{URL: lease.URL})
	if err != nil || !strings.HasPrefix(session.URL, internal.URL) {
		t.Fatal(session, err)
	}
	for _, invalid := range []string{"https://public.example/admin", "https://user:pass@public.example/v1/direct/payload.signature", "https://public.example/v1/direct/payload.signature#fragment", "https://public.example/v1/direct/payload%2fsignature"} {
		if _, err := client.TransferURL(invalid); err == nil {
			t.Fatalf("unsafe transfer accepted: %s", invalid)
		}
	}
	if _, err := client.WithTransferBaseURL("http://internal.example/path"); err == nil {
		t.Fatal("non-origin accepted")
	}
}
