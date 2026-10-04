//go:build linux

package integration_test

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	httpadapter "github.com/k2b-dev/filegate/v7/adapter/http"
	"github.com/k2b-dev/filegate/v7/domain"
	sdk "github.com/k2b-dev/filegate/v7/sdk/filegate"
)

func targetExecutionRoots(t *testing.T, history bool) (*fixture, *fixture, *sdk.Client) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("Unix execution integration requires a root daemon")
	}
	cloud, ipa := setup(t, history, history), setup(t, history, history)
	cloud.r.Config.Name, ipa.r.Config.Name = "cloud", "ipa"
	ipa.r.Config.Execution = true
	if err := os.Chmod(ipa.data, 0755); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("t", 32)
	server := httptest.NewServer(httpadapter.New([]*domain.Root{cloud.r, ipa.r}, httpadapter.Options{Token: token}))
	t.Cleanup(server.Close)
	client, err := sdk.New(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	return cloud, ipa, client
}

func targetUnixActor() *sdk.ExecutionContext {
	return &sdk.ExecutionContext{Mode: "unix", Identity: &domain.ExecutionIdentity{UID: executionUID, GID: executionGID, Groups: []uint32{executionGroup}}}
}

func targetActorRoot(t *testing.T, client *sdk.Client) *sdk.Root {
	t.Helper()
	root, err := client.Root("ipa").WithExecution(*targetUnixActor().Identity)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func targetHTTPError(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != status {
		t.Fatalf("expected HTTP %d, got %v", status, err)
	}
}

func targetAbsent(t *testing.T, x *fixture, p string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(x.data, p)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected visible destination %q: %v", p, err)
	}
}

func TestTargetExecutionServiceToUnixSeparatesPublicationOwnership(t *testing.T) {
	cloud, ipa, client := targetExecutionRoots(t, false)
	put(t, cloud.r, "private", "cloud bytes", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	uid, gid := 0, int(executionGroup)
	if _, err := ipa.r.Mkdir("shared", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "2770"}}); err != nil {
		t.Fatal(err)
	}
	setACL(t, ipa.r, "shared", domain.DefaultACL, sharedACL())
	owner := 22001
	acl := domain.ACL{Entries: []domain.ACLEntry{{Tag: domain.ACLOwner, Permissions: "rw-"}, {Tag: domain.ACLOwningGroup, Permissions: "rw-"}, {Tag: domain.ACLOther, Permissions: "---"}}}
	result, err := client.Root("cloud").Transfer(ctx, sdk.TransferRequest{Path: "private", TargetRoot: "ipa", TargetPath: "shared/copied", TargetExecution: targetUnixActor(), WriteOptions: domain.WriteOptions{Ownership: &domain.Ownership{UID: &owner, GID: &gid, Mode: "0660"}, AccessACL: &acl}})
	if err != nil || result.State != domain.TransferCompleted || result.Node == nil || result.Node.Path != "shared/copied" {
		t.Fatal(result, err)
	}
	assertKernelRights(t, ipa, "shared/copied", 0660, owner, gid)
	assertACL(t, ipa.r, "shared/copied", domain.AccessACL, acl)
	if read(t, ipa.r, "shared/copied") != "cloud bytes" || read(t, cloud.r, "private") != "cloud bytes" {
		t.Fatal("transfer changed bytes")
	}
	// Without explicit ownership, creation belongs to the executing actor and
	// inherits the target directory's setgid group and default ACL.
	result, err = client.Root("cloud").Transfer(ctx, sdk.TransferRequest{Path: "private", TargetRoot: "ipa", TargetPath: "shared/inherited", TargetExecution: targetUnixActor()})
	if err != nil || result.Node == nil {
		t.Fatal(result, err)
	}
	assertKernelRights(t, ipa, "shared/inherited", 0660, int(executionUID), gid)
}

func TestTargetExecutionUnixToServiceAndInheritedActor(t *testing.T) {
	cloud, ipa, client := targetExecutionRoots(t, false)
	uid, gid := int(executionUID), int(executionGID)
	put(t, ipa.r, "private", "ipa bytes", domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0600"}})
	source := targetActorRoot(t, client)
	// Omission inherits the source actor. A service-only root must reject it.
	_, err := source.Transfer(ctx, sdk.TransferRequest{Path: "private", TargetRoot: "cloud", TargetPath: "inherited"})
	targetHTTPError(t, err, 409)
	targetAbsent(t, cloud, "inherited")
	result, err := source.Transfer(ctx, sdk.TransferRequest{Path: "private", TargetRoot: "cloud", TargetPath: "copied", TargetExecution: &sdk.ExecutionContext{Mode: "service"}})
	if err != nil || result.Node == nil || result.Node.UID != 0 {
		t.Fatal(result, err)
	}
	if read(t, cloud.r, "copied") != "ipa bytes" {
		t.Fatal("wrong copied content")
	}
}

func TestTargetExecutionDenialsNeverFallbackOrPublishPartialTrees(t *testing.T) {
	cloud, ipa, client := targetExecutionRoots(t, false)
	source := targetActorRoot(t, client)
	put(t, ipa.r, "denied", "secret", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	_, err := source.Transfer(ctx, sdk.TransferRequest{Path: "denied", TargetRoot: "cloud", TargetPath: "denied-source", TargetExecution: &sdk.ExecutionContext{Mode: "service"}})
	targetHTTPError(t, err, 403)
	targetAbsent(t, cloud, "denied-source")
	// The readable sibling cannot make a failed recursive transfer appear successful.
	if _, err := ipa.r.Mkdir("tree", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "0755"}}); err != nil {
		t.Fatal(err)
	}
	put(t, ipa.r, "tree/a-readable", "public", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0644"}})
	put(t, ipa.r, "tree/z-denied", "secret", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	_, err = source.Transfer(ctx, sdk.TransferRequest{Path: "tree", TargetRoot: "cloud", TargetPath: "partial", TargetExecution: &sdk.ExecutionContext{Mode: "service"}})
	targetHTTPError(t, err, 403)
	targetAbsent(t, cloud, "partial")
	put(t, cloud.r, "source", "cloud secret", domain.WriteOptions{})
	if _, err := ipa.r.Mkdir("readonly", domain.DirectoryOptions{Ownership: &domain.Ownership{DirMode: "0755"}}); err != nil {
		t.Fatal(err)
	}
	_, err = client.Root("cloud").Transfer(ctx, sdk.TransferRequest{Path: "source", TargetRoot: "ipa", TargetPath: "readonly/no", TargetExecution: targetUnixActor()})
	targetHTTPError(t, err, 403)
	targetAbsent(t, ipa, "readonly/no")
	// Explicit final ownership is not permission to bypass the target actor.
	uid, gid := int(executionUID), int(executionGroup)
	_, err = client.Root("cloud").Transfer(ctx, sdk.TransferRequest{Path: "source", TargetRoot: "ipa", TargetPath: "readonly/owned", TargetExecution: targetUnixActor(), WriteOptions: domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0660"}}})
	targetHTTPError(t, err, 403)
	targetAbsent(t, ipa, "readonly/owned")
}

func TestTargetExecutionSameRootRequiresSameActor(t *testing.T) {
	_, ipa, client := targetExecutionRoots(t, false)
	uid, gid := int(executionUID), int(executionGID)
	if _, err := ipa.r.Mkdir("owned", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "0700"}}); err != nil {
		t.Fatal(err)
	}
	put(t, ipa.r, "owned/source", "bytes", domain.WriteOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, Mode: "0600"}})
	source := targetActorRoot(t, client)
	other := targetUnixActor()
	other.Identity.UID++
	for _, target := range []*sdk.ExecutionContext{{Mode: "service"}, other} {
		_, err := source.Transfer(ctx, sdk.TransferRequest{Path: "owned/source", TargetRoot: "ipa", TargetPath: "owned/rejected", TargetExecution: target})
		targetHTTPError(t, err, 400)
		targetAbsent(t, ipa, "owned/rejected")
	}
	// Duplicate groups normalize to the same actor and are accepted.
	same := targetUnixActor()
	same.Identity.Groups = append(same.Identity.Groups, executionGroup)
	result, err := source.Transfer(ctx, sdk.TransferRequest{Path: "owned/source", TargetRoot: "ipa", TargetPath: "owned/copy", TargetExecution: same})
	if err != nil || result.Node == nil {
		t.Fatal(result, err)
	}
	if read(t, ipa.r, "owned/copy") != "bytes" {
		t.Fatal("wrong same-root copy")
	}
}

func TestTargetExecutionHistoricalCopyPreservesSource(t *testing.T) {
	cloud, ipa, client := targetExecutionRoots(t, true)
	uid, gid := int(executionUID), int(executionGID)
	if _, err := ipa.r.Mkdir("owned", domain.DirectoryOptions{Ownership: &domain.Ownership{UID: &uid, GID: &gid, DirMode: "0700"}}); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"cloud-to-ipa", "ipa-to-cloud"} {
		t.Run(direction, func(t *testing.T) {
			src, dst, source, path, target, actor := cloud, ipa, client.Root("cloud"), "original", "owned/from-cloud", targetUnixActor()
			options := domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}}
			if direction == "ipa-to-cloud" {
				src, dst, source, path, target, actor = ipa, cloud, targetActorRoot(t, client), "owned/original", "from-ipa", &sdk.ExecutionContext{Mode: "service"}
				options.Ownership.UID, options.Ownership.GID = &uid, &gid
			}
			put(t, src.r, path, "historical", options)
			version, err := src.r.Snapshot(path, true, domain.Metadata{"message": "retained"})
			if err != nil {
				t.Fatal(err)
			}
			options.OnConflict = "overwrite"
			put(t, src.r, path, "current", options)
			before, err := src.r.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			versionsBefore, err := src.r.Versions(path)
			if err != nil {
				t.Fatal(err)
			}
			node, err := source.CopyVersion(ctx, version.ID, sdk.VersionCopyRequest{Path: path, TargetRoot: dst.r.Config.Name, TargetPath: target, TargetExecution: actor})
			if err != nil || node.Path != target {
				t.Fatal(node, err)
			}
			after, err := src.r.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			versionsAfter, err := src.r.Versions(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(versionsBefore, versionsAfter) {
				t.Fatalf("source metadata/history changed: before=%+v after=%+v", before, after)
			}
			if read(t, src.r, path) != "current" || read(t, dst.r, target) != "historical" {
				t.Fatal("historical copy changed original or selected wrong content")
			}
		})
	}
	// Historical bytes still require readable current source permissions.
	put(t, ipa.r, "secret", "history", domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0600"}})
	version, err := ipa.r.Snapshot("secret", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = targetActorRoot(t, client).CopyVersion(ctx, version.ID, sdk.VersionCopyRequest{Path: "secret", TargetRoot: "cloud", TargetPath: "denied-history", TargetExecution: &sdk.ExecutionContext{Mode: "service"}})
	targetHTTPError(t, err, 403)
	targetAbsent(t, cloud, "denied-history")
}
