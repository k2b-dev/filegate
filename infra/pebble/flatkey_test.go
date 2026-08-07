package pebble

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestFlatKeyEncodingRoundTrip pins the wire format. Two calls with
// the same (mount, relPath) MUST produce identical bytes (Pebble
// requires byte-stable keys for correct overwrites).
func TestFlatKeyEncodingRoundTrip(t *testing.T) {
	cases := []struct {
		mount, rel string
	}{
		{"data", "photos/2024/cat.jpg"},
		{"data", ""},                 // mount-only, edge
		{"deep-bucket", "a/b/c/d/e"}, // depth
		{"unicode", "café/grüße.txt"},
	}
	for _, tc := range cases {
		k1 := flatKeyForPath(tc.mount, tc.rel)
		k2 := flatKeyForPath(tc.mount, tc.rel)
		if !bytes.Equal(k1, k2) {
			t.Fatalf("non-deterministic key for (%q, %q)", tc.mount, tc.rel)
		}
		gotMount, gotRel, ok := flatKeySplit(k1)
		if !ok {
			t.Fatalf("split failed for %q", tc.mount)
		}
		if gotMount != tc.mount || gotRel != tc.rel {
			t.Fatalf("split: got (%q, %q), want (%q, %q)", gotMount, gotRel, tc.mount, tc.rel)
		}
	}
}

// TestFlatKeyMountIsolation verifies that two mounts with similar
// names ("data", "data2") don't collide — the 0x00 separator after
// the mount name is what makes the prefix unambiguous.
func TestFlatKeyMountIsolation(t *testing.T) {
	idx, cleanup := openTestIndex(t)
	defer cleanup()

	id1 := newTestFileID(t)
	id2 := newTestFileID(t)
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutFlatKey("data", "x.txt", id1)
		b.PutFlatKey("data2", "x.txt", id2)
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	got1, err := idx.LookupByFlatKey("data", "x.txt")
	if err != nil {
		t.Fatalf("lookup data: %v", err)
	}
	if got1 != id1 {
		t.Fatalf("data: got %v, want %v", got1, id1)
	}
	got2, err := idx.LookupByFlatKey("data2", "x.txt")
	if err != nil {
		t.Fatalf("lookup data2: %v", err)
	}
	if got2 != id2 {
		t.Fatalf("data2: got %v, want %v", got2, id2)
	}

	// Iteration scoped to "data" must NOT see data2's entry.
	var got []string
	if err := idx.IterateFlatKeys("data", "", "", 0, func(rel string, _ domain.FileID) (bool, error) {
		got = append(got, rel)
		return true, nil
	}); err != nil {
		t.Fatalf("iter: %v", err)
	}
	if len(got) != 1 || got[0] != "x.txt" {
		t.Fatalf("iter data: %v, want [x.txt]", got)
	}
}

// TestIterateFlatKeysPrefixAndAfter pins the listing semantics that
// S3 ListObjectsV2 will rely on: prefix-bounded scan in lexical order,
// "after" is a strict-greater bound, limit caps results.
func TestIterateFlatKeysPrefixAndAfter(t *testing.T) {
	idx, cleanup := openTestIndex(t)
	defer cleanup()

	keys := []string{
		"a.txt", "b.txt", "photos/2023/cat.jpg",
		"photos/2024/cat.jpg", "photos/2024/dog.jpg", "photos/2025/cat.jpg",
		"zzz.bin",
	}
	if err := idx.Batch(func(b domain.Batch) error {
		for _, k := range keys {
			b.PutFlatKey("data", k, newTestFileID(t))
		}
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// Prefix scan
	var got []string
	if err := idx.IterateFlatKeys("data", "photos/2024/", "", 0, func(rel string, _ domain.FileID) (bool, error) {
		got = append(got, rel)
		return true, nil
	}); err != nil {
		t.Fatalf("iter prefix: %v", err)
	}
	want := []string{"photos/2024/cat.jpg", "photos/2024/dog.jpg"}
	if !equalStrings(got, want) {
		t.Fatalf("prefix: got %v, want %v", got, want)
	}

	// "after" cursor — strict-greater
	got = nil
	if err := idx.IterateFlatKeys("data", "photos/", "photos/2024/cat.jpg", 0, func(rel string, _ domain.FileID) (bool, error) {
		got = append(got, rel)
		return true, nil
	}); err != nil {
		t.Fatalf("iter after: %v", err)
	}
	want = []string{"photos/2024/dog.jpg", "photos/2025/cat.jpg"}
	if !equalStrings(got, want) {
		t.Fatalf("after: got %v, want %v", got, want)
	}

	// limit cap
	got = nil
	if err := idx.IterateFlatKeys("data", "", "", 2, func(rel string, _ domain.FileID) (bool, error) {
		got = append(got, rel)
		return true, nil
	}); err != nil {
		t.Fatalf("iter limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit: got %d entries, want 2", len(got))
	}
}

// TestDelFlatKeysUnderBoundary checks the boundary-condition that
// "foo" must NOT match "foobar" — only descendants joined by "/".
func TestDelFlatKeysUnderBoundary(t *testing.T) {
	idx, cleanup := openTestIndex(t)
	defer cleanup()

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutFlatKey("data", "foo/a.txt", newTestFileID(t))
		b.PutFlatKey("data", "foo/b.txt", newTestFileID(t))
		b.PutFlatKey("data", "foobar.txt", newTestFileID(t))
		b.PutFlatKey("data", "foo-keep.txt", newTestFileID(t))
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := idx.Batch(func(b domain.Batch) error {
		b.DelFlatKeysUnder("data", "foo")
		return nil
	}); err != nil {
		t.Fatalf("del: %v", err)
	}

	var remaining []string
	if err := idx.IterateFlatKeys("data", "", "", 0, func(rel string, _ domain.FileID) (bool, error) {
		remaining = append(remaining, rel)
		return true, nil
	}); err != nil {
		t.Fatalf("iter: %v", err)
	}
	want := []string{"foo-keep.txt", "foobar.txt"}
	if !equalStrings(remaining, want) {
		t.Fatalf("after del: got %v, want %v (foo/a.txt and foo/b.txt should be gone, foobar.txt and foo-keep.txt should survive)",
			remaining, want)
	}
}

// TestReKeyFlatPrefixMovesDescendants verifies the directory-rename
// primitive: every entry under (oldMount, oldPrefix) gets re-keyed
// to (newMount, newPrefix), preserving file IDs.
func TestReKeyFlatPrefixMovesDescendants(t *testing.T) {
	idx, cleanup := openTestIndex(t)
	defer cleanup()

	idA := newTestFileID(t)
	idB := newTestFileID(t)
	idKeep := newTestFileID(t)
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutFlatKey("data", "old/a.txt", idA)
		b.PutFlatKey("data", "old/sub/b.txt", idB)
		b.PutFlatKey("data", "untouched.txt", idKeep)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := idx.Batch(func(b domain.Batch) error {
		b.ReKeyFlatPrefix("data", "old", "data", "new")
		return nil
	}); err != nil {
		t.Fatalf("rekey: %v", err)
	}

	checkAt := func(mount, rel string, want domain.FileID) {
		t.Helper()
		got, err := idx.LookupByFlatKey(mount, rel)
		if err != nil {
			t.Fatalf("lookup (%s,%s): %v", mount, rel, err)
		}
		if got != want {
			t.Fatalf("lookup (%s,%s): got %v, want %v", mount, rel, got, want)
		}
	}
	checkAt("data", "new/a.txt", idA)
	checkAt("data", "new/sub/b.txt", idB)
	checkAt("data", "untouched.txt", idKeep)

	// Old keys must be gone.
	if _, err := idx.LookupByFlatKey("data", "old/a.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old/a.txt should be gone, got err=%v", err)
	}
	if _, err := idx.LookupByFlatKey("data", "old/sub/b.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old/sub/b.txt should be gone, got err=%v", err)
	}
}

// TestReKeyFlatPrefixCrossMount verifies that ReKeyFlatPrefix can
// move an entire subtree to a different mount (e.g. Transfer between
// mounts in the existing API).
func TestReKeyFlatPrefixCrossMount(t *testing.T) {
	idx, cleanup := openTestIndex(t)
	defer cleanup()

	idA := newTestFileID(t)
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutFlatKey("src", "things/x.txt", idA)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := idx.Batch(func(b domain.Batch) error {
		b.ReKeyFlatPrefix("src", "things", "dst", "received")
		return nil
	}); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	got, err := idx.LookupByFlatKey("dst", "received/x.txt")
	if err != nil {
		t.Fatalf("lookup new: %v", err)
	}
	if got != idA {
		t.Fatalf("got %v, want %v", got, idA)
	}
	if _, err := idx.LookupByFlatKey("src", "things/x.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old key should be gone, got err=%v", err)
	}
}

// helpers

func openTestIndex(t *testing.T) (*Index, func()) {
	t.Helper()
	idx, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	return idx, func() { _ = idx.Close() }
}

func newTestFileID(t *testing.T) domain.FileID {
	t.Helper()
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return domain.FileID(u)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
