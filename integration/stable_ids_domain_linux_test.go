//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v7/domain"
)

type stableIDMoveFailure struct {
	domain.State
	fail bool
}

func (s *stableIDMoveFailure) Put(key string, value any) error {
	if s.fail && strings.HasPrefix(key, "mutation/") {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var mutation struct{ Applied bool }
		if err = json.Unmarshal(b, &mutation); err != nil {
			return err
		}
		if mutation.Applied {
			s.fail = false
			return errors.New("injected applied move failure")
		}
	}
	return s.State.Put(key, value)
}

func TestStableIDsResolveRecoversBeforeClaimLookup(t *testing.T) {
	t.Run("move", func(t *testing.T) {
		x := setup(t, true, true)
		x.r.Config.Managed = true
		original := put(t, x.r, "before", "content", domain.WriteOptions{})
		x.r.State = &stableIDMoveFailure{State: x.state, fail: true}
		if _, err := x.r.Move("before", "after"); err == nil || !strings.Contains(err.Error(), "injected applied move failure") {
			t.Fatalf("move failure not injected: %v", err)
		}
		resolved, err := x.r.Resolve(original.ID)
		if err != nil || resolved.ID != original.ID || resolved.Path != "after" {
			t.Fatalf("first resolve did not recover current path: %+v %v", resolved, err)
		}
	})
	t.Run("publication", func(t *testing.T) {
		x := setup(t, true, false)
		x.r.Config.Managed = true
		failure := &failingState{State: x.state, fail: true}
		x.r.State = failure
		if _, err := x.r.Put(ctx, "published", strings.NewReader("content"), domain.WriteOptions{}); err == nil {
			t.Fatal("publication failure not injected")
		}
		failure.fail = false
		f, err := x.files.Open("published", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		id, err := x.files.ID(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := x.r.Resolve(id)
		if err != nil || resolved.ID != id || resolved.Path != "published" {
			t.Fatalf("first resolve did not recover identity claim: %+v %v", resolved, err)
		}
	})
}

func TestStableIDsOpenByIDRecoversBeforeOpening(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	original := put(t, x.r, "before", "content", domain.WriteOptions{})
	x.r.State = &stableIDMoveFailure{State: x.state, fail: true}
	if _, err := x.r.Move("before", "after"); err == nil {
		t.Fatal("move failure not injected")
	}
	assertStableIDContent(t, x.r, original.ID, "after", "content")
}

type stableIDMoveBatchFailure struct {
	domain.State
	batches int
}

func (s *stableIDMoveBatchFailure) Batch(changes []domain.Change) error {
	for _, change := range changes {
		if change.Delete && strings.HasPrefix(change.Key, "i/") {
			s.batches++
			if s.batches == 2 {
				return errors.New("injected partial move index failure")
			}
			break
		}
	}
	return s.State.Batch(changes)
}

func TestStableIDsDirectoryMoveRecoversPartialClaimBatches(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			x := setup(t, true, false)
			x.r.Config.Managed = true
			identities := map[string]string{}
			for i := 0; i < 40; i++ {
				path := fmt.Sprintf("tree/file-%02d", i)
				identities[path] = put(t, x.r, path, path, domain.WriteOptions{}).ID
			}
			failure := &stableIDMoveBatchFailure{State: x.state}
			x.r.State = failure
			if _, err := x.r.Move("tree", "moved"); err == nil || !strings.Contains(err.Error(), "injected partial move index failure") {
				t.Fatalf("partial move failure not injected: %v", err)
			}
			if failure.batches != 2 {
				t.Fatal("move did not commit one claim batch before failing")
			}
			if restart {
				reopen(t, x)
			}
			// The final child belongs to a batch not yet reconciled. Resolve must
			// recover before consuming its old durable path on the first request.
			last, err := x.r.Resolve(identities["tree/file-39"])
			if err != nil || last.Path != "moved/file-39" {
				t.Fatalf("first resolve after partial move: %+v %v", last, err)
			}
			for path, id := range identities {
				resolved, err := x.r.Resolve(id)
				if err != nil || resolved.ID != id || resolved.Path != "moved"+strings.TrimPrefix(path, "tree") {
					t.Fatalf("partial recovery lost identity: %+v %v", resolved, err)
				}
			}
		})
	}
}

func TestStableIDsRejectMalformedIDsAndAcceptExistingUUIDs(t *testing.T) {
	x := setup(t, true, false)
	x.r.Config.Managed = true
	valid := "1b156f80-0ec0-45aa-aacb-9c6089e9aade"
	for _, id := range []string{"", "not-a-uuid", strings.ToUpper(valid), "{" + valid + "}", strings.ReplaceAll(valid, "-", "")} {
		if _, err := x.r.Resolve(id); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed ID resolved: %q %v", id, err)
		}
		if f, _, err := x.r.OpenByID(id); !errors.Is(err, domain.ErrInvalid) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("malformed ID opened: %q %v", id, err)
		}
		if f, _, err := x.r.OpenWithID("file", id); !errors.Is(err, domain.ErrInvalid) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("malformed bound ID opened: %q %v", id, err)
		}
	}
	if _, err := x.r.Resolve(valid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("valid legacy UUID did not reach lookup: %v", err)
	}
	x.r.Config.Managed = false
	if _, err := x.r.Resolve(""); !errors.Is(err, domain.ErrDisabled) {
		t.Fatalf("ID validation preceded capability gate: %v", err)
	}
}

func TestStableIDsRequireManagedIndexAndRetainInternalVersionIdentity(t *testing.T) {
	for _, index := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%v/managed=%v", index, managed), func(t *testing.T) {
				x := setup(t, index, index)
				x.r.Config.Managed = managed
				n := put(t, x.r, "file", "content", domain.WriteOptions{})
				info, err := x.r.Info()
				if err != nil || info.StableIDs != (index && managed) || x.r.StableIDs() != info.StableIDs {
					t.Fatalf("capability: %+v %v", info, err)
				}
				if (n.ID != "") != index {
					t.Fatalf("internal identity depends on managed mode: %+v", n)
				}
				if index {
					version, err := x.r.Snapshot("file", true, nil)
					if err != nil || version.FileID != n.ID {
						t.Fatalf("internal version identity: %+v %v", version, err)
					}
				}
				if index && managed {
					resolved, err := x.r.Resolve(n.ID)
					if err != nil || resolved.ID != n.ID {
						t.Fatal(resolved, err)
					}
					assertStableIDContent(t, x.r, n.ID, "file", "content")
					return
				}
				if _, err = x.r.Resolve(n.ID); !errors.Is(err, domain.ErrDisabled) {
					t.Fatalf("resolve enabled without managed index: %v", err)
				}
				if f, _, err := x.r.OpenByID(n.ID); !errors.Is(err, domain.ErrDisabled) {
					if f != nil {
						f.Close()
					}
					t.Fatalf("ID open enabled without managed index: %v", err)
				}
				if f, _, err := x.r.OpenWithID("file", n.ID); !errors.Is(err, domain.ErrDisabled) {
					if f != nil {
						f.Close()
					}
					t.Fatalf("bound open enabled without managed index: %v", err)
				}
			})
		}
	}
}

func assertStableIDContent(t *testing.T, r *domain.Root, id, path, content string) {
	t.Helper()
	f, n, err := r.OpenByID(id)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bytes, err := io.ReadAll(f)
	if err != nil || n.ID != id || n.Path != path || string(bytes) != content {
		t.Fatalf("ID content: %+v %q %v", n, bytes, err)
	}
}

func assertStableIDOpenMissing(t *testing.T, r *domain.Root, path, id string) {
	t.Helper()
	f, _, err := r.OpenWithID(path, id)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, os.ErrNotExist) || f != nil {
		t.Fatalf("bound open retained moved/replaced identity at %q: %v", path, err)
	}
}

func TestStableIDsLifecycleAndDirectoryDescendants(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	original := put(t, x.r, "tree/sub/file", "original", domain.WriteOptions{})
	identities := map[string]string{"tree/sub/file": original.ID}
	for _, path := range []string{"tree", "tree/sub"} {
		n, err := x.r.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		identities[path] = n.ID
	}
	for _, id := range identities {
		u, err := uuid.Parse(id)
		if err != nil || u.Version() != 7 {
			t.Fatalf("identity is not UUIDv7: %q %v", id, err)
		}
	}
	version, err := x.r.Snapshot(original.Path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	overwritten := put(t, x.r, original.Path, "overwritten", domain.WriteOptions{OnConflict: "overwrite"})
	if overwritten.ID != original.ID {
		t.Fatal("overwrite changed identity")
	}
	restored, err := x.r.Restore(original.Path, version.ID)
	if err != nil || restored.ID != original.ID {
		t.Fatalf("restore changed identity: %+v %v", restored, err)
	}
	if _, err = x.r.Move("tree", "moved"); err != nil {
		t.Fatal(err)
	}
	for path, id := range identities {
		resolved, err := x.r.Resolve(id)
		if err != nil || resolved.ID != id || resolved.Path != "moved"+strings.TrimPrefix(path, "tree") {
			t.Fatalf("move lost descendant identity %q: %+v %v", path, resolved, err)
		}
	}
	assertStableIDContent(t, x.r, original.ID, "moved/sub/file", "original")
	assertStableIDOpenMissing(t, x.r, original.Path, original.ID)
	reused := put(t, x.r, original.Path, "replacement", domain.WriteOptions{})
	if reused.ID == original.ID {
		t.Fatal("reused path inherited moved identity")
	}
	assertStableIDOpenMissing(t, x.r, original.Path, original.ID)
	f, n, err := x.r.OpenWithID("moved/sub/file", original.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if n.ID != original.ID {
		t.Fatal("bound current-path open changed identity")
	}
	if err = x.r.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	reopen(t, x)
	for path, id := range identities {
		resolved, err := x.r.Resolve(id)
		if err != nil || resolved.Path != "moved"+strings.TrimPrefix(path, "tree") {
			t.Fatalf("rebuild/restart lost identity: %+v %v", resolved, err)
		}
	}
	if err = x.r.Remove("moved", true); err != nil {
		t.Fatal(err)
	}
	for _, id := range identities {
		if _, err := x.r.Resolve(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete retained identity %q: %v", id, err)
		}
	}
	recreated := put(t, x.r, "moved/sub/file", "recreated", domain.WriteOptions{})
	if recreated.ID == original.ID {
		t.Fatal("delete/recreate reused identity")
	}
	assertStableIDOpenMissing(t, x.r, recreated.Path, original.ID)
	assertStableIDContent(t, x.r, recreated.ID, recreated.Path, "recreated")
}

func TestStableIDsNativeOverwriteRetiresDestinationIdentity(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	source := put(t, x.r, "source", "source", domain.WriteOptions{})
	target := put(t, x.r, "target", "target", domain.WriteOptions{})
	moved, err := x.r.MoveWithOptions(source.Path, target.Path, domain.WriteOptions{OnConflict: "overwrite"})
	if err != nil || moved.ID != source.ID {
		t.Fatalf("native overwrite identity: %+v %v", moved, err)
	}
	if _, err = x.r.Resolve(target.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced target identity retained: %v", err)
	}
	assertStableIDContent(t, x.r, source.ID, target.Path, "source")
	assertStableIDOpenMissing(t, x.r, target.Path, target.ID)
	reopen(t, x)
	if _, err = x.r.Resolve(target.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart revived replaced identity: %v", err)
	}
}

func TestStableIDsOpenedDescriptorRetainsVerifiedContent(t *testing.T) {
	x := setup(t, true, false)
	x.r.Config.Managed = true
	original := put(t, x.r, "file", "original", domain.WriteOptions{})
	f, opened, err := x.r.OpenByID(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	updated := put(t, x.r, original.Path, "updated", domain.WriteOptions{OnConflict: "overwrite"})
	if updated.ID != opened.ID || updated.Revision == opened.Revision {
		t.Fatal("overwrite did not preserve identity and change revision")
	}
	if _, err = x.r.Move(original.Path, "moved"); err != nil {
		t.Fatal(err)
	}
	bytes, err := io.ReadAll(f)
	if err != nil || string(bytes) != "original" || opened.ID != original.ID {
		t.Fatalf("opened descriptor changed after overwrite/move: %+v %q %v", opened, bytes, err)
	}
	assertStableIDContent(t, x.r, original.ID, "moved", "updated")
}

func TestStableIDsIDOpenInitializesAndRefreshesManagedRevision(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%v", bound), func(t *testing.T) {
			x := setup(t, true, false)
			original := put(t, x.r, "file", "content", domain.WriteOptions{})
			if original.Revision != "" {
				t.Fatal("unmanaged publication assigned a revision")
			}
			x.r.Config.Managed = true
			reopen(t, x)
			open := func() (*os.File, domain.Node, error) {
				if bound {
					return x.r.OpenWithID(original.Path, original.ID)
				}
				return x.r.OpenByID(original.ID)
			}
			f, first, err := open()
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			if first.ID != original.ID || first.Revision == "" {
				t.Fatalf("first ID read did not initialize managed revision: %+v", first)
			}
			pathNode, err := x.r.Stat(original.Path)
			if err != nil || pathNode.Revision != first.Revision || pathNode.ID != original.ID {
				t.Fatalf("path read changed ID-read revision: %+v %v", pathNode, err)
			}
			if err = os.Chmod(filepath.Join(x.data, original.Path), 0600); err != nil {
				t.Fatal(err)
			}
			f, changed, err := open()
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			if changed.ID != original.ID || changed.Revision == "" || changed.Revision == first.Revision {
				t.Fatalf("ID read did not refresh observed metadata drift: %+v", changed)
			}
			pathNode, err = x.r.Stat(original.Path)
			if err != nil || pathNode.Revision != changed.Revision {
				t.Fatalf("path read changed refreshed ID-read revision: %+v %v", pathNode, err)
			}
		})
	}
}

func TestStableIDsCopiedXattrCannotReplaceClaimedFile(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	original := put(t, x.r, "original", "original", domain.WriteOptions{})
	f, err := x.files.Open("forged", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.files.SetID(f, original.ID); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err = f.WriteString("forged"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if err = os.Rename(filepath.Join(x.data, "forged"), filepath.Join(x.data, original.Path)); err != nil {
		t.Fatal(err)
	}
	assertStableIDOpenMissing(t, x.r, original.Path, original.ID)
	if f, _, err := x.r.OpenByID(original.ID); !errors.Is(err, os.ErrNotExist) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("copied xattr stole identity: %v", err)
	}
	if err = x.r.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := x.r.Stat(original.Path)
	if err != nil || replacement.ID == original.ID {
		t.Fatalf("rebuild adopted copied identity: %+v %v", replacement, err)
	}
	if _, err = x.r.Resolve(original.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old identity redirected after rebuild: %v", err)
	}
}

func TestStableIDsCrossRootMoveAndCopiesUseDestinationIdentity(t *testing.T) {
	src, dst := setup(t, true, true), setup(t, true, true)
	src.r.Config.Managed, dst.r.Config.Managed = true, true
	dst.r.Config.Name = "destination"
	original := put(t, src.r, "original", "original", domain.WriteOptions{})
	copied, err := domain.Transfer(ctx, src.r, original.Path, dst.r, "copy", false, domain.WriteOptions{})
	if err != nil || copied.ID == "" || copied.ID == original.ID {
		t.Fatalf("copy carried source identity: %+v %v", copied, err)
	}
	target := put(t, dst.r, "existing", "existing", domain.WriteOptions{})
	overwritten, err := domain.Transfer(ctx, src.r, original.Path, dst.r, target.Path, false, domain.WriteOptions{OnConflict: "overwrite"})
	if err != nil || overwritten.ID != target.ID {
		t.Fatalf("copy overwrite changed destination identity: %+v %v", overwritten, err)
	}
	moved, err := domain.TransferMove(ctx, src.r, original.Path, dst.r, "moved", domain.WriteOptions{}, uuid.NewString())
	if err != nil || moved.Node == nil || moved.Node.ID == "" || moved.Node.ID == original.ID {
		t.Fatalf("cross-root move carried source identity: %+v %v", moved, err)
	}
	if _, err = src.r.Resolve(original.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-root source identity retained: %v", err)
	}
	assertStableIDContent(t, dst.r, moved.Node.ID, "moved", "original")
}
