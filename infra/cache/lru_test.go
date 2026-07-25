package cache

import "testing"

func TestStatsCountsHitsAndMisses(t *testing.T) {
	c, err := NewLRU[string, int](8)
	if err != nil {
		t.Fatalf("NewLRU: %v", err)
	}

	c.Add("a", 1)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected hit for a")
	}
	for range 3 {
		if _, ok := c.Get("missing"); ok {
			t.Fatal("expected miss for missing")
		}
	}

	got := c.Stats()
	if got.Hits != 1 {
		t.Errorf("hits = %d, want 1", got.Hits)
	}
	if got.Misses != 3 {
		t.Errorf("misses = %d, want 3", got.Misses)
	}
	if got.Entries != 1 {
		t.Errorf("entries = %d, want 1", got.Entries)
	}
	if got.Capacity != 8 {
		t.Errorf("capacity = %d, want 8", got.Capacity)
	}
	if want := 0.25; got.HitRatio() != want {
		t.Errorf("hit ratio = %v, want %v", got.HitRatio(), want)
	}
}

func TestStatsHitRatioWithoutLookups(t *testing.T) {
	c, err := NewLRU[string, int](4)
	if err != nil {
		t.Fatalf("NewLRU: %v", err)
	}
	if ratio := c.Stats().HitRatio(); ratio != 0 {
		t.Errorf("hit ratio = %v, want 0 before any lookup", ratio)
	}
}

func TestStatsOnNilCache(t *testing.T) {
	// Callers hold optional caches; Stats has to stay safe on the nil path the
	// way Get and Add already are.
	var c *LRU[string, int]
	if got := c.Stats(); got != (Stats{}) {
		t.Errorf("nil cache stats = %+v, want zero value", got)
	}
}

func TestCapacityDefaultIsReported(t *testing.T) {
	c, err := NewLRU[string, int](0)
	if err != nil {
		t.Fatalf("NewLRU: %v", err)
	}
	if got := c.Stats().Capacity; got != 1024 {
		t.Errorf("capacity = %d, want the 1024 default", got)
	}
}
