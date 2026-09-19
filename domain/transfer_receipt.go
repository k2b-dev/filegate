package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
)

const (
	TransferPrepared      = "prepared"
	TransferSourcePending = "source_pending"
	TransferCompleted     = "completed"
	TransferAbandoned     = "abandoned"
	TransferRetention     = 7 * 24 * time.Hour
	TransferPendingLimit  = 128
	transferPrefix        = "transfer/receipt/"
	transferCountKey      = "transfer/pending-count"
	transferExpiryPrefix  = "transfer/expiry/"
)

// TransferResult distinguishes an atomically published destination from a
// completed cross-root move. The destination owns the recovery receipt.
type TransferResult struct {
	Node       *Node  `json:"node,omitempty"`
	ID         string `json:"id,omitempty"`
	State      string `json:"state"`
	SourceRoot string `json:"sourceRoot,omitempty"`
}

type transferReceipt struct {
	ID                    string
	State                 string
	SourceRoot            string
	SourceStore           string
	DestinationStore      string
	SourcePath            string
	TargetPath            string
	RequestHash           string
	Options               WriteOptions
	Execution             *ExecutionIdentity
	SourceGeneration      string
	DestinationGeneration string
	Node                  *Node
	TerminalAt            *time.Time
	RetainUntil           *time.Time
	AckPending            bool
}

func validTransferID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func (t transferReceipt) result() TransferResult {
	out := TransferResult{ID: t.ID, State: t.State, SourceRoot: t.SourceRoot}
	if t.Node != nil {
		n := *t.Node
		out.Node = &n
	}
	return out
}
func (t transferReceipt) terminal() bool {
	return t.State == TransferCompleted || t.State == TransferAbandoned
}
func (t transferReceipt) ackKey() string { return "transfer-delete/" + t.DestinationStore + "/" + t.ID }
func (r *Root) storeID() (string, error) {
	var id string
	if err := r.State.Get("root/store-id", &id); err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: missing root store binding", ErrConflict)
	}
	return id, nil
}
func (r *Root) transferReceipt(id string) (transferReceipt, error) {
	if !validTransferID(id) {
		return transferReceipt{}, ErrInvalid
	}
	var t transferReceipt
	if err := r.State.Get(transferPrefix+id, &t); err != nil {
		return t, err
	}
	if t.ID != id {
		return t, fmt.Errorf("%w: invalid transfer receipt", ErrConflict)
	}
	return t, nil
}
func (r *Root) pendingTransfers() (int, error) {
	var count int
	err := r.State.Get(transferCountKey, &count)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err == nil && (count < 0 || count > TransferPendingLimit) {
		return 0, fmt.Errorf("%w: invalid transfer count", ErrConflict)
	}
	return count, err
}
func transferRequestHash(srcStore, dstStore, p, to string, o WriteOptions, execution *ExecutionIdentity) (string, error) {
	b, err := json.Marshal(struct {
		SourceStore      string
		DestinationStore string
		Path             string
		Target           string
		Options          WriteOptions
		Execution        *ExecutionIdentity
	}{srcStore, dstStore, p, to, o, execution})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// TransferMove requires a caller-chosen UUID so a lost first response remains
// recoverable. Reusing that UUID with another request is a conflict.
func TransferMove(ctx context.Context, src *Root, p string, dst *Root, to string, o WriteOptions, id string) (TransferResult, error) {
	if err := ctx.Err(); err != nil {
		return TransferResult{}, err
	}
	if !validTransferID(id) || src.rootShared == dst.rootShared || !sameExecution(src.execution, dst.execution) {
		return TransferResult{}, ErrInvalid
	}
	var err error
	if p, err = validWrite(p); err != nil {
		return TransferResult{}, err
	}
	if to, err = validWrite(to); err != nil {
		return TransferResult{}, err
	}
	if err = ValidateOptions(o); err != nil {
		return TransferResult{}, err
	}
	unlock := lockRoots(src, dst)
	defer unlock()
	if err = src.guard(); err != nil {
		return TransferResult{}, err
	}
	if err = dst.guard(); err != nil {
		return TransferResult{}, err
	}
	sourceStore, err := src.storeID()
	if err != nil {
		return TransferResult{}, err
	}
	destinationStore, err := dst.storeID()
	if err != nil {
		return TransferResult{}, err
	}
	hash, err := transferRequestHash(sourceStore, destinationStore, p, to, o, src.execution)
	if err != nil {
		return TransferResult{}, err
	}
	t, err := dst.transferReceipt(id)
	if err == nil {
		if t.RequestHash != hash || t.SourceRoot != src.Config.Name {
			return TransferResult{}, ErrConflict
		}
		if t.terminal() {
			return t.result(), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return TransferResult{}, err
	} else {
		if !src.Config.Managed || !dst.Config.Managed {
			return TransferResult{}, fmt.Errorf("%w: cross-root moves require two managed roots", ErrDisabled)
		}
		if _, err = src.node(p, false); err != nil {
			return TransferResult{}, err
		}
		count, e := dst.pendingTransfers()
		if e != nil {
			return TransferResult{}, e
		}
		if count >= TransferPendingLimit {
			return TransferResult{}, fmt.Errorf("%w: too many unresolved transfers", ErrLimit)
		}
		generation, e := src.writeGeneration()
		if e != nil {
			return TransferResult{}, e
		}
		t = transferReceipt{ID: id, State: TransferPrepared, SourceRoot: src.Config.Name, SourceStore: sourceStore, DestinationStore: destinationStore, SourcePath: p, TargetPath: to, RequestHash: hash, Options: o, Execution: src.Execution(), SourceGeneration: generation}
		receipt, e := encoded(transferPrefix+id, t)
		if e != nil {
			return TransferResult{}, e
		}
		counter, e := encoded(transferCountKey, count+1)
		if e != nil {
			return TransferResult{}, e
		}
		if e = dst.State.Batch([]Change{receipt, counter}); e != nil {
			return TransferResult{}, e
		}
	}
	result, err := resumeTransferLocked(ctx, src, dst, t)
	// Once the target is published, failure to remove the source is an explicit
	// pending move. A backend can inspect and retry its known receipt ID.
	if err != nil {
		if latest, e := dst.transferReceipt(id); e == nil && latest.State == TransferSourcePending {
			return latest.result(), nil
		}
	}
	return result, err
}

func transferBindings(src, dst *Root, t transferReceipt) error {
	if src.Config.Name != t.SourceRoot || src.rootShared == dst.rootShared {
		return ErrConflict
	}
	source, err := src.storeID()
	if err != nil {
		return err
	}
	destination, err := dst.storeID()
	if err != nil {
		return err
	}
	if source != t.SourceStore || destination != t.DestinationStore {
		return fmt.Errorf("%w: transfer root binding changed", ErrConflict)
	}
	return nil
}
func checkTransferGeneration(r *Root, want string) error {
	got, err := r.writeGeneration()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: root changed since the transfer; source preserved", ErrPrecondition)
	}
	return nil
}
func resumeTransferLocked(ctx context.Context, src, dst *Root, t transferReceipt) (TransferResult, error) {
	if err := ctx.Err(); err != nil {
		return t.result(), err
	}
	if err := transferBindings(src, dst, t); err != nil {
		return t.result(), err
	}
	if t.terminal() {
		return t.result(), nil
	}
	if !src.Config.Managed || !dst.Config.Managed {
		return t.result(), fmt.Errorf("%w: cross-root moves require two managed roots", ErrDisabled)
	}
	if !sameExecution(t.Execution, src.execution) || !sameExecution(t.Execution, dst.execution) {
		return t.result(), os.ErrPermission
	}
	if t.State == TransferPrepared {
		if err := checkTransferGeneration(src, t.SourceGeneration); err != nil {
			return t.result(), err
		}
		_, copyErr := copyLocked(ctx, src, t.SourcePath, dst, t.TargetPath, t.Options, t.ID)
		// A rename may have succeeded before a receipt/index write failed. Local
		// publication recovery resolves that seam before deciding whether to copy.
		if err := dst.guard(); err != nil {
			return t.result(), err
		}
		fresh, err := dst.transferReceipt(t.ID)
		if err != nil {
			return t.result(), err
		}
		t = fresh
		if t.State == TransferPrepared {
			if copyErr == nil {
				copyErr = fmt.Errorf("transfer publication did not persist its receipt")
			}
			return t.result(), copyErr
		}
	}
	if t.State != TransferSourcePending || t.Node == nil {
		return t.result(), ErrConflict
	}
	var deleted bool
	err := src.State.Get(t.ackKey(), &deleted)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return t.result(), err
	}
	if !deleted {
		if err = checkTransferGeneration(src, t.SourceGeneration); err != nil {
			return t.result(), err
		}
		if err = checkTransferGeneration(dst, t.DestinationGeneration); err != nil {
			return t.result(), err
		}
		if err = ctx.Err(); err != nil {
			return t.result(), err
		}
		if err = src.removeWithReceipt(t.SourcePath, true, t.ackKey()); err != nil {
			return t.result(), err
		}
	}
	return completeTransfer(src, dst, t)
}

// completeTransfer changes durable receipts only; it never repeats source removal.
func completeTransfer(src, dst *Root, t transferReceipt) (TransferResult, error) {
	finished, err := dst.finishTransfer(t, TransferCompleted)
	if err != nil {
		return t.result(), err
	}
	// Delete acknowledgement first; retaining the terminal receipt makes this
	// ordering safe even if either following write or the process fails.
	if err = src.State.Delete(t.ackKey()); err == nil {
		finished.AckPending = false
		_ = dst.State.Put(transferPrefix+t.ID, finished)
	}
	return finished.result(), nil
}

// transferPublicationChanges participates in the same batch as the destination
// publication. Startup can recover this phase without opening the source root.
func (r *Root) transferPublicationChanges(p publication) ([]Change, error) {
	if p.TransferID == "" {
		return nil, nil
	}
	t, err := r.transferReceipt(p.TransferID)
	if err != nil {
		return nil, err
	}
	if t.State != TransferPrepared || p.WriteGeneration == "" {
		return nil, ErrConflict
	}
	n := p.Node
	t.Node, t.State, t.DestinationGeneration = &n, TransferSourcePending, p.WriteGeneration
	change, err := encoded(transferPrefix+t.ID, t)
	return []Change{change}, err
}

func transferExpiryKey(t transferReceipt) string {
	return transferExpiryPrefix + t.RetainUntil.UTC().Format("20060102T150405.000000000Z") + "/" + t.ID
}
func (r *Root) finishTransfer(t transferReceipt, state string) (transferReceipt, error) {
	if t.terminal() {
		return t, nil
	}
	count, err := r.pendingTransfers()
	if err != nil {
		return t, err
	}
	if count == 0 {
		return t, fmt.Errorf("%w: missing pending transfer count", ErrConflict)
	}
	now := r.now().UTC()
	until := now.Add(TransferRetention)
	t.State, t.TerminalAt, t.RetainUntil, t.AckPending = state, &now, &until, true
	receipt, err := encoded(transferPrefix+t.ID, t)
	if err != nil {
		return t, err
	}
	counter, err := encoded(transferCountKey, count-1)
	if err != nil {
		return t, err
	}
	expiry, err := encoded(transferExpiryKey(t), t.ID)
	if err != nil {
		return t, err
	}
	err = r.State.Batch([]Change{receipt, counter, expiry})
	return t, err
}

func (r *Root) TransferStatus(id string) (TransferResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return TransferResult{}, err
	}
	t, err := r.transferReceipt(id)
	if err == nil && r.execution != nil && !sameExecution(r.execution, t.Execution) {
		return t.result(), os.ErrPermission
	}
	return t.result(), err
}

// ResumeTransfer is backend-authorized and restores the stored technical
// identity. It never substitutes daemon permissions for a failed actor action.
func ResumeTransfer(ctx context.Context, src, dst *Root, id string) (TransferResult, error) {
	if err := ctx.Err(); err != nil {
		return TransferResult{}, err
	}
	// A durable source ACK means all authorized namespace work already finished.
	// Finalize those receipts even after execution/managed capabilities are
	// disabled, without creating workers or touching filesystem contents.
	var t transferReceipt
	var result TransferResult
	var complete bool
	err := func() error {
		unlock := lockRoots(src, dst)
		defer unlock()
		var err error
		t, err = dst.transferReceipt(id)
		if err != nil {
			return err
		}
		if src.execution != nil && !sameExecution(src.execution, t.Execution) || dst.execution != nil && !sameExecution(dst.execution, t.Execution) {
			return os.ErrPermission
		}
		if err = transferBindings(src, dst, t); err != nil {
			return err
		}
		if t.terminal() {
			result, complete = t.result(), true
			return nil
		}
		if t.State == TransferSourcePending {
			var deleted bool
			if err = src.State.Get(t.ackKey(), &deleted); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if deleted {
				result, err = completeTransfer(src, dst, t)
				complete = true
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return t.result(), err
	}
	if complete {
		return result, nil
	}

	source, closeSource, err := src.WithExecution(ctx, t.Execution)
	if err != nil {
		return t.result(), err
	}
	defer closeSource()
	destination, closeDestination, err := dst.WithExecution(ctx, t.Execution)
	if err != nil {
		return t.result(), err
	}
	defer closeDestination()
	unlock := lockRoots(source, destination)
	defer unlock()
	if err = source.guard(); err != nil {
		return t.result(), err
	}
	if err = destination.guard(); err != nil {
		return t.result(), err
	}
	t, err = destination.transferReceipt(id)
	if err != nil {
		return t.result(), err
	}
	return resumeTransferLocked(ctx, source, destination, t)
}

// AbandonTransfer forgets the deletion intent; it never removes the published
// destination or the source. Its terminal receipt remains idempotently readable.
func (r *Root) AbandonTransfer(id string) (TransferResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return TransferResult{}, err
	}
	t, err := r.transferReceipt(id)
	if err != nil {
		return TransferResult{}, err
	}
	if r.execution != nil && !sameExecution(r.execution, t.Execution) {
		return t.result(), os.ErrPermission
	}
	t, err = r.finishTransfer(t, TransferAbandoned)
	return t.result(), err
}

// CleanupTransfers expires only terminal receipts. A receipt with an outstanding
// source acknowledgement survives its minimum retention until that exact source
// store is available for cleanup. Pending deletion intents never expire.
func CleanupTransfers(ctx context.Context, roots map[string]*Root) error {
	for _, dst := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst.mu.Lock()
		var cursor string
		err := dst.State.Get("transfer/cleanup-cursor", &cursor)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			dst.mu.Unlock()
			return err
		}
		var ids []string
		last := ""
		stop := errors.New("transfer cleanup page complete")
		cutoff := transferExpiryPrefix + dst.now().UTC().Format("20060102T150405.000000000Z") + "/\xff"
		err = dst.State.ScanAfter(transferExpiryPrefix, cursor, func(k string, b []byte) error {
			if k > cutoff {
				return stop
			}
			var id string
			if e := json.Unmarshal(b, &id); e != nil {
				return e
			}
			ids = append(ids, id)
			last = k
			if len(ids) == TransferPendingLimit {
				return stop
			}
			return nil
		})
		if err == nil || len(ids) < TransferPendingLimit {
			last = ""
		}
		if err == nil || errors.Is(err, stop) {
			err = dst.State.Put("transfer/cleanup-cursor", last)
		}
		dst.mu.Unlock()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := cleanupTransfer(roots, dst, id); err != nil {
				return err
			}
		}
	}
	return nil
}
func cleanupTransfer(roots map[string]*Root, dst *Root, id string) error {
	dst.mu.Lock()
	t, err := dst.transferReceipt(id)
	if err != nil {
		dst.mu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !t.terminal() || t.RetainUntil == nil || dst.now().Before(*t.RetainUntil) {
		dst.mu.Unlock()
		return nil
	}
	if !t.AckPending {
		err = dst.State.Batch([]Change{{Key: transferPrefix + id, Delete: true}, {Key: transferExpiryKey(t), Delete: true}})
		dst.mu.Unlock()
		return err
	}
	dst.mu.Unlock()
	src := roots[t.SourceRoot]
	if src == nil || src.rootShared == dst.rootShared {
		return nil
	}
	unlock := lockRoots(src, dst)
	defer unlock()
	if err := src.guard(); err != nil {
		return err
	}
	if err := dst.guard(); err != nil {
		return err
	}
	t, err = dst.transferReceipt(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !t.terminal() || t.RetainUntil == nil || dst.now().Before(*t.RetainUntil) {
		return nil
	}
	// A root name reused for a different store must not cause an old receipt to
	// erase an unrelated acknowledgement or become reusable prematurely.
	if err = transferBindings(src, dst, t); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	if err = src.State.Delete(t.ackKey()); err != nil {
		return err
	}
	return dst.State.Batch([]Change{{Key: transferPrefix + id, Delete: true}, {Key: transferExpiryKey(t), Delete: true}})
}
