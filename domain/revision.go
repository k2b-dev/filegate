package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Precondition applies to the requested file path, never a renamed substitute.
// IfMatch is an opaque Node.Revision; IfNoneMatch requires an absent destination.
type Precondition struct {
	IfMatch     string `json:"ifMatch,omitempty"`
	IfNoneMatch bool   `json:"ifNoneMatch,omitempty"`
}

// A managed root promises that all writers use Filegate. The fingerprint detects
// observed external mutations but cannot turn POSIX rename into an external CAS.
type fileFingerprint struct {
	Device          uint64
	Inode           uint64
	Size            int64
	ModifiedSeconds int64
	ModifiedNanos   int64
	ChangedSeconds  int64
	ChangedNanos    int64
}
type managedRevision struct {
	Token       string
	Fingerprint fileFingerprint
}

const managedRevisionPrefix = "managed/revision/"

func validatePrecondition(o WriteOptions) error {
	p := o.Precondition
	if p == nil {
		return nil
	}
	if (p.IfMatch == "" && !p.IfNoneMatch || p.IfMatch != "" && p.IfNoneMatch) || o.OnConflict == "rename" || p.IfNoneMatch && o.OnConflict == "overwrite" {
		return fmt.Errorf("%w: choose one publication condition; conditions cannot rename or overwrite an IfNoneMatch target", ErrInvalid)
	}
	if len(p.IfMatch) > 128 || strings.ContainsAny(p.IfMatch, "\"\\\r\n\x00") {
		return fmt.Errorf("%w: ifMatch must be an opaque revision", ErrInvalid)
	}
	return nil
}
func (r *Root) fingerprint(st os.FileInfo) (fileFingerprint, error) {
	provider, ok := r.Files.(interface {
		ChangeTime(os.FileInfo) (int64, int64)
	})
	if !ok {
		return fileFingerprint{}, fmt.Errorf("%w: managed revisions require kernel change timestamps", ErrDisabled)
	}
	dev, ino, _, _, _ := r.Files.Identity(st)
	sec, nsec := provider.ChangeTime(st)
	m := st.ModTime()
	return fileFingerprint{Device: dev, Inode: ino, Size: st.Size(), ModifiedSeconds: m.Unix(), ModifiedNanos: int64(m.Nanosecond()), ChangedSeconds: sec, ChangedNanos: nsec}, nil
}

// revisionFor never assigns a file ID or changes the live inode. An explicit read
// may persist a new token when no record exists or an external change is observed.
func (r *Root) revisionFor(p string, st os.FileInfo, assign bool) (string, error) {
	if !r.Config.Managed || !st.Mode().IsRegular() {
		return "", nil
	}
	fingerprint, err := r.fingerprint(st)
	if err != nil {
		return "", err
	}
	var rec managedRevision
	err = r.State.Get(managedRevisionPrefix+p, &rec)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && rec.Fingerprint == fingerprint {
		return rec.Token, nil
	}
	if !assign {
		return "", nil
	}
	rec = managedRevision{Token: newID(), Fingerprint: fingerprint}
	if err = r.State.Put(managedRevisionPrefix+p, rec); err != nil {
		return "", err
	}
	return rec.Token, nil
}

func (r *Root) withRevision(n Node, f *os.File, assign bool) (Node, error) {
	if !r.Config.Managed || n.Directory {
		return n, nil
	}
	st, err := f.Stat()
	if err != nil {
		return n, err
	}
	n.Revision, err = r.revisionFor(n.Path, st, assign)
	return n, err
}

// checkPrecondition runs under the root lock, before parents, xattrs, history or
// publication intents can change. A failed condition leaves the destination alone.
func (r *Root) checkPrecondition(p string, condition *Precondition) error {
	if condition == nil {
		return nil
	}
	if !r.Config.Managed {
		return fmt.Errorf("%w: publication conditions require a managed root with exclusive Filegate writers", ErrDisabled)
	}
	if err := r.guard(); err != nil {
		return err
	}
	st, err := r.Files.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		if condition.IfNoneMatch {
			return nil
		}
		return fmt.Errorf("%w: target does not exist", ErrPrecondition)
	}
	if err != nil {
		return err
	}
	if condition.IfNoneMatch {
		return fmt.Errorf("%w: target already exists", ErrPrecondition)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: target is not a regular file", ErrPrecondition)
	}
	token, err := r.revisionFor(p, st, false)
	if err != nil {
		return err
	}
	if token == "" || token != condition.IfMatch {
		return fmt.Errorf("%w: target revision changed", ErrPrecondition)
	}
	return nil
}

// publicationRevision is finalized after rename, whose ctime change must be part
// of the persisted fingerprint. The token was already recorded in the intent so
// recovery and immutable session receipts retain exactly the same revision.
func (r *Root) publicationRevision(p publication) (*Change, error) {
	if p.Node.Revision == "" {
		return nil, nil
	}
	service := r
	if r.control != nil {
		service = r.control
	}
	st, err := service.Files.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	fingerprint, err := service.fingerprint(st)
	if err != nil {
		return nil, err
	}
	if fingerprint.Device != p.Claim.Device || fingerprint.Inode != p.Claim.Inode {
		return nil, fmt.Errorf("%w: publication inode changed", ErrConflict)
	}
	c, err := encoded(managedRevisionPrefix+p.Path, managedRevision{Token: p.Node.Revision, Fingerprint: fingerprint})
	return &c, err
}

// finishRevisionMutation applies bounded batches while the durable mutation
// intent guards access. Each moved record deletes its source key in the same
// batch, so recovery resumes after the last completed batch without losing tokens.
func (r *Root) finishRevisionMutation(from, to string, remove bool) error {
	changes := make([]Change, 0, 256)
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		if err := r.State.Batch(changes); err != nil {
			return err
		}
		changes = changes[:0]
		return nil
	}
	apply := func(k string, b []byte) error {
		if len(changes) > 254 {
			if err := flush(); err != nil {
				return err
			}
		}
		var rec managedRevision
		if err := json.Unmarshal(b, &rec); err != nil {
			return err
		}
		changes = append(changes, Change{Key: k, Delete: true})
		if remove {
			if len(changes) >= 256 {
				return flush()
			}
			return nil
		}
		source := strings.TrimPrefix(k, managedRevisionPrefix)
		target := to + strings.TrimPrefix(source, from)
		if source == from {
			// Native rename changes this inode's ctime, not descendant inodes.
			// Coordination metadata is read by the service after the authorized
			// namespace operation; this must not add recursive read permission
			// requirements to an ordinary directory rename.
			service := r
			if r.control != nil {
				service = r.control
			}
			st, err := service.Files.Stat(target)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			fp, err := service.fingerprint(st)
			if err != nil {
				return err
			}
			if fp.Device != rec.Fingerprint.Device || fp.Inode != rec.Fingerprint.Inode {
				return nil
			}
			rec.Fingerprint = fp
		}
		c, err := encoded(managedRevisionPrefix+target, rec)
		if err != nil {
			return err
		}
		changes = append(changes, c)
		if len(changes) >= 256 {
			return flush()
		}
		return nil
	}
	var rec managedRevision
	if err := r.State.Get(managedRevisionPrefix+from, &rec); err == nil {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err = apply(managedRevisionPrefix+from, b); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := r.State.Scan(managedRevisionPrefix+from+"/", apply); err != nil {
		return err
	}
	return flush()
}

// The write generation is a conservative root-wide proof used by recoverable
// cross-root transfers. It advances atomically with each namespace publication.
func (r *Root) writeGeneration() (string, error) {
	var token string
	err := r.State.Get("root/write-generation", &token)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return token, err
}
func generationChange(token string) Change {
	return Change{Key: "root/write-generation", Value: []byte(strconv.Quote(token))}
}

// initializeManagedSetting invalidates outstanding transfer proofs whenever the
// operator changes the exclusive-writer contract. It runs after local recovery:
// an old publication must not restore an earlier generation after this barrier.
func (r *Root) initializeManagedSetting() error {
	var previous bool
	err := r.State.Get("root/managed", &previous)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && previous == r.Config.Managed {
		return nil
	}
	setting, err := encoded("root/managed", r.Config.Managed)
	if err != nil {
		return err
	}
	return r.State.Batch([]Change{setting, generationChange(newID())})
}
