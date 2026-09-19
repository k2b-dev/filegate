package domain

import (
	"context"
	"fmt"
	"os"
)

// CopyVersion publishes historical bytes at another path without restoring or
// modifying the source. Ownership, ACL, conflict and conditional-publication
// options describe the destination, whose ordinary publication rules apply.
func CopyVersion(ctx context.Context, src *Root, p, id string, dst *Root, to string, o WriteOptions) (Node, error) {
	if !src.Config.Versioning.Enabled {
		return Node{}, ErrDisabled
	}
	var err error
	if p, err = validWrite(p); err != nil {
		return Node{}, err
	}
	if to, err = validWrite(to); err != nil {
		return Node{}, err
	}
	if src.rootShared == dst.rootShared && p == to {
		return Node{}, fmt.Errorf("%w: historical copy requires another destination path", ErrInvalid)
	}
	if err = ValidateOptions(o); err != nil {
		return Node{}, err
	}
	if err = ctx.Err(); err != nil {
		return Node{}, err
	}
	unlock := lockRoots(src, dst)
	defer unlock()
	if err = src.guard(); err != nil {
		return Node{}, err
	}
	if err = dst.guard(); err != nil {
		return Node{}, err
	}
	// Current read permission authorizes history. Never assign an ID during this
	// lookup: a missing/replaced current file must remain unchanged on failure.
	current, err := src.Files.Open(p, os.O_RDONLY, 0)
	if err != nil {
		return Node{}, err
	}
	node, err := src.nodeFile(p, current, false)
	current.Close()
	if err != nil {
		return Node{}, err
	}
	if node.Directory || node.ID == "" {
		return Node{}, os.ErrNotExist
	}
	var version Version
	if err = src.State.Get("v/"+node.ID+"/"+id, &version); err != nil {
		return Node{}, err
	}
	if version.Size > dst.MaxBytes {
		return Node{}, ErrLimit
	}
	historical, err := src.Files.Open(".filegate/versions/"+version.ID, os.O_RDONLY, 0)
	if err != nil {
		return Node{}, err
	}
	defer historical.Close()
	temp := ".filegate/staging/" + newID()
	output, err := dst.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Node{}, err
	}
	defer func() { output.Close(); _ = dst.Files.Remove(temp, false) }()
	size, err := copyStream(output, &contextReader{ctx, historical}, dst.MaxBytes)
	if err != nil {
		return Node{}, err
	}
	if size != version.Size {
		return Node{}, fmt.Errorf("%w: historical content size changed", ErrConflict)
	}
	if err = ctx.Err(); err != nil {
		return Node{}, err
	}
	return dst.publish(to, temp, output, o, false, "")
}
