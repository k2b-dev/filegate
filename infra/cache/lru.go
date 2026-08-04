package cache

import (
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Stats is a point-in-time view of cache occupancy and effectiveness.
//
// Hits and Misses are cumulative since process start, so a caller that wants a
// rate should sample twice and subtract rather than expecting a windowed value.
type Stats struct {
	Entries  int
	Capacity int
	Hits     uint64
	Misses   uint64
}

// HitRatio reports hits as a fraction of lookups, or 0 when nothing was looked
// up yet.
func (s Stats) HitRatio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// LRU wraps the underlying cache implementation so shared cache behavior
// can be reused across HTTP adapters.
type LRU[K comparable, V any] struct {
	cache    *lru.Cache[K, V]
	capacity int
	hits     atomic.Uint64
	misses   atomic.Uint64
}

// NewLRU creates an LRU cache with the given capacity. If size is <= 0, defaults to 1024.
func NewLRU[K comparable, V any](size int) (*LRU[K, V], error) {
	if size <= 0 {
		size = 1024
	}
	c, err := lru.New[K, V](size)
	if err != nil {
		return nil, err
	}
	return &LRU[K, V]{cache: c, capacity: size}, nil
}

func (l *LRU[K, V]) Get(key K) (V, bool) {
	if l == nil || l.cache == nil {
		var zero V
		return zero, false
	}
	value, ok := l.cache.Get(key)
	if ok {
		l.hits.Add(1)
	} else {
		l.misses.Add(1)
	}
	return value, ok
}

func (l *LRU[K, V]) Add(key K, value V) {
	if l == nil || l.cache == nil {
		return
	}
	l.cache.Add(key, value)
}

func (l *LRU[K, V]) Remove(key K) {
	if l == nil || l.cache == nil {
		return
	}
	l.cache.Remove(key)
}

// Stats reports occupancy and cumulative hit/miss counts. Safe on a nil cache,
// which reports zeroes.
func (l *LRU[K, V]) Stats() Stats {
	if l == nil || l.cache == nil {
		return Stats{}
	}
	return Stats{
		Entries:  l.cache.Len(),
		Capacity: l.capacity,
		Hits:     l.hits.Load(),
		Misses:   l.misses.Load(),
	}
}
