package filegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecutionClientImmutableScope(t *testing.T) {
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("X-Filegate-Execution"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	groups := []uint32{200, 100, 200}
	scoped, err := client.Root("nfs").WithExecution(ExecutionIdentity{UID: 1001, GID: 100, Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	groups[0] = 999
	if _, err := scoped.Stat(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Root("nfs").Stat(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if headers[0] != `{"uid":1001,"gid":100,"groups":[100,200]}` || headers[1] != "" {
		t.Fatalf("scope leaked or mutated: %v", headers)
	}
	if _, err := client.WithExecution(ExecutionIdentity{UID: 0, GID: 100}); err == nil {
		t.Fatal("accepted root identity")
	}
}
