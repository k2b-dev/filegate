//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v5/domain"
)

func transferRoots(t *testing.T) (*fixture, *fixture) {
	t.Helper()
	src, dst := setup(t, false, false), setup(t, false, false)
	src.r.Config.Name, dst.r.Config.Name = "source", "destination"
	src.r.Config.Managed, dst.r.Config.Managed = true, true
	reopen(t, src)
	reopen(t, dst)
	return src, dst
}

type transferDeniedRemove struct {
	domain.Files
	path string
}

func (f *transferDeniedRemove) ChangeTime(st os.FileInfo) (int64, int64) {
	return f.Files.(interface {
		ChangeTime(os.FileInfo) (int64, int64)
	}).ChangeTime(st)
}

func (f *transferDeniedRemove) Rename(from, to string, replace bool) error {
	if from == f.path && strings.HasPrefix(to, ".filegate/") {
		return os.ErrPermission
	}
	return f.Files.Rename(from, to, replace)
}
func pendingTransfer(t *testing.T, src, dst *fixture) domain.TransferResult {
	t.Helper()
	put(t, src.r, "original", "original bytes", domain.WriteOptions{})
	src.r.Files = &transferDeniedRemove{Files: src.files, path: "original"}
	result, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, uuid.NewString())
	if err != nil || result.State != domain.TransferSourcePending || result.Node == nil {
		t.Fatalf("pending transfer: %+v %v", result, err)
	}
	src.r.Files = src.files
	return result
}

func TestTransferReceiptLostResponseAndImmutableRequest(t *testing.T) {
	src, dst := transferRoots(t)
	put(t, src.r, "original", "hello", domain.WriteOptions{})
	id := uuid.NewString()
	first, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, id)
	if err != nil || first.State != domain.TransferCompleted {
		t.Fatal(first, err)
	}
	if _, err = src.r.Stat("original"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source retained", err)
	}
	reopen(t, src)
	reopen(t, dst)
	// The response was lost; replay succeeds although the original path is gone.
	again, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, id)
	if err != nil || again.State != domain.TransferCompleted || *again.Node != *first.Node {
		t.Fatal(again, err)
	}
	if _, err = domain.TransferMove(ctx, src.r, "original", dst.r, "elsewhere", domain.WriteOptions{}, id); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("id reused with another target", err)
	}
	if got := read(t, dst.r, "copy"); got != "hello" {
		t.Fatal(got)
	}
}

func TestTransferPendingResumeRequiresUnchangedRoots(t *testing.T) {
	for _, changed := range []string{"source", "destination"} {
		t.Run(changed, func(t *testing.T) {
			src, dst := transferRoots(t)
			pending := pendingTransfer(t, src, dst)
			root := src.r
			if changed == "destination" {
				root = dst.r
			}
			put(t, root, "unrelated", "new data", domain.WriteOptions{})
			if _, err := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID); !errors.Is(err, domain.ErrPrecondition) {
				t.Fatal("changed root allowed deletion", err)
			}
			if got := read(t, src.r, "original"); got != "original bytes" {
				t.Fatal(got)
			}
			if got := read(t, dst.r, "copy"); got != "original bytes" {
				t.Fatal(got)
			}
			abandoned, err := dst.r.AbandonTransfer(pending.ID)
			if err != nil || abandoned.State != domain.TransferAbandoned {
				t.Fatal(abandoned, err)
			}
			if got, err := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID); err != nil || got.State != domain.TransferAbandoned {
				t.Fatal(got, err)
			}
		})
	}
}

func TestTransferPendingResumeAfterRestart(t *testing.T) {
	src, dst := transferRoots(t)
	pending := pendingTransfer(t, src, dst)
	reopen(t, src)
	reopen(t, dst)
	got, err := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID)
	if err != nil || got.State != domain.TransferCompleted || *got.Node != *pending.Node {
		t.Fatal(got, err)
	}
	if _, err = src.r.Stat("original"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

type transferBatchFailure struct {
	domain.State
	state     string
	deleteAck bool
	fail      bool
}

func (s *transferBatchFailure) Batch(changes []domain.Change) error {
	if s.fail {
		for _, c := range changes {
			if s.deleteAck && strings.HasPrefix(c.Key, "transfer-delete/") && !c.Delete {
				return errors.New("injected transfer delete receipt failure")
			}
			if strings.HasPrefix(c.Key, "transfer/receipt/") && !c.Delete {
				var r struct{ State string }
				if err := json.Unmarshal(c.Value, &r); err != nil {
					return err
				}
				if r.State == s.state {
					return errors.New("injected transfer phase failure")
				}
			}
		}
	}
	return s.State.Batch(changes)
}

func TestTransferPublicationRecoveryDoesNotCopyTwice(t *testing.T) {
	src, dst := transferRoots(t)
	put(t, src.r, "original", "source", domain.WriteOptions{})
	dst.r.State = &transferBatchFailure{State: dst.state, state: domain.TransferSourcePending, fail: true}
	id := uuid.NewString()
	if _, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, id); err == nil {
		t.Fatal("expected publication receipt failure")
	}
	if b, err := os.ReadFile(filepath.Join(dst.data, "copy")); err != nil || string(b) != "source" {
		t.Fatal(string(b), err)
	}
	if read(t, src.r, "original") != "source" {
		t.Fatal("source removed before publication receipt")
	}
	reopen(t, dst)
	status, err := dst.r.TransferStatus(id)
	if err != nil || status.State != domain.TransferSourcePending {
		t.Fatal(status, err)
	}
	got, err := domain.ResumeTransfer(ctx, src.r, dst.r, id)
	if err != nil || got.State != domain.TransferCompleted {
		t.Fatal(got, err)
	}
}

func TestTransferDeleteAcknowledgementProtectsRecreatedPath(t *testing.T) {
	for _, phase := range []string{"source-ack", "destination-complete"} {
		t.Run(phase, func(t *testing.T) {
			src, dst := transferRoots(t)
			put(t, src.r, "original", "old", domain.WriteOptions{})
			if phase == "source-ack" {
				src.r.State = &transferBatchFailure{State: src.state, deleteAck: true, fail: true}
			} else {
				dst.r.State = &transferBatchFailure{State: dst.state, state: domain.TransferCompleted, fail: true}
			}
			id := uuid.NewString()
			got, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, id)
			if err != nil || got.State != domain.TransferSourcePending {
				t.Fatal(got, err)
			}
			reopen(t, src)
			reopen(t, dst)
			put(t, src.r, "original", "replacement must survive", domain.WriteOptions{})
			got, err = domain.ResumeTransfer(ctx, src.r, dst.r, id)
			if err != nil || got.State != domain.TransferCompleted {
				t.Fatal(got, err)
			}
			if read(t, src.r, "original") != "replacement must survive" {
				t.Fatal("recreated source was deleted")
			}
			if read(t, dst.r, "copy") != "old" {
				t.Fatal("destination overwritten")
			}
		})
	}
}

func TestTransferPreparedRetriesAndPendingCapacity(t *testing.T) {
	src, dst := transferRoots(t)
	put(t, src.r, "original", "source", domain.WriteOptions{})
	put(t, dst.r, "occupied", "existing", domain.WriteOptions{})
	var ids []string
	for i := 0; i < domain.TransferPendingLimit; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		if _, err := domain.TransferMove(ctx, src.r, "original", dst.r, "occupied", domain.WriteOptions{}, id); !errors.Is(err, domain.ErrConflict) && !errors.Is(err, os.ErrExist) {
			t.Fatal(i, err)
		}
	}
	if _, err := domain.TransferMove(ctx, src.r, "original", dst.r, "occupied", domain.WriteOptions{}, uuid.NewString()); !errors.Is(err, domain.ErrLimit) {
		t.Fatal("pending limit not enforced", err)
	}
	if _, err := dst.r.AbandonTransfer(ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := dst.r.Remove("occupied", false); err != nil {
		t.Fatal(err)
	}
	// A failed preparation published no target. Its retry uses current target
	// conflict rules; destination changes before publication are harmless.
	got, err := domain.ResumeTransfer(ctx, src.r, dst.r, ids[0])
	if err != nil || got.State != domain.TransferCompleted {
		t.Fatal(got, err)
	}
	if read(t, dst.r, "occupied") != "source" {
		t.Fatal("missing retried copy")
	}
}

func TestTransferRequiresManagedAndBoundRoots(t *testing.T) {
	src, dst := transferRoots(t)
	put(t, src.r, "original", "source", domain.WriteOptions{})
	dst.r.Config.Managed = false
	if _, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, uuid.NewString()); !errors.Is(err, domain.ErrDisabled) {
		t.Fatal(err)
	}
	dst.r.Config.Managed = true
	src.r.Files = &transferDeniedRemove{Files: src.files, path: "original"}
	pending, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, uuid.NewString())
	if err != nil || pending.State != domain.TransferSourcePending {
		t.Fatal(pending, err)
	}
	other := setup(t, false, false)
	other.r.Config.Name = "source"
	other.r.Config.Managed = true
	put(t, other.r, "original", "unrelated store", domain.WriteOptions{})
	if _, err = domain.ResumeTransfer(ctx, other.r, dst.r, pending.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("rebound root accepted", err)
	}
	if read(t, other.r, "original") != "unrelated store" {
		t.Fatal("unrelated root mutated")
	}
}

func expireTransfer(t *testing.T, dst *fixture, id string) {
	t.Helper()
	var receipt map[string]any
	if err := dst.state.Get("transfer/receipt/"+id, &receipt); err != nil {
		t.Fatal(err)
	}
	original, err := time.Parse(time.RFC3339Nano, receipt["RetainUntil"].(string))
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	receipt["RetainUntil"] = past
	b, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	idJSON, _ := json.Marshal(id)
	key := func(at time.Time) string {
		return "transfer/expiry/" + at.UTC().Format("20060102T150405.000000000Z") + "/" + id
	}
	if err := dst.state.Batch([]domain.Change{{Key: key(original), Delete: true}, {Key: key(past), Value: idJSON}, {Key: "transfer/receipt/" + id, Value: b}}); err != nil {
		t.Fatal(err)
	}
}
func TestTransferRetentionKeepsPendingAndUnacknowledgedTerminal(t *testing.T) {
	src, dst := transferRoots(t)
	pending := pendingTransfer(t, src, dst)
	if err := domain.CleanupTransfers(ctx, map[string]*domain.Root{"destination": dst.r}); err != nil {
		t.Fatal(err)
	}
	if got, err := dst.r.TransferStatus(pending.ID); err != nil || got.State != domain.TransferSourcePending {
		t.Fatal(got, err)
	}
	if _, err := dst.r.AbandonTransfer(pending.ID); err != nil {
		t.Fatal(err)
	}
	expireTransfer(t, dst, pending.ID)
	if err := domain.CleanupTransfers(ctx, map[string]*domain.Root{"destination": dst.r}); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.r.TransferStatus(pending.ID); err != nil {
		t.Fatal("forgot receipt while source unavailable", err)
	}
	if err := domain.CleanupTransfers(ctx, map[string]*domain.Root{"source": src.r, "destination": dst.r}); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.r.TransferStatus(pending.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("terminal receipt did not expire", err)
	}
	if read(t, src.r, "original") != "original bytes" || read(t, dst.r, "copy") != "original bytes" {
		t.Fatal("abandon/cleanup removed content")
	}
}

func TestTransferDirectoryProofIncludesNestedWrites(t *testing.T) {
	for _, changed := range []string{"source", "destination"} {
		t.Run(changed, func(t *testing.T) {
			src, dst := transferRoots(t)
			put(t, src.r, "original/nested/file", "old bytes", domain.WriteOptions{})
			src.r.Files = &transferDeniedRemove{Files: src.files, path: "original"}
			pending, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, uuid.NewString())
			if err != nil || pending.State != domain.TransferSourcePending || !pending.Node.Directory {
				t.Fatal(pending, err)
			}
			src.r.Files = src.files
			root, p := src.r, "original/nested/file"
			if changed == "destination" {
				root, p = dst.r, "copy/nested/file"
			}
			put(t, root, p, "changed nested bytes", domain.WriteOptions{OnConflict: "overwrite"})
			if _, err = domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID); !errors.Is(err, domain.ErrPrecondition) {
				t.Fatal("nested change did not prevent source deletion", err)
			}
			if _, err = src.r.Stat("original/nested/file"); err != nil {
				t.Fatal("source tree was removed", err)
			}
		})
	}
}

func TestTransferConcurrentResumeAndAbandon(t *testing.T) {
	for i := 0; i < 8; i++ {
		src, dst := transferRoots(t)
		pending := pendingTransfer(t, src, dst)
		start := make(chan struct{})
		type outcome struct {
			result domain.TransferResult
			err    error
		}
		results := make(chan outcome, 2)
		go func() {
			<-start
			r, e := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID)
			results <- outcome{r, e}
		}()
		go func() { <-start; r, e := dst.r.AbandonTransfer(pending.ID); results <- outcome{r, e} }()
		close(start)
		a, b := <-results, <-results
		if a.err != nil || b.err != nil || a.result.State != b.result.State {
			t.Fatalf("competing outcomes: %+v %+v", a, b)
		}
		_, err := src.r.Stat("original")
		switch a.result.State {
		case domain.TransferAbandoned:
			if err != nil {
				t.Fatal("abandon lost source", err)
			}
		case domain.TransferCompleted:
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed move retained source", err)
			}
		default:
			t.Fatal("not terminal", a.result)
		}
		if read(t, dst.r, "copy") != "original bytes" {
			t.Fatal("target changed")
		}
	}
}

func TestTransferManagedModeChangesInvalidatePendingProof(t *testing.T) {
	for _, changed := range []string{"source", "destination"} {
		t.Run(changed, func(t *testing.T) {
			src, dst := transferRoots(t)
			pending := pendingTransfer(t, src, dst)
			x, p := src, "original"
			if changed == "destination" {
				x, p = dst, "copy"
			}
			x.r.Config.Managed = false
			reopen(t, x)
			put(t, x.r, p, "written while unmanaged", domain.WriteOptions{OnConflict: "overwrite"})
			x.r.Config.Managed = true
			reopen(t, x)
			if _, err := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID); !errors.Is(err, domain.ErrPrecondition) {
				t.Fatal("managed toggle retained stale proof", err)
			}
			if _, err := src.r.Stat("original"); err != nil {
				t.Fatal("source deleted", err)
			}
			if read(t, x.r, p) != "written while unmanaged" {
				t.Fatal("new content lost")
			}
		})
	}
}

type transferAckCleanupFailure struct {
	domain.State
	fail bool
}

func (s *transferAckCleanupFailure) Delete(key string) error {
	if s.fail && strings.HasPrefix(key, "transfer-delete/") {
		return errors.New("injected ack cleanup failure")
	}
	return s.State.Delete(key)
}
func TestTransferExpiredIDCannotReuseStaleDeleteAcknowledgement(t *testing.T) {
	src, dst := transferRoots(t)
	put(t, src.r, "original", "old bytes", domain.WriteOptions{})
	state := &transferAckCleanupFailure{State: src.state, fail: true}
	src.r.State = state
	id := uuid.NewString()
	result, err := domain.TransferMove(ctx, src.r, "original", dst.r, "copy", domain.WriteOptions{}, id)
	if err != nil || result.State != domain.TransferCompleted {
		t.Fatal(result, err)
	}
	expireTransfer(t, dst, id)
	put(t, src.r, "original", "new bytes", domain.WriteOptions{})
	roots := map[string]*domain.Root{"source": src.r, "destination": dst.r}
	if err = domain.CleanupTransfers(ctx, roots); err == nil {
		t.Fatal("cleanup failure ignored")
	}
	if result, err = dst.r.TransferStatus(id); err != nil || result.State != domain.TransferCompleted {
		t.Fatal("receipt expired before ack cleanup", result, err)
	}
	state.fail = false
	if err = domain.CleanupTransfers(ctx, roots); err != nil {
		t.Fatal(err)
	}
	if _, err = dst.r.TransferStatus(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt retained after safe cleanup", err)
	}
	result, err = domain.TransferMove(ctx, src.r, "original", dst.r, "new-copy", domain.WriteOptions{}, id)
	if err != nil || result.State != domain.TransferCompleted {
		t.Fatal(result, err)
	}
	if _, err = src.r.Stat("original"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old ack skipped the new source deletion", err)
	}
	if read(t, dst.r, "new-copy") != "new bytes" {
		t.Fatal("wrong reused-ID content")
	}
}

func TestTransferAcknowledgedDeletionFinalizesWithCapabilitiesDisabled(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("execution identity requires root")
	}
	src, dst := transferRoots(t)
	for _, x := range []*fixture{src, dst} {
		x.r.Config.Execution = true
		if err := os.Chmod(x.data, 0777); err != nil {
			t.Fatal(err)
		}
	}
	put(t, src.r, "original", "old bytes", domain.WriteOptions{})
	dst.r.State = &transferBatchFailure{State: dst.state, state: domain.TransferCompleted, fail: true}
	identity := &domain.ExecutionIdentity{UID: 31021, GID: 31021, Groups: []uint32{31021}}
	source, closeSource, err := src.r.WithExecution(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	destination, closeDestination, err := dst.r.WithExecution(ctx, identity)
	if err != nil {
		closeSource()
		t.Fatal(err)
	}
	id := uuid.NewString()
	pending, err := domain.TransferMove(ctx, source, "original", destination, "copy", domain.WriteOptions{}, id)
	closeSource()
	closeDestination()
	if err != nil || pending.State != domain.TransferSourcePending {
		t.Fatal(pending, err)
	}
	wrong, closeWrong, err := src.r.WithExecution(ctx, &domain.ExecutionIdentity{UID: 31022, GID: 31022})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = domain.ResumeTransfer(ctx, wrong, dst.r, id); !errors.Is(err, os.ErrPermission) {
		t.Fatal("mismatched actor finalized receipt", err)
	}
	closeWrong()
	put(t, src.r, "original", "replacement", domain.WriteOptions{})
	for _, x := range []*fixture{src, dst} {
		x.r.Config.Managed, x.r.Config.Execution = false, false
		reopen(t, x)
	}
	got, err := domain.ResumeTransfer(ctx, src.r, dst.r, id)
	if err != nil || got.State != domain.TransferCompleted {
		t.Fatal("durably deleted source still required capabilities", got, err)
	}
	if read(t, src.r, "original") != "replacement" {
		t.Fatal("ACK finalization touched recreated source")
	}
	if read(t, dst.r, "copy") != "old bytes" {
		t.Fatal("ACK finalization touched destination")
	}
}

func TestTransferWithoutDeleteAcknowledgementStillRequiresManagedRoots(t *testing.T) {
	src, dst := transferRoots(t)
	pending := pendingTransfer(t, src, dst)
	src.r.Config.Managed = false
	reopen(t, src)
	if _, err := domain.ResumeTransfer(ctx, src.r, dst.r, pending.ID); !errors.Is(err, domain.ErrDisabled) {
		t.Fatal("unacknowledged deletion bypassed disabled managed mode", err)
	}
	if read(t, src.r, "original") != "original bytes" {
		t.Fatal("source removed")
	}
}
