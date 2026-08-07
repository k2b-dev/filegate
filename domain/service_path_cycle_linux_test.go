//go:build linux

package domain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/k2b-dev/filegate/v3/domain"
)

// TestPathResolversRejectParentCycle pins the parent-chain depth guard:
// a corrupted index with a parent cycle must yield an error instead of
// spinning ResolveAbsPath / VirtualPath forever.
func TestPathResolversRejectParentCycle(t *testing.T) {
	svc, idx, cleanup := newServiceWithIndex(t)
	defer cleanup()

	idA := domain.FileID(uuid.MustParse("019cb9ae-76c1-7807-ba50-cbb05a08ec01"))
	idB := domain.FileID(uuid.MustParse("019cb9ae-76c1-7807-ba50-cbb05a08ec02"))
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{ID: idA, ParentID: idB, Name: "a", IsDir: true})
		b.PutEntity(domain.Entity{ID: idB, ParentID: idA, Name: "b", IsDir: true})
		return nil
	}); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}

	if _, err := svc.VirtualPath(idA); err == nil {
		t.Fatal("VirtualPath on cyclic chain returned nil error")
	}
	if _, err := svc.ResolveAbsPath(idA); err == nil {
		t.Fatal("ResolveAbsPath on cyclic chain returned nil error")
	}
}
