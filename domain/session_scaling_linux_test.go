//go:build linux

package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sessionCountingState struct {
	State
	values                                 map[string][]byte
	gets, batches, reads, writes, maxValue int
}

func (s *sessionCountingState) Get(key string, value any) error {
	s.gets++
	b, ok := s.values[key]
	if !ok {
		return os.ErrNotExist
	}
	s.reads += len(b)
	return json.Unmarshal(b, value)
}
func (s *sessionCountingState) Put(key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.values[key] = b
	return nil
}
func (s *sessionCountingState) Batch(changes []Change) error {
	s.batches++
	for _, change := range changes {
		if change.Delete {
			delete(s.values, change.Key)
			continue
		}
		s.values[change.Key] = append([]byte{}, change.Value...)
		s.writes += len(change.Value)
		if len(change.Value) > s.maxValue {
			s.maxValue = len(change.Value)
		}
	}
	return nil
}

// Only state scaling is measured here. A reusable tmpfs inode removes disk
// throughput from the result; integration tests cover real chunk durability.
type sessionScalingFiles struct {
	Files
	path string
}

func (f sessionScalingFiles) Open(string, int, os.FileMode) (*os.File, error) {
	return os.OpenFile(f.path, os.O_RDWR|os.O_TRUNC, 0600)
}
func (f sessionScalingFiles) Rename(string, string, bool) error { return nil }
func (f sessionScalingFiles) Remove(string, bool) error         { return nil }

func TestSessionChunkStateAndResponsesScaleLinearly(t *testing.T) {
	directory, err := os.MkdirTemp("/dev/shm", "filegate-session-")
	if err != nil {
		t.Skipf("tmpfs unavailable: %v", err)
	}
	defer os.RemoveAll(directory)
	name := filepath.Join(directory, "chunk")
	if err := os.WriteFile(name, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var priorRead, priorWrite int
	for _, count := range []int{1000, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			state := &sessionCountingState{values: map[string][]byte{}}
			root := &Root{Config: RootConfig{Name: "test"}, State: state, Files: sessionScalingFiles{path: name}, MaxBytes: 10000, rootShared: &rootShared{now: time.Now}}
			session, err := root.CreateSession("file", int64(count), WriteOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			session.ChunkSize = 1
			if err := state.Put("session/"+session.ID, session); err != nil {
				t.Fatal(err)
			}
			state.gets, state.batches, state.reads, state.writes, state.maxValue = 0, 0, 0, 0, 0
			for index := 0; index < count; index++ {
				result, err := root.PutSegment(context.Background(), session.ID, index, strings.NewReader("x"))
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if len(encoded) > 600 {
					t.Fatalf("growing response: segment %d, bytes %d", index, len(encoded))
				}
			}
			if state.gets != 5*count || state.batches != count {
				t.Fatalf("nonconstant state operations: gets=%d batches=%d chunks=%d", state.gets, state.batches, count)
			}
			if state.maxValue > 600 {
				t.Fatalf("growing state value: %d", state.maxValue)
			}
			if count == 10000 && (state.reads > 11*priorRead || state.writes > 11*priorWrite) {
				t.Fatalf("superlinear bytes: read %d -> %d, write %d -> %d", priorRead, state.reads, priorWrite, state.writes)
			}
			priorRead, priorWrite = state.reads, state.writes
			t.Logf("chunks=%d reads=%d written=%d gets=%d batches=%d largest=%d", count, state.reads, state.writes, state.gets, state.batches, state.maxValue)
		})
	}
}
