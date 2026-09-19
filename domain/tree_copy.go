package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

const treeEntryLimit = 100000
const treeDepthLimit = 128
const treeBatchLimit = 512

type preparedTreeNode struct {
	Node     Node
	Claim    claim
	Revision *managedRevision
	Metadata Metadata
}

func treeManifestPrefix(temp string) string { return "tree/" + path.Base(temp) + "/" }

// copyLocked prepares a complete destination before publishing its root name.
// Both root locks are held by Transfer. Parent directory creation retains the
// ordinary write contract; a failed copy never publishes a partial target tree.
func copyLocked(ctx context.Context, src *Root, p string, dst *Root, to string, o WriteOptions, transferID string) (result Node, err error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	source, err := src.Files.Open(p, os.O_RDONLY, 0)
	if err != nil {
		return Node{}, err
	}
	defer source.Close()
	st, err := source.Stat()
	if err != nil {
		return Node{}, err
	}
	if src.rootShared == dst.rootShared && p == to && o.OnConflict != "rename" {
		if o.OnConflict == "overwrite" {
			return Node{}, ErrInvalid
		}
		return Node{}, ErrConflict
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return Node{}, ErrInvalid
		}
		temp := ".filegate/staging/" + newID()
		f, err := dst.Files.Open(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return Node{}, err
		}
		defer func() { f.Close(); _ = dst.Files.Remove(temp, false) }()
		if _, err := copyStream(f, &contextReader{ctx, source}, dst.MaxBytes); err != nil {
			return Node{}, err
		}
		if err := ctx.Err(); err != nil {
			return Node{}, err
		}
		return dst.publishTransfer(to, temp, f, o, false, "", transferID)
	}
	if err := dst.checkPrecondition(to, o.Precondition); err != nil {
		return Node{}, err
	}
	target, _, err := dst.chooseTarget(to, true, o.OnConflict)
	if err != nil {
		return Node{}, err
	}
	if src.rootShared == dst.rootShared && (strings.HasPrefix(to, p+"/") || strings.HasPrefix(target, p+"/") || target == p) {
		return Node{}, ErrInvalid
	}
	if err := dst.parents(target, o.Ownership); err != nil {
		return Node{}, err
	}
	parent, inherited, err := dst.parentPermissions(target)
	unsupported := errors.Is(err, ErrACLUnsupported)
	if err != nil && !unsupported {
		return Node{}, err
	}
	if parent == nil || !parent.IsDir() {
		return Node{}, ErrInvalid
	}
	temp := ".filegate/staging/" + newID()
	if err := dst.Files.Mkdir(temp, 0700); err != nil {
		return Node{}, err
	}
	intentRecorded := false
	defer func() {
		cleanup := dst.Files.Remove(temp, true)
		if cleanup != nil && !errors.Is(cleanup, os.ErrNotExist) {
			err = errors.Join(err, cleanup)
		}
		if !intentRecorded {
			if cleanup := dst.deleteTreeManifest(temp); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	manifest := []Change{}
	flush := func() error {
		if len(manifest) == 0 {
			return nil
		}
		err := dst.State.Batch(manifest)
		manifest = manifest[:0]
		return err
	}
	entries := 0
	var rootClaim claim
	var prepare func(string, string, string, *os.File, os.FileInfo, ACL, bool, int) (Node, error)
	prepare = func(sourcePath, stagePath, relative string, input *os.File, parent os.FileInfo, inherited ACL, unsupported bool, depth int) (Node, error) {
		if err := ctx.Err(); err != nil {
			return Node{}, err
		}
		entries++
		if entries > treeEntryLimit || depth > treeDepthLimit {
			return Node{}, ErrLimit
		}
		inputInfo, err := input.Stat()
		if err != nil {
			return Node{}, err
		}
		directory := inputInfo.IsDir()
		var f *os.File
		if directory {
			f, err = dst.Files.Open(stagePath, os.O_RDONLY, 0)
		} else {
			f, err = dst.Files.Open(stagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		}
		if err != nil {
			return Node{}, err
		}
		defer f.Close()
		id := ""
		if dst.Config.Index {
			id = newID()
			if err := dst.Files.SetID(f, id); err != nil {
				return Node{}, err
			}
		}
		if directory {
			options := DirectoryOptions{Ownership: o.Ownership}
			if o.AccessACL != nil {
				options.ACL = &DirectoryACLs{Access: o.AccessACL}
			}
			if err := dst.prepareDirectory(f, parent, inherited, unsupported, options); err != nil {
				return Node{}, err
			}
			prepared, err := f.Stat()
			if err != nil {
				return Node{}, err
			}
			defaults, err := dst.Files.GetACL(f, DefaultACL)
			childUnsupported := errors.Is(err, ErrACLUnsupported)
			if err != nil && !childUnsupported {
				return Node{}, err
			}
			// Keep owner traversal while constructing children, preserving setgid and
			// the default ACL. Restore final access rights after the subtree is ready.
			if err := dst.chmod(f, 0700|prepared.Mode()&os.ModeSetgid); err != nil {
				return Node{}, err
			}
			for {
				children, readErr := input.ReadDir(256)
				for _, child := range children {
					childSource := path.Join(sourcePath, child.Name())
					if _, err := CleanPath(childSource); err != nil || child.Type()&os.ModeSymlink != 0 {
						return Node{}, fmt.Errorf("%w: transfer contains unsupported entry", ErrInvalid)
					}
					childInput, err := src.Files.Open(childSource, os.O_RDONLY, 0)
					if err != nil {
						return Node{}, err
					}
					childInfo, err := childInput.Stat()
					if err != nil {
						childInput.Close()
						return Node{}, err
					}
					childStage := path.Join(stagePath, child.Name())
					if childInfo.IsDir() {
						err = dst.Files.Mkdir(childStage, 0700)
					} else if !childInfo.Mode().IsRegular() {
						err = ErrInvalid
					}
					if err == nil {
						_, err = prepare(childSource, childStage, path.Join(relative, child.Name()), childInput, prepared, defaults, childUnsupported, depth+1)
					}
					childInput.Close()
					if err != nil {
						return Node{}, err
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return Node{}, readErr
				}
				if err := ctx.Err(); err != nil {
					return Node{}, err
				}
			}
			if err := dst.prepareDirectory(f, parent, inherited, unsupported, options); err != nil {
				return Node{}, err
			}
		} else {
			if _, err := copyStream(f, &contextReader{ctx, input}, dst.MaxBytes); err != nil {
				return Node{}, err
			}
			if err := dst.preparePublication(stagePath, f, false, o.Ownership, o.AccessACL); err != nil {
				return Node{}, err
			}
		}
		if err := f.Sync(); err != nil {
			return Node{}, err
		}
		info, err := f.Stat()
		if err != nil {
			return Node{}, err
		}
		dev, ino, uid, gid, _ := dst.Files.Identity(info)
		n := Node{Root: dst.Config.Name, Path: relative, ID: id, Directory: directory, Size: info.Size(), Modified: info.ModTime().UTC(), Mode: fmt.Sprintf("%04o", UnixMode(info.Mode())), UID: uid, GID: gid}
		if directory {
			n.Size = 0
		}
		record := preparedTreeNode{Node: n, Claim: claim{Device: dev, Inode: ino, Path: relative}, Metadata: o.Metadata}
		if dst.Config.Managed && !directory {
			fp, err := dst.fingerprint(info)
			if err != nil {
				return Node{}, err
			}
			record.Node.Revision = newID()
			record.Revision = &managedRevision{Token: record.Node.Revision, Fingerprint: fp}
			n = record.Node
		}
		if relative == "." {
			rootClaim = record.Claim
		}
		if relative != "." {
			change, err := encoded(treeManifestPrefix(temp)+relative, record)
			if err != nil {
				return Node{}, err
			}
			manifest = append(manifest, change)
			if len(manifest) >= treeBatchLimit {
				if err := flush(); err != nil {
					return Node{}, err
				}
			}
		}
		return n, nil
	}
	node, err := prepare(p, temp, ".", source, parent, inherited, unsupported, 0)
	if err != nil {
		return Node{}, err
	}
	if err := flush(); err != nil {
		return Node{}, err
	}
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	node.Path = target
	rec := publication{Tree: true, TransferID: transferID, Path: target, Temp: temp, Node: node, Claim: claim{Device: rootClaim.Device, Inode: rootClaim.Inode, Path: target}, Metadata: o.Metadata}
	if dst.Config.Managed {
		rec.WriteGeneration = newID()
	}
	key := "pending/" + newID()
	if err := dst.State.Put(key, rec); err != nil {
		return Node{}, err
	}
	intentRecorded = true
	dst.needsRecovery = true
	if err := dst.renamePublication(key, &rec, false, to, o.OnConflict); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return Node{}, ErrCrossDevice
		}
		if o.Precondition != nil && o.Precondition.IfNoneMatch && errors.Is(err, os.ErrExist) {
			return Node{}, ErrPrecondition
		}
		return Node{}, err
	}
	if err := dst.finishPublication(key, rec); err != nil {
		return Node{}, err
	}
	dst.needsRecovery = false
	dst.invalidateStats()
	return rec.Node, nil
}

// Finish from immutable prepared metadata, not a privileged walk of published
// paths. Each consumed manifest row is deleted in the same durable batch as its
// derived rows, so an interrupted finalization can replay only unfinished work.
func (r *Root) finishTreePublication(p publication) error {
	var changes []Change
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		err := r.State.Batch(changes)
		changes = changes[:0]
		return err
	}
	count := 0
	err := r.State.Scan(treeManifestPrefix(p.Temp), func(key string, b []byte) error {
		count++
		if count > treeEntryLimit {
			return ErrLimit
		}
		var record preparedTreeNode
		if err := json.Unmarshal(b, &record); err != nil {
			return err
		}
		relative, err := validWrite(record.Node.Path)
		if err != nil {
			return err
		}
		record.Node.Path = path.Join(p.Path, relative)
		record.Claim.Path = record.Node.Path
		add := func(key string, value any) error {
			c, err := encoded(key, value)
			if err != nil {
				return err
			}
			changes = append(changes, c)
			return nil
		}
		if record.Node.ID != "" {
			if err := add("identity/"+record.Node.ID, record.Claim); err != nil {
				return err
			}
			if err := add("current/"+record.Node.ID, revision{record.Metadata}); err != nil {
				return err
			}
			cs, err := r.indexChanges(record.Node, nil)
			if err != nil {
				return err
			}
			changes = append(changes, cs...)
		}
		if record.Revision != nil {
			if err := add(managedRevisionPrefix+record.Node.Path, record.Revision); err != nil {
				return err
			}
		}
		changes = append(changes, Change{Key: key, Delete: true})
		if len(changes) >= treeBatchLimit {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (r *Root) deleteTreeManifest(temp string) error {
	var changes []Change
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		err := r.State.Batch(changes)
		changes = changes[:0]
		return err
	}
	if err := r.State.Scan(treeManifestPrefix(temp), func(key string, _ []byte) error {
		changes = append(changes, Change{Key: key, Delete: true})
		if len(changes) >= treeBatchLimit {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}

// Startup runs this after publication recovery. Unreferenced manifests came from
// interrupted preparation and cannot represent a published tree.
func (r *Root) cleanupTreeManifests() error {
	live := map[string]bool{}
	if err := r.State.Scan("pending/", func(_ string, b []byte) error {
		var p publication
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		if p.Tree {
			live[path.Base(p.Temp)] = true
		}
		return nil
	}); err != nil {
		return err
	}
	var changes []Change
	flush := func() error {
		if len(changes) == 0 {
			return nil
		}
		err := r.State.Batch(changes)
		changes = changes[:0]
		return err
	}
	if err := r.State.Scan("tree/", func(key string, _ []byte) error {
		parts := strings.SplitN(strings.TrimPrefix(key, "tree/"), "/", 2)
		if !live[parts[0]] {
			changes = append(changes, Change{Key: key, Delete: true})
		}
		if len(changes) >= treeBatchLimit {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}
