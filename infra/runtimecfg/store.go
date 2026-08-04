// Package runtimecfg persists the last-applied configuration manifest and
// runtime resources such as S3 access keys.
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
	keyManifest     = "config/manifest"
	prefixResource  = "r/"
	prefixBootstrap = "meta/bootstrapped/"
)

// AppliedManifest is the complete desired state accepted by the daemon.
//
// Values are dotted config paths in their canonical JSON representation. The
// revision is a digest of Values, not a sequence number, so the same manifest
// always has the same identity.
type AppliedManifest struct {
	Values    map[string]any `json:"values"`
	Revision  string         `json:"revision"`
	AppliedAt int64          `json:"appliedAt"`
	AppliedBy string         `json:"appliedBy"`
}

// Store is the durable home for declarative configuration and runtime
// resources.
type Store struct {
	db *pebble.DB

	// Shutdown has several error branches that can each reach Close, and
	// closing Pebble twice panics, so closing is made idempotent here rather
	// than relying on every caller to track it.
	closeOnce sync.Once
	closeErr  error
}

// Open creates or opens the store at path.
func Open(path string) (*Store, error) {
	db, err := pebble.Open(path, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("runtimecfg: open %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
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

// Manifest returns the last complete manifest. A missing manifest is a normal
// first-boot state.
func (s *Store) Manifest() (AppliedManifest, bool, error) {
	value, closer, err := s.db.Get([]byte(keyManifest))
	if errors.Is(err, pebble.ErrNotFound) {
		return AppliedManifest{}, false, nil
	}
	if err != nil {
		return AppliedManifest{}, false, err
	}
	defer func() { _ = closer.Close() }()

	var manifest AppliedManifest
	if err := json.Unmarshal(value, &manifest); err != nil {
		return AppliedManifest{}, false, fmt.Errorf("runtimecfg: decode manifest: %w", err)
	}
	if manifest.Values == nil {
		manifest.Values = map[string]any{}
	}
	return manifest, true, nil
}

// SetManifest atomically replaces the complete desired state.
func (s *Store) SetManifest(manifest AppliedManifest) error {
	if manifest.Values == nil {
		manifest.Values = map[string]any{}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("runtimecfg: encode manifest: %w", err)
	}
	return s.db.Set([]byte(keyManifest), encoded, pebble.Sync)
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
