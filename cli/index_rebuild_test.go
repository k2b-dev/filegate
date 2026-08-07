package cli

import (
	"os"
	"path/filepath"
	"testing"

	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func TestRebuildIndexPathWithBackup(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index")
	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		t.Fatalf("mkdir index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(indexPath, "CURRENT"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	backupPath, err := rebuildIndexPath(indexPath, true)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if backupPath == "" {
		t.Fatalf("expected backup path")
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupPath, "CURRENT")); err != nil {
		t.Fatalf("backup content missing: %v", err)
	}
	entries, err := os.ReadDir(indexPath)
	if err != nil {
		t.Fatalf("read rebuilt dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rebuilt dir not empty: %d entries", len(entries))
	}
}

func TestRebuildIndexPathWithoutBackup(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index")
	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		t.Fatalf("mkdir index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(indexPath, "foo"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	backupPath, err := rebuildIndexPath(indexPath, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if backupPath != "" {
		t.Fatalf("unexpected backup path: %s", backupPath)
	}
	entries, err := os.ReadDir(indexPath)
	if err != nil {
		t.Fatalf("read rebuilt dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rebuilt dir not empty: %d entries", len(entries))
	}
}

func TestRebuildIndexPathCreatesMissingDir(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "missing-index")
	backupPath, err := rebuildIndexPath(indexPath, true)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if backupPath != "" {
		t.Fatalf("unexpected backup for missing path: %s", backupPath)
	}
	if st, err := os.Stat(indexPath); err != nil || !st.IsDir() {
		t.Fatalf("index dir not created: stat=%v err=%v", st, err)
	}
}

func TestRebuildIndexPathFailsWhenIndexInUse(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index")
	idx, err := indexpebble.Open(indexPath, 8<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	if _, err := rebuildIndexPath(indexPath, true); err == nil {
		t.Fatalf("expected in-use error")
	}
}
