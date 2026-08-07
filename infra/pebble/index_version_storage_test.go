package pebble

import (
	"testing"

	"github.com/google/uuid"

	"github.com/k2b-dev/filegate/v3/domain"
)

// openTempIndex opens a fresh Pebble index in t.TempDir(). Cleanup is
// registered automatically.
func openTempIndex(t *testing.T) *Index {
	t.Helper()
	dir := t.TempDir()
	idx, err := Open(dir, 16<<20)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func mustNewVersionID(t *testing.T) domain.VersionID {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return domain.VersionID(u)
}

func mustNewFileID(t *testing.T) domain.FileID {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return domain.FileID(u)
}

func TestVersionRoundTripThroughPebble(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)
	vid := mustNewVersionID(t)

	want := domain.VersionMeta{
		VersionID: vid,
		FileID:    fid,
		Timestamp: 1_700_000_000_111,
		Size:      8192,
		Mode:      0o600,
		Pinned:    true,
		Label:     "before-refactor",
	}

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutVersion(want)
		return nil
	}); err != nil {
		t.Fatalf("batch put: %v", err)
	}

	got, err := idx.GetVersion(fid, vid)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if got == nil {
		t.Fatalf("GetVersion returned nil")
	}
	if *got != want {
		t.Fatalf("round-trip mismatch:\n got=%#v\nwant=%#v", *got, want)
	}
}

func TestGetVersionMissingReturnsErrNotFound(t *testing.T) {
	idx := openTempIndex(t)
	_, err := idx.GetVersion(mustNewFileID(t), mustNewVersionID(t))
	if err != domain.ErrNotFound {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestListVersionsOrderedByTimestamp(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)

	// UUIDv7 IDs are time-sorted, so creating them in chronological order
	// produces ascending Pebble keys. We assert ListVersions returns them
	// in the same order.
	want := make([]domain.VersionMeta, 0, 5)
	for i := 0; i < 5; i++ {
		vid := mustNewVersionID(t)
		meta := domain.VersionMeta{
			VersionID: vid,
			FileID:    fid,
			Timestamp: int64(1_700_000_000_000 + i*1000),
			Size:      int64((i + 1) * 100),
		}
		want = append(want, meta)
		if err := idx.Batch(func(b domain.Batch) error {
			b.PutVersion(meta)
			return nil
		}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	got, err := idx.ListVersions(fid, domain.VersionID{}, 100)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("count=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].VersionID != want[i].VersionID {
			t.Fatalf("order mismatch at %d: got %v, want %v", i, got[i].VersionID, want[i].VersionID)
		}
	}
}

func TestListVersionsCursorSkipsAfter(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)
	vids := make([]domain.VersionID, 0, 4)
	for i := 0; i < 4; i++ {
		vid := mustNewVersionID(t)
		vids = append(vids, vid)
		if err := idx.Batch(func(b domain.Batch) error {
			b.PutVersion(domain.VersionMeta{VersionID: vid, FileID: fid, Timestamp: int64(i)})
			return nil
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	page, err := idx.ListVersions(fid, vids[1], 100)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("page size=%d, want 2 (after vids[1])", len(page))
	}
	if page[0].VersionID != vids[2] || page[1].VersionID != vids[3] {
		t.Fatalf("cursor advance wrong: %v", page)
	}
}

func TestListVersionsScopedToFile(t *testing.T) {
	idx := openTempIndex(t)
	fileA := mustNewFileID(t)
	fileB := mustNewFileID(t)

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutVersion(domain.VersionMeta{VersionID: mustNewVersionID(t), FileID: fileA, Timestamp: 1})
		b.PutVersion(domain.VersionMeta{VersionID: mustNewVersionID(t), FileID: fileB, Timestamp: 2})
		b.PutVersion(domain.VersionMeta{VersionID: mustNewVersionID(t), FileID: fileA, Timestamp: 3})
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	gotA, _ := idx.ListVersions(fileA, domain.VersionID{}, 100)
	gotB, _ := idx.ListVersions(fileB, domain.VersionID{}, 100)
	if len(gotA) != 2 {
		t.Fatalf("fileA versions=%d, want 2", len(gotA))
	}
	if len(gotB) != 1 {
		t.Fatalf("fileB versions=%d, want 1", len(gotB))
	}
}

func TestLatestVersionTimestampReturnsNewest(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)
	for i := 0; i < 5; i++ {
		if err := idx.Batch(func(b domain.Batch) error {
			b.PutVersion(domain.VersionMeta{
				VersionID: mustNewVersionID(t),
				FileID:    fid,
				Timestamp: int64(1_700_000_000_000 + i*1000),
			})
			return nil
		}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	ts, err := idx.LatestVersionTimestamp(fid)
	if err != nil {
		t.Fatalf("LatestVersionTimestamp: %v", err)
	}
	if ts != 1_700_000_004_000 {
		t.Fatalf("ts=%d, want %d (newest)", ts, 1_700_000_004_000)
	}
}

func TestLatestVersionTimestampReturnsZeroWhenNoVersions(t *testing.T) {
	idx := openTempIndex(t)
	ts, err := idx.LatestVersionTimestamp(mustNewFileID(t))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ts != 0 {
		t.Fatalf("ts=%d, want 0", ts)
	}
}

func TestDelVersionRemovesEntry(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)
	vid := mustNewVersionID(t)

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutVersion(domain.VersionMeta{VersionID: vid, FileID: fid, Timestamp: 1})
		return nil
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := idx.Batch(func(b domain.Batch) error {
		b.DelVersion(fid, vid)
		return nil
	}); err != nil {
		t.Fatalf("del: %v", err)
	}

	if _, err := idx.GetVersion(fid, vid); err != domain.ErrNotFound {
		t.Fatalf("after delete err=%v, want ErrNotFound", err)
	}
}

func TestPutVersionRejectsZeroIDs(t *testing.T) {
	idx := openTempIndex(t)
	err := idx.Batch(func(b domain.Batch) error {
		b.PutVersion(domain.VersionMeta{})
		return nil
	})
	if err == nil {
		t.Fatalf("expected error for zero IDs")
	}
}

func TestMarkVersionsDeletedSetsDeletedAt(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)

	for i := 0; i < 3; i++ {
		if err := idx.Batch(func(b domain.Batch) error {
			b.PutVersion(domain.VersionMeta{
				VersionID: mustNewVersionID(t),
				FileID:    fid,
				Timestamp: int64(i),
			})
			return nil
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	const deletedAt int64 = 1_700_000_999_999
	n, err := idx.MarkVersionsDeleted(fid, deletedAt)
	if err != nil {
		t.Fatalf("MarkVersionsDeleted: %v", err)
	}
	if n != 3 {
		t.Fatalf("marked=%d, want 3", n)
	}
	got, _ := idx.ListVersions(fid, domain.VersionID{}, 100)
	for _, v := range got {
		if v.DeletedAt != deletedAt {
			t.Fatalf("DeletedAt=%d, want %d", v.DeletedAt, deletedAt)
		}
	}
}

func TestMarkVersionsDeletedSkipsAlreadyMarked(t *testing.T) {
	idx := openTempIndex(t)
	fid := mustNewFileID(t)
	vidA, vidB := mustNewVersionID(t), mustNewVersionID(t)

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutVersion(domain.VersionMeta{VersionID: vidA, FileID: fid, Timestamp: 1, DeletedAt: 100})
		b.PutVersion(domain.VersionMeta{VersionID: vidB, FileID: fid, Timestamp: 2})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := idx.MarkVersionsDeleted(fid, 200)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Fatalf("marked=%d, want 1 (only vidB had zero)", n)
	}
	a, _ := idx.GetVersion(fid, vidA)
	if a.DeletedAt != 100 {
		t.Fatalf("vidA DeletedAt=%d, want 100 (preserved)", a.DeletedAt)
	}
	b, _ := idx.GetVersion(fid, vidB)
	if b.DeletedAt != 200 {
		t.Fatalf("vidB DeletedAt=%d, want 200", b.DeletedAt)
	}
}

func TestMarkVersionsDeletedRejectsNonPositiveTimestamp(t *testing.T) {
	idx := openTempIndex(t)
	if _, err := idx.MarkVersionsDeleted(mustNewFileID(t), 0); err != domain.ErrInvalidArgument {
		t.Fatalf("err=%v, want ErrInvalidArgument", err)
	}
}
