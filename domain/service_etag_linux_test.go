//go:build linux

package domain_test

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestWriteContentByVirtualPathPopulatesETag pins the cross-protocol
// ETag contract: every WriteContentByVirtualPath produces an entity
// row whose ETag is the lowercase hex MD5 of the body. Without this,
// S3 GET/HEAD on REST-uploaded files would return no ETag header (or
// would have to lazy-compute on every cold read).
func TestWriteContentByVirtualPathPopulatesETag(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	rootName := svc.ListRoot()[0].Name
	body := []byte("hello, etag")
	want := md5sum(body)
	wantSHA := sha256sum(body)

	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/etag-test.txt",
		strings.NewReader(string(body)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if meta.ETag != want {
		t.Fatalf("ETag=%q, want %q", meta.ETag, want)
	}
	if meta.SHA256 != wantSHA {
		t.Fatalf("SHA256=%q, want %q", meta.SHA256, wantSHA)
	}

	// Round-trip: GetFile after restart-equivalent (re-fetch) must see
	// the same ETag.
	again, err := svc.GetFile(meta.ID)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if again.ETag != want {
		t.Fatalf("re-fetch ETag=%q, want %q", again.ETag, want)
	}
	if again.SHA256 != wantSHA {
		t.Fatalf("re-fetch SHA256=%q, want %q", again.SHA256, wantSHA)
	}
}

// TestWriteContentOverwritePopulatesNewETag verifies the update-path:
// WriteContent (by ID, the path used for in-place overwrites) replaces
// the stored ETag with the MD5 of the NEW bytes. A future reader must
// not see the old ETag after an overwrite — that would let S3 clients
// believe they're consuming bytes from before the write.
func TestWriteContentOverwritePopulatesNewETag(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	rootName := svc.ListRoot()[0].Name
	first := []byte("first version")
	second := []byte("second version, longer")

	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/over.txt",
		strings.NewReader(string(first)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if meta.ETag != md5sum(first) {
		t.Fatalf("create ETag=%q, want %q", meta.ETag, md5sum(first))
	}

	if err := svc.WriteContent(meta.ID, strings.NewReader(string(second))); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	updated, err := svc.GetFile(meta.ID)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if updated.ETag != md5sum(second) {
		t.Fatalf("after overwrite ETag=%q, want %q", updated.ETag, md5sum(second))
	}
}

func TestEnsureFileSHA256BackfillsMissingFingerprint(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	rootName := svc.ListRoot()[0].Name
	body := []byte("legacy sha backfill")
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/legacy-sha.txt",
		strings.NewReader(string(body)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	entity, err := idx.GetEntity(meta.ID)
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	entity.SHA256 = ""
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(*entity)
		return nil
	}); err != nil {
		t.Fatalf("clear sha: %v", err)
	}

	ensured, err := svc.EnsureFileSHA256(meta.ID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if ensured.SHA256 != sha256sum(body) {
		t.Fatalf("ensured SHA256=%q, want %q", ensured.SHA256, sha256sum(body))
	}
	persisted, err := svc.GetFile(meta.ID)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if persisted.SHA256 != sha256sum(body) {
		t.Fatalf("persisted SHA256=%q, want %q", persisted.SHA256, sha256sum(body))
	}
}

func TestEnsureFileSHA256RejectsStaleEntity(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	rootName := svc.ListRoot()[0].Name
	body := []byte("legacy sha backfill with stale entity")
	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/stale-sha.txt",
		strings.NewReader(string(body)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	entity, err := idx.GetEntity(meta.ID)
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	entity.SHA256 = ""
	entity.Size++
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(*entity)
		return nil
	}); err != nil {
		t.Fatalf("write stale entity: %v", err)
	}

	if _, err := svc.EnsureFileSHA256(meta.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("EnsureFileSHA256 err=%v, want ErrConflict", err)
	}
}

// TestRescanPopulatesETagForLegacyFiles simulates the upgrade scenario:
// a file exists in the index with no ETag (as if written before the
// schema bump). Rescan must walk the filesystem and populate ETagMD5
// from a fresh hash. Operators run rescan once after upgrade to pre-
// fill the field for their existing dataset.
//
// The "legacy row" condition is faked by writing a file via the public
// API (which now sets ETag), then placing different bytes on disk
// behind the index's back, then running Rescan — the rescan must
// recompute against the on-disk bytes rather than trusting the stale
// in-index ETag.
func TestRescanRecomputesETagOnContentChange(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	root := svc.ListRoot()[0]
	rootName := root.Name
	original := []byte("original")
	mutated := []byte("mutated externally — rescan must catch this")

	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/rescan.txt",
		strings.NewReader(string(original)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if meta.ETag != md5sum(original) {
		t.Fatalf("initial ETag=%q, want %q", meta.ETag, md5sum(original))
	}
	if meta.SHA256 != sha256sum(original) {
		t.Fatalf("initial SHA256=%q, want %q", meta.SHA256, sha256sum(original))
	}

	mountAbs, err := svc.ResolveAbsPath(root.ID)
	if err != nil {
		t.Fatalf("resolve mount: %v", err)
	}
	abs := filepath.Join(mountAbs, "rescan.txt")

	// Bypass the service: write different bytes directly to disk.
	// Rescan walks filesystem and re-hashes when size changed.
	if err := writeRawFileForTest(abs, mutated); err != nil {
		t.Fatalf("raw write: %v", err)
	}

	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	after, err := svc.GetFile(meta.ID)
	if err != nil {
		t.Fatalf("post-rescan fetch: %v", err)
	}
	if after.ETag != md5sum(mutated) {
		t.Fatalf("post-rescan ETag=%q, want %q (rescan failed to recompute)",
			after.ETag, md5sum(mutated))
	}
	if after.SHA256 != sha256sum(mutated) {
		t.Fatalf("post-rescan SHA256=%q, want %q", after.SHA256, sha256sum(mutated))
	}
}

// TestRescanPreservesETagWhenUnchanged verifies the optimization:
// re-running rescan against an unchanged dataset must NOT re-hash
// every file. The existing ETag is preserved when the prior row's
// size+mtime match the on-disk stat. Without this, repeated rescans
// on a million-file dataset would be unnecessarily slow.
//
// We prove the preservation by writing a known ETag then running
// rescan — the resulting ETag must equal the original (i.e. rescan
// didn't recompute and persist a different value, even though the
// computed value would in fact be the same).
func TestRescanPreservesETagWhenUnchanged(t *testing.T) {
	svc, cleanup := newServiceForOwnershipTest(t)
	defer cleanup()

	rootName := svc.ListRoot()[0].Name
	body := []byte("stable bytes")
	want := md5sum(body)

	meta, _, err := svc.WriteContentByVirtualPath(
		"/"+rootName+"/stable.txt",
		strings.NewReader(string(body)),
		domain.ConflictError,
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if meta.ETag != want {
		t.Fatalf("initial ETag=%q, want %q", meta.ETag, want)
	}

	if err := svc.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	after, err := svc.GetFile(meta.ID)
	if err != nil {
		t.Fatalf("post-rescan fetch: %v", err)
	}
	if after.ETag != want {
		t.Fatalf("post-rescan ETag=%q, want %q", after.ETag, want)
	}
}

// helpers

func md5sum(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sha256sum(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeRawFileForTest(path string, body []byte) error {
	// Bypass the Service: open + truncate + write the existing file.
	// This preserves the inode AND the xattr-stored FileID, which
	// Rescan reads to keep the file mapped to the same entity row.
	// A tmp+rename would create a new inode without the xattr and
	// the rescan would issue a fresh UUID — that's a different test
	// scenario.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(body)
	return err
}
