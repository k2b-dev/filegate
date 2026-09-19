package domain

import (
	"context"
	"fmt"
	"os"
	"slices"
)

// ExecutionIdentity is a technical Unix identity supplied by a trusted backend.
// Ownership remains a separate, privileged provisioning instruction.
type ExecutionIdentity struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups"`
}

// ExecutionFiles must mediate live metadata mutations as well as path access.
// A scoped adapter must not inherit privileged os.File methods accidentally.
type ExecutionFiles interface {
	Files
	MetadataOpen(string) (*os.File, error)
	Chmod(*os.File, os.FileMode) error
	Chown(*os.File, int, int) error
}

func NormalizeExecution(in *ExecutionIdentity) (*ExecutionIdentity, error) {
	if in == nil {
		return nil, nil
	}
	if in.UID == 0 || in.UID == ^uint32(0) || in.GID == ^uint32(0) || len(in.Groups) > 64 {
		return nil, fmt.Errorf("%w: execution requires a nonzero UID, valid GIDs and at most 64 supplementary groups", ErrInvalid)
	}
	out := *in
	out.Groups = append([]uint32{}, in.Groups...)
	for _, g := range out.Groups {
		if g == ^uint32(0) {
			return nil, ErrInvalid
		}
	}
	slices.Sort(out.Groups)
	out.Groups = slices.Compact(out.Groups)
	return &out, nil
}

// WithExecution creates a request view. Close it only after all file operations
// have finished. Locks, durable state and recovery remain owned by the root.
func (r *Root) WithExecution(ctx context.Context, identity *ExecutionIdentity) (*Root, func(), error) {
	identity, err := NormalizeExecution(identity)
	if err != nil {
		return nil, nil, err
	}
	base := r
	if r.control != nil {
		base = r.control
	}
	if identity == nil {
		return base, func() {}, nil
	}
	if !base.Config.Execution {
		return nil, nil, fmt.Errorf("%w: Unix execution is disabled for this root", ErrDisabled)
	}
	provider, ok := base.Files.(interface {
		WithExecution(context.Context, uint32, uint32, []uint32) (Files, func(), error)
	})
	if !ok {
		return nil, nil, fmt.Errorf("%w: Unix execution requires Linux", ErrDisabled)
	}
	files, close, err := provider.WithExecution(ctx, identity.UID, identity.GID, identity.Groups)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := files.(ExecutionFiles); !ok {
		close()
		return nil, nil, fmt.Errorf("%w: execution adapter cannot enforce metadata permissions", ErrDisabled)
	}
	view := &Root{rootShared: base.rootShared, Config: base.Config, Files: files, State: base.State, MaxBytes: base.MaxBytes, control: base, execution: identity}
	return view, close, nil
}

func (r *Root) Execution() *ExecutionIdentity {
	out, _ := NormalizeExecution(r.execution)
	return out
}

func sameExecution(a, b *ExecutionIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UID == b.UID && a.GID == b.GID && slices.Equal(a.Groups, b.Groups)
}

// Metadata handles are never used for reading content. The execution adapter
// checks traversal and pins the inode before the service reads its metadata.
func (r *Root) openMetadata(p string) (*os.File, error) {
	if f, ok := r.Files.(interface {
		MetadataOpen(string) (*os.File, error)
	}); ok {
		return f.MetadataOpen(p)
	}
	return r.Files.Open(p, os.O_RDONLY, 0)
}

func (r *Root) chmod(f *os.File, mode os.FileMode) error {
	if fs, ok := r.Files.(interface {
		Chmod(*os.File, os.FileMode) error
	}); ok {
		if err := fs.Chmod(f, mode); err != nil {
			return err
		}
		return verifyMode(f, mode)
	}
	return chmod(f, mode)
}
