package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type versionCountingState struct {
	failAtBatch int
	State
	values                                           map[string][]byte
	keys                                             []string
	gets, scans, reverse, visited, batches, maxBatch int
	failBatch                                        bool
}

func newVersionState() *versionCountingState {
	return &versionCountingState{values: map[string][]byte{}}
}
func (s *versionCountingState) reset() {
	s.gets = 0
	s.scans = 0
	s.reverse = 0
	s.visited = 0
	s.batches = 0
	s.maxBatch = 0
}
func (s *versionCountingState) Get(key string, out any) error {
	s.gets++
	b, ok := s.values[key]
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal(b, out)
}
func (s *versionCountingState) Put(key string, value any) error {
	c, err := encoded(key, value)
	if err != nil {
		return err
	}
	return s.Batch([]Change{c})
}
func (s *versionCountingState) Delete(key string) error {
	return s.Batch([]Change{{Key: key, Delete: true}})
}
func (s *versionCountingState) Batch(changes []Change) error {
	s.batches++
	if len(changes) > s.maxBatch {
		s.maxBatch = len(changes)
	}
	if s.failBatch || s.failAtBatch != 0 && s.batches == s.failAtBatch {
		return errors.New("injected batch failure")
	}
	for _, change := range changes {
		_, exists := s.values[change.Key]
		i := sort.SearchStrings(s.keys, change.Key)
		if change.Delete {
			delete(s.values, change.Key)
			if exists {
				s.keys = append(s.keys[:i], s.keys[i+1:]...)
			}
			continue
		}
		s.values[change.Key] = append([]byte(nil), change.Value...)
		if !exists {
			s.keys = append(s.keys, "")
			copy(s.keys[i+1:], s.keys[i:])
			s.keys[i] = change.Key
		}
	}
	return nil
}
func (s *versionCountingState) Scan(prefix string, fn func(string, []byte) error) error {
	s.scans++
	// Callbacks may write other key families while the scan remains a snapshot.
	keys := append([]string(nil), s.keys...)
	for i := sort.SearchStrings(keys, prefix); i < len(keys) && strings.HasPrefix(keys[i], prefix); i++ {
		s.visited++
		if err := fn(keys[i], s.values[keys[i]]); err != nil {
			return err
		}
	}
	return nil
}
func (s *versionCountingState) ScanBefore(prefix, before string, fn func(string, []byte) error) error {
	s.reverse++
	if before == "" {
		before = prefix + "\xff"
	}
	for i := sort.SearchStrings(s.keys, before) - 1; i >= 0 && strings.HasPrefix(s.keys[i], prefix); i-- {
		s.visited++
		if err := fn(s.keys[i], s.values[s.keys[i]]); err != nil {
			return err
		}
	}
	return nil
}
func (s *versionCountingState) Close() error { return nil }
func (s *versionCountingState) seed(key string, value any) {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	s.values[key] = b
	s.keys = append(s.keys, key)
}

type versionTestFiles struct {
	Files
	directory string
}

func (f *versionTestFiles) Open(path string, flag int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(filepath.Join(f.directory, path), flag, mode)
}
func (f *versionTestFiles) ID(*os.File) (string, error) { return "file", nil }
func (f *versionTestFiles) Identity(os.FileInfo) (uint64, uint64, uint32, uint32, uint64) {
	return 1, 1, 0, 0, 1
}
func (f *versionTestFiles) Clone(src, dst *os.File) (bool, error) {
	_, err := io.Copy(dst, src)
	return false, err
}
func (f *versionTestFiles) Sync(string) error { return nil }
func (f *versionTestFiles) Remove(path string, _ bool) error {
	return os.Remove(filepath.Join(f.directory, path))
}
func versionFixture(t *testing.T) (*Root, *versionCountingState, *time.Time) {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, ".filegate/versions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file"), []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	state := newVersionState()
	root := &Root{rootShared: &rootShared{now: func() time.Time { return now }}, State: state, Files: &versionTestFiles{directory: directory}, Config: RootConfig{Name: "test", Index: true, Versioning: Versioning{Enabled: true, Cooldown: time.Minute}}}
	return root, state, &now
}
func TestVersionCooldownWorkIndependentOfHistorySize(t *testing.T) {
	for _, size := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root, state, now := versionFixture(t)
			base := *now
			var latest versionHead
			for i := 0; i < size; i++ {
				v := Version{ID: fmt.Sprintf("%09d", i), FileID: "file", Created: base.Add(time.Duration(i) * time.Second)}
				head := versionHead{ID: v.ID, Created: v.Created}
				latest = head
				state.seed("v/file/"+v.ID, v)
				state.seed(versionOrderKey("file", head), head)
			}
			state.seed("vl/file", latest)
			sort.Strings(state.keys)
			*now = latest.Created.Add(30 * time.Second)
			state.reset()
			v, err := root.snapshot(Node{Path: "file", ID: "file"}, false, Metadata{}, false)
			if err != nil || v != nil {
				t.Fatalf("skipped snapshot: %v %v", v, err)
			}
			if state.gets != 1 || state.scans != 0 || state.reverse != 0 || state.batches != 0 {
				t.Fatalf("skip cost depends on history: %+v", []int{state.gets, state.scans, state.reverse, state.batches})
			}
			*now = latest.Created.Add(time.Minute)
			state.reset()
			v, err = root.snapshot(Node{Path: "file", ID: "file"}, false, Metadata{}, false)
			if err != nil || v == nil {
				t.Fatalf("append: %v %v", v, err)
			}
			if state.gets != 2 || state.scans != 0 || state.reverse != 0 || state.batches != 1 || state.maxBatch != 3 {
				t.Fatalf("append cost: %+v", []int{state.gets, state.scans, state.reverse, state.batches, state.maxBatch})
			}
			state.reset()
			if err = root.removeVersion(*v); err != nil {
				t.Fatal(err)
			}
			if state.gets != 1 || state.reverse != 1 || state.visited != 1 || state.scans != 0 || state.batches != 1 || state.maxBatch != 3 {
				t.Fatalf("delete latest cost: %+v", []int{state.gets, state.reverse, state.visited, state.scans, state.batches, state.maxBatch})
			}
			actual, err := root.latestVersion("file")
			if err != nil || actual.ID != latest.ID {
				t.Fatalf("previous pointer: %+v %v", actual, err)
			}
		})
	}
}
func TestVersionOrderStartupRebuildBoundedAndIdempotent(t *testing.T) {
	root, state, now := versionFixture(t)
	for i := 0; i < 1400; i++ {
		v := Version{ID: fmt.Sprintf("%09d", i), FileID: fmt.Sprintf("file-%d", i%2), Created: now.Add(time.Duration(i) * time.Second), Pinned: i%3 == 0, Metadata: Metadata{"message": "retained"}}
		state.seed("v/"+v.FileID+"/"+v.ID, v)
	}
	sort.Strings(state.keys)
	state.failAtBatch = 2
	if err := root.ensureVersionOrder(); err == nil {
		t.Fatal("expected interrupted rebuild")
	}
	if _, exists := state.values[versionOrderFormat]; exists {
		t.Fatal("interrupted rebuild marked complete")
	}
	state.failAtBatch = 0
	state.reset()
	if err := root.ensureVersionOrder(); err != nil {
		t.Fatal(err)
	}
	if state.maxBatch > 512 || state.scans != 1 || state.visited != 1400 {
		t.Fatalf("rebuild cost: maxBatch=%d scans=%d rows=%d", state.maxBatch, state.scans, state.visited)
	}
	for _, id := range []string{"file-0", "file-1"} {
		head, err := root.latestVersion(id)
		if err != nil || head.ID == "" {
			t.Fatalf("missing head %s: %+v %v", id, head, err)
		}
	}
	state.reset()
	if err := root.ensureVersionOrder(); err != nil {
		t.Fatal(err)
	}
	if state.gets != 1 || state.scans != 0 || state.batches != 0 {
		t.Fatal("startup repeated full rebuild")
	}
	// Repeating an interrupted rebuild is safe; the durable records are unchanged.
	if err := state.Delete(versionOrderFormat); err != nil {
		t.Fatal(err)
	}
	if err := root.ensureVersionOrder(); err != nil {
		t.Fatal(err)
	}
	versions, err := root.versions("file-0")
	if err != nil || len(versions) != 700 || versions[0].Metadata["message"] != "retained" {
		t.Fatalf("durable history changed: %d %v", len(versions), err)
	}
}
func TestVersionOrderAtomicAppendAndChronologicalDelete(t *testing.T) {
	root, state, now := versionFixture(t)
	first := Version{ID: "first", FileID: "file", Created: *now}
	if err := root.recordVersion(first, versionHead{}); err != nil {
		t.Fatal(err)
	}
	head, err := root.latestVersion("file")
	if err != nil {
		t.Fatal(err)
	}
	second := Version{ID: "second", FileID: "file", Created: now.Add(time.Second)}
	state.failBatch = true
	if err := root.recordVersion(second, head); err == nil {
		t.Fatal("expected batch failure")
	}
	state.failBatch = false
	current, _ := root.latestVersion("file")
	if current.ID != "first" {
		t.Fatal("failed append moved cooldown pointer")
	}
	if _, exists := state.values["v/file/second"]; exists {
		t.Fatal("failed append created partial record")
	}
	// Clock rollback and equal creation times keep deterministic chronological order.
	older := Version{ID: "older", FileID: "file", Created: now.Add(-time.Minute)}
	if err := root.recordVersion(older, head); err != nil {
		t.Fatal(err)
	}
	equal := Version{ID: "z-last", FileID: "file", Created: *now}
	if err := root.recordVersion(equal, head); err != nil {
		t.Fatal(err)
	}
	if err := root.removeVersionRecord(equal); err != nil {
		t.Fatal(err)
	}
	current, _ = root.latestVersion("file")
	if current.ID != "first" {
		t.Fatalf("same-time previous: %s", current.ID)
	}
	if err := root.removeVersionRecord(first); err != nil {
		t.Fatal(err)
	}
	current, _ = root.latestVersion("file")
	if current.ID != "older" {
		t.Fatalf("older previous: %s", current.ID)
	}
	if err := root.removeVersionRecord(older); err != nil {
		t.Fatal(err)
	}
	current, _ = root.latestVersion("file")
	if current.ID != "" {
		t.Fatalf("last delete left pointer: %s", current.ID)
	}
}
func TestAutosaveCooldownRetainsMinuteContentAndManualPin(t *testing.T) {
	root, _, now := versionFixture(t)
	start := *now
	files := root.Files.(*versionTestFiles)
	for second := 0; second < 120; second++ {
		*now = start.Add(time.Duration(second) * time.Second)
		_, err := root.snapshot(Node{Path: "file", ID: "file"}, false, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(files.directory, "file"), []byte(fmt.Sprintf("write-%d", second)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := root.versions("file")
	if err != nil || len(versions) != 2 {
		t.Fatalf("120 writes produced %d versions: %v", len(versions), err)
	}
	var captured []string
	for _, v := range versions {
		b, err := os.ReadFile(filepath.Join(files.directory, ".filegate/versions", v.ID))
		if err != nil {
			t.Fatal(err)
		}
		captured = append(captured, string(b))
	}
	if strings.Join(captured, ",") != "write-59,initial" {
		t.Fatalf("cooldown contents: %v", captured)
	}
	pin, err := root.snapshot(Node{Path: "file", ID: "file"}, true, Metadata{"message": "checkpoint"}, true)
	if err != nil || pin == nil {
		t.Fatalf("manual pin during cooldown: %v %v", pin, err)
	}
	*now = now.Add(time.Second)
	forced, err := root.snapshot(Node{Path: "file", ID: "file"}, false, nil, true)
	if err != nil || forced == nil {
		t.Fatal("forced restore snapshot was coalesced", err)
	}
	root.Config.Versioning.Keep = Keep{Last: 1}
	if _, err = root.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	versions, err = root.versions("file")
	if err != nil || len(versions) != 2 {
		t.Fatalf("prune did not retain last+pin: %d %v", len(versions), err)
	}
	found := false
	for _, v := range versions {
		if v.ID == pin.ID && v.Pinned {
			found = true
		}
	}
	if !found {
		t.Fatal("manual checkpoint pin was pruned")
	}
}

func TestVersionDeleteStateFailurePreservesReadableHistory(t *testing.T) {
	root, state, _ := versionFixture(t)
	version, err := root.snapshot(Node{Path: "file", ID: "file"}, false, Metadata{}, true)
	if err != nil {
		t.Fatal(err)
	}
	state.failBatch = true
	if err = root.removeVersion(*version); err == nil {
		t.Fatal("expected durable metadata deletion failure")
	}
	state.failBatch = false
	var retained Version
	if err = state.Get("v/file/"+version.ID, &retained); err != nil {
		t.Fatalf("failed deletion changed history: %v", err)
	}
	file, err := root.Files.Open(".filegate/versions/"+version.ID, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("failed deletion destroyed referenced blob: %v", err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil || string(content) != "initial" {
		t.Fatalf("history bytes after failure: %q %v", content, err)
	}
	if err = root.removeVersion(*version); err != nil {
		t.Fatal(err)
	}
}
