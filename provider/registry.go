package provider

import (
	"context"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// DefaultListLimit bounds an enumeration (List/Query through the scope-lister
// adapter) when the caller imposes no positive limit, so a resolver can never
// materialise an unbounded object set off a provider.
const DefaultListLimit = 1000

// typeEntry binds one object-type's provider to its per-type cache and the
// resolved config that built the cache.
type typeEntry struct {
	provider ObjectProvider
	cache    CacheBackend
	config   CacheConfig
}

// Registry maps an object-type to its ObjectProvider plus a per-type metadata
// cache. It is the seam the engine, scope, and rules layers resolve a type's
// provider through. It is safe for concurrent use: providers are registered at
// startup and read on the hot path under an RWMutex, and each per-type cache is
// independently concurrency-safe.
//
// A *Registry also satisfies the scope.ObjectLister contract (see List), so it
// is passed directly as engine.ScopeDeps{Lister: reg} to let implicit/exclusive
// scope resolvers enumerate (the E2-S4 wiring).
type Registry struct {
	mu       sync.RWMutex
	entries  map[string]*typeEntry
	defaults CacheConfig
	newCache func(CacheConfig) CacheBackend
}

// compile-time assertion: a *Registry is a usable scope object-lister.
var _ ObjectLister = (*Registry)(nil)

// RegistryOption configures a Registry at construction.
type RegistryOption func(*Registry)

// WithDefaultCacheConfig sets the cache config new registrations inherit when
// they pass no per-type overrides. Unset fields still fall back to the package
// defaults at cache construction.
func WithDefaultCacheConfig(cfg CacheConfig) RegistryOption {
	return func(r *Registry) { r.defaults = cfg }
}

// WithCacheFactory swaps the cache backend constructor every per-type cache is
// built from. The default builds a MemoryCache; a host supplies this to plug a
// custom CacheBackend. A networked backend (e.g. Redis) is out of scope.
func WithCacheFactory(f func(CacheConfig) CacheBackend) RegistryOption {
	return func(r *Registry) {
		if f != nil {
			r.newCache = f
		}
	}
}

// NewRegistry returns an empty registry. By default each registered type gets an
// in-memory LRU cache tuned by the default cache config (DefaultTTL/DefaultMaxSize).
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		entries:  make(map[string]*typeEntry),
		newCache: func(cfg CacheConfig) CacheBackend { return NewMemoryCache(cfg) },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register binds provider to objectType with a per-type cache configured from
// the registry defaults plus opts. It rejects an empty type, a nil provider, or
// a duplicate registration with APERTURE_PROVIDER_INVALID.
func (r *Registry) Register(objectType string, provider ObjectProvider, opts ...CacheOption) error {
	if objectType == "" {
		return aerr.New(aerr.APERTURE_PROVIDER_INVALID, "provider: cannot register an empty object type")
	}
	if provider == nil {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_INVALID,
			"provider: cannot register a nil provider", map[string]any{"object_type": objectType})
	}
	cfg := r.defaults
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg = cfg.withDefaults()

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[objectType]; dup {
		return aerr.WithContext(aerr.APERTURE_PROVIDER_INVALID,
			"provider: object type already has a registered provider",
			map[string]any{"object_type": objectType})
	}
	r.entries[objectType] = &typeEntry{
		provider: provider,
		cache:    r.newCache(cfg),
		config:   cfg,
	}
	return nil
}

// MustRegister is Register that panics on error; for host startup wiring where a
// registration failure is a programming error.
func (r *Registry) MustRegister(objectType string, provider ObjectProvider, opts ...CacheOption) {
	if err := r.Register(objectType, provider, opts...); err != nil {
		panic(err)
	}
}

// Has reports whether objectType has a registered provider.
func (r *Registry) Has(objectType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[objectType]
	return ok
}

// Keys returns the registered object-type keys (unordered).
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	return out
}

// entry resolves the type entry for objectType, or APERTURE_PROVIDER_UNREGISTERED.
func (r *Registry) entry(objectType string) (*typeEntry, error) {
	r.mu.RLock()
	e, ok := r.entries[objectType]
	r.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
			"provider: no provider registered for object type",
			map[string]any{"object_type": objectType})
	}
	return e, nil
}

// Fetch returns id's metadata, serving it from the type's cache when fresh and
// otherwise pulling it through the provider and caching the result. A cache hit
// never calls the provider. The object-type is id's terminal segment type; an
// unregistered type yields APERTURE_PROVIDER_UNREGISTERED.
func (r *Registry) Fetch(ctx context.Context, id identity.Identity) (Metadata, error) {
	e, err := r.entry(terminalType(id))
	if err != nil {
		return nil, err
	}
	key := id.String()
	if md, ok := e.cache.Get(key); ok {
		return md, nil
	}
	md, err := e.provider.Fetch(ctx, id)
	if err != nil {
		return nil, providerError(err)
	}
	e.cache.Set(key, md)
	return md, nil
}

// List satisfies scope.ObjectLister: it returns up to limit object identities of
// objectType that match pattern, by querying the type's provider and bounding
// the result by both the pattern and the limit. It opportunistically warms the
// per-type cache with each returned object's metadata, since the provider call
// already paid to produce it. limit <= 0 means DefaultListLimit.
//
// The signature is byte-for-byte scope.ObjectLister, so a *Registry is wired
// directly as engine.ScopeDeps{Lister: reg}.
func (r *Registry) List(ctx context.Context, objectType string, pattern identity.Pattern, limit int) ([]identity.Identity, error) {
	e, err := r.entry(objectType)
	if err != nil {
		return nil, err
	}
	limit = boundLimit(limit)
	objs, err := e.provider.Query(ctx, Filter{Pattern: &pattern, Limit: limit})
	if err != nil {
		return nil, providerError(err)
	}
	out := make([]identity.Identity, 0, len(objs))
	for _, obj := range objs {
		if !pattern.Matches(obj.ID) {
			continue
		}
		if obj.Metadata != nil {
			e.cache.Set(obj.ID.String(), obj.Metadata)
		}
		out = append(out, obj.ID)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Invalidate drops id's cached metadata so the next Fetch re-pulls it from the
// provider. It reports whether an entry was present. The object-type is id's
// terminal segment type; an unregistered type yields
// APERTURE_PROVIDER_UNREGISTERED. This is the per-object invalidation hook a host
// calls when its source of truth changes.
func (r *Registry) Invalidate(id identity.Identity) (bool, error) {
	e, err := r.entry(terminalType(id))
	if err != nil {
		return false, err
	}
	return e.cache.Delete(id.String()), nil
}

// InvalidateType clears every cached entry for objectType. Use it when a whole
// type's data changed underneath the cache.
func (r *Registry) InvalidateType(objectType string) error {
	e, err := r.entry(objectType)
	if err != nil {
		return err
	}
	e.cache.Clear()
	return nil
}

// InvalidateAll clears every per-type cache. Use it sparingly (e.g. a global
// data reload); it does not unregister providers.
func (r *Registry) InvalidateAll() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		e.cache.Clear()
	}
}

// Stats returns the cache counters for objectType, or false when the type has no
// registered provider. Exposes the hit/miss/eviction metrics for observability
// and the latency benchmark (E4-S4).
func (r *Registry) Stats(objectType string) (Stats, bool) {
	r.mu.RLock()
	e, ok := r.entries[objectType]
	r.mu.RUnlock()
	if !ok {
		return Stats{}, false
	}
	return e.cache.Stats(), true
}

// boundLimit normalises a caller limit to a positive bound.
func boundLimit(limit int) int {
	if limit <= 0 || limit > DefaultListLimit {
		return DefaultListLimit
	}
	return limit
}

// providerError normalises an error returned by a host provider. An error
// already carrying an Aperture (or pulse) code passes through verbatim — so a
// provider's APERTURE_NOT_FOUND for an absent object reaches the caller intact —
// while a plain error is wrapped as APERTURE_PROVIDER_FETCH.
func providerError(err error) error {
	if err == nil {
		return nil
	}
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_PROVIDER_FETCH, "provider: object source returned an error", err)
}
