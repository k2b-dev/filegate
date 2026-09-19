//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v5/domain"
)

type collisionPublicationFiles struct {
	domain.Files
	data       string
	collisions int
}

func (f *collisionPublicationFiles) Rename(from, to string, replace bool) error {
	if strings.HasPrefix(from, ".filegate/staging/") && f.collisions == 0 {
		f.collisions++
		if err := os.WriteFile(filepath.Join(f.data, to), []byte("external"), 0600); err != nil {
			return err
		}
	}
	return f.Files.Rename(from, to, replace)
}
func (f *collisionPublicationFiles) ChangeTime(st os.FileInfo) (int64, int64) {
	return f.Files.(interface {
		ChangeTime(os.FileInfo) (int64, int64)
	}).ChangeTime(st)
}
func TestConflictRetrySessionReceiptRecoversActualTarget(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	session, err := x.r.CreateSession("report.txt", 3, domain.WriteOptions{OnConflict: "rename"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.PutSegment(ctx, session.ID, 0, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	x.r.Files = &collisionPublicationFiles{Files: x.files, data: x.data}
	x.r.State = &failingState{State: x.state, fail: true}
	if _, err = x.r.CommitSession(ctx, session.ID); err == nil {
		t.Fatal("failure not injected")
	}
	var pending struct {
		Path    string
		Node    domain.Node
		Receipt domain.Session
	}
	if err = x.state.Scan("pending/", func(_ string, b []byte) error { return json.Unmarshal(b, &pending) }); err != nil {
		t.Fatal(err)
	}
	if pending.Path == "report.txt" || !strings.HasSuffix(pending.Path, ".txt") || pending.Node.Path != pending.Path || pending.Receipt.Result == nil || pending.Receipt.Result.Path != pending.Path {
		t.Fatal("retry target not durable", pending)
	}
	reopen(t, x)
	got, err := x.r.CommitSession(ctx, session.ID)
	if err != nil || got != pending.Node {
		t.Fatal(got, pending.Node, err)
	}
	if read(t, x.r, got.Path) != "new" || read(t, x.r, "report.txt") != "external" {
		t.Fatal("collision damaged content")
	}
}

func TestFilePublicationCanRenamePastOccupiedDirectory(t *testing.T) {
	x := setup(t, false, false)
	if _, err := x.r.Mkdir("occupied", domain.DirectoryOptions{}); err != nil {
		t.Fatal(err)
	}
	n, err := x.r.Put(ctx, "occupied", strings.NewReader("file"), domain.WriteOptions{OnConflict: "rename"})
	if err != nil || n.Path == "occupied" || n.Directory {
		t.Fatal(n, err)
	}
	if read(t, x.r, n.Path) != "file" {
		t.Fatal("wrong copied content")
	}
}

type deletionFinalizationFailure struct {
	domain.State
	fail bool
}

func (s *deletionFinalizationFailure) Batch(changes []domain.Change) error {
	for _, change := range changes {
		if s.fail && change.Delete && strings.HasPrefix(change.Key, "mutation/") {
			return errors.New("injected deletion finalization failure")
		}
	}
	return s.State.Batch(changes)
}
func TestPublicationRecoversPriorDeleteBeforeReusingAbsentPath(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	old := put(t, x.r, "file", "old", domain.WriteOptions{})
	state := &deletionFinalizationFailure{State: x.state, fail: true}
	x.r.State = state
	if err := x.r.Remove("file", false); err == nil {
		t.Fatal("missing deletion failure")
	}
	// The file is already absent, but durable deletion is not yet complete. A
	// publication must not skip recovery merely because no live Node can be read.
	if _, err := x.r.Put(ctx, "file", strings.NewReader("premature"), domain.WriteOptions{}); err == nil {
		t.Fatal("published before prior deletion could recover")
	}
	if _, err := os.Stat(filepath.Join(x.data, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("blocked publication mutated target", err)
	}
	state.fail = false
	created := put(t, x.r, "file", "new", domain.WriteOptions{})
	if created.ID == old.ID || created.Revision == old.Revision {
		t.Fatal("recreated file inherited deleted identity")
	}
	reopen(t, x)
	got, err := x.r.Resolve(created.ID)
	if err != nil || got != created {
		t.Fatal("old deletion erased replacement", got, err)
	}
	if read(t, x.r, "file") != "new" {
		t.Fatal("replacement content lost")
	}
}
