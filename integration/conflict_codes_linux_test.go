//go:build linux

package integration_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
	sdk "github.com/k2b-dev/filegate/v6/sdk/filegate"
)

func requireConflictCode(t *testing.T, err error, code string) {
	t.Helper()
	var response *sdk.APIError
	if !errors.As(err, &response) || response.Status != 409 || response.Code != code {
		t.Fatalf("expected409 %s, got %v", code, err)
	}
}
func TestHTTPPathAndIdempotencyConflictsAreDistinct(t *testing.T) {
	_, _, client := server(t)
	root := client.Root("test")
	if _, err := root.Put(ctx, "file", strings.NewReader("old"), 3, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := root.Put(ctx, "file", strings.NewReader("new"), 3, domain.WriteOptions{})
	requireConflictCode(t, err, "path_conflict")
	if _, err = root.Mkdir(ctx, "directory", sdk.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = root.Mkdir(ctx, "directory", sdk.DirectoryOptions{})
	requireConflictCode(t, err, "path_conflict")
	_, err = root.Transfer(ctx, sdk.TransferRequest{Path: "file", TargetRoot: "test", TargetPath: "file"})
	requireConflictCode(t, err, "path_conflict")
	if _, err = root.Put(ctx, "other", strings.NewReader("other"), 5, domain.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = root.Transfer(ctx, sdk.TransferRequest{Path: "other", TargetRoot: "test", TargetPath: "file", Move: true})
	requireConflictCode(t, err, "path_conflict")
	first, err := root.CreateSession(ctx, "future", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "same-key"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := root.CreateSession(ctx, "future", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "same-key"})
	if err != nil || same.Session.ID != first.Session.ID {
		t.Fatal("identical retry did not replay", same, err)
	}
	_, err = root.CreateSession(ctx, "changed", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "same-key"})
	requireConflictCode(t, err, "idempotency_conflict")
	occupied, err := root.CreateSession(ctx, "file", 0, domain.WriteOptions{}, sdk.SessionCreateOptions{IdempotencyKey: "different-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = root.CommitSession(ctx, occupied.Session.ID)
	requireConflictCode(t, err, "path_conflict")
	incomplete, err := root.CreateSession(ctx, "unfinished", 1, domain.WriteOptions{}, sdk.SessionCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = root.CommitSession(ctx, incomplete.Session.ID)
	requireConflictCode(t, err, "conflict")
}

func TestSessionIdempotencyConflictDoesNotClassifyChunkConflicts(t *testing.T) {
	x := setup(t, false, false)
	session, err := x.r.CreateSession("file", 3, domain.WriteOptions{}, "key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.r.CreateSession("other", 3, domain.WriteOptions{}, "key")
	if !errors.Is(err, domain.ErrIdempotencyConflict) || !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPathConflict) {
		t.Fatal("wrong key mismatch type", err)
	}
	if _, err = x.r.PutSegment(ctx, session.ID, 0, strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	_, err = x.r.PutSegment(ctx, session.ID, 0, strings.NewReader("two"))
	if !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrIdempotencyConflict) || errors.Is(err, domain.ErrPathConflict) {
		t.Fatal("wrong chunk conflict type", err)
	}
}

type privatePublicationCollision struct {
	domain.Files
	calls int
}

func (f *privatePublicationCollision) Rename(string, string, bool) error {
	f.calls++
	return os.ErrExist
}
func TestPrivatePublicationCollisionIsNotAPathConflictOrPrecondition(t *testing.T) {
	x, _, client := server(t)
	x.r.Config.Managed = true
	files := &privatePublicationCollision{Files: x.files}
	x.r.Files = files
	_, err := client.Root("test").Put(ctx, "missing", strings.NewReader("new"), 3, domain.WriteOptions{Precondition: &domain.Precondition{IfNoneMatch: true}})
	requireConflictCode(t, err, "conflict")
	if files.calls != 1 {
		t.Fatal("retried an unproven target collision", files.calls)
	}
	if _, err = os.Stat(filepath.Join(x.data, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func TestUnprovenMoveCollisionStaysGeneric(t *testing.T) {
	x := setup(t, false, false)
	put(t, x.r, "source", "source", domain.WriteOptions{})
	files := &privatePublicationCollision{Files: x.files}
	x.r.Files = files
	_, err := x.r.MoveWithOptions("source", "missing", domain.WriteOptions{OnConflict: "rename"})
	if !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPathConflict) || files.calls != 1 {
		t.Fatal("unproven move collision classified/retried as path", files.calls, err)
	}
}

type publicationSelectionRace struct {
	domain.Files
	data   string
	checks int
}

func (f *publicationSelectionRace) Stat(p string) (os.FileInfo, error) {
	if p == "raced" {
		f.checks++
		if f.checks == 2 {
			if err := os.WriteFile(filepath.Join(f.data, p), []byte("external"), 0600); err != nil {
				return nil, err
			}
		}
	}
	return f.Files.Stat(p)
}
func TestIfNoneMatchTargetSelectionRaceReturnsPrecondition(t *testing.T) {
	x, _, client := server(t)
	x.r.Config.Managed = true
	x.r.Files = &publicationSelectionRace{Files: x.files, data: x.data}
	_, err := client.Root("test").Put(ctx, "raced", strings.NewReader("new"), 3, domain.WriteOptions{Precondition: &domain.Precondition{IfNoneMatch: true}})
	var response *sdk.APIError
	if !errors.As(err, &response) || response.Status != 412 || response.Code != "precondition_failed" {
		t.Fatal("advisory selection race lost precondition semantics", err)
	}
	if got := read(t, x.r, "raced"); got != "external" {
		t.Fatal("replaced raced target", got)
	}
}
