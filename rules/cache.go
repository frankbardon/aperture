package rules

import (
	"sync"
	"time"
)

// Clock is the time source the cache reads for TTL expiry. It is injected so
// tests drive expiry deterministically instead of sleeping; production uses the
// real clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// CacheStats reports the compiled-rule cache's counters. They expose the
// hit/miss/eviction behaviour the latency benchmark (E4-S4) tunes against.
type CacheStats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	Entries   int
}

// compiledCache is the concurrency-safe compiled-rule cache. It keys a *Compiled
// by the rule's canonical hash, so distinct rule references whose ASTs render to
// the same expression share one compiled program. An optional TTL (with the
// injected clock) bounds entry lifetime; TTL <= 0 keeps entries until explicitly
// invalidated. The cache is the NFR lever that keeps per-Check rule cost bounded:
// the expensive expr-lang compile happens once per canonical form.
type compiledCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	clock   Clock
	entries map[string]cacheEntry

	hits      uint64
	misses    uint64
	evictions uint64
}

type cacheEntry struct {
	compiled  *Compiled
	expiresAt time.Time // zero when no TTL applies
}

func newCompiledCache(ttl time.Duration, clock Clock) *compiledCache {
	if clock == nil {
		clock = realClock{}
	}
	return &compiledCache{
		ttl:     ttl,
		clock:   clock,
		entries: make(map[string]cacheEntry),
	}
}

// get returns the cached program for hash when present and unexpired. An expired
// entry is dropped and counted as a miss (and an eviction).
func (c *compiledCache) get(hash string) (*Compiled, bool) {
	c.mu.RLock()
	e, ok := c.entries[hash]
	c.mu.RUnlock()
	if ok && !c.expired(e) {
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return e.compiled, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-read under the write lock: another goroutine may have refreshed it.
	if e, ok := c.entries[hash]; ok {
		if !c.expired(e) {
			c.hits++
			return e.compiled, true
		}
		delete(c.entries, hash)
		c.evictions++
	}
	c.misses++
	return nil, false
}

// put stores compiled under its own hash, stamping an expiry when a TTL applies.
func (c *compiledCache) put(compiled *Compiled) {
	var expiresAt time.Time
	if c.ttl > 0 {
		expiresAt = c.clock.Now().Add(c.ttl)
	}
	c.mu.Lock()
	c.entries[compiled.hash] = cacheEntry{compiled: compiled, expiresAt: expiresAt}
	c.mu.Unlock()
}

func (c *compiledCache) expired(e cacheEntry) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return !c.clock.Now().Before(e.expiresAt)
}

// invalidate drops a single cached entry by hash, reporting whether one was
// present.
func (c *compiledCache) invalidate(hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[hash]; ok {
		delete(c.entries, hash)
		c.evictions++
		return true
	}
	return false
}

// clear drops every cached entry.
func (c *compiledCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictions += uint64(len(c.entries))
	c.entries = make(map[string]cacheEntry)
}

func (c *compiledCache) stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		Entries:   len(c.entries),
	}
}
