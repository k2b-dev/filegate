// Package pebble stores derived index rows and durable Filegate state.
package pebble

import (
	"encoding/json"
	"errors"
	"github.com/cockroachdb/pebble"
	"github.com/k2b-dev/filegate/v4/domain"
	"os"
)

type State struct{ db *pebble.DB }

func Open(path string) (*State, error) {
	db, e := pebble.Open(path, &pebble.Options{})
	if e != nil {
		return nil, e
	}
	return &State{db}, nil
}
func (s *State) Get(k string, v any) error {
	b, c, e := s.db.Get([]byte(k))
	if errors.Is(e, pebble.ErrNotFound) {
		return os.ErrNotExist
	}
	if e != nil {
		return e
	}
	defer c.Close()
	return json.Unmarshal(b, v)
}
func (s *State) Put(k string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return s.db.Set([]byte(k), b, pebble.Sync)
}
func (s *State) Delete(k string) error { return s.db.Delete([]byte(k), pebble.Sync) }
func (s *State) Batch(cs []domain.Change) error {
	b := s.db.NewBatch()
	defer b.Close()
	for _, c := range cs {
		var e error
		if c.Delete {
			e = b.Delete([]byte(c.Key), nil)
		} else {
			e = b.Set([]byte(c.Key), c.Value, nil)
		}
		if e != nil {
			return e
		}
	}
	return b.Commit(pebble.Sync)
}
func (s *State) Scan(prefix string, fn func(string, []byte) error) error {
	i, e := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte(prefix), UpperBound: []byte(prefix + "\xff")})
	if e != nil {
		return e
	}
	defer i.Close()
	for i.First(); i.Valid(); i.Next() {
		if e := fn(string(i.Key()), i.Value()); e != nil {
			return e
		}
	}
	return i.Error()
}
func (s *State) Close() error { return s.db.Close() }
