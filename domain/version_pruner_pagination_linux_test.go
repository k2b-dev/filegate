//go:build linux

package domain_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/eventbus"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

// TestPrunerPaginatesAllVersionsForSingleFile pins the pagination
// invariant: when a single file has more versions than the index's
// per-call cap (1000), the pruner walks every page inside the per-file
// lock. The previous bug walked only the first page, so a hot file
// with 1500 versions and an aggressive bucket policy retained the tail
// 500 forever.
//
// Seeds the index directly with synthetic VersionMeta records so we
// don't have to do 1500 real writes (that would also tickle other
// behaviours we're not testing here).
func TestPrunerPaginatesAllVersionsForSingleFile(t *testing.T) {
	baseDir := t.TempDir()
	indexDir := t.TempDir()
	idx, err := indexpebble.Open(indexDir, 32<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer idx.Close()
	bus := eventbus.New()
	defer bus.Close()
	svc, err := domain.NewService(idx, filesystem.New(), bus, []string{baseDir}, 1000)
	if err != nil {
		t.Fatalf("svc: %v", err)
	}
	// Aggressive policy: keep at most 1 live version. Without
	// pagination, the pruner would only see the first 1000 records and
	// keep 1 from those, leaving the 500-record tail untouched.
	svc.EnableVersioning(domain.VersioningConfig{
		Cooldown:         50 * time.Millisecond,
		MinSizeForAutoV1: 0,
		MaxLabelBytes:    2048,
		MaxPinnedPerFile: 100,
		RetentionBuckets: []domain.RetentionBucketConfig{
			{KeepFor: 24 * time.Hour, MaxCount: 1},
		},
	}, true)

	// Create a real file (so the entity exists for ResolveAbsPath).
	root := svc.ListRoot()[0]
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+root.Name+"/hot.bin",
		strings.NewReader("seed"),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mountAbs, _ := svc.ResolveAbsPath(root.ID)
	versionsDir := filepath.Join(mountAbs, ".fg-versions", meta.ID.String())

	// Seed 1500 synthetic version records spread across 12h so they
	// all fall inside the bucket window. Blob files don't matter for
	// pagination (deleteVersionBlobAndRecord tolerates missing blobs);
	// we only care that the metadata pruning sees every record.
	now := time.Now().UnixMilli()
	const totalVersions = 1500
	for i := 0; i < totalVersions; i++ {
		u, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		vid := domain.VersionID(u)
		ts := now - int64(totalVersions-i)*1000
		if err := idx.Batch(func(b domain.Batch) error {
			b.PutVersion(domain.VersionMeta{
				VersionID: vid,
				FileID:    meta.ID,
				Timestamp: ts,
				Size:      8,
				MountName: root.Name,
			})
			return nil
		}); err != nil {
			t.Fatalf("seed put %d: %v", i, err)
		}
	}

	preCount := countVersions(t, svc, meta.ID)
	if preCount < totalVersions {
		t.Fatalf("seed count=%d, want >= %d", preCount, totalVersions)
	}

	stats, err := svc.PruneVersions()
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	postCount := countVersions(t, svc, meta.ID)
	// Bucket allows MaxCount=1, plus the auto V1 from the create above
	// (which the bucket may or may not pick depending on timestamp
	// distribution). Assert the total dropped well below 1000 — the
	// pre-fix behaviour would have left ~1499.
	if postCount >= 1000 {
		t.Fatalf("post-prune count=%d — pruner did not page past 1000 (stats=%+v)", postCount, stats)
	}
	// Sanity: pruner reported deleting a meaningful number.
	if stats.VersionsDeleted < totalVersions-100 {
		t.Fatalf("VersionsDeleted=%d, want at least %d", stats.VersionsDeleted, totalVersions-100)
	}

	_ = versionsDir // silence unused var when blobs aren't real
}

func countVersions(t *testing.T, svc *domain.Service, fileID domain.FileID) int {
	t.Helper()
	// Page in chunks of 500. The Service-level ListVersions caps the
	// per-call limit at the index's hard cap of 1000; using 500 here
	// avoids the fetch+1 boundary that would prevent NextCursor from
	// advancing on a full page (a Service-level issue orthogonal to
	// what this test is validating).
	total := 0
	cursor := domain.VersionID{}
	for {
		page, err := svc.ListVersions(fileID, cursor, 500)
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		total += len(page.Items)
		if page.NextCursor.IsZero() {
			break
		}
		cursor = page.NextCursor
	}
	return total
}
