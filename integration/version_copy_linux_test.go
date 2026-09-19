//go:build linux

package integration_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
	"golang.org/x/sys/unix"
)

func historicalCopySource(t *testing.T, x *fixture) (domain.Node, domain.Version) {
	t.Helper()
	put(t, x.r, "source.txt", "historical", domain.WriteOptions{Metadata: domain.Metadata{"message": "original"}})
	version, err := x.r.Snapshot("source.txt", true, domain.Metadata{"message": "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	current := put(t, x.r, "source.txt", "current", domain.WriteOptions{OnConflict: "overwrite", Metadata: domain.Metadata{"message": "current"}})
	return current, version
}
func assertHistoricalSourceUnchanged(t *testing.T, x *fixture, before domain.Node, versions []domain.Version) {
	t.Helper()
	after, err := x.r.Stat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("copy changed current source: before=%+v after=%+v", before, after)
	}
	if content := read(t, x.r, "source.txt"); content != "current" {
		t.Fatalf("copy changed source bytes: %s", content)
	}
	afterVersions, err := x.r.Versions("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterVersions, versions) {
		t.Fatalf("copy changed source history: before=%+v after=%+v", versions, afterVersions)
	}
}
func TestCopyVersionPublishesAnotherIdentityWithoutRestoringSource(t *testing.T) {
	for _, crossRoot := range []bool{false, true} {
		name := "same-root"
		if crossRoot {
			name = "cross-root"
		}
		t.Run(name, func(t *testing.T) {
			source := setup(t, true, true)
			destination := source
			if crossRoot {
				destination = setup(t, true, true)
				destination.r.Config.Name = "destination"
			}
			before, version := historicalCopySource(t, source)
			history, err := source.r.Versions("source.txt")
			if err != nil {
				t.Fatal(err)
			}
			result, err := domain.CopyVersion(ctx, source.r, "source.txt", version.ID, destination.r, "copies/history.txt", domain.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Path != "copies/history.txt" || result.ID == "" || result.ID == before.ID {
				t.Fatalf("new destination identity: %+v", result)
			}
			if content := read(t, destination.r, result.Path); content != "historical" {
				t.Fatalf("wrong historical content: %s", content)
			}
			ownHistory, err := destination.r.Versions(result.Path)
			if err != nil || len(ownHistory) != 0 {
				t.Fatalf("copy inherited source history: %+v %v", ownHistory, err)
			}
			assertHistoricalSourceUnchanged(t, source, before, history)
		})
	}
}
func TestCopyVersionConflictsAndDestinationHistory(t *testing.T) {
	x := setup(t, true, true)
	before, version := historicalCopySource(t, x)
	history, err := x.r.Versions("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	target := put(t, x.r, "target.txt", "destination-old", domain.WriteOptions{})
	if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "target.txt", domain.WriteOptions{}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("default conflict: %v", err)
	}
	if got := read(t, x.r, "target.txt"); got != "destination-old" {
		t.Fatalf("conflict mutated target: %s", got)
	}
	renamed, err := domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "target.txt", domain.WriteOptions{OnConflict: "rename"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Path == "target.txt" || renamed.ID == target.ID || read(t, x.r, renamed.Path) != "historical" {
		t.Fatalf("rename result: %+v", renamed)
	}
	overwritten, err := domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "target.txt", domain.WriteOptions{OnConflict: "overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	if overwritten.ID != target.ID || read(t, x.r, "target.txt") != "historical" {
		t.Fatalf("overwrite changed target identity: %+v", overwritten)
	}
	targetHistory, err := x.r.Versions("target.txt")
	if err != nil || len(targetHistory) != 1 {
		t.Fatalf("destination history: %+v %v", targetHistory, err)
	}
	blob, err := x.r.OpenVersion("target.txt", targetHistory[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil || string(data) != "destination-old" {
		t.Fatalf("destination recovery bytes: %q %v", data, err)
	}
	if _, err = x.r.Mkdir("folder", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"error", "overwrite"} {
		if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "folder", domain.WriteOptions{OnConflict: policy}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("directory target %s: %v", policy, err)
		}
	}
	folderRename, err := domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "folder", domain.WriteOptions{OnConflict: "rename"})
	if err != nil || folderRename.Path == "folder" || folderRename.Directory {
		t.Fatalf("rename occupied directory: %+v %v", folderRename, err)
	}
	folder, err := x.r.Stat("folder")
	if err != nil || !folder.Directory {
		t.Fatalf("directory conflict changed original target: %+v %v", folder, err)
	}
	for _, policy := range []string{"error", "rename", "overwrite"} {
		if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "source.txt", domain.WriteOptions{OnConflict: policy}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("same source target %s: %v", policy, err)
		}
	}
	assertHistoricalSourceUnchanged(t, x, before, history)
}
func TestCopyVersionFailuresLeaveOriginalAndDestinationUnchanged(t *testing.T) {
	x := setup(t, true, true)
	before, version := historicalCopySource(t, x)
	history, err := x.r.Versions("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = domain.CopyVersion(cancelled, x.r, "source.txt", version.ID, x.r, "cancelled", domain.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy: %v", err)
	}
	if _, err = domain.CopyVersion(ctx, x.r, "source.txt", "missing", x.r, "missing", domain.WriteOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing version: %v", err)
	}
	maxBytes := x.r.MaxBytes
	x.r.MaxBytes = 1
	if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "oversized", domain.WriteOptions{}); !errors.Is(err, domain.ErrLimit) {
		t.Fatalf("destination size limit: %v", err)
	}
	x.r.MaxBytes = maxBytes
	if err = os.Remove(filepath.Join(x.data, ".filegate/versions", version.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "missing-blob", domain.WriteOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing blob: %v", err)
	}
	for _, target := range []string{"cancelled", "missing", "oversized", "missing-blob"} {
		if _, err = os.Stat(filepath.Join(x.data, target)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed copy published %s: %v", target, err)
		}
	}
	assertHistoricalSourceUnchanged(t, x, before, history)
	x.r.Config.Versioning.Enabled = false
	if _, err = domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "disabled", domain.WriteOptions{}); !errors.Is(err, domain.ErrDisabled) {
		t.Fatalf("disabled history: %v", err)
	}
}
func TestCopyVersionDoesNotAssignIdentityToExternallyReplacedSource(t *testing.T) {
	x := setup(t, true, true)
	_, version := historicalCopySource(t, x)
	current := filepath.Join(x.data, "source.txt")
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.CopyVersion(ctx, x.r, "source.txt", version.ID, x.r, "copy", domain.WriteOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced source: %v", err)
	}
	value := make([]byte, 16)
	if _, err := unix.Getxattr(current, "user.filegate.id", value); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("failed historical lookup assigned an identity: %v", err)
	}
}
func TestCopyVersionExecutionAndExplicitDestinationRights(t *testing.T) {
	x := executionFixture(t, true, true)
	before, version := historicalCopySource(t, x)
	if _, err := x.r.SetOwnership("source.txt", &domain.Ownership{Mode: "0600"}); err != nil {
		t.Fatal(err)
	}
	actor := executionView(t, x)
	if _, err := domain.CopyVersion(ctx, actor, "source.txt", version.ID, actor, "denied-source", domain.WriteOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable source history copied: %v", err)
	}
	if _, err := x.r.SetOwnership("source.txt", &domain.Ownership{Mode: "0644"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(x.data, "denied"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.CopyVersion(ctx, actor, "source.txt", version.ID, actor, "denied/copy", domain.WriteOptions{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unwritable destination accepted: %v", err)
	}
	uid, gid := 23001, 23002
	acl := namedACL(23003)
	result, err := domain.CopyVersion(ctx, actor, "source.txt", version.ID, actor, "rights-copy", domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid}, AccessACL: &acl})
	if err != nil {
		t.Fatal(err)
	}
	assertKernelRights(t, x, result.Path, 0750, uid, gid)
	normalized, err := domain.NormalizeACL(acl)
	if err != nil {
		t.Fatal(err)
	}
	assertACL(t, x.r, result.Path, domain.AccessACL, normalized)
	if content := read(t, x.r, result.Path); content != "historical" {
		t.Fatal(content)
	}
	after, err := x.r.Stat("source.txt")
	if err != nil || after.ID != before.ID || !after.Modified.Equal(before.Modified) {
		t.Fatalf("rights failures changed source: %+v %v", after, err)
	}
}
