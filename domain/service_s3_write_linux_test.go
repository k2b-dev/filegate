//go:build linux

package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestWriteObjectS3CreatesAndPersistsMetadata verifies the new
// S3-style entry-point: it creates a file when none exists and
// stores the S3-only metadata fields the caller supplied. REST
// writes leave those fields empty; this catches a regression where
// the S3 write path forgets to set them.
func TestWriteObjectS3CreatesAndPersistsMetadata(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	meta, created, err := svc.WriteObjectS3(
		"/"+mountName+"/s3/img.png",
		strings.NewReader("png-bytes"),
		domain.S3WriteOptions{
			ContentType:        "image/png",
			ContentEncoding:    "gzip",
			ContentDisposition: `attachment; filename="img.png"`,
			UserMetadata:       []byte(`{"x-amz-meta-author":"alice"}`),
		},
	)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !created {
		t.Fatalf("created=false on fresh write")
	}
	entity, err := idx.GetEntity(meta.ID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	if entity.ContentType != "image/png" {
		t.Fatalf("ContentType=%q", entity.ContentType)
	}
	if entity.ContentEncoding != "gzip" {
		t.Fatalf("ContentEncoding=%q", entity.ContentEncoding)
	}
	if entity.ContentDisposition == "" {
		t.Fatalf("ContentDisposition empty")
	}
	if string(entity.S3UserMetadata) != `{"x-amz-meta-author":"alice"}` {
		t.Fatalf("S3UserMetadata=%q", entity.S3UserMetadata)
	}
	if entity.ETagMD5 == "" {
		t.Fatalf("ETag empty")
	}
}

// TestWriteObjectS3IfNoneMatchAnyRejectsExisting pins S3 PutObject's
// "If-None-Match: *" semantic: a write to an existing key with the
// flag set must return ErrConflict and leave the existing object
// untouched.
func TestWriteObjectS3IfNoneMatchAnyRejectsExisting(t *testing.T) {
	svc, _, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	original, _, err := svc.WriteObjectS3(
		"/"+mountName+"/cond.txt",
		strings.NewReader("v1"),
		domain.S3WriteOptions{},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, err = svc.WriteObjectS3(
		"/"+mountName+"/cond.txt",
		strings.NewReader("v2"),
		domain.S3WriteOptions{IfNoneMatchAny: true},
	)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	// Original must still be there with v1's ETag.
	stillThere, err := svc.GetFile(original.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stillThere.ETag != original.ETag {
		t.Fatalf("ETag changed under failed conditional write")
	}
}

// TestWriteObjectS3OverwritePreservesS3Metadata verifies that
// overwriting via WriteObjectS3 sets the new S3 metadata from opts
// and that the multipart_etag is cleared (single-PUT writes are
// always single-MD5).
func TestWriteObjectS3OverwritePreservesS3Metadata(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	first, _, err := svc.WriteObjectS3(
		"/"+mountName+"/over.txt",
		strings.NewReader("v1"),
		domain.S3WriteOptions{ContentType: "text/plain"},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Synthetic multipart_etag from a previous (imagined) multipart
	// upload — overwrite via single-PUT must clear it.
	if err := idx.Batch(func(b domain.Batch) error {
		entity, err := idx.GetEntity(first.ID)
		if err != nil {
			return err
		}
		entity.MultipartETag = "fake-multipart-3"
		b.PutEntity(*entity)
		return nil
	}); err != nil {
		t.Fatalf("inject multipart: %v", err)
	}

	second, created, err := svc.WriteObjectS3(
		"/"+mountName+"/over.txt",
		strings.NewReader("v2 different"),
		domain.S3WriteOptions{ContentType: "text/csv"},
	)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if created {
		t.Fatalf("created=true on overwrite")
	}
	if second.ID != first.ID {
		t.Fatalf("file ID changed across overwrite: was %v now %v", first.ID, second.ID)
	}
	entity, err := idx.GetEntity(second.ID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	if entity.ContentType != "text/csv" {
		t.Fatalf("ContentType=%q, want text/csv", entity.ContentType)
	}
	if entity.MultipartETag != "" {
		t.Fatalf("MultipartETag=%q, should be cleared on single-PUT overwrite",
			entity.MultipartETag)
	}
}

// TestWriteObjectS3RestOverwriteClearsS3Metadata is the
// counter-test: a REST WriteContent on an S3-uploaded file MUST
// clear all S3-only metadata fields. This is the cross-protocol
// consistency rule from plan §7 — without it, post-REST-overwrite
// reads via S3 GET would lie about Content-Type / encoding etc.
func TestWriteObjectS3RestOverwriteClearsS3Metadata(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	mountName := svc.ListRoot()[0].Name
	first, _, err := svc.WriteObjectS3(
		"/"+mountName+"/cross.txt",
		strings.NewReader("hi"),
		domain.S3WriteOptions{
			ContentType:        "text/plain",
			ContentEncoding:    "gzip",
			ContentDisposition: "inline",
			UserMetadata:       []byte(`{"x-amz-meta-k":"v"}`),
		},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// REST-style overwrite via WriteContent.
	if err := svc.WriteContent(first.ID, strings.NewReader("rest overwrite")); err != nil {
		t.Fatalf("rest write: %v", err)
	}
	entity, err := idx.GetEntity(first.ID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	for name, got := range map[string]string{
		"ContentType":        entity.ContentType,
		"ContentEncoding":    entity.ContentEncoding,
		"ContentDisposition": entity.ContentDisposition,
	} {
		if got != "" {
			t.Fatalf("%s=%q after REST overwrite; should be cleared", name, got)
		}
	}
	if len(entity.S3UserMetadata) != 0 {
		t.Fatalf("S3UserMetadata=%q after REST overwrite; should be cleared",
			entity.S3UserMetadata)
	}
}
