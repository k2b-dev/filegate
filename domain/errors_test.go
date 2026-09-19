package domain

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestSpecificConflictsPreserveGeneralClassification(t *testing.T) {
	for _, err := range []error{ErrPathConflict, ErrIdempotencyConflict, pathConflict(os.ErrExist)} {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("lost generic conflict: %v", err)
		}
	}
	if errors.Is(ErrPathConflict, ErrIdempotencyConflict) || errors.Is(ErrIdempotencyConflict, ErrPathConflict) {
		t.Fatal("specific conflicts overlap")
	}
	wrapped := pathConflict(os.ErrExist)
	if !errors.Is(wrapped, os.ErrExist) || !errors.Is(wrapped, ErrPathConflict) {
		t.Fatal("atomic publication cannot recognize EEXIST", wrapped)
	}
	if errors.Is(ErrConflict, ErrPathConflict) || errors.Is(ErrConflict, ErrIdempotencyConflict) {
		t.Fatal("structural conflict has a specific classification")
	}
}

func TestSameRootTransfersRejectDifferentExecutionBeforeFilesystemAccess(t *testing.T) {
	shared := &rootShared{}
	actor := &ExecutionIdentity{UID: 1001, GID: 1001}
	other := &ExecutionIdentity{UID: 1002, GID: 1001}
	for _, move := range []bool{false, true} {
		for _, target := range []*ExecutionIdentity{nil, other} {
			src := &Root{rootShared: shared, execution: actor}
			dst := &Root{rootShared: shared, execution: target}
			// Files and State intentionally absent: rejection must happen before opens,
			// locks requiring a live request, or a source/destination mutation.
			if _, err := Transfer(context.Background(), src, "file", dst, "other", move, WriteOptions{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("move=%t target=%+v: %v", move, target, err)
			}
		}
	}
}

type structuralConflictState struct{ State }

func (s structuralConflictState) Scan(string, func(string, []byte) error) error { return ErrConflict }
func TestImplicitParentCreationPreservesStructuralConflicts(t *testing.T) {
	root := &Root{rootShared: &rootShared{needsRecovery: true}, State: structuralConflictState{}}
	err := root.makeDirectory("parent", nil)
	if !errors.Is(err, ErrConflict) || errors.Is(err, os.ErrExist) {
		t.Fatal("structural recovery error treated as an existing parent", err)
	}
	st, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = &Root{rootShared: &rootShared{}, Files: &conflictProbeFiles{existing: st, allOccupied: true}}
	if err = root.makeDirectory("parent", nil); !errors.Is(err, os.ErrExist) {
		t.Fatal("occupied parent no longer recognized", err)
	}
}
