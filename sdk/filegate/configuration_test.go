package filegate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/config/schema":
			_ = json.NewEncoder(w).Encode(ConfigSchemaResponse{Keys: []ConfigKeySchema{{Path: "metrics.enabled"}}})
		case "GET /v1/config":
			_ = json.NewEncoder(w).Encode(ConfigValuesResponse{Values: []ConfigValue{{Path: "metrics.enabled"}}})
		case "POST /v1/config/plan":
			var req map[string]map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode plan request: %v", err)
			}
			if enabled, ok := req["values"]["metrics.enabled"].(bool); !ok || !enabled {
				t.Fatalf("plan values=%#v", req["values"])
			}
			_ = json.NewEncoder(w).Encode(ConfigManifestPlanResponse{CurrentRevision: "old", ProposedRevision: "new"})
		case "POST /v1/config/apply":
			var req struct {
				Values           map[string]any `json:"values"`
				ExpectedRevision string         `json:"expectedRevision"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode apply request: %v", err)
			}
			if req.ExpectedRevision != "old" || req.Values["metrics.enabled"] != true {
				t.Fatalf("apply request=%#v", req)
			}
			_ = json.NewEncoder(w).Encode(ConfigManifestApplyResponse{Manifest: ConfigManifestStatus{Revision: "new"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	schema, err := client.Config.Schema(ctx)
	if err != nil || len(schema.Keys) != 1 {
		t.Fatalf("schema=%#v err=%v", schema, err)
	}
	values, err := client.Config.Values(ctx)
	if err != nil || len(values.Values) != 1 {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	plan, err := client.Config.Plan(ctx, map[string]any{"metrics.enabled": true})
	if err != nil || plan.ProposedRevision != "new" {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	applied, err := client.Config.Apply(ctx, map[string]any{"metrics.enabled": true}, "old")
	if err != nil || applied.Manifest.Revision != "new" {
		t.Fatalf("apply=%#v err=%v", applied, err)
	}
}

func TestS3KeysClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /v1/s3/keys":
			_ = json.NewEncoder(w).Encode(S3KeyListResponse{Items: []S3Key{{AccessKey: "key-one"}}, Total: 1})
		case "POST /v1/s3/keys":
			_ = json.NewEncoder(w).Encode(S3KeyCreated{S3Key: S3Key{AccessKey: "key-one"}, SecretKey: "created-secret"})
		case "PATCH /v1/s3/keys/key-one":
			_ = json.NewEncoder(w).Encode(S3Key{AccessKey: "key-one", Disabled: true})
		case "POST /v1/s3/keys/key-one/rotate":
			_ = json.NewEncoder(w).Encode(S3KeyCreated{S3Key: S3Key{AccessKey: "key-one"}, SecretKey: "rotated-secret"})
		case "DELETE /v1/s3/keys/key-one":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /v1/s3/keys/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"key not found"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	list, err := client.S3Keys.List(ctx)
	if err != nil || list.Total != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	created, err := client.S3Keys.Create(ctx, S3KeyCreateRequest{Buckets: []string{"*"}})
	if err != nil || created.SecretKey != "created-secret" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	disabled := true
	updated, err := client.S3Keys.Update(ctx, "key-one", S3KeyUpdateRequest{Disabled: &disabled})
	if err != nil || !updated.Disabled {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	rotated, err := client.S3Keys.Rotate(ctx, "key-one")
	if err != nil || rotated.SecretKey != "rotated-secret" {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
	if err := client.S3Keys.Delete(ctx, "key-one"); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if err := client.S3Keys.Delete(ctx, " "); err == nil {
		t.Fatalf("expected empty access key error")
	}
	if err := client.S3Keys.Delete(ctx, "missing"); err == nil {
		t.Fatalf("expected API error")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			t.Fatalf("delete error=%v", err)
		}
	}
}

func TestSystemPrune(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/versions/prune" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(PruneResponse{FilesScanned: 3, VersionsDeleted: 2})
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.System.Prune(context.Background())
	if err != nil || result.FilesScanned != 3 || result.VersionsDeleted != 2 {
		t.Fatalf("prune=%#v err=%v", result, err)
	}
}
