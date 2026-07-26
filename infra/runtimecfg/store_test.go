package runtimecfg

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

type s3key struct {
	AccessKey string   `json:"accessKey"`
	Buckets   []string `json:"buckets"`
}

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func reopen(t *testing.T, store *Store, path string) *Store {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestOverridesRoundTripAndSurviveReopen(t *testing.T) {
	store, path := openTemp(t)

	if err := store.SetOverrides(map[string]any{"upload.max_upload_bytes": 1234, "server.access_log_enabled": false}); err != nil {
		t.Fatalf("set: %v", err)
	}

	store = reopen(t, store, path)
	got := store.Overrides()
	if len(got) != 2 {
		t.Fatalf("overrides = %d, want 2", len(got))
	}
	if string(got["upload.max_upload_bytes"]) != "1234" {
		t.Errorf("max_upload_bytes = %s, want 1234", got["upload.max_upload_bytes"])
	}
}

func TestNilOverrideClearsBackToDefault(t *testing.T) {
	store, _ := openTemp(t)

	if err := store.SetOverrides(map[string]any{"upload.expiry": "1h"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.SetOverrides(map[string]any{"upload.expiry": nil}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, present := store.Overrides()["upload.expiry"]; present {
		t.Error("cleared override is still stored; the key would never fall back to its default")
	}
}

func TestResourcesRoundTrip(t *testing.T) {
	store, path := openTemp(t)

	if err := store.PutResource("s3key", "AKIA1", s3key{AccessKey: "AKIA1", Buckets: []string{"photos"}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	store = reopen(t, store, path)

	var got s3key
	if err := store.GetResource("s3key", "AKIA1", &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessKey != "AKIA1" || len(got.Buckets) != 1 {
		t.Errorf("resource = %+v, want the stored key", got)
	}

	list, err := store.ListResources("s3key")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list = %d, want 1", len(list))
	}

	if err := store.GetResource("s3key", "missing", &got); err != ErrNotFound {
		t.Errorf("missing resource err = %v, want ErrNotFound", err)
	}
}

// The failure this guards against is a revoked credential coming back: an
// operator deletes a compromised key, the deployment still carries the seed
// variable, and a restart resurrects it.
func TestDeletedResourceStaysDeletedAcrossRestart(t *testing.T) {
	store, path := openTemp(t)

	if err := store.PutResource("s3key", "AKIA1", s3key{AccessKey: "AKIA1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.MarkBootstrapped("s3key", time.Now()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := store.DeleteResource("s3key", "AKIA1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	store = reopen(t, store, path)

	var got s3key
	if err := store.GetResource("s3key", "AKIA1", &got); err != ErrNotFound {
		t.Fatalf("deleted key came back after restart: err = %v", err)
	}
	if _, done, err := store.Bootstrapped("s3key"); err != nil || !done {
		t.Fatalf("bootstrap marker lost: done=%v err=%v", done, err)
	}
}

func TestBootstrapMarkerIsStickyAndSurvivesCorruption(t *testing.T) {
	store, path := openTemp(t)

	if _, done, err := store.Bootstrapped("s3key"); err != nil || done {
		t.Fatalf("fresh store reports bootstrapped: done=%v err=%v", done, err)
	}

	stamp := time.Now().UTC().Truncate(time.Second)
	if err := store.MarkBootstrapped("s3key", stamp); err != nil {
		t.Fatalf("mark: %v", err)
	}
	store = reopen(t, store, path)

	at, done, err := store.Bootstrapped("s3key")
	if err != nil || !done {
		t.Fatalf("marker not persisted: done=%v err=%v", done, err)
	}
	if !at.Equal(stamp) {
		t.Errorf("marker time = %v, want %v", at, stamp)
	}

	// An unreadable marker must still count as bootstrapped, otherwise a
	// corrupt byte would re-run seeding and resurrect deleted resources.
	if err := store.db.Set([]byte(prefixBootstrap+"s3key"), []byte("not-a-timestamp"), nil); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, done, err := store.Bootstrapped("s3key"); err != nil || !done {
		t.Errorf("corrupt marker read as un-bootstrapped: done=%v err=%v", done, err)
	}
}

func TestOverridesAreIsolatedFromResources(t *testing.T) {
	store, _ := openTemp(t)

	if err := store.SetOverrides(map[string]any{"upload.expiry": "2h"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.PutResource("s3key", "AKIA1", s3key{AccessKey: "AKIA1"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if len(store.Overrides()) != 1 {
		t.Errorf("resources leaked into overrides: %v", store.Overrides())
	}
	list, _ := store.ListResources("s3key")
	if len(list) != 1 {
		t.Errorf("overrides leaked into resources: %v", list)
	}

	var raw json.RawMessage
	if err := store.GetResource("s3key", "AKIA1", &raw); err != nil {
		t.Fatalf("get raw: %v", err)
	}
}
