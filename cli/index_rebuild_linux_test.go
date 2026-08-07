//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func TestIndexRebuildPreservesFileIDFromXAttr(t *testing.T) {
	basePath := t.TempDir()
	indexPath := filepath.Join(t.TempDir(), "index")
	targetFile := filepath.Join(basePath, "probe.txt")
	if err := os.WriteFile(targetFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	store := filesystem.New()
	wantID, err := domain.ParseFileID("019cba65-532e-7bff-8e57-cdfab7b1757a")
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	if err := store.SetID(targetFile, wantID); err != nil {
		t.Fatalf("set xattr id: %v", err)
	}

	openSvc := func() (*indexpebble.Index, *domain.Service, error) {
		idx, err := indexpebble.Open(indexPath, 16<<20)
		if err != nil {
			return nil, nil, err
		}
		svc, err := domain.NewService(idx, store, eventbus.New(), []string{basePath}, 1024)
		if err != nil {
			_ = idx.Close()
			return nil, nil, err
		}
		return idx, svc, nil
	}

	idx1, svc1, err := openSvc()
	if err != nil {
		t.Fatalf("open service #1: %v", err)
	}
	before, err := svc1.GetFileByVirtualPath(filepath.Base(basePath) + "/probe.txt")
	if err != nil {
		t.Fatalf("resolve before rebuild: %v", err)
	}
	if before.ID != wantID {
		t.Fatalf("before rebuild id=%s want=%s", before.ID.String(), wantID.String())
	}
	_ = idx1.Close()

	if _, err := rebuildIndexPath(indexPath, true); err != nil {
		t.Fatalf("rebuild index path: %v", err)
	}

	idx2, svc2, err := openSvc()
	if err != nil {
		t.Fatalf("open service #2: %v", err)
	}
	defer idx2.Close()

	after, err := svc2.GetFileByVirtualPath(filepath.Base(basePath) + "/probe.txt")
	if err != nil {
		t.Fatalf("resolve after rebuild: %v", err)
	}
	if after.ID != wantID {
		t.Fatalf("after rebuild id=%s want=%s", after.ID.String(), wantID.String())
	}
}
