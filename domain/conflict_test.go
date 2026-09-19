package domain

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConflictNamePreservesExtensionAndNameLimit(t *testing.T) {
	for _, in := range []struct {
		name      string
		directory bool
		extension string
	}{
		{"a/report.txt", false, ".txt"},
		{"a/" + strings.Repeat("ü", 120) + ".pdf", false, ".pdf"},
		{"a/" + strings.Repeat("x", 255), true, ""},
		{".hidden", false, ".hidden"},
		{"a/." + strings.Repeat("ü", 126), false, ""},
	} {
		got, err := conflictName(in.name, in.directory)
		if err != nil || path.Dir(got) != path.Dir(in.name) || len(path.Base(got)) > 255 || !utf8.ValidString(got) {
			t.Fatalf("%s: %s %v", in.name, got, err)
		}
		if in.extension != "" && !strings.HasSuffix(got, in.extension) {
			t.Fatal("lost extension", got)
		}
	}
}

type conflictProbeFiles struct {
	Files
	existing    os.FileInfo
	probes      int
	allOccupied bool
}

func (f *conflictProbeFiles) Stat(p string) (os.FileInfo, error) {
	f.probes++
	if p == "file" || f.allOccupied {
		return f.existing, nil
	}
	return nil, os.ErrNotExist
}
func TestConflictChoiceHasBoundedCost(t *testing.T) {
	st, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := &conflictProbeFiles{existing: st}
	root := &Root{Files: files}
	for i := 0; i < 10000; i++ {
		if _, exists, err := root.chooseTarget("file", false, "rename"); err != nil || exists {
			t.Fatal(exists, err)
		}
	}
	if files.probes != 20000 {
		t.Fatal("duplicate cost depends on previous duplicates", files.probes)
	}
	files.allOccupied = true
	files.probes = 0
	if _, _, err := root.chooseTarget("file", false, "rename"); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if files.probes != conflictAttempts+1 {
		t.Fatal("unbounded conflict scan", files.probes)
	}
}

type conflictJournal struct {
	State
	records []publication
}

func (s *conflictJournal) Put(_ string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var p publication
	if err = json.Unmarshal(b, &p); err != nil {
		return err
	}
	s.records = append(s.records, p)
	return nil
}

type conflictRenameFiles struct {
	syncCalls int
	syncErr   error
	Files
	stage, other     os.FileInfo
	journal          *conflictJournal
	calls            int
	collisions       int
	alreadyPublished bool
}

func (f *conflictRenameFiles) Identity(st os.FileInfo) (uint64, uint64, uint32, uint32, uint64) {
	if st.Name() == f.stage.Name() {
		return 1, 2, 0, 0, 1
	}
	return 1, 3, 0, 0, 1
}
func (f *conflictRenameFiles) Stat(p string) (os.FileInfo, error) {
	if p == "stage" || f.alreadyPublished {
		return f.stage, nil
	}
	return f.other, nil
}
func (f *conflictRenameFiles) Sync(string) error {
	f.syncCalls++
	return f.syncErr
}
func (f *conflictRenameFiles) Rename(from, to string, replace bool) error {
	f.calls++
	if replace || from != "stage" {
		return ErrInvalid
	}
	last := f.journal.records[len(f.journal.records)-1]
	if last.Path != to || last.Node.Path != to || last.Claim.Path != to || last.Receipt.Result.Path != to {
		return errors.New("rename target was not durably journaled")
	}
	if f.calls <= f.collisions || f.alreadyPublished {
		return os.ErrExist
	}
	return nil
}
func TestPublicationConflictRetriesUpdateJournalAndReceipt(t *testing.T) {
	for _, collisions := range []int{2, conflictAttempts} {
		t.Run(string(rune('a'+collisions)), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(path.Join(dir, "stage"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			stage, err := os.Stat(path.Join(dir, "stage"))
			if err != nil {
				t.Fatal(err)
			}
			other, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			journal := &conflictJournal{}
			n := Node{Path: "file.txt", Revision: "unchanged-token"}
			rec := publication{Path: n.Path, Temp: "stage", Node: n, Claim: claim{Device: 1, Inode: 2, Path: n.Path}, WriteGeneration: "unchanged-generation", Receipt: &sessionReceipt{Session: Session{Result: &n}}}
			if err = journal.Put("pending/test", rec); err != nil {
				t.Fatal(err)
			}
			files := &conflictRenameFiles{stage: stage, other: other, journal: journal, collisions: collisions}
			root := &Root{Files: files, State: journal}
			err = root.renamePublication("pending/test", &rec, false, "file.txt", "rename")
			if collisions == conflictAttempts {
				if !errors.Is(err, ErrLimit) || files.calls != conflictAttempts {
					t.Fatal(files.calls, err)
				}
			} else if err != nil || files.calls != collisions+1 {
				t.Fatal(files.calls, err)
			}
			if rec.Node.Revision != n.Revision || rec.WriteGeneration != "unchanged-generation" || rec.Receipt.Result.Path != rec.Path {
				t.Fatal("retry changed frozen publication", rec)
			}
			// An ambiguous NOREPLACE response must retain the destination when its
			// inode proves the original call already published successfully.
			files.calls = 0
			files.alreadyPublished = true
			before := len(journal.records)
			if err = root.renamePublication("pending/test", &rec, false, "file.txt", "rename"); err != nil || len(journal.records) != before {
				t.Fatal("abandoned an already published inode", err)
			}
			if files.syncCalls != 2 {
				t.Fatal("ambiguous success omitted durability barriers", files.syncCalls)
			}
			failure := errors.New("injected directory sync failure")
			files.syncErr = failure
			if err = root.renamePublication("pending/test", &rec, false, "file.txt", "rename"); !errors.Is(err, failure) || len(journal.records) != before {
				t.Fatal("sync failure lost pending publication", err)
			}

		})
	}
}
