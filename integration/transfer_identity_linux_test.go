//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v7/domain"
)

func transferActor(t *testing.T, r *domain.Root, actor *domain.ExecutionIdentity) (*domain.Root, func()) {
	t.Helper()
	scoped, close, err := r.WithExecution(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	return scoped, close
}

func prepareTransferActorRoot(t *testing.T, x *fixture, actor *domain.ExecutionIdentity) {
	t.Helper()
	x.r.Config.Execution = true
	if actor != nil {
		if err := os.Chown(x.data, int(actor.UID), int(actor.GID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(x.data, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestTransferIndependentActorsResumeAndBindRequest(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("execution identity requires root")
	}
	sourceActor := &domain.ExecutionIdentity{UID: 32021, GID: 32021}
	for _, test := range []struct {
		name        string
		targetActor *domain.ExecutionIdentity
	}{
		{"different_actor", &domain.ExecutionIdentity{UID: 32022, GID: 32022}},
		{"explicit_service", nil},
		{"legacy_shared_actor", sourceActor},
	} {
		t.Run(test.name, func(t *testing.T) {
			src, dst := transferRoots(t)
			prepareTransferActorRoot(t, src, sourceActor)
			prepareTransferActorRoot(t, dst, test.targetActor)
			put(t, src.r, "original", "private source bytes", domain.WriteOptions{})
			if err := os.Chown(filepath.Join(src.data, "original"), int(sourceActor.UID), int(sourceActor.GID)); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(src.data, "original"), 0600); err != nil {
				t.Fatal(err)
			}
			// Source is readable but cannot be detached yet. Publication must use
			// the independent destination actor, then preserve a pending receipt.
			if err := os.Chmod(src.data, 0500); err != nil {
				t.Fatal(err)
			}
			source, closeSource := transferActor(t, src.r, sourceActor)
			target, closeTarget := transferActor(t, dst.r, test.targetActor)
			id := uuid.NewString()
			pending, err := domain.TransferMove(ctx, source, "original", target, "copy", domain.WriteOptions{}, id)
			if err != nil || pending.State != domain.TransferSourcePending {
				t.Fatal(pending, err)
			}
			if got := read(t, dst.r, "copy"); got != "private source bytes" {
				t.Fatal(got)
			}
			if _, err := target.TransferStatus(id); err != nil {
				t.Fatal(err)
			}
			// A changed source or destination identity must never reuse this UUID.
			if _, err := domain.TransferMove(ctx, src.r, "original", target, "copy", domain.WriteOptions{}, id); !errors.Is(err, domain.ErrIdempotencyConflict) {
				t.Fatal("changed source identity reused transfer", err)
			}
			wrong, closeWrong := transferActor(t, dst.r, &domain.ExecutionIdentity{UID: 32023, GID: 32023})
			if _, err := domain.TransferMove(ctx, source, "original", wrong, "copy", domain.WriteOptions{}, id); !errors.Is(err, domain.ErrIdempotencyConflict) {
				t.Fatal("changed destination identity reused transfer", err)
			}
			if _, err := wrong.TransferStatus(id); !errors.Is(err, os.ErrPermission) {
				t.Fatal("wrong destination status", err)
			}
			if _, err := wrong.AbandonTransfer(id); !errors.Is(err, os.ErrPermission) {
				t.Fatal("wrong destination abandon", err)
			}
			if _, err := domain.ResumeTransfer(ctx, src.r, wrong, id); !errors.Is(err, os.ErrPermission) {
				t.Fatal("wrong destination resume", err)
			}
			closeWrong()
			closeSource()
			closeTarget()
			if test.name == "legacy_shared_actor" {
				// A v6 receipt contains only Execution; no destination marker.
				var old map[string]json.RawMessage
				if err := dst.state.Get("transfer/receipt/"+id, &old); err != nil {
					t.Fatal(err)
				}
				delete(old, "DestinationExecution")
				if err := dst.state.Put("transfer/receipt/"+id, old); err != nil {
					t.Fatal(err)
				}
			}
			reopen(t, src)
			reopen(t, dst)
			// Restoring a receipt must not replace a denied source actor with the
			// root service, even when called through unscoped backend roots.
			if _, err := domain.ResumeTransfer(ctx, src.r, dst.r, id); !errors.Is(err, os.ErrPermission) {
				t.Fatal("resume bypassed source permissions", err)
			}
			if err := os.Chmod(src.data, 0700); err != nil {
				t.Fatal(err)
			}
			result, err := domain.ResumeTransfer(ctx, src.r, dst.r, id)
			if err != nil || result.State != domain.TransferCompleted {
				t.Fatal(result, err)
			}
			if _, err := os.Stat(filepath.Join(src.data, "original")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source not removed", err)
			}
			if got := read(t, dst.r, "copy"); got != "private source bytes" {
				t.Fatal(got)
			}
		})
	}
}

func TestTransferServiceSourceAndActorTarget(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("execution identity requires root")
	}
	src, dst := transferRoots(t)
	actor := &domain.ExecutionIdentity{UID: 32024, GID: 32024}
	prepareTransferActorRoot(t, dst, actor)
	put(t, src.r, "original", "service bytes", domain.WriteOptions{})
	if err := os.Chmod(dst.data, 0500); err != nil {
		t.Fatal(err)
	}
	target, closeTarget := transferActor(t, dst.r, actor)
	id := uuid.NewString()
	_, err := domain.TransferMove(ctx, src.r, "original", target, "copy", domain.WriteOptions{}, id)
	closeTarget()
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("destination actor permissions bypassed", err)
	}
	reopen(t, src)
	reopen(t, dst)
	if _, err := domain.ResumeTransfer(ctx, src.r, dst.r, id); !errors.Is(err, os.ErrPermission) {
		t.Fatal("resume bypassed destination actor permissions", err)
	}
	if _, err := os.Stat(filepath.Join(dst.data, "copy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied copy published", err)
	}
	if err := os.Chmod(dst.data, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := domain.ResumeTransfer(ctx, src.r, dst.r, id)
	if err != nil || result.State != domain.TransferCompleted {
		t.Fatal(result, err)
	}
	if got := read(t, dst.r, "copy"); got != "service bytes" {
		t.Fatal(got)
	}
}
