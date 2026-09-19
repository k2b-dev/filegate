//go:build linux

package integration_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k2b-dev/filegate/v6/domain"
)

func TestTransferConflictMatrix(t *testing.T) {
	for _, directory := range []bool{false, true} {
		for _, move := range []bool{false, true} {
			for _, policy := range []string{"error", "rename", "overwrite"} {
				t.Run(strings.Join([]string{map[bool]string{true: "directory", false: "file"}[directory], map[bool]string{true: "move", false: "copy"}[move], policy}, "/"), func(t *testing.T) {
					x := setup(t, true, true)
					x.r.Config.Managed = true
					source, target := "source.txt", "target.txt"
					if directory {
						source, target = "source", "target"
						put(t, x.r, source+"/child", "source", domain.WriteOptions{})
						put(t, x.r, target+"/child", "target", domain.WriteOptions{})
					} else {
						put(t, x.r, source, "source", domain.WriteOptions{})
						put(t, x.r, target, "target", domain.WriteOptions{})
					}
					before, e := x.r.Stat(source)
					if e != nil {
						t.Fatal(e)
					}
					n, e := domain.Transfer(ctx, x.r, source, x.r, target, move, domain.WriteOptions{OnConflict: policy})
					reject := policy == "error" || directory && policy == "overwrite"
					if reject {
						if !errors.Is(e, domain.ErrConflict) {
							t.Fatalf("expected conflict, got %v", e)
						}
						return
					}
					if e != nil {
						t.Fatal(e)
					}
					if policy == "rename" && n.Path == target {
						t.Fatal("did not return actual renamed target")
					}
					if policy == "overwrite" && n.Path != target {
						t.Fatal(n)
					}
					if move && n.ID != before.ID {
						t.Fatal("move changed identity")
					}
					if !move && n.ID == before.ID {
						t.Fatal("copy reused source identity")
					}
					selected := n.Path
					if directory {
						selected += "/child"
					}
					if read(t, x.r, selected) != "source" {
						t.Fatal("wrong bytes")
					}
					if move {
						if _, e = os.Stat(filepath.Join(x.data, source)); !errors.Is(e, os.ErrNotExist) {
							t.Fatal("source retained", e)
						}
					} else if _, e = x.r.Stat(source); e != nil {
						t.Fatal(e)
					}
					page, e := x.r.Search(ctx, "", ".", domain.ListingOptions{Limit: 100, Sort: "name"})
					if e != nil {
						t.Fatal(e)
					}
					for _, item := range page.Items {
						if move && (item.Path == source || strings.HasPrefix(item.Path, source+"/")) {
							t.Fatal("stale source index", item)
						}
					}
				})
			}
		}
	}
}

func TestDuplicateExactPathWithRename(t *testing.T) {
	for _, directory := range []bool{false, true} {
		x := setup(t, true, false)
		p := "same"
		if directory {
			put(t, x.r, p+"/child", "x", domain.WriteOptions{})
		} else {
			put(t, x.r, p, "x", domain.WriteOptions{})
		}
		for _, move := range []bool{false, true} {
			n, e := domain.Transfer(ctx, x.r, p, x.r, p, move, domain.WriteOptions{OnConflict: "rename"})
			if e != nil {
				t.Fatal(directory, move, e)
			}
			if n.Path == p {
				t.Fatal("same target")
			}
		}
	}
}

func TestNativeMovePreservesHistoryAndRevision(t *testing.T) {
	x := setup(t, true, true)
	x.r.Config.Managed = true
	old := put(t, x.r, "source", "one", domain.WriteOptions{})
	old = put(t, x.r, "source", "two", domain.WriteOptions{OnConflict: "overwrite"})
	put(t, x.r, "dest", "old", domain.WriteOptions{})
	put(t, x.r, "dest", "new", domain.WriteOptions{OnConflict: "overwrite"})
	n, e := x.r.MoveWithOptions("source", "dest", domain.WriteOptions{OnConflict: "overwrite"})
	if e != nil {
		t.Fatal(e)
	}
	if n.ID != old.ID || n.Revision != old.Revision {
		t.Fatalf("identity/revision changed: %+v => %+v", old, n)
	}
	vs, e := x.r.Versions("dest")
	if e != nil || len(vs) != 1 {
		t.Fatal(vs, e)
	}
	f, e := x.r.OpenVersion("dest", vs[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	reopen(t, x)
	after, e := x.r.Stat("dest")
	if e != nil || after.Revision != n.Revision {
		t.Fatal(after, e)
	}
}

type ambiguousMoveFiles struct {
	domain.Files
	source    string
	syncCalls int
	failSync  bool
}

func (f *ambiguousMoveFiles) Rename(a, b string, replace bool) error {
	err := f.Files.Rename(a, b, replace)
	if err == nil && a == f.source {
		return os.ErrExist
	}
	return err
}
func (f *ambiguousMoveFiles) Sync(p string) error {
	f.syncCalls++
	if f.failSync {
		return errors.New("ambiguous move sync failure")
	}
	return f.Files.Sync(p)
}
func TestNativeMoveRecoversAmbiguousAppliedRename(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		x := setup(t, false, false)
		put(t, x.r, "source", "source", domain.WriteOptions{})
		put(t, x.r, "target", "occupied", domain.WriteOptions{})
		files := &ambiguousMoveFiles{Files: x.files, source: "source", failSync: failSync}
		x.r.Files = files
		result, err := x.r.MoveWithOptions("source", "target", domain.WriteOptions{OnConflict: "rename"})
		if failSync {
			if err == nil {
				t.Fatal("sync failure ignored")
			}
			if files.syncCalls != 1 {
				t.Fatal(files.syncCalls)
			}
			files.failSync = false
			// Trigger the same durable reconciliation used after a lost response.
			page, e := x.r.List(ctx, ".", domain.ListingOptions{})
			if e != nil {
				t.Fatal(e)
			}
			found := false
			for _, n := range page.Items {
				if n.Path != "target" {
					result = n
					found = true
				}
			}
			if !found {
				t.Fatal(page)
			}
		} else if err != nil || files.syncCalls != 2 {
			t.Fatal(err, files.syncCalls)
		}
		if result.Path == "source" || result.Path == "target" {
			t.Fatal(result)
		}
		if read(t, x.r, result.Path) != "source" || read(t, x.r, "target") != "occupied" {
			t.Fatal("wrong content")
		}
		if _, e := os.Stat(filepath.Join(x.data, "source")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
	}
}
