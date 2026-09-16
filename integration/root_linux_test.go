//go:build linux

package integration_test

import (
	"context"
	"errors"
	"github.com/k2b-dev/filegate/v4/domain"
	"github.com/k2b-dev/filegate/v4/infra/filesystem"
	"github.com/k2b-dev/filegate/v4/infra/pebble"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

type fixture struct {
	r         *domain.Root
	data      string
	statePath string
	state     domain.State
	files     domain.Files
}

func setup(t *testing.T, index, versions bool) *fixture {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if e := os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	f, e := filesystem.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	s, e := pebble.Open(filepath.Join(base, "state"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := domain.NewRoot(domain.RootConfig{Name: "test", Path: data, Index: index, Versioning: domain.Versioning{Enabled: versions, Cooldown: time.Minute, Keep: domain.Keep{Last: 2}}}, f, s, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	x := &fixture{r, data, filepath.Join(base, "state"), s, f}
	t.Cleanup(func() { x.state.Close(); x.files.Close() })
	return x
}
func put(t *testing.T, r *domain.Root, p, bytes string, o domain.WriteOptions) domain.Node {
	t.Helper()
	n, e := r.Put(ctx, p, strings.NewReader(bytes), o)
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func read(t *testing.T, r *domain.Root, p string) string {
	t.Helper()
	f, e := r.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	b, e := io.ReadAll(f)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func reopen(t *testing.T, x *fixture) {
	t.Helper()
	x.state.Close()
	x.files.Close()
	f, e := filesystem.Open(x.data)
	if e != nil {
		t.Fatal(e)
	}
	s, e := pebble.Open(x.statePath)
	if e != nil {
		t.Fatal(e)
	}
	r, e := domain.NewRoot(x.r.Config, f, s, x.r.MaxBytes)
	if e != nil {
		t.Fatal(e)
	}
	x.r = r
	x.state = s
	x.files = f
}
func TestVersionsMetadataCooldownRestoreAndRebuild(t *testing.T) {
	x := setup(t, true, true)
	a := put(t, x.r, "a", "A", domain.WriteOptions{Metadata: domain.Metadata{"message": "A"}})
	vs, e := x.r.Versions("a")
	if e != nil || len(vs) != 0 {
		t.Fatalf("new upload versions %v %v", vs, e)
	}
	b := put(t, x.r, "a", "B", domain.WriteOptions{OnConflict: "overwrite", Metadata: domain.Metadata{"message": "B"}})
	if a.ID != b.ID {
		t.Fatal("overwrite changed ID")
	}
	put(t, x.r, "a", "C", domain.WriteOptions{OnConflict: "overwrite", Metadata: domain.Metadata{"message": "C"}})
	vs, e = x.r.Versions("a")
	if e != nil || len(vs) != 1 || vs[0].Metadata["message"] != "A" {
		t.Fatalf("wrong revision/cooldown: %+v %v", vs, e)
	}
	if _, e = x.r.Restore("a", vs[0].ID); e != nil {
		t.Fatal(e)
	}
	if read(t, x.r, "a") != "A" {
		t.Fatal("restore content")
	}
	vs, e = x.r.Versions("a")
	if e != nil || len(vs) != 2 {
		t.Fatalf("restore did not capture current: %v %v", vs, e)
	}
	if _, e = x.r.Move("a", "dir/renamed"); e != nil {
		t.Fatal(e)
	}
	if e = x.r.Rebuild(ctx); e != nil {
		t.Fatal(e)
	}
	reopen(t, x)
	n, e := x.r.Resolve(a.ID)
	if e != nil || n.Path != "dir/renamed" {
		t.Fatalf("identity lost: %v %v", n, e)
	}
	vs, e = x.r.Versions(n.Path)
	if e != nil || len(vs) != 2 {
		t.Fatalf("rebuild lost history: %v %v", vs, e)
	}
	if e = x.r.Remove("dir", true); e != nil {
		t.Fatal(e)
	}
	info, e := x.r.Info()
	if e != nil || info.Versions != 0 {
		t.Fatalf("delete left history: %v %v", info, e)
	}
}
func TestNoIndexUsesFilesystemAndNoXattrs(t *testing.T) {
	x := setup(t, false, false)
	n := put(t, x.r, "a", "a", domain.WriteOptions{})
	if n.ID != "" {
		t.Fatal("no-index allocated identity")
	}
	if e := os.WriteFile(filepath.Join(x.data, "external"), []byte("b"), 0600); e != nil {
		t.Fatal(e)
	}
	page, e := x.r.List(".", "", 20)
	if e != nil || len(page.Items) != 2 {
		t.Fatalf("external file missing %v %v", page, e)
	}
	file, e := x.files.Open("a", os.O_RDONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	if _, e = x.files.ID(file); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("xattr assigned %v", e)
	}
	if e = x.r.Rebuild(ctx); !errors.Is(e, domain.ErrDisabled) {
		t.Fatal(e)
	}
	if _, e = x.r.RefreshStats(ctx, 1); !errors.Is(e, domain.ErrLimit) {
		t.Fatal("stats ignored bound")
	}
}
func TestCopiedXattrNeverStealsHistory(t *testing.T) {
	x := setup(t, true, true)
	original := put(t, x.r, "a", "A", domain.WriteOptions{})
	v, e := x.r.Snapshot("a", true, nil)
	if e != nil {
		t.Fatal(e)
	}
	f, e := x.files.Open("copy", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	f.WriteString("forged")
	if e = x.files.SetID(f, original.ID); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = os.Remove(filepath.Join(x.data, "a")); e != nil {
		t.Fatal(e)
	}
	if e = x.r.Rebuild(ctx); e != nil {
		t.Fatal(e)
	}
	copy, e := x.r.Stat("copy")
	if e != nil || copy.ID == original.ID {
		t.Fatalf("identity stolen %v %v", copy, e)
	}
	vs, e := x.r.Versions("copy")
	if e != nil || len(vs) != 0 {
		t.Fatal("copied ID exposed history", v, vs, e)
	}
}
func TestSymlinksAndPrivatePathsRejected(t *testing.T) {
	x := setup(t, true, true)
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(x.data, "escape")); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"../bad", "/bad", ".filegate/staging/a", "escape/a"} {
		if _, e := x.r.Put(ctx, p, strings.NewReader("bad"), domain.WriteOptions{}); e == nil {
			t.Fatal("unsafe path accepted", p)
		}
	}
	if e := os.Symlink(".filegate", filepath.Join(x.data, "alias")); e != nil {
		t.Fatal(e)
	}
	if _, e := x.r.List("alias", "", 10); e == nil {
		t.Fatal("private symlink readable")
	}
}
func TestSessionsIdempotentDurableAndIntegrity(t *testing.T) {
	x := setup(t, true, true)
	s, e := x.r.CreateSession("a", 3, domain.WriteOptions{Metadata: domain.Metadata{"message": "upload"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("xyz")); !errors.Is(e, domain.ErrConflict) {
		t.Fatal(e)
	}
	if e = x.r.Rebuild(ctx); e != nil {
		t.Fatal(e)
	}
	reopen(t, x)
	a, e := x.r.CommitSession(ctx, s.ID)
	if e != nil {
		t.Fatal(e)
	}
	b, e := x.r.CommitSession(ctx, s.ID)
	if e != nil || a.ID != b.ID {
		t.Fatal("commit not idempotent", e)
	}
	info, e := x.r.Info()
	if e != nil || info.ActiveUploads != 0 || info.StagingBytes != 0 {
		t.Fatal("completed session counted", info, e)
	}
	s, e = x.r.CreateSession("b", 3, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("abc")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(x.data, ".filegate/staging", s.ID+"-0"), []byte("xyz"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.CommitSession(ctx, s.ID); e == nil {
		t.Fatal("corrupt segment committed")
	}
}

type failingState struct {
	domain.State
	fail bool
}

func (s *failingState) Batch(cs []domain.Change) error {
	for _, c := range cs {
		if s.fail && c.Delete && strings.HasPrefix(c.Key, "pending/") {
			return errors.New("injected commit failure")
		}
	}
	return s.State.Batch(cs)
}
func TestFailedPublicationRecoversBeforeNextReadAndRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(strings.Join([]string{"restart", strconv.FormatBool(restart)}, "="), func(t *testing.T) {
			x := setup(t, true, true)
			a := put(t, x.r, "a", "A", domain.WriteOptions{})
			s := &failingState{State: x.state, fail: true}
			x.r.State = s
			if _, e := x.r.Put(ctx, "a", strings.NewReader("B"), domain.WriteOptions{OnConflict: "overwrite"}); e == nil {
				t.Fatal("failure not injected")
			}
			s.fail = false
			if restart {
				reopen(t, x)
			}
			b, e := x.r.Stat("a")
			if e != nil || a.ID != b.ID {
				t.Fatal("recovery detached history", b, e)
			}
			if read(t, x.r, "a") != "B" {
				t.Fatal("wrong content")
			}
			vs, e := x.r.Versions("a")
			if e != nil || len(vs) != 1 {
				t.Fatal("lost versions", e)
			}
			page, e := x.r.Search(ctx, "a", ".", "", 10, 100)
			if e != nil || len(page.Items) != 1 || page.Items[0].ID != a.ID {
				t.Fatal("wrong generation", page, e)
			}
		})
	}
}

type failingFiles struct{ domain.Files }

func (f failingFiles) Clone(*os.File, *os.File) (bool, error) {
	return false, errors.New("snapshot storage failed")
}
func TestSnapshotFailurePreservesCurrentBytes(t *testing.T) {
	x := setup(t, true, true)
	put(t, x.r, "a", "A", domain.WriteOptions{})
	x.r.Files = failingFiles{x.files}
	if _, e := x.r.Put(ctx, "a", strings.NewReader("B"), domain.WriteOptions{OnConflict: "overwrite"}); e == nil {
		t.Fatal("snapshot failure ignored")
	}
	if read(t, x.r, "a") != "A" {
		t.Fatal("overwrite lost original")
	}
}
func TestTransferDirectoriesAndHistoryBoundary(t *testing.T) {
	a, b := setup(t, true, true), setup(t, false, false)
	b.r.Config.Name = "second"
	n := put(t, a.r, "dir/a", "A", domain.WriteOptions{})
	if _, e := a.r.Snapshot("dir/a", true, nil); e != nil {
		t.Fatal(e)
	}
	if _, e := domain.Transfer(ctx, a.r, "dir", b.r, "copy", false, domain.WriteOptions{}); e != nil {
		t.Fatal(e)
	}
	if read(t, b.r, "copy/a") != "A" {
		t.Fatal("copy failed")
	}
	if _, e := domain.Transfer(ctx, a.r, "dir/a", b.r, "moved", true, domain.WriteOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e := a.r.Resolve(n.ID); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("source retained", e)
	}
}
func TestUnsafePrivateDirectoryFailsStartup(t *testing.T) {
	base := t.TempDir()
	if e := os.Mkdir(filepath.Join(base, ".filegate"), 0755); e != nil {
		t.Fatal(e)
	}
	f, e := filesystem.Open(base)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	s, e := pebble.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = domain.NewRoot(domain.RootConfig{Name: "test", Path: base}, f, s, 1000); e == nil {
		t.Fatal("unsafe private directory accepted")
	}
}

func TestCrossRootMoveNeverDeletesSkippedEntries(t *testing.T) {
	a, b := setup(t, false, false), setup(t, false, false)
	b.r.Config.Name = "second"
	put(t, a.r, "dir/a", "A", domain.WriteOptions{})
	if e := os.Symlink("a", filepath.Join(a.data, "dir/link")); e != nil {
		t.Fatal(e)
	}
	if _, e := domain.Transfer(ctx, a.r, "dir", b.r, "copied", true, domain.WriteOptions{}); e == nil {
		t.Fatal("move accepted unsupported symlink")
	}
	if read(t, a.r, "dir/a") != "A" {
		t.Fatal("source content lost")
	}
	if _, e := os.Lstat(filepath.Join(a.data, "dir/link")); e != nil {
		t.Fatal("source link lost", e)
	}
}
func TestFailedSessionPublicationRetryIsIdempotent(t *testing.T) {
	x := setup(t, true, true)
	s, e := x.r.CreateSession("a", 1, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("a")); e != nil {
		t.Fatal(e)
	}
	fail := &failingState{State: x.state, fail: true}
	x.r.State = fail
	if _, e = x.r.CommitSession(ctx, s.ID); e == nil {
		t.Fatal("failure not injected")
	}
	fail.fail = false
	n, e := x.r.CommitSession(ctx, s.ID)
	if e != nil {
		t.Fatal(e)
	}
	if n.Path != "a" {
		t.Fatal(n)
	}
	versions, e := x.r.Versions("a")
	if e != nil || len(versions) != 0 {
		t.Fatal("retry re-published", versions, e)
	}
}

func TestRootRejectsConcurrentDaemonAndMismatchedState(t *testing.T) {
	x := setup(t, true, true)
	put(t, x.r, "a", "A", domain.WriteOptions{})
	v, e := x.r.Snapshot("a", true, nil)
	if e != nil {
		t.Fatal(e)
	}
	second, e := filesystem.Open(x.data)
	if e != nil {
		t.Fatal(e)
	}
	otherState, e := pebble.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	_, e = domain.NewRoot(x.r.Config, second, otherState, x.r.MaxBytes)
	second.Close()
	otherState.Close()
	if e == nil {
		t.Fatal("second root owner accepted")
	}
	x.files.Close()
	f, e := filesystem.Open(x.data)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	other, e := pebble.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if _, e = domain.NewRoot(x.r.Config, f, other, x.r.MaxBytes); e == nil {
		t.Fatal("mismatched state accepted")
	}
	if _, e = os.Stat(filepath.Join(x.data, ".filegate/versions", v.ID)); e != nil {
		t.Fatal("unrelated state removed version", e)
	}
}
func TestNoIndexSearchBudgetIsScoped(t *testing.T) {
	x := setup(t, false, false)
	for _, p := range []string{"elsewhere/a", "elsewhere/b", "wanted/c"} {
		put(t, x.r, p, "x", domain.WriteOptions{})
	}
	page, e := x.r.Search(ctx, "c", "wanted", "", 10, 2)
	if e != nil || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
}
func TestRelocatedPrivateDirectoryCannotBeRead(t *testing.T) {
	x := setup(t, true, true)
	put(t, x.r, "a", "A", domain.WriteOptions{})
	v, e := x.r.Snapshot("a", true, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(x.data, ".filegate"), filepath.Join(x.data, "renamed-private")); e != nil {
		t.Fatal(e)
	}
	if _, e = x.r.Open("renamed-private/versions/" + v.ID); e == nil {
		t.Fatal("relocated private content accessible")
	}
}

func TestSlowSegmentDoesNotHoldRootLock(t *testing.T) {
	x := setup(t, true, true)
	s, e := x.r.CreateSession("a", 3, domain.WriteOptions{})
	if e != nil {
		t.Fatal(e)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := x.r.PutSegment(ctx, s.ID, 0, &signaledReader{reader, started}); done <- e }()
	<-started
	written := make(chan error, 1)
	go func() { _, e := x.r.Put(ctx, "other", strings.NewReader("x"), domain.WriteOptions{}); written <- e }()
	select {
	case e := <-written:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("segment network read holds root lock")
	}
	writer.Write([]byte("abc"))
	writer.Close()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

type signaledReader struct {
	io.Reader
	started chan struct{}
}

func (r *signaledReader) Read(b []byte) (int, error) {
	if r.started != nil {
		close(r.started)
		r.started = nil
	}
	return r.Reader.Read(b)
}
func TestCooldownExpiresAndPinsSurvivePruning(t *testing.T) {
	x := setup(t, true, true)
	n := put(t, x.r, "a", "A", domain.WriteOptions{})
	put(t, x.r, "a", "B", domain.WriteOptions{OnConflict: "overwrite"})
	put(t, x.r, "a", "C", domain.WriteOptions{OnConflict: "overwrite"})
	vs, e := x.r.Versions("a")
	if e != nil || len(vs) != 1 {
		t.Fatal(vs, e)
	}
	v := vs[0]
	v.Created = time.Now().Add(-2 * time.Minute)
	if e = x.state.Put("v/"+n.ID+"/"+v.ID, v); e != nil {
		t.Fatal(e)
	}
	put(t, x.r, "a", "D", domain.WriteOptions{OnConflict: "overwrite"})
	vs, e = x.r.Versions("a")
	if e != nil || len(vs) != 2 {
		t.Fatal(vs, e)
	}
	f, e := x.r.OpenVersion("a", vs[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "C" {
		t.Fatal("cooldown captured wrong content", string(b))
	}
	pin, e := x.r.Snapshot("a", true, domain.Metadata{"message": "keep"})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if _, e = x.r.Snapshot("a", false, nil); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = x.r.Prune(ctx); e != nil {
		t.Fatal(e)
	}
	vs, e = x.r.Versions("a")
	if e != nil || len(vs) != 3 {
		t.Fatal(vs, e)
	}
	found := false
	for _, v := range vs {
		found = found || v.ID == pin.ID
	}
	if !found {
		t.Fatal("pin pruned")
	}
}
