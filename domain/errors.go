package domain

import "fmt"

// Specific conflicts retain ErrConflict classification for domain callers while
// allowing HTTP clients to distinguish an occupied path from key misuse.
var (
	ErrPathConflict        = fmt.Errorf("%w: target path already exists", ErrConflict)
	ErrIdempotencyConflict = fmt.Errorf("%w: idempotency key was already used with different parameters", ErrConflict)
)

// Preserve the confirmed target collision's filesystem cause for domain callers.
func pathConflict(err error) error {
	return fmt.Errorf("%w: %w", ErrPathConflict, err)
}

// A raw EEXIST can come from private staging as well as the public destination.
// Only a positively observed target may be classified as an occupied path.
func (r *Root) classifyPathConflict(target string, err error) error {
	if _, statErr := r.Files.Stat(target); statErr == nil {
		return pathConflict(err)
	}
	return ErrConflict
}
