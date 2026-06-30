package provider

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic clock for TTL tests — advanced explicitly so TTL
// expiry never depends on wall-clock sleeps (and is never flaky).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestMemoryCacheHitMiss(t *testing.T) {
	c := NewMemoryCache(CacheConfig{})
	if _, ok := c.Get("k"); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.Set("k", Metadata{"v": 1})
	md, ok := c.Get("k")
	if !ok {
		t.Fatal("expected a hit after Set")
	}
	if md["v"] != 1 {
		t.Fatalf("value = %v, want 1", md["v"])
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v, want Hits=1 Misses=1", s)
	}
	if s.Entries != 1 {
		t.Fatalf("entries = %d, want 1", s.Entries)
	}
}

func TestMemoryCacheTTLExpiry(t *testing.T) {
	clk := newFakeClock()
	c := NewMemoryCache(CacheConfig{TTL: 100 * time.Millisecond, Now: clk.Now})
	c.Set("k", Metadata{"v": "fresh"})

	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired before its TTL elapsed")
	}
	// Advance to exactly the TTL boundary: now == expiresAt counts as expired.
	clk.Advance(100 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry survived past its TTL")
	}
	s := c.Stats()
	if s.Expirations != 1 {
		t.Fatalf("expirations = %d, want 1", s.Expirations)
	}
	if s.Entries != 0 {
		t.Fatalf("expired entry was not dropped: entries = %d", s.Entries)
	}
}

func TestMemoryCacheZeroTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := NewMemoryCache(CacheConfig{TTL: -1, Now: clk.Now}) // <0 -> no expiry
	c.Set("k", Metadata{"v": 1})
	clk.Advance(1000 * time.Hour)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry with disabled TTL expired")
	}
}

func TestMemoryCacheMaxSizeEviction(t *testing.T) {
	c := NewMemoryCache(CacheConfig{MaxSize: 2, TTL: -1})
	c.Set("a", Metadata{"n": 1})
	c.Set("b", Metadata{"n": 2})
	// Touch "a" so "b" becomes the least-recently-used victim.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected a to be present")
	}
	c.Set("c", Metadata{"n": 3}) // exceeds cap -> evicts LRU ("b")

	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU entry b was not evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently-used entry a was wrongly evicted")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("newest entry c is missing")
	}
	s := c.Stats()
	if s.Evictions != 1 {
		t.Fatalf("evictions = %d, want 1", s.Evictions)
	}
	if s.Entries != 2 {
		t.Fatalf("entries = %d, want 2 (cap)", s.Entries)
	}
}

func TestMemoryCacheSetRefreshesInPlace(t *testing.T) {
	clk := newFakeClock()
	c := NewMemoryCache(CacheConfig{TTL: 100 * time.Millisecond, Now: clk.Now})
	c.Set("k", Metadata{"v": 1})
	clk.Advance(60 * time.Millisecond)
	c.Set("k", Metadata{"v": 2}) // refresh resets the TTL window
	clk.Advance(60 * time.Millisecond)
	md, ok := c.Get("k")
	if !ok {
		t.Fatal("refreshed entry expired on the old TTL")
	}
	if md["v"] != 2 {
		t.Fatalf("value = %v, want 2", md["v"])
	}
	if s := c.Stats(); s.Entries != 1 {
		t.Fatalf("in-place refresh changed entry count: %d", s.Entries)
	}
}

func TestMemoryCacheDeleteAndClear(t *testing.T) {
	c := NewMemoryCache(CacheConfig{TTL: -1})
	c.Set("a", Metadata{})
	c.Set("b", Metadata{})
	if !c.Delete("a") {
		t.Fatal("Delete of present key reported false")
	}
	if c.Delete("a") {
		t.Fatal("Delete of absent key reported true")
	}
	c.Clear()
	if s := c.Stats(); s.Entries != 0 {
		t.Fatalf("Clear left %d entries", s.Entries)
	}
	// One delete of "a" + clearing the remaining "b" = 2 invalidations.
	if s := c.Stats(); s.Invalidations != 2 {
		t.Fatalf("invalidations = %d, want 2", s.Invalidations)
	}
}

func TestMemoryCacheConcurrentAccess(t *testing.T) {
	c := NewMemoryCache(CacheConfig{MaxSize: 64, TTL: -1})
	const workers = 32
	const iters = 500
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := string(rune('a' + (i+w)%26))
				if _, ok := c.Get(key); !ok {
					c.Set(key, Metadata{"w": w, "i": i})
				}
				if i%50 == 0 {
					c.Delete(key)
				}
				_ = c.Stats()
			}
		}(w)
	}
	wg.Wait()
	// Invariant: never exceed the cap.
	if s := c.Stats(); s.Entries > 64 {
		t.Fatalf("entries = %d exceeds cap 64", s.Entries)
	}
}

func TestCacheConfigDefaults(t *testing.T) {
	cfg := CacheConfig{}.withDefaults()
	if cfg.TTL != DefaultTTL {
		t.Errorf("default TTL = %v, want %v", cfg.TTL, DefaultTTL)
	}
	if cfg.MaxSize != DefaultMaxSize {
		t.Errorf("default MaxSize = %d, want %d", cfg.MaxSize, DefaultMaxSize)
	}
	if cfg.Now == nil {
		t.Error("default clock is nil")
	}

	// Explicit zero TTL via the option disables expiry rather than adopting the default.
	var opt CacheConfig
	WithTTL(0)(&opt)
	if got := opt.withDefaults().TTL; got != 0 {
		t.Errorf("explicit zero TTL = %v, want 0 (no expiry)", got)
	}
	var optMax CacheConfig
	WithMaxSize(0)(&optMax)
	if got := optMax.withDefaults().MaxSize; got != 0 {
		t.Errorf("explicit zero MaxSize = %d, want 0 (unbounded)", got)
	}
}
