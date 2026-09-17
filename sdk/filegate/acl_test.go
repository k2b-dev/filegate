package filegate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestACLRequests(t *testing.T) {
	id := uint32(0)
	acl := ACL{Entries: []ACLEntry{
		{Tag: "owner", Permissions: "rwx"},
		{Tag: "user", ID: &id, Permissions: "r-x"},
		{Tag: "owningGroup", Permissions: "rwx"},
		{Tag: "mask", Permissions: "rwx"},
		{Tag: "other", Permissions: "---"},
	}}
	methods := []string{"GET", "PUT", "DELETE"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls >= len(methods) {
			t.Error("unexpected request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		wantMethod := methods[calls]
		calls++
		if req.Method != wantMethod || req.URL.Path != "/v1/roots/freeipa/acl" || req.URL.Query().Get("path") != "groups/a & b" || req.URL.Query().Get("scope") != "default" {
			t.Errorf("unexpected ACL request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing backend authentication")
		}
		if req.Method == "PUT" {
			if req.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON content type")
			}
			var got ACL
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || !reflect.DeepEqual(got, acl) {
				t.Errorf("ACL did not round-trip: %#v, %v", got, err)
			}
		}
		if req.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(acl)
	}))
	defer server.Close()
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	root := client.Root("freeipa")
	ctx := context.Background()
	got, err := root.GetACL(ctx, "groups/a & b", DefaultACL)
	if err != nil || !reflect.DeepEqual(got, acl) {
		t.Fatalf("GetACL = %#v, %v", got, err)
	}
	got, err = root.SetACL(ctx, "groups/a & b", DefaultACL, acl)
	if err != nil || !reflect.DeepEqual(got, acl) {
		t.Fatalf("SetACL = %#v, %v", got, err)
	}
	if err := root.ClearDefaultACL(ctx, "groups/a & b"); err != nil {
		t.Fatal(err)
	}
	if calls != len(methods) {
		t.Fatalf("got %d requests", calls)
	}
}

func TestACLClientPreservesUnsupportedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("scope") != "access" {
			t.Error("access scope lost")
		}
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"acl_not_supported","message":"POSIX ACLs are not supported by this filesystem or mount"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Root("freeipa").GetACL(context.Background(), ".", AccessACL)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 501 || apiErr.Code != "acl_not_supported" || apiErr.Message == "" {
		t.Fatalf("error details lost: %v", err)
	}
}
