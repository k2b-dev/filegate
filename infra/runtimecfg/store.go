// Package runtimecfg persists configuration overrides and runtime resources
// such as S3 access keys.
//
// It deliberately opens its own Pebble instance, separate from the metadata
// index. The index is a derived artifact that can be rebuilt from the
// filesystem at any time, and `fg index rescan --new` removes its directory
// outright, so anything authoritative sharing that store would be destroyed by
// a routine rebuild. Runtime configuration is reconstructible from nothing and
// must therefore live somewhere disposable operations cannot reach.
package runtimecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

// ErrNotFound is returned for a missing key or resource.
var ErrNotFound = errors.New("runtimecfg: not found")

const (
	prefixOverride  = "o/"
	prefixResource  = "r/"
	prefixBootstrap = "meta/bootstrapped/"
)

// Store is the durable home for runtime configuration.
type Store struct {
	db *pebble.DB

	// Shutdown has several error branches that can each reach Close, and
	// closing Pebble twice panics, so closing is made idempotent here rather
	// than relying on every caller to track it.
	closeOnce sync.Once
	closeErr  error

	// Overrides are read on every request once the snapshot is wired up, so
	// they are cached in memory and only re-read when something writes.
	mu        sync.RWMutex
	overrides map[string]json.RawMessage
}

// Open creates or opens the store at path.
func Open(path string) (*Store, error) {
	db, err := pebble.Open(path, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("runtimecfg: open %q: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.reloadOverrides(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

func (s *Store) reloadOverrides() error {
	loaded := make(map[string]json.RawMessage)
	if err := s.each(prefixOverride, func(key string, value []byte) error {
		loaded[key] = append(json.RawMessage(nil), value...)
		return nil
	}); err != nil {
		return err
	}

	s.mu.Lock()
	s.overrides = loaded
	s.mu.Unlock()
	return nil
}

func (s *Store) each(prefix string, fn func(key string, value []byte) error) error {
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte(prefix),
		UpperBound: []byte(prefixUpperBound(prefix)),
	})
	if err != nil {
		return err
	}
	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		value, err := iter.ValueAndErr()
		if err != nil {
			return err
		}
		if err := fn(strings.TrimPrefix(string(iter.Key()), prefix), value); err != nil {
			return err
		}
	}
	return iter.Error()
}

func prefixUpperBound(prefix string) string {
	raw := []byte(prefix)
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] < 0xff {
			out := append([]byte(nil), raw[:i+1]...)
			out[i]++
			return string(out)
		}
	}
	return ""
}

// Overrides returns every stored config override, keyed by dotted config path.
//
// Only overrides are stored, never the fully resolved configuration: a value
// nobody has touched keeps following its built-in default, including when that
// default changes in a later release.
func (s *Store) Overrides() map[string]json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]json.RawMessage, len(s.overrides))
	for key, value := range s.overrides {
		out[key] = value
	}
	return out
}

// SetOverrides writes config overrides atomically. A nil value clears the key,
// which makes it fall back to the static source or the built-in default.
func (s *Store) SetOverrides(values map[string]any) error {
	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	for path, value := range values {
		key := []byte(prefixOverride + path)
		if value == nil {
			if err := batch.Delete(key, nil); err != nil {
				return err
			}
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("runtimecfg: encode %q: %w", path, err)
		}
		if err := batch.Set(key, encoded, nil); err != nil {
			return err
		}
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		return err
	}
	return s.reloadOverrides()
}

// PutResource stores one runtime resource, such as an S3 access key.
func (s *Store) PutResource(kind, id string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("runtimecfg: encode %s/%s: %w", kind, id, err)
	}
	return s.db.Set([]byte(resourceKey(kind, id)), encoded, pebble.Sync)
}

// GetResource loads one resource into out.
func (s *Store) GetResource(kind, id string, out any) error {
	value, closer, err := s.db.Get([]byte(resourceKey(kind, id)))
	if errors.Is(err, pebble.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	return json.Unmarshal(value, out)
}

// ListResources returns every stored resource of a kind, as raw JSON so the
// caller decodes into its own type.
func (s *Store) ListResources(kind string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage)
	err := s.each(prefixResource+kind+"/", func(id string, value []byte) error {
		out[id] = append(json.RawMessage(nil), value...)
		return nil
	})
	return out, err
}

// DeleteResource removes a resource permanently. Deletion must survive a
// restart even when a seed variable that originally created the resource is
// still set; see Bootstrapped.
func (s *Store) DeleteResource(kind, id string) error {
	return s.db.Delete([]byte(resourceKey(kind, id)), pebble.Sync)
}

func resourceKey(kind, id string) string {
	return prefixResource + kind + "/" + id
}

// Bootstrapped reports whether seeding has already run against this store.
//
// This is what makes environment and CLI seeding a one-time bootstrap rather
// than a reconciliation loop. Without it, an operator who revokes a compromised
// credential would find it recreated at the next restart, because the variable
// that originally seeded it is still sitting in a deployment file.
// The marker is per kind: a deployment that enables S3 only later must still
// get its configured keys seeded, which a single global marker would prevent.
func (s *Store) Bootstrapped(kind string) (time.Time, bool, error) {
	value, closer, err := s.db.Get([]byte(prefixBootstrap + kind))
	if errors.Is(err, pebble.ErrNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = closer.Close() }()

	stamp, parseErr := time.Parse(time.RFC3339Nano, string(value))
	if parseErr != nil {
		// A corrupt marker still proves the store was initialized; treating it
		// as un-bootstrapped would re-seed and resurrect deleted resources.
		return time.Time{}, true, nil
	}
	return stamp, true, nil
}

// MarkBootstrapped records that seeding has run. Idempotent.
func (s *Store) MarkBootstrapped(kind string, at time.Time) error {
	return s.db.Set([]byte(prefixBootstrap+kind), []byte(at.UTC().Format(time.RFC3339Nano)), pebble.Sync)
}
