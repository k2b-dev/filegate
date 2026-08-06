package domain

import (
	"errors"
	"fmt"
	"testing"
)

var errFlatKeyCallbackReentry = errors.New("index method called from flat-key callback")

type guardedSweepIndex struct {
	Index
	entries      []string
	inCallback   bool
	iterateCalls int
	deleted      int
}

func (i *guardedSweepIndex) IterateFlatKeys(_ string, _ string, after string, limit int, fn func(string, FileID) (bool, error)) error {
	if limit != 4096 {
		return fmt.Errorf("flat-key page limit = %d, want 4096", limit)
	}
	i.iterateCalls++
	seen := 0
	for n, rel := range i.entries {
		if rel <= after {
			continue
		}
		var id FileID
		id[0] = byte(n >> 8)
		id[1] = byte(n)
		i.inCallback = true
		cont, err := fn(rel, id)
		i.inCallback = false
		if err != nil {
			return err
		}
		seen++
		if !cont || seen == limit {
			break
		}
	}
	return nil
}

func (i *guardedSweepIndex) GetEntity(FileID) (*Entity, error) {
	if i.inCallback {
		return nil, errFlatKeyCallbackReentry
	}
	return nil, ErrNotFound
}

func (i *guardedSweepIndex) Batch(fn func(Batch) error) error {
	if i.inCallback {
		return errFlatKeyCallbackReentry
	}
	return fn(&countingFlatKeyBatch{index: i})
}

type countingFlatKeyBatch struct {
	Batch
	index *guardedSweepIndex
}

func (b *countingFlatKeyBatch) DelFlatKey(string, string) {
	b.index.deleted++
}

func TestSweepStaleFlatKeysPagesBeforeIndexLookups(t *testing.T) {
	idx := &guardedSweepIndex{entries: make([]string, 4097)}
	for n := range idx.entries {
		idx.entries[n] = fmt.Sprintf("file-%04d", n)
	}
	svc := &Service{idx: idx, mountNames: []string{"root"}}

	if err := svc.sweepStaleFlatKeysForMounts(nil); err != nil {
		t.Fatalf("sweep stale flat keys: %v", err)
	}
	if idx.iterateCalls != 2 {
		t.Fatalf("iterator calls = %d, want 2 bounded pages", idx.iterateCalls)
	}
	if idx.deleted != len(idx.entries) {
		t.Fatalf("deleted flat keys = %d, want %d", idx.deleted, len(idx.entries))
	}
}
