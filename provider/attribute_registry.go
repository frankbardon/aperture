package provider

import (
	"context"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
)

// attributeLayerEntry binds ONE layer of one slot to its own cache and the
// resolved config that built it. Each cache is independent — its own TTL, its own
// size cap, its own counters — because the sources behind them have genuinely
// different change rates and cardinalities: an account's plan changes rarely and
// there are few accounts, while a user directory is large and its bags churn, and
// a shared SQL directory and a local inline block are no more alike than two
// slots are.
//
// A per-LAYER cache rather than one merged cache per slot, because a layer's ttl:
// is its own revocation window and is declared per source. One cache could honour
// at most one of two declarations, and both ways of choosing are wrong: taking
// the longer window silently LENGTHENS the time a revoked shared attribute keeps
// authorizing, and taking the shorter one silently ignores a declaration an
// operator made. Two caches honour both, and the merge is what composes them.
type attributeLayerEntry struct {
	provider AttributeProvider
	cache    CacheBackend
	config   CacheConfig
}

// attributeSlotEntry holds one slot's layers. At least one is non-nil — an entry
// is only ever created by a successful registration, so a slot present in the map
// is a slot with a provider, and Has cannot report an empty shell.
type attributeSlotEntry struct {
	shared *attributeLayerEntry
	local  *attributeLayerEntry
}

// layer returns the entry for one layer, or nil when that layer is unfilled.
func (e *attributeSlotEntry) layer(l AttributeLayer) *attributeLayerEntry {
	switch l {
	case AttributeLayerShared:
		return e.shared
	case AttributeLayerLocal:
		return e.local
	default:
		return nil
	}
}

// set fills one layer. The caller has already refused a duplicate.
func (e *attributeSlotEntry) set(l AttributeLayer, le *attributeLayerEntry) {
	switch l {
	case AttributeLayerShared:
		e.shared = le
	case AttributeLayerLocal:
		e.local = le
	}
}

// sole returns the one filled layer when a slot has exactly one, and nil when it
// has two. It is the fast path every single-source deployment takes: one cache
// read, one bag, no merge and no allocation.
func (e *attributeSlotEntry) sole() *attributeLayerEntry {
	switch {
	case e.shared != nil && e.local == nil:
		return e.shared
	case e.local != nil && e.shared == nil:
		return e.local
	default:
		return nil
	}
}

// filled returns the slot's layers in PRECEDENCE order, highest first, skipping
// the unfilled ones. It is what the invalidation and stats paths walk, so those
// can never reach one layer and miss the other.
func (e *attributeSlotEntry) filled() []*attributeLayerEntry {
	out := make([]*attributeLayerEntry, 0, 2)
	if e.shared != nil {
		out = append(out, e.shared)
	}
	if e.local != nil {
		out = append(out, e.local)
	}
	return out
}

// AttributeRegistry maps each of the three attribute slots to up to TWO layered
// AttributeProviders — a shared one and a local one — each with its own bag
// cache. It is the seam the engine and rules layers resolve a decision's
// principal and account attributes through. It is safe for concurrent use:
// providers are registered at startup and read on the hot path under an RWMutex,
// and each per-layer cache is independently concurrency-safe.
//
// # Two layers per slot, shared over local
//
// A slot's bag is the MERGE of its layers, with the shared layer winning every
// key both serve; a slot with one layer is that layer's bag verbatim, which is
// every single-source deployment. AttributeLayer (attribute_layer.go) is the
// whole account of why the two exist, why the shared one wins unconditionally,
// and why the merge does not touch leniency. Register fills the shared layer and
// RegisterLocal the local one; a third registration for either layer is still
// refused.
//
// # It is NOT an object lister, and that is a property of the type
//
// A *Registry deliberately satisfies the scope.ObjectLister contract, so it can
// be handed straight to a scope resolver as the thing that enumerates "all
// objects of a type". An *AttributeRegistry deliberately does NOT, and there is
// no compile-time assertion here to match registry.go's precisely because the
// assertion is the thing being refused.
//
// The reason is not tidiness. A scope resolver enumerating through an
// ObjectLister is answering "which objects does this grant reach?" mid-decision.
// If the principal directory were reachable through that seam, the principal
// table would become an enumerable object set inside a decision — every
// principal in the deployment listable by anything holding a lister, with the
// grant's own scope as the only bound, and no admin tier consulted. Attribute
// enumeration is a system-tier admin read; it is not a scope-resolution source.
//
// Go's typing is STRUCTURAL, so intending that is worth nothing. A method that
// happens to be spelled List(ctx, string, identity.Pattern, int)
// ([]identity.Identity, error) satisfies scope.ObjectLister whether or not
// anybody meant it to, and the wiring mistake it enables is silent — the
// resolver compiles, runs, and enumerates. Containment is therefore structural
// too:
//
//   - enumeration is called Enumerate, not List;
//   - it is keyed by an AttributeSlot, not a bare object-type string;
//   - it takes an AttributeFilter, which has no identity.Pattern to bound with
//     (see AttributeFilter);
//   - it returns []AttributeRecord — bare string keys — not
//     []identity.Identity.
//
// Any one of those makes the signature unassignable; all four make it
// unassignable by accident. TestAttributeRegistryIsNotAScopeLister asserts the
// negative against the real scope.ObjectLister interface, so the guarantee
// cannot rot into a comment.
type AttributeRegistry struct {
	mu       sync.RWMutex
	slots    map[AttributeSlot]*attributeSlotEntry
	defaults CacheConfig
	newCache func(CacheConfig) CacheBackend
}

// AttributeRegistryOption configures an AttributeRegistry at construction.
type AttributeRegistryOption func(*AttributeRegistry)

// WithAttributeDefaultCacheConfig sets the cache config a slot inherits when it
// is registered with no per-slot overrides. Unset fields still fall back to the
// package defaults (DefaultTTL / DefaultMaxSize) at cache construction.
func WithAttributeDefaultCacheConfig(cfg CacheConfig) AttributeRegistryOption {
	return func(r *AttributeRegistry) { r.defaults = cfg }
}

// WithAttributeCacheFactory swaps the cache backend constructor every per-slot
// cache is built from. The default builds a MemoryCache; a host supplies this to
// plug a custom CacheBackend. A networked backend (e.g. Redis) is out of scope.
func WithAttributeCacheFactory(f func(CacheConfig) CacheBackend) AttributeRegistryOption {
	return func(r *AttributeRegistry) {
		if f != nil {
			r.newCache = f
		}
	}
}

// NewAttributeRegistry returns a registry with no slot filled. Each slot is
// registered separately and gets its own in-memory LRU cache tuned by the
// registry defaults plus that slot's options.
//
// A slot left unregistered is not an error at construction: a deployment with no
// machine principals wires no machine provider. It becomes
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED at the first fetch against that slot,
// which is a configuration diagnostic rather than an empty bag silently reading
// as "this principal has no attributes".
func NewAttributeRegistry(opts ...AttributeRegistryOption) *AttributeRegistry {
	r := &AttributeRegistry{
		slots:    make(map[AttributeSlot]*attributeSlotEntry, len(AttributeSlots())),
		newCache: func(cfg CacheConfig) CacheBackend { return NewMemoryCache(cfg) },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register binds provider to slot's SHARED layer with a cache configured from the
// registry defaults plus opts. It is the method a deployment's own wiring uses —
// a database wiring row, a seed document's attribute_providers: entry, a host's
// one directory — and for a slot with a single source it is the only method
// needed: the bag it serves is the slot's bag, unmerged and verbatim.
//
// A slot outside the closed set is APERTURE_ATTRIBUTE_SLOT_UNKNOWN; a nil
// provider, or a SECOND shared registration for a slot that already has one, is
// APERTURE_ATTRIBUTE_PROVIDER_INVALID.
//
// A duplicate WITHIN a layer is refused rather than replaced because "last writer
// wins" over a slot is how one deployment's directory quietly shadows another's
// during wiring, and the failure surfaces as attributes that are merely wrong
// rather than absent. That is why the second provider a slot accepts is not a
// duplicate but a DIFFERENT LAYER with a stated, unconfigurable winner — see
// RegisterLocal and AttributeLayer.
func (r *AttributeRegistry) Register(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) error {
	return r.register(slot, AttributeLayerShared, provider, opts...)
}

// RegisterLocal binds provider to slot's LOCAL layer, which layers UNDER the
// shared one: on every key both layers serve the shared layer's value is what a
// decision reads, and the local layer contributes only keys the shared layer does
// not serve. It is the method this INSTANCE's own wiring uses — a seed document's
// attributes: block, a provider a Go host registers for itself.
//
// Refusals are Register's, per layer: a second LOCAL registration for a slot that
// already has one is APERTURE_ATTRIBUTE_PROVIDER_INVALID, so a slot accepts
// exactly two providers and a third is refused whichever layer it names.
//
// A slot whose ONLY registration is local behaves exactly as a slot whose only
// registration is shared: one cache, one provider, the bag verbatim. The layer it
// occupies is still recorded, because it is what a surface listing the wiring
// reports and what a later shared registration layers over.
//
// The precedence is not an option and does not depend on registration order. A
// local bag that could override a shared key would let a file on one machine
// change what a deployment-wide rule compares against, on that machine only, with
// nothing in a verdict or a trace to say so — see AttributeLayer.
func (r *AttributeRegistry) RegisterLocal(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) error {
	return r.register(slot, AttributeLayerLocal, provider, opts...)
}

// register is the one implementation behind both registration methods and both
// Must forms, so the slot check, the nil check, the config resolution and the
// duplicate refusal have exactly one definition each.
func (r *AttributeRegistry) register(slot AttributeSlot, layer AttributeLayer, provider AttributeProvider, opts ...CacheOption) error {
	if !slot.Valid() {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			"provider: cannot register an attribute provider under an unknown slot",
			map[string]any{"slot": string(slot), "slots": slotNames()})
	}
	if !layer.Valid() {
		return attributeLayerError(slot, layer)
	}
	if provider == nil {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: cannot register a nil attribute provider",
			map[string]any{"slot": string(slot), "layer": string(layer)})
	}
	cfg := r.defaults
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg = cfg.withDefaults()

	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.slots[slot]
	if ok && e.layer(layer) != nil {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: attribute slot already has a registered provider in this layer",
			map[string]any{"slot": string(slot), "layer": string(layer), "layers": layerNames()})
	}
	if !ok {
		// Created only once the registration is known to succeed: an entry in the
		// map is a slot with a provider, so Has and RegisteredSlots can never
		// report an empty shell left behind by a refusal.
		e = &attributeSlotEntry{}
		r.slots[slot] = e
	}
	e.set(layer, &attributeLayerEntry{
		provider: provider,
		cache:    r.newCache(cfg),
		config:   cfg,
	})
	return nil
}

// MustRegister is Register that panics on error; for host startup wiring where a
// registration failure is a programming error.
func (r *AttributeRegistry) MustRegister(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) {
	if err := r.Register(slot, provider, opts...); err != nil {
		panic(err)
	}
}

// MustRegisterLocal is RegisterLocal that panics on error, for the same reason
// MustRegister does.
func (r *AttributeRegistry) MustRegisterLocal(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) {
	if err := r.RegisterLocal(slot, provider, opts...); err != nil {
		panic(err)
	}
}

// Has reports whether slot has a registered provider in EITHER layer. An unknown
// slot is simply false — Has is a question, not an assertion.
//
// It is deliberately layer-blind: every caller asking it is asking "will a fetch
// against this slot reach a provider?", and the answer to that does not depend on
// which layer answers. Layers reports the finer fact.
func (r *AttributeRegistry) Has(slot AttributeSlot) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.slots[slot]
	return ok
}

// Layers returns the layers slot has a provider in, in PRECEDENCE order (shared
// first). An unregistered or unknown slot returns nil.
//
// It is what a surface DISPLAYING the wiring asks in order to say where a slot's
// bags come from, without re-deriving the precedence rule: the first element is
// the layer that wins a contested key.
func (r *AttributeRegistry) Layers(slot AttributeSlot) []AttributeLayer {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	out := make([]AttributeLayer, 0, len(AttributeLayers()))
	for _, l := range AttributeLayers() {
		if e.layer(l) != nil {
			out = append(out, l)
		}
	}
	return out
}

// RegisteredSlots returns the slots that have a provider, in AttributeSlots()
// order so the result is stable and diffable. It is deliberately not called
// Keys: the KEY SET of this registry is fixed at three, and what varies is which
// of them are filled.
func (r *AttributeRegistry) RegisteredSlots() []AttributeSlot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AttributeSlot, 0, len(r.slots))
	for _, slot := range AttributeSlots() {
		if _, ok := r.slots[slot]; ok {
			out = append(out, slot)
		}
	}
	return out
}

// entry resolves slot's entry, or the coded error that says why it cannot: an
// unknown slot is APERTURE_ATTRIBUTE_SLOT_UNKNOWN (a programming error at the
// call site), an empty one is APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED (a wiring
// gap). The two are distinct because their fixups are.
func (r *AttributeRegistry) entry(slot AttributeSlot) (*attributeSlotEntry, error) {
	if !slot.Valid() {
		return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			"provider: not an attribute slot",
			map[string]any{"slot": string(slot), "slots": slotNames()})
	}
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED,
			"provider: no attribute provider registered for slot",
			map[string]any{"slot": string(slot)})
	}
	return e, nil
}

// Fetch returns the attribute bag for id in slot, serving each of the slot's
// layers from its own cache when fresh and otherwise pulling it through that
// layer's provider and caching the result. A cache hit never calls a provider.
//
// # Two layers, merged with the shared one winning
//
// A slot with one layer returns that layer's bag VERBATIM — the same map the
// cache is holding, no copy, no merge, which is the path every single-source
// deployment is on. A slot with two returns their merge over a fresh map, with the
// shared layer's value standing on every key both serve (see mergeAttributeBags).
//
// Both layers are consulted, and what each one's failure means differs:
//
//   - APERTURE_NOT_FOUND from a layer means that layer has no record for this key.
//     It contributes nothing and the other layer's bag is the answer, because a
//     local block adding a subject the shared directory does not carry — or a
//     shared directory carrying one the local file does not — is the ordinary
//     case, not a failure. Only when EVERY layer reports it does Fetch report it,
//     and it reports the coded error a layer actually raised, so its registry
//     fixups survive.
//   - Anything else — an unreachable directory, a bag the value model rejects —
//     aborts the fetch and surfaces VERBATIM. It is emphatically NOT answered out
//     of the other layer: a shared directory that is down must not be silently
//     replaced by one machine's local file, which would turn an outage into an
//     authorization change that looks exactly like a correct decision.
//
// id is a BARE KEY — a principal id for the user and machine slots, an account
// id for the account slot — and Aperture never parses it. Two keys are refused
// outright, before any provider is consulted: the empty string, which names
// nobody, and "*", the account wildcard, which would ask for the attributes of
// every account and could only be answered with one account's data served as
// another's.
//
// The returned bag is READ-ONLY, transitively. It is the whole decision's view
// of this subject, shared across every object being checked and every concurrent
// decision for the same key, so a write through it is not one bad read — see the
// blast-radius note in attribute.go.
func (r *AttributeRegistry) Fetch(ctx context.Context, slot AttributeSlot, id string) (Metadata, error) {
	e, err := r.entry(slot)
	if err != nil {
		return nil, err
	}
	if err := attributeKeyError(slot, id); err != nil {
		return nil, err
	}
	if only := e.sole(); only != nil {
		return fetchAttributeLayer(ctx, only, id)
	}
	shared, sharedErr := fetchAttributeLayer(ctx, e.shared, id)
	if sharedErr != nil && aerr.CodeOf(sharedErr) != aerr.APERTURE_NOT_FOUND {
		return nil, sharedErr
	}
	local, localErr := fetchAttributeLayer(ctx, e.local, id)
	if localErr != nil && aerr.CodeOf(localErr) != aerr.APERTURE_NOT_FOUND {
		return nil, localErr
	}
	if sharedErr != nil && localErr != nil {
		// Neither layer knows this key, which is the single-layer NOT_FOUND
		// unchanged. The SHARED layer's refusal is the one returned: it is the
		// deployment's own answer, and the two carry the same code and the same
		// fixups anyway.
		return nil, sharedErr
	}
	return mergeAttributeBags(shared, local), nil
}

// fetchAttributeLayer serves one layer: its cache first, its provider second,
// caching what the provider returned.
//
// This is the ONLY writer of a layer's cache, and it stays that way. The cache is
// the DECISION PATH's view of a subject; an enumeration's bags are Query's
// projection, which the loaders' own contract allows to be narrower, and warming
// this cache from one substitutes a display projection for the authoritative bag
// (see Enumerate). Two layers means two caches, and the rule is per layer: a
// layer's cache holds what that layer's Fetch returned and nothing else.
func fetchAttributeLayer(ctx context.Context, l *attributeLayerEntry, id string) (Metadata, error) {
	if md, ok := l.cache.Get(id); ok {
		return md, nil
	}
	md, err := l.provider.Fetch(ctx, id)
	if err != nil {
		return nil, attributeError(err)
	}
	l.cache.Set(id, md)
	return md, nil
}

// Enumerate returns up to filter.Limit records of slot that satisfy
// filter.Fields, by querying the slot's provider and re-enforcing both bounds on
// what comes back. A positive limit is honoured as given; a non-positive one
// means DefaultListLimit.
//
// This read is UNCAPPED on purpose. It is the SYSTEM-TIER ADMIN READ of a
// directory, and an operator answering "who is in the user slot?" may legitimately
// need the whole of it — a page size chosen here would only make the honest
// answer arrive in pieces. It is not a scope-resolution source, and its signature
// is built so it cannot be mistaken for one — see the type doc on
// AttributeRegistry for why each part of it differs from scope.ObjectLister.List.
// What keeps it safe is the authority required to reach it (service tier), not a
// number in this package.
//
// # It does NOT write the slot's cache, and that is the contract
//
// The slot's cache is FETCH's cache — the decision path's view of a subject. An
// enumeration's bags are Query's answer, and nothing in the AttributeProvider
// contract says Query returns the same bag Fetch does. The SQL loader makes the
// divergence explicit and legal: AttributeConfig.ListQuery is OPTIONAL and is
// only required to select a bare id, so
//
//	get_one: SELECT department, clearance, to_jsonb(teams) AS teams FROM users WHERE id = $1
//	get_all: SELECT u.id AS id, u.department FROM users u
//
// is a correct, documented pair in which Query's bag is a strict SUBSET of
// Fetch's. Warming the fetch cache from it substitutes the DISPLAY projection for
// the authoritative bag, for the whole of the slot's ttl, for every subject the
// listing returned.
//
// The consequence is an access-control change, not a stale read: `principal.teams`
// is then ABSENT rather than wrong, so every membership predicate over it is
// false. In an inclusive grant that denies; in an EXCLUSIVE one a rule that stops
// selecting stops EXCLUDING, so an administrator running
// `aperture attributes query user` silently widens access until the ttl expires,
// and no verdict, trace or note says why. That is the same hazard
// rules.TestAMissingBagWidensAnExclusiveGrant describes, reached from the other
// direction — a bag that is present but shorter.
//
// Aperture cannot make the warm safe by INSPECTING it. It cannot compare the two
// projections: an attribute bag is opaque host data, an absent key is
// indistinguishable from a key whose value is genuinely unset (metadataValue maps
// a NULL to an OMITTED field on purpose), and a provider may legitimately answer
// Query from a search index and Fetch from the system of record. Only the
// implementation knows, which is why the object seam asks it (FetchCompleteLister)
// and this one does not ask at all.
//
// The object Registry.List had the same bug from the same cause, and it is fixed
// DIFFERENTLY, because the two calls are not the same kind of call. List is a
// DECISION-PATH call whose Fetch follows immediately in the same candidate walk
// (engine.walkAllowed), so its warm is paid back within the same decision and
// removing it would put a round trip per candidate into the widest fan-out
// Aperture has. It therefore keeps the warm, CONDITIONAL on the provider promising
// through FetchCompleteLister that a listed bag is the bag its own Fetch would
// return — sqlprovider derives that from the two statements' real column
// projections, so nothing is taken on trust. See typeEntry.warmsFromListing.
//
// Enumerate has no Fetch behind it at all — it is an admin listing rendered to an
// operator — so there was nothing to conditionalise and nothing to repay: the warm
// bought nothing and cost the decision path its bag. Removal is the whole fix here,
// and an AttributeProvider is given no promise to make, deliberately. A slot's bags
// are only ever read by a decision, and a directory read has no business writing to
// what a decision reads.
//
// Fields is re-enforced through MatchFields rather than trusted to the provider.
// The object Registry leaves Fields entirely to its provider because there
// Query's answer bounds an authorization; here the registry is the only shared
// place the predicate can be applied identically for every provider, and the
// cost is one map walk over an already-bounded page. A provider that pushes the
// predicate into its own storage still passes, because MatchFields is the rule
// it was pushing down.
// # Both layers, merged before the predicate runs
//
// A slot's layers are enumerated separately and merged per KEY, shared winning,
// exactly as Fetch merges one subject's bags (see mergeAttributeRecords) — so the
// bag the listing shows for a key is the bag a Fetch of that key would return,
// which is the whole reason an operator reads this listing.
//
// Fields is applied to the MERGED bag, not to each layer's. Filtering per layer
// and merging afterwards would answer a different question in both directions: a
// record admitted on a local value the shared layer overrides would not match the
// bag it is shown with, and a record whose only matching value comes from the
// other layer would be dropped despite matching.
//
// Limit is passed to each layer and re-enforced on the merge, so a bounded listing
// stays bounded. A slot whose shared layer alone fills the bound can therefore hide
// keys only the local layer knows; that is what a bound means, and it is the same
// truncation a single layer already had.
func (r *AttributeRegistry) Enumerate(ctx context.Context, slot AttributeSlot, filter AttributeFilter) ([]AttributeRecord, error) {
	e, err := r.entry(slot)
	if err != nil {
		return nil, err
	}
	limit := boundLimit(filter.Limit)
	filter.Limit = limit
	if only := e.sole(); only != nil {
		records, err := only.provider.Query(ctx, filter)
		if err != nil {
			return nil, attributeError(err)
		}
		return boundAttributeRecords(records, filter.Fields, limit), nil
	}
	shared, err := e.shared.provider.Query(ctx, filter)
	if err != nil {
		return nil, attributeError(err)
	}
	local, err := e.local.provider.Query(ctx, filter)
	if err != nil {
		return nil, attributeError(err)
	}
	return boundAttributeRecords(mergeAttributeRecords(shared, local), filter.Fields, limit), nil
}

// boundAttributeRecords re-enforces Fields and the limit on what an enumeration
// produced. It is one implementation for the single-layer and merged paths, so
// neither can drift into filtering differently.
//
// Deliberately no cache write anywhere in it. See Enumerate's doc: a slot's caches
// are the DECISION PATH's, and these bags are Query's projection, which the
// loaders' own contract allows to be narrower.
func boundAttributeRecords(records []AttributeRecord, fields map[string]any, limit int) []AttributeRecord {
	out := make([]AttributeRecord, 0, min(len(records), limit))
	for _, rec := range records {
		if !MatchFields(rec.Attributes, fields) {
			continue
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// principalSlot maps a PRINCIPAL KIND to the slot that serves it. It is a
// deliberately narrower door than ParseAttributeSlot: only user and machine are
// principal kinds, and "account" — a real slot — is not one.
//
// Routing a principal fetch through ParseAttributeSlot would let the string
// "account" resolve the ACCOUNT directory and serve a tenant's bag as a
// principal's. There is no caller that wants that, so the mapping that makes it
// expressible does not exist. An unrecognised or empty kind reports false rather
// than defaulting: a machine answered for out of the human directory is exactly
// the substitution PrincipalResolver's contract forbids.
func principalSlot(kind string) (AttributeSlot, bool) {
	switch AttributeSlot(kind) {
	case AttributeSlotUser:
		return AttributeSlotUser, true
	case AttributeSlotMachine:
		return AttributeSlotMachine, true
	default:
		return "", false
	}
}

// Attributes resolves a principal's attribute bag by KIND, so a user principal
// is answered from the user slot and a machine principal from the machine slot.
//
// Its signature is rules.PrincipalResolver's, so an *AttributeRegistry is handed
// straight to rules.WithPrincipalResolver — structurally, without this package
// importing rules (provider imports only identity and errors). That direction is
// the same one *Registry already satisfies rules.MetadataFetcher in.
//
// It returns the host's bag alone. The `id` and `kind` a rule reads off
// `principal` are the rules engine's floor, stamped over whatever comes back —
// this method never invents them, and a nil return is the correct, complete
// answer for "the host knows nothing about this principal".
//
// # Leniency, and its one deliberate limit
//
// A missing SOURCE is not a failed decision. Two cases yield a nil bag and no
// error:
//
//   - the kind names no principal slot (an empty kind is the live case — a
//     decision path that never had the principal's record in hand), and
//   - the slot has no registered provider in either layer
//     (APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED), or no layer that has one has a
//     record for this key (APERTURE_NOT_FOUND).
//
// Layering does not widen that set, and it is asked of the SLOT rather than of a
// layer: a key one layer knows is answered out of that layer (Fetch merges what
// the layers have), and only a key NO layer knows reaches this collapse. The codes
// are the two they were.
//
// A deployment that wires a user directory and no machine directory must keep
// deciding, and it does: its machine principals evaluate against the floor. This
// mirrors internal/cli's lenientFetcher, which collapses the same two codes for
// object metadata and for the same reason — an absent source must not become a
// non-decision.
//
// Everything else — a directory that is unreachable, a bag the value model
// rejects — surfaces VERBATIM, keeping its code and its registry fixups, and the
// caller treats it as a non-decision. An outage must not read as "this principal
// has no attributes", because that is an authorization change wearing an
// infrastructure failure's clothes.
//
// A key that can never name one subject is a third thing, and it is refused
// rather than collapsed: an empty key, or the account wildcard, is a CALLER that
// has not resolved what it is asking about, not a deployment that chose not to
// wire a directory (see attributeKeyError).
//
// # The asymmetry leniency leaves, which is accepted rather than solved
//
// An absent attribute makes every comparison against it false. That is deny-safe
// in an INCLUSIVE grant, where a rule that fails to select covers nothing — and
// access-WIDENING in an EXCLUSIVE one, where selection means "excluded", so a
// rule that stops selecting stops excluding and the object the exclusion was
// written to withhold becomes covered.
//
// The alternative is worse in the direction that matters more: erroring on an
// absent bag makes a deployment with an unwired slot undecidable for every
// principal of that kind, which is the outage the leniency exists to prevent.
// The mitigation is therefore VISIBILITY, not refusal — a decision trace says
// when a bag came back floor-only — plus `principal.kind`, which is what lets a
// rule author state a rule's dependence on a directory instead of hiding it (see
// rules.principalBag). rules.TestAMissingBagWidensAnExclusiveGrant keeps the
// hazard executable, so it cannot quietly stop being true.
func (r *AttributeRegistry) Attributes(ctx context.Context, kind, principal string) (map[string]any, error) {
	slot, ok := principalSlot(kind)
	if !ok {
		return nil, nil
	}
	md, err := r.Fetch(ctx, slot, principal)
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, aerr.APERTURE_NOT_FOUND:
			return nil, nil
		default:
			return nil, err
		}
	}
	return md, nil
}

// AccountAttributes resolves the ACTIVE account's attribute bag from the account
// slot — the tenant's plan, region, feature flags a rule reads off `account`.
//
// Its signature is rules.AccountResolver's, so an *AttributeRegistry is handed
// straight to rules.WithAccountResolver, structurally, without this package
// importing rules. It is spelled AccountAttributes rather than Attributes so that
// ONE registry can satisfy both resolver seams: Go has no overloading, and
// Attributes is already the principal seam's method. A host wires the same reg
// into WithPrincipalResolver and WithAccountResolver and gets both directories
// and both caches from it.
//
// It returns the host's bag alone. The `id` a rule reads off `account` is the
// rules engine's floor, stamped over whatever comes back, and a nil return is the
// correct, complete answer for "the host knows nothing about this account".
//
// # The wildcard is a call-site bug, not a wiring gap
//
// account is a concrete account id. "*" — the all-accounts grant sentinel — is
// refused with APERTURE_ATTRIBUTE_PROVIDER_INVALID (by Fetch's shared key guard,
// so the refusal has exactly one definition), and it is refused rather than
// answered leniently because the two failures are not the same kind of thing. An
// unregistered slot is a deployment that has no account directory and must keep
// deciding; a wildcard key is a caller that has not resolved the sentinel to the
// account the decision is actually in, and the only bag that could satisfy it is
// one account's data served as another's. The rules engine resolves the sentinel
// to the floor before it ever gets here, so this is the backstop.
//
// # Leniency, otherwise identical to the principal seam
//
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED (no account directory wired) and
// APERTURE_NOT_FOUND (a directory that has no record for this account) both yield
// a nil bag and no error: the decision proceeds against the floor. Everything
// else — an unreachable directory, a bag the value model rejects — surfaces
// VERBATIM with its code and its registry fixups, and the caller treats it as a
// non-decision. An outage must not read as "this account has no attributes".
func (r *AttributeRegistry) AccountAttributes(ctx context.Context, account string) (Metadata, error) {
	md, err := r.Fetch(ctx, AttributeSlotAccount, account)
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, aerr.APERTURE_NOT_FOUND:
			return nil, nil
		default:
			return nil, err
		}
	}
	return md, nil
}

// Invalidate drops the cached bag for id in slot so the next Fetch re-pulls it
// from the provider. It reports whether an entry was present. An unknown slot is
// APERTURE_ATTRIBUTE_SLOT_UNKNOWN and an unfilled one is
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, exactly as a Fetch would report
// them.
//
// # This is a security control, not a performance knob
//
// The object Registry's Invalidate (registry.go) exists so a host whose source
// of truth changed can stop serving a stale row. The three methods here have the
// same shape and a different reason, and the difference is worth stating because
// it decides how a deployment tunes ttl:.
//
// An object's metadata going stale for a TTL is usually tolerable: a document's
// category is a fact about a thing, and a decision made against yesterday's
// category is wrong in the way a cache is always wrong. An attribute bag is the
// ASKER'S standing — the clearance, the department, the plan — and a revoked
// clearance that stays cached keeps AUTHORIZING for the rest of the window.
// Principals are the classic revoke case, so the freshness window a slot's ttl:
// buys in fetch traffic is paid for in the time a revocation takes to become
// true. An operator who has just removed someone's access needs a way to make
// that true now; this is that way.
//
// The key is validated through the SAME guard Fetch uses, so the empty string
// and the account wildcard are refused here as well. Neither can ever have been
// cached — Fetch refuses them before a provider is consulted — so the refusal
// costs a caller nothing it could otherwise have had, and it answers "why did
// invalidating '*' report nothing?" with the reason instead of a false.
//
// # It clears EVERY layer, and the loop is why
//
// A slot's bag is the merge of up to two independently cached layers, so dropping
// one of them leaves the other still authorizing — the revoked clearance is gone
// from the shared cache and still being read out of the local one, or the reverse.
// Every layer is therefore deleted from unconditionally (no short-circuit), and the
// bool is the OR: it reports whether anything was dropped, which is what an
// operator asking "was this cached?" means.
func (r *AttributeRegistry) Invalidate(slot AttributeSlot, id string) (bool, error) {
	e, err := r.entry(slot)
	if err != nil {
		return false, err
	}
	if err := attributeKeyError(slot, id); err != nil {
		return false, err
	}
	dropped := false
	for _, l := range e.filled() {
		// Not `dropped = dropped || l.cache.Delete(id)`: || short-circuits, and a
		// layer whose Delete is never called is a layer that keeps serving.
		if l.cache.Delete(id) {
			dropped = true
		}
	}
	return dropped, nil
}

// InvalidateSlot clears every cached bag for slot. Use it when a whole directory
// changed underneath the cache — a bulk role sync, a department reorg, a
// restored backup — rather than invalidating each key the change touched.
//
// It is spelled InvalidateSlot, not InvalidateType: the object registry's unit
// is an open-ended object TYPE and this one's is a closed SLOT, and the two are
// deliberately different words everywhere they appear (see the type doc).
//
// It clears BOTH of the slot's layers, for the reason Invalidate does: a directory
// that changed underneath one cache leaves the merged bag wrong whichever layer
// still holds it.
func (r *AttributeRegistry) InvalidateSlot(slot AttributeSlot) error {
	e, err := r.entry(slot)
	if err != nil {
		return err
	}
	for _, l := range e.filled() {
		l.cache.Clear()
	}
	return nil
}

// InvalidateAll clears every registered slot's cache. It does not unregister
// providers, and it is not an error on a registry with no slot filled — "drop
// everything" is satisfiable by a registry that is holding nothing.
//
// It is the blunt instrument, and the right one exactly once: an operator who
// knows a directory changed but not which subjects it changed. Every decision
// that follows re-reads its bags, so a large slot pays a burst of provider
// traffic; that is the price of the guarantee, and it is cheaper than the window
// it closes.
// It clears every LAYER of every slot. "Drop everything" that left one layer
// warm would be the worst of the three: the operator has been told the window is
// closed and half of it is still open.
func (r *AttributeRegistry) InvalidateAll() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.slots {
		for _, l := range e.filled() {
			l.cache.Clear()
		}
	}
}

// Stats returns the cache counters for slot, or false when the slot has no
// registered provider. The counters are never pooled ACROSS SLOTS: a user
// directory's hit rate says nothing about an account cache's, and one number
// covering both would hide the slot that is actually missing.
//
// A layered slot's counters ARE summed across its layers, which is a different
// thing and the right one: the question this answers is "what is this process
// holding, and how is it doing, for this party", and one subject's bag really is
// held once per layer. Entries therefore counts a key both layers serve twice,
// because it is cached twice — two entries is what is in memory and what the two
// ttls will expire. A caller that needs the layers apart asks
// CacheConfigForLayer for the configuration and Layers for the shape.
func (r *AttributeRegistry) Stats(slot AttributeSlot) (Stats, bool) {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return Stats{}, false
	}
	var out Stats
	for _, l := range e.filled() {
		s := l.cache.Stats()
		out.Hits += s.Hits
		out.Misses += s.Misses
		out.Evictions += s.Evictions
		out.Expirations += s.Expirations
		out.Invalidations += s.Invalidations
		out.Entries += s.Entries
	}
	return out, true
}

// CacheConfigFor returns the resolved cache configuration of the layer that
// GOVERNS slot — the shared one when it is filled, otherwise the local one — or
// false when the slot is empty. It is how a caller confirms that a per-slot
// override actually took (the defaults are filled in at registration, so the
// returned config is what the cache is really running, not what was passed).
//
// The shared layer is the one reported because it is the layer a decision's answer
// comes from on every key both serve, so its ttl: is the window in which a REVOKED
// shared attribute keeps authorizing — the number the operator surfaces exist to
// show. A slot's other layer is read with CacheConfigForLayer; a single-layer slot
// reports that layer either way, which is every deployment with one source.
func (r *AttributeRegistry) CacheConfigFor(slot AttributeSlot) (CacheConfig, bool) {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return CacheConfig{}, false
	}
	governing := e.filled()[0]
	return governing.config, true
}

// CacheConfigForLayer returns the resolved cache configuration of ONE of a slot's
// layers, or false when that layer is unfilled (or the slot or layer is unknown).
//
// It exists because a layer's ttl: and max_size: are declared per SOURCE and
// honoured per source — a shared SQL directory on a five-minute window and a local
// inline block that never expires are two caches, not an average — so a caller
// confirming that a per-layer override took has to be able to ask about the layer
// it set it on.
func (r *AttributeRegistry) CacheConfigForLayer(slot AttributeSlot, layer AttributeLayer) (CacheConfig, bool) {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return CacheConfig{}, false
	}
	l := e.layer(layer)
	if l == nil {
		return CacheConfig{}, false
	}
	return l.config, true
}
