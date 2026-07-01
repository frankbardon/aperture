package provider

import (
	"container/list"
	"sync"
	"time"
)

// Default per-type cache tuning. These are deliberately conservative; E4-S4
// benchmarks and tunes them against the p99 < 1ms Check NFR.
const (
	// DefaultTTL is how long a cached metadata entry stays fresh when a type does
	// not override it. Zero TTL (set explicitly) means entries never expire.
	DefaultTTL = 30 * time.Second
	// DefaultMaxSize bounds a per-type cache's entry count when a type does not
	// override it, so a provider with a huge domain cannot grow the cache without
	// bound. Zero (set explicitly) means unbounded.
	DefaultMaxSize = 10_000
)

// CacheConfig tunes one object-type's cache. The zero value is meaningful: it
// adopts DefaultTTL and DefaultMaxSize via withDefaults, and a nil Now clock
// falls back to time.Now. Now is the injection seam tests use to drive TTL
// expiry deterministically instead of sleeping on the wall clock.
type CacheConfig struct {
	// TTL is the freshness window for an entry. <0 is treated as 0 (no expiry).
	TTL time.Duration
	// MaxSize caps the entry count; the least-recently-used entry is evicted when
	// the cap is exceeded. <=0 means unbounded.
	MaxSize int
	// Now is the clock the cache reads for TTL decisions. nil means time.Now.
	Now func() time.Time
	// ttlSet/maxSet record whether the caller explicitly set TTL/MaxSize so the
	// defaults only fill genuinely-unset fields. Set through the CacheOption
	// helpers; the exported fields stay plain for direct struct construction.
	ttlSet bool
	maxSet bool
}

// withDefaults returns cfg with unset fields filled from the package defaults.
func (cfg CacheConfig) withDefaults() CacheConfig {
	if !cfg.ttlSet && cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.TTL < 0 {
		cfg.TTL = 0
	}
	if !cfg.maxSet && cfg.MaxSize == 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if cfg.MaxSize < 0 {
		cfg.MaxSize = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}

// CacheOption tunes a per-type CacheConfig at registration. Options compose; the
// last to set a facet wins.
type CacheOption func(*CacheConfig)

// WithTTL sets the freshness window. d <= 0 disables expiry (entries live until
// evicted or invalidated).
func WithTTL(d time.Duration) CacheOption {
	return func(c *CacheConfig) { c.TTL, c.ttlSet = d, true }
}

// WithMaxSize caps the entry count. n <= 0 means unbounded.
func WithMaxSize(n int) CacheOption {
	return func(c *CacheConfig) { c.MaxSize, c.maxSet = n, true }
}

// WithClock injects the clock the cache reads for TTL decisions. Primarily a
// test seam for deterministic TTL expiry; production leaves it nil (time.Now).
func WithClock(now func() time.Time) CacheOption {
	return func(c *CacheConfig) { c.Now = now }
}

// Stats is a snapshot of a cache's counters. It is value-copied out of the cache
// under its lock, so reading it never races the cache's own bookkeeping.
type Stats struct {
	// Hits is the number of Get calls served from a live entry.
	Hits int64
	// Misses is the number of Get calls with no live entry (absent or expired).
	Misses int64
	// Evictions is the number of entries dropped to honour MaxSize.
	Evictions int64
	// Expirations is the number of entries found expired on read and dropped.
	Expirations int64
	// Invalidations is the number of entries dropped by the explicit
	// invalidation API (Delete / Clear).
	Invalidations int64
	// Entries is the live entry count at snapshot time.
	Entries int
}

// CacheBackend is the storage substrate for one object-type's metadata cache.
// Implementations MUST be safe for concurrent use and own their own TTL expiry,
// size eviction, and counter bookkeeping. The in-memory MemoryCache is the
// default; a host may supply its own (e.g. a process-shared cache) via the
// Registry's cache factory. A networked backend such as Redis is out of scope.
type CacheBackend interface {
	// Get returns the live value for key and records a hit; a miss (absent or
	// expired) records a miss and returns ok=false.
	Get(key string) (Metadata, bool)
	// Set stores value under key, refreshing its TTL, and evicts the
	// least-recently-used entry if the size cap is exceeded.
	Set(key string, value Metadata)
	// Delete drops key, counting an invalidation when an entry was present, and
	// reports whether one was removed.
	Delete(key string) bool
	// Clear drops every entry, counting each as an invalidation.
	Clear()
	// Stats returns a snapshot of the counters plus the live entry count.
	Stats() Stats
}

// compile-time assertion: the default backend satisfies the interface.
var _ CacheBackend = (*MemoryCache)(nil)

// cacheItem is one stored entry. expiresAt zero means the entry never expires.
type cacheItem struct {
	key       string
	value     Metadata
	expiresAt time.Time
}

// MemoryCache is the default in-memory CacheBackend: an LRU map with optional
// per-entry TTL and hit/miss/eviction/expiry/invalidation counters. All state is
// guarded by a single mutex, so it is safe for concurrent use; the critical
// sections are O(1) (map + intrusive list) to stay off the latency budget.
type MemoryCache struct {
	mu      sync.Mutex
	ll      *list.List               // front = most-recently-used
	items   map[string]*list.Element // key -> *list.Element holding *cacheItem
	ttl     time.Duration
	maxSize int
	now     func() time.Time
	stats   Stats
}

// NewMemoryCache builds an in-memory cache from cfg, filling unset fields with
// the package defaults.
func NewMemoryCache(cfg CacheConfig) *MemoryCache {
	cfg = cfg.withDefaults()
	return &MemoryCache{
		ll:      list.New(),
		items:   make(map[string]*list.Element),
		ttl:     cfg.TTL,
		maxSize: cfg.MaxSize,
		now:     cfg.Now,
	}
}

// expired reports whether it is past its TTL as of now.
func (c *MemoryCache) expired(it *cacheItem, now time.Time) bool {
	return !it.expiresAt.IsZero() && !now.Before(it.expiresAt)
}

// Get returns the live value for key, moving it to the most-recently-used
// position. An expired entry is dropped and reported as a miss.
func (c *MemoryCache) Get(key string) (Metadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.stats.Misses++
		return nil, false
	}
	it := el.Value.(*cacheItem)
	if c.expired(it, c.now()) {
		c.removeElement(el)
		c.stats.Expirations++
		c.stats.Misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.stats.Hits++
	return it.value, true
}

// Set stores value under key. An existing entry is refreshed in place; a new
// entry that pushes the cache past MaxSize evicts the least-recently-used one.
func (c *MemoryCache) Set(key string, value Metadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if c.ttl > 0 {
		expiresAt = c.now().Add(c.ttl)
	}
	if el, ok := c.items[key]; ok {
		it := el.Value.(*cacheItem)
		it.value = value
		it.expiresAt = expiresAt
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheItem{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = el
	if c.maxSize > 0 && c.ll.Len() > c.maxSize {
		if back := c.ll.Back(); back != nil {
			c.removeElement(back)
			c.stats.Evictions++
		}
	}
}

// Delete drops key and counts an invalidation when an entry was present.
func (c *MemoryCache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return false
	}
	c.removeElement(el)
	c.stats.Invalidations++
	return true
}

// Clear drops every entry, counting each as an invalidation.
func (c *MemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Invalidations += int64(c.ll.Len())
	c.ll.Init()
	c.items = make(map[string]*list.Element)
}

// Stats snapshots the counters plus the live entry count.
func (c *MemoryCache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = c.ll.Len()
	return s
}

// removeElement unlinks el from both the list and the index. Caller holds mu.
func (c *MemoryCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*cacheItem).key)
}
