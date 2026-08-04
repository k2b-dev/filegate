package runtimecfg

import (
	"os"
	"path/filepath"
	"testing"
)

// The reason this package opens its own Pebble instead of sharing the index:
// `fg index rescan --new` removes the index directory outright, so credentials
// living there would be destroyed by a routine rebuild. This asserts the
// directories are genuinely independent.
func TestIndexRebuildDoesNotTouchTheRuntimeStore(t *testing.T) {
	base := t.TempDir()
	indexPath := filepath.Join(base, "index")
	configPath := filepath.Join(base, "config")

	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		t.Fatalf("mkdir index: %v", err)
	}

	store, err := Open(configPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.PutResource("s3key", "AKIA1", map[string]string{"secret": "kept"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// What `index rescan --new --skip-backup` does to the index directory.
	if err := os.RemoveAll(indexPath); err != nil {
		t.Fatalf("simulated rebuild: %v", err)
	}

	reopened, err := Open(configPath)
	if err != nil {
		t.Fatalf("reopen after rebuild: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	var got map[string]string
	if err := reopened.GetResource("s3key", "AKIA1", &got); err != nil {
		t.Fatalf("credential lost to an index rebuild: %v", err)
	}
	if got["secret"] != "kept" {
		t.Errorf("resource = %v, want the stored secret", got)
	}
}

// Guards the configuration mistake rather than the code: pointing both stores
// at one directory would reintroduce exactly the coupling this package avoids.
func TestStorePathMustDifferFromIndexPath(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "same")

	store, err := Open(shared)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Pebble holds an exclusive lock, so a second opener on the same directory
	// fails loudly instead of silently sharing state.
	if second, err := Open(shared); err == nil {
		_ = second.Close()
		t.Error("two stores opened the same directory; a shared path would go unnoticed")
	}
}
