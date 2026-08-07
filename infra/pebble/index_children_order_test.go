package pebble

import (
	"errors"
	"testing"

	"github.com/k2b-dev/filegate/v3/domain"
)

func testID(seed byte) domain.FileID {
	var id domain.FileID
	id[15] = seed
	return id
}

func TestListChildrenDirsFirstAndCursorAcrossTypeBoundary(t *testing.T) {
	idx, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	rootID := testID(1)
	parentID := testID(2)
	children := []domain.DirEntry{
		{ID: testID(3), Name: "zdir", IsDir: true},
		{ID: testID(4), Name: "adir", IsDir: true},
		{ID: testID(5), Name: "a.txt", IsDir: false},
		{ID: testID(6), Name: "0.txt", IsDir: false},
	}

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{ID: rootID, ParentID: domain.FileID{}, Name: "root", IsDir: true})
		b.PutEntity(domain.Entity{ID: parentID, ParentID: rootID, Name: "parent", IsDir: true})
		b.PutChild(rootID, "parent", domain.DirEntry{ID: parentID, Name: "parent", IsDir: true})
		for _, child := range children {
			b.PutEntity(domain.Entity{
				ID:       child.ID,
				ParentID: parentID,
				Name:     child.Name,
				IsDir:    child.IsDir,
			})
			b.PutChild(parentID, child.Name, child)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	all, err := idx.ListChildren(parentID, domain.ChildCursor{}, 10)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("children=%d, want=4", len(all))
	}
	want := []string{"adir", "zdir", "0.txt", "a.txt"}
	for i := range want {
		if all[i].Name != want[i] {
			t.Fatalf("child[%d]=%q, want=%q", i, all[i].Name, want[i])
		}
	}

	page1, err := idx.ListChildren(parentID, domain.ChildCursor{}, 2)
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	if len(page1) != 2 || page1[0].Name != "adir" || page1[1].Name != "zdir" {
		t.Fatalf("page1=%v", page1)
	}

	cursor := domain.ChildCursor{Name: page1[1].Name, IsDir: page1[1].IsDir}
	page2, err := idx.ListChildren(parentID, cursor, 10)
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if len(page2) != 2 || page2[0].Name != "0.txt" || page2[1].Name != "a.txt" {
		t.Fatalf("page2=%v", page2)
	}
}

// TestListChildrenCursorSurvivesDeletedEntry pins the typed-cursor
// guarantee: pagination resumes correctly even when the cursor entry
// was deleted between pages — no duplicates, no error. The name-only
// cursor used to fall back to a scan from the beginning here.
func TestListChildrenCursorSurvivesDeletedEntry(t *testing.T) {
	idx, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	parentID := testID(30)
	children := []domain.DirEntry{
		{ID: testID(31), Name: "adir", IsDir: true},
		{ID: testID(32), Name: "zdir", IsDir: true},
		{ID: testID(33), Name: "a.txt", IsDir: false},
		{ID: testID(34), Name: "m.txt", IsDir: false},
		{ID: testID(35), Name: "z.txt", IsDir: false},
	}
	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{ID: parentID, Name: "parent", IsDir: true})
		for _, child := range children {
			b.PutEntity(domain.Entity{ID: child.ID, ParentID: parentID, Name: child.Name, IsDir: child.IsDir})
			b.PutChild(parentID, child.Name, child)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	// File cursor deleted between pages: resume after "m.txt" must
	// return only "z.txt" — not re-list the dirs or earlier files.
	if err := idx.Batch(func(b domain.Batch) error {
		b.DelChild(parentID, "m.txt")
		b.DelEntity(testID(34))
		return nil
	}); err != nil {
		t.Fatalf("delete cursor entry: %v", err)
	}
	page, err := idx.ListChildren(parentID, domain.ChildCursor{Name: "m.txt", IsDir: false}, 10)
	if err != nil {
		t.Fatalf("list after deleted file cursor: %v", err)
	}
	if len(page) != 1 || page[0].Name != "z.txt" {
		t.Fatalf("page after deleted file cursor=%v, want [z.txt]", page)
	}

	// Dir cursor deleted between pages: resume after "adir" must
	// return the remaining dir, then the files — "adir" itself gone.
	if err := idx.Batch(func(b domain.Batch) error {
		b.DelChild(parentID, "adir")
		b.DelEntity(testID(31))
		return nil
	}); err != nil {
		t.Fatalf("delete dir cursor entry: %v", err)
	}
	page, err = idx.ListChildren(parentID, domain.ChildCursor{Name: "adir", IsDir: true}, 10)
	if err != nil {
		t.Fatalf("list after deleted dir cursor: %v", err)
	}
	wantNames := []string{"zdir", "a.txt", "z.txt"}
	if len(page) != len(wantNames) {
		t.Fatalf("page after deleted dir cursor=%v, want %v", page, wantNames)
	}
	for i := range wantNames {
		if page[i].Name != wantNames[i] {
			t.Fatalf("page[%d]=%q, want %q", i, page[i].Name, wantNames[i])
		}
	}
}

func TestLookupChildFindsDirAndFile(t *testing.T) {
	idx, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	parentID := testID(9)
	dir := domain.DirEntry{ID: testID(10), Name: "dir", IsDir: true}
	file := domain.DirEntry{ID: testID(11), Name: "file.txt", IsDir: false}

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{ID: parentID, Name: "parent", IsDir: true})
		b.PutEntity(domain.Entity{ID: dir.ID, ParentID: parentID, Name: dir.Name, IsDir: true})
		b.PutEntity(domain.Entity{ID: file.ID, ParentID: parentID, Name: file.Name, IsDir: false})
		b.PutChild(parentID, dir.Name, dir)
		b.PutChild(parentID, file.Name, file)
		return nil
	}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	gotDir, err := idx.LookupChild(parentID, dir.Name)
	if err != nil {
		t.Fatalf("lookup dir: %v", err)
	}
	if !gotDir.IsDir || gotDir.ID != dir.ID {
		t.Fatalf("lookup dir mismatch: %+v", gotDir)
	}

	gotFile, err := idx.LookupChild(parentID, file.Name)
	if err != nil {
		t.Fatalf("lookup file: %v", err)
	}
	if gotFile.IsDir || gotFile.ID != file.ID {
		t.Fatalf("lookup file mismatch: %+v", gotFile)
	}
}

func TestDelChildDeletesBothTypeSlots(t *testing.T) {
	idx, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer idx.Close()

	parentID := testID(20)
	file := domain.DirEntry{ID: testID(21), Name: "ghost", IsDir: false}

	if err := idx.Batch(func(b domain.Batch) error {
		b.PutEntity(domain.Entity{ID: parentID, Name: "parent", IsDir: true})
		b.PutEntity(domain.Entity{ID: file.ID, ParentID: parentID, Name: file.Name, IsDir: false})
		b.PutChild(parentID, file.Name, file)
		return nil
	}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	if err := idx.Batch(func(b domain.Batch) error {
		b.DelChild(parentID, "ghost")
		return nil
	}); err != nil {
		t.Fatalf("delete child: %v", err)
	}

	_, err = idx.LookupChild(parentID, "ghost")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lookup err=%v, want ErrNotFound", err)
	}
}
