//go:build linux

package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestManagedRevisionStableAcrossReadsAndRestart(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-index", true: "indexed"}[indexed], func(t *testing.T) {
			x := setup(t, indexed, false)
			x.r.Config.Managed = true
			first := put(t, x.r, "file", "original", domain.WriteOptions{})
			if first.Revision == "" {
				t.Fatal("publication lacks revision")
			}
			for i := 0; i < 2; i++ {
				got, err := x.r.Stat("file")
				if err != nil || got.Revision != first.Revision {
					t.Fatalf("unstable revision: %+v %v", got, err)
				}
			}
			reopen(t, x)
			got, err := x.r.Stat("file")
			if err != nil || got.Revision != first.Revision {
				t.Fatalf("restart revision: %+v %v", got, err)
			}
			info, err := x.r.Info()
			if err != nil || !info.Managed {
				t.Fatalf("capability: %+v %v", info, err)
			}
			updated := put(t, x.r, "file", "new", domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: first.Revision}})
			if updated.Revision == first.Revision {
				t.Fatal("publication reused revision")
			}
		})
	}
}

func TestManagedRevisionInitialReadAndExternalChangeDetection(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	name := filepath.Join(x.data, "external")
	if err := os.WriteFile(name, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := x.r.Stat("external")
	if err != nil || first.Revision == "" {
		t.Fatal(first, err)
	}
	// Preserve size and mtime deliberately: the kernel ctime must reject this write.
	if err = os.WriteFile(name, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(name, first.Modified, first.Modified); err != nil {
		t.Fatal(err)
	}
	_, err = x.r.Put(ctx, "external", strings.NewReader("bad"), domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: first.Revision}})
	if !errors.Is(err, domain.ErrPrecondition) {
		t.Fatalf("external write accepted: %v", err)
	}
	if got := read(t, x.r, "external"); got != "two" {
		t.Fatal(got)
	}
	second, err := x.r.Stat("external")
	if err != nil || second.Revision == first.Revision {
		t.Fatal(second, err)
	}
}

func revisionState(t *testing.T, x *fixture) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := x.state.Scan("", func(k string, b []byte) error { out[k] = string(b); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestManagedPreconditionFailureDoesNotMutateTargetOrState(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	original := put(t, x.r, "file", "original", domain.WriteOptions{})
	before := revisionState(t, x)
	for _, target := range []string{"file", "missing/child"} {
		_, err := x.r.Put(ctx, target, strings.NewReader("bad"), domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: "outdated"}})
		if !errors.Is(err, domain.ErrPrecondition) {
			t.Fatalf("%s: %v", target, err)
		}
	}
	if _, err := os.Stat(filepath.Join(x.data, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed condition created parents", err)
	}
	if after := revisionState(t, x); !reflect.DeepEqual(before, after) {
		t.Fatal("failed condition changed durable state")
	}
	got, err := x.r.Stat("file")
	if err != nil || got != original {
		t.Fatal("failed condition changed node", got, err)
	}
	if read(t, x.r, "file") != "original" {
		t.Fatal("failed condition changed bytes")
	}
	// Existing files without xattrs/revisions must not acquire either on failure.
	if err = os.WriteFile(filepath.Join(x.data, "unassigned"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	before = revisionState(t, x)
	_, err = x.r.Put(ctx, "unassigned", strings.NewReader("bad"), domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: "unknown"}})
	if !errors.Is(err, domain.ErrPrecondition) {
		t.Fatal(err)
	}
	if after := revisionState(t, x); !reflect.DeepEqual(before, after) {
		t.Fatal("condition initialized identity or revision")
	}
	f, err := x.files.Open("unassigned", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if id, err := x.files.ID(f); err == nil || id != "" {
		t.Fatal("condition assigned file ID", id, err)
	}
}

func TestManagedConcurrentPublicationHasOneWinner(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "match", true: "absent"}[create], func(t *testing.T) {
			x := setup(t, false, false)
			x.r.Config.Managed = true
			options := domain.WriteOptions{Precondition: &domain.Precondition{IfNoneMatch: true}}
			if !create {
				old := put(t, x.r, "file", "old", domain.WriteOptions{})
				options = domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: old.Revision}}
			}
			start := make(chan struct{})
			results := make(chan error, 8)
			var group sync.WaitGroup
			for i := 0; i < 8; i++ {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					_, err := x.r.Put(ctx, "file", strings.NewReader("new"), options)
					results <- err
				}()
			}
			close(start)
			group.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else if !errors.Is(err, domain.ErrPrecondition) {
					t.Fatal(err)
				}
			}
			if successes != 1 {
				t.Fatalf("%d winners", successes)
			}
		})
	}
}

func TestManagedSessionConflictRetainsSegmentsAndOpenState(t *testing.T) {
	x := setup(t, false, false)
	x.r.Config.Managed = true
	old := put(t, x.r, "file", "old", domain.WriteOptions{})
	s, err := x.r.CreateSession("file", 3, domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: old.Revision}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	put(t, x.r, "file", "other", domain.WriteOptions{OnConflict: "overwrite"})
	for i := 0; i < 2; i++ {
		if _, err = x.r.CommitSession(ctx, s.ID); !errors.Is(err, domain.ErrPrecondition) {
			t.Fatal(err)
		}
		got, err := x.r.Session(s.ID)
		if err != nil || got.State != domain.SessionOpen || got.Received != 3 {
			t.Fatal(got, err)
		}
	}
	if read(t, x.r, "file") != "other" {
		t.Fatal("commit overwrote newer revision")
	}
	if _, err = os.Stat(filepath.Join(x.data, ".filegate/staging", s.ID+"-0")); err != nil {
		t.Fatal("segment lost", err)
	}
}

func TestManagedPublicationRecoveryPreservesRevisionAndReceipt(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	s := createFilledSession(t, x, "file", "abc")
	x.r.State = &failingState{State: x.state, fail: true}
	if _, err := x.r.CommitSession(ctx, s.ID); err == nil {
		t.Fatal("failure not injected")
	}
	var pending struct{ Node domain.Node }
	if err := x.state.Scan("pending/", func(_ string, b []byte) error { return json.Unmarshal(b, &pending) }); err != nil {
		t.Fatal(err)
	}
	if pending.Node.Revision == "" {
		t.Fatal("intent lacks revision")
	}
	reopen(t, x)
	n, err := x.r.CommitSession(ctx, s.ID)
	if err != nil || n.Revision != pending.Node.Revision {
		t.Fatal(n, err)
	}
	got, err := x.r.Stat("file")
	if err != nil || got.Revision != n.Revision {
		t.Fatal(got, err)
	}
	again, err := x.r.CommitSession(ctx, s.ID)
	if err != nil || again != n {
		t.Fatal(again, err)
	}
}

func TestUnmanagedRootRejectsPublicationConditions(t *testing.T) {
	x := setup(t, false, false)
	for _, pre := range []*domain.Precondition{{IfMatch: "token"}, {IfNoneMatch: true}} {
		_, err := x.r.Put(ctx, "missing/file", strings.NewReader("body"), domain.WriteOptions{Precondition: pre})
		if !errors.Is(err, domain.ErrDisabled) {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(x.data, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// Inject a writer between the absent-target check and the rename syscall. Even
// outside the managed-writer contract, IfNoneMatch must never replace that file.
type revisionCreateRaceFiles struct {
	domain.Files
	target string
}

func (f *revisionCreateRaceFiles) Rename(from, to string, replace bool) error {
	if to == "file" {
		if replace {
			return errors.New("IfNoneMatch attempted replace")
		}
		if err := os.WriteFile(f.target, []byte("external"), 0600); err != nil {
			return err
		}
	}
	return f.Files.Rename(from, to, replace)
}
func (f *revisionCreateRaceFiles) ChangeTime(st os.FileInfo) (int64, int64) {
	return f.Files.(interface {
		ChangeTime(os.FileInfo) (int64, int64)
	}).ChangeTime(st)
}
func TestManagedIfNoneMatchUsesAtomicNoReplace(t *testing.T) {
	x := setup(t, false, false)
	x.r.Config.Managed = true
	x.r.Files = &revisionCreateRaceFiles{Files: x.files, target: filepath.Join(x.data, "file")}
	_, err := x.r.Put(ctx, "file", strings.NewReader("upload"), domain.WriteOptions{Precondition: &domain.Precondition{IfNoneMatch: true}})
	if !errors.Is(err, domain.ErrPrecondition) {
		t.Fatal(err)
	}
	if content := read(t, x.r, "file"); content != "external" {
		t.Fatal("replaced raced destination", content)
	}
}

func TestManagedRestoreAndMetadataChangesInvalidateOldRevision(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	put(t, x.r, "file", "first", domain.WriteOptions{})
	last := put(t, x.r, "file", "second", domain.WriteOptions{OnConflict: "overwrite"})
	versions, err := x.r.Versions("file")
	if err != nil || len(versions) != 1 {
		t.Fatal(versions, err)
	}
	restored, err := x.r.Restore("file", versions[0].ID)
	if err != nil || restored.Revision == "" || restored.Revision == last.Revision {
		t.Fatal(restored, err)
	}
	if err = os.Chmod(filepath.Join(x.data, "file"), 0400); err != nil {
		t.Fatal(err)
	}
	_, err = x.r.Put(ctx, "file", strings.NewReader("bad"), domain.WriteOptions{OnConflict: "overwrite", Precondition: &domain.Precondition{IfMatch: restored.Revision}})
	if !errors.Is(err, domain.ErrPrecondition) {
		t.Fatal("metadata modification left stale revision valid", err)
	}
}

func TestManagedOpenWithNodePinsRevisionToDownloadedContent(t *testing.T) {
	x := setup(t, true, false)
	x.r.Config.Managed = true
	first := put(t, x.r, "file", "first", domain.WriteOptions{})
	f, n, err := x.r.OpenWithNode("file")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	put(t, x.r, "file", "second", domain.WriteOptions{OnConflict: "overwrite"})
	content, err := io.ReadAll(f)
	if err != nil || string(content) != "first" || n.Revision != first.Revision {
		t.Fatal(string(content), n, err)
	}
}

func TestManagedUnreadablePublicationAndRecoveryWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run this test as an unprivileged Linux user")
	}
	for _, indexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "index-free", true: "indexed"}[indexed], func(t *testing.T) {
			for _, fail := range []bool{false, true} {
				t.Run(map[bool]string{false: "normal", true: "recover"}[fail], func(t *testing.T) {
					x := setup(t, indexed, false)
					x.r.Config.Managed = true
					s, err := x.r.CreateSession("file", 3, domain.WriteOptions{Ownership: &domain.Ownership{Mode: "0000"}}, "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = x.r.PutSegment(ctx, s.ID, 0, strings.NewReader("new")); err != nil {
						t.Fatal(err)
					}
					if fail {
						x.r.State = &failingState{State: x.state, fail: true}
					}
					n, err := x.r.CommitSession(ctx, s.ID)
					if !fail && (err != nil || n.Revision == "" || n.Mode != "0000") {
						t.Fatal(n, err)
					}
					if fail && err == nil {
						t.Fatal("missing injected failure")
					}
					reopen(t, x)
					final, err := x.r.CommitSession(ctx, s.ID)
					if err != nil || final.Revision == "" || final.Mode != "0000" {
						t.Fatal(final, err)
					}
					st, err := x.files.Stat("file")
					if err != nil || st.Mode().Perm() != 0 {
						t.Fatal(st, err)
					}
				})
			}
		})
	}
}
