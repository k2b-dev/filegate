package filegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeparateTransferExecutionWireContract(t *testing.T) {
	var calls []struct {
		Header string
		Target ExecutionContext
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target ExecutionContext `json:"targetExecution"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		calls = append(calls, struct {
			Header string
			Target ExecutionContext
		}{r.Header.Get("X-Filegate-Execution"), body.Target})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "backend")
	if err != nil {
		t.Fatal(err)
	}
	actor := ExecutionIdentity{UID: 1001, GID: 100, Groups: []uint32{200}}
	source, err := client.Root("ipa").WithExecution(actor)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Transfer(context.Background(), TransferRequest{Path: "a", TargetRoot: "cloud", TargetPath: "b", TargetExecution: &ExecutionContext{Mode: "service"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Root("cloud").CopyVersion(context.Background(), "version", VersionCopyRequest{Path: "a", TargetRoot: "ipa", TargetPath: "b", TargetExecution: &ExecutionContext{Mode: "unix", Identity: &actor}})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Header == "" || calls[0].Target.Mode != "service" || calls[1].Header != "" || calls[1].Target.Identity.UID != 1001 {
		t.Fatalf("wrong execution bindings: %+v", calls)
	}
}
