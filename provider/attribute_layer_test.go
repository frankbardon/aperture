package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// The layered attribute slot: two providers per slot, shared over local.
//
// These tests are rules/principal_floor_test.go's shape one tier down. That file
// proves the engine's floor cannot be shadowed by a provider bag; this one proves
// the SHARED layer cannot be shadowed by a LOCAL one, and the two compose in one
// direction — floor over shared over local — which the last test here asserts end
// to end through the resolver seams rules actually calls.

// ---------------------------------------------------------------------------
// The precedence
// ---------------------------------------------------------------------------

// TestTheSharedLayerIsNotShadowedByTheLocalBag is this story's
// TestTheFloorIsNotShadowedByTheProviderBag, asserted per slot.
//
// The load-bearing half is the COLLISION. A local layer that could redefine a key
// the shared layer serves would let a file on one machine change what a
// deployment-wide rule compares against — `principal.clearance >= 3` answered from
// a local override on one instance and the directory on every other, the same
// grant, a different verdict, with nothing in a trace or a note to say which layer
// answered. So the assertion is not merely "both layers are read": it is that the
// shared value STANDS on every key both serve, per slot, because the slots are
// filled by different wiring and a fix applied to one is not a fix applied to all.
func TestTheSharedLayerIsNotShadowedByTheLocalBag(t *testing.T) {
	ctx := context.Background()
	for _, slot := range AttributeSlots() {
		t.Run(slot.String(), func(t *testing.T) {
			shared := &countingAttributes{bags: map[string]Metadata{
				"k": {"department": "eng", "clearance": int64(5)},
			}}
			local := &countingAttributes{bags: map[string]Metadata{
				// department collides; team and clearance do not (clearance is the
				// LOWER value on purpose — a shadowing merge would read as an
				// access change, not as a typo).
				"k": {"department": "sales", "clearance": int64(1), "team": "atlas"},
			}}
			reg := NewAttributeRegistry()
			reg.MustRegister(slot, shared)
			reg.MustRegisterLocal(slot, local)

			md, err := reg.Fetch(ctx, slot, "k")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if md["department"] != "eng" {
				t.Errorf("department = %#v; the local layer shadowed the shared one", md["department"])
			}
			if md["clearance"] != int64(5) {
				t.Errorf("clearance = %#v; want the shared layer's 5", md["clearance"])
			}
			if md["team"] != "atlas" {
				t.Errorf("team = %#v; the local layer's own key was dropped", md["team"])
			}
			if len(md) != 3 {
				t.Errorf("merged bag = %#v; want exactly the union of the two", md)
			}

			// Neither input was written through. Both are cached values shared
			// across every object in the decision and every concurrent decision
			// for this key, so a merge that stamped into either would be a write
			// through a read-only value at the widest blast radius Aperture has.
			if sharedBag := shared.bags["k"]; len(sharedBag) != 2 || sharedBag["team"] != nil {
				t.Errorf("the shared provider's own bag was mutated: %#v", sharedBag)
			}
			if localBag := local.bags["k"]; localBag["department"] != "sales" {
				t.Errorf("the local provider's own bag was mutated: %#v", localBag)
			}
		})
	}
}

// TestRegistrationOrderDoesNotDecidePrecedence: the winner is the LAYER, not the
// call order. A precedence that depended on which builder ran first would be
// re-derivable by anyone wiring in Go, and the seed builder's own ordering would
// become load-bearing for an authorization answer.
func TestRegistrationOrderDoesNotDecidePrecedence(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		wire func(*AttributeRegistry, AttributeProvider, AttributeProvider)
	}{
		{
			name: "shared first",
			wire: func(r *AttributeRegistry, s, l AttributeProvider) {
				r.MustRegister(AttributeSlotUser, s)
				r.MustRegisterLocal(AttributeSlotUser, l)
			},
		},
		{
			name: "local first",
			wire: func(r *AttributeRegistry, s, l AttributeProvider) {
				r.MustRegisterLocal(AttributeSlotUser, l)
				r.MustRegister(AttributeSlotUser, s)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAttributeRegistry()
			tc.wire(reg,
				&countingAttributes{bags: map[string]Metadata{"u-1": {"tier": "shared"}}},
				&countingAttributes{bags: map[string]Metadata{"u-1": {"tier": "local"}}})
			md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if md["tier"] != "shared" {
				t.Fatalf("tier = %#v; the winner must be the layer, not the registration order", md["tier"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Two, and no more
// ---------------------------------------------------------------------------

// TestASlotAcceptsTwoLayersAndRefusesAThird pins the arity. A slot holds a shared
// and a local provider; a SECOND registration in either layer is still the refusal
// it always was, because "last writer wins" over one layer is the shadowing this
// package refuses — the relaxation is a second LAYER with a stated winner, not a
// second candidate for the same one.
func TestASlotAcceptsTwoLayersAndRefusesAThird(t *testing.T) {
	ctx := context.Background()
	shared := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
	local := &countingAttributes{bags: map[string]Metadata{"u-1": {"team": "atlas"}}}

	reg := NewAttributeRegistry()
	if err := reg.Register(AttributeSlotUser, shared); err != nil {
		t.Fatalf("shared register: %v", err)
	}
	if err := reg.RegisterLocal(AttributeSlotUser, local); err != nil {
		t.Fatalf("local register: %v", err)
	}
	if got, want := reg.Layers(AttributeSlotUser),
		[]AttributeLayer{AttributeLayerShared, AttributeLayerLocal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Layers(user) = %v, want %v", got, want)
	}

	for _, tc := range []struct {
		name string
		put  func(AttributeProvider) error
	}{
		{name: "a third in the shared layer", put: func(p AttributeProvider) error {
			return reg.Register(AttributeSlotUser, p)
		}},
		{name: "a third in the local layer", put: func(p AttributeProvider) error {
			return reg.RegisterLocal(AttributeSlotUser, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			third := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "third"}}}
			err := tc.put(third)
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID (err=%v)", code, err)
			}
			// Exactly one Aperture-coded error in the chain: a refusal this
			// package raises itself is not something it also wraps.
			if d := codedDepth(err); d != 1 {
				t.Fatalf("coded chain depth = %d, want 1: %v", d, err)
			}
			if third.fetches.Load() != 0 {
				t.Fatal("the refused provider was consulted")
			}
			md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if md["department"] != "eng" || md["team"] != "atlas" {
				t.Fatalf("the refused registration took effect: %#v", md)
			}
		})
	}
}

// TestASingleLocalLayerBehavesLikeASingleSharedOne: a slot whose only registration
// is local is an ordinary one-source slot. It matters because the seed builder
// registers every inline attributes: block that way, so this is the shape most
// deployments with inline bags are actually in.
func TestASingleLocalLayerBehavesLikeASingleSharedOne(t *testing.T) {
	ctx := context.Background()
	local := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
	reg := NewAttributeRegistry()
	reg.MustRegisterLocal(AttributeSlotUser, local)

	if !reg.Has(AttributeSlotUser) {
		t.Fatal("a local-only slot does not report Has")
	}
	if got, want := reg.Layers(AttributeSlotUser), []AttributeLayer{AttributeLayerLocal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Layers(user) = %v, want %v", got, want)
	}
	if got, want := reg.RegisteredSlots(), []AttributeSlot{AttributeSlotUser}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisteredSlots() = %v, want %v", got, want)
	}
	md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The same map the provider handed over, not a merged copy: the single-layer
	// path allocates nothing, which is the path every existing deployment is on.
	if !reflect.DeepEqual(md, local.bags["u-1"]) {
		t.Fatalf("bag = %#v, want the provider's own", md)
	}
	if _, ok := reg.CacheConfigForLayer(AttributeSlotUser, AttributeLayerShared); ok {
		t.Fatal("an unfilled layer reported a cache config")
	}
}

// TestARefusedRegistrationLeavesNoSlotBehind: a slot entry is created only once a
// registration is known to succeed, so a refusal cannot leave an empty shell that
// Has and RegisteredSlots report as wired — which would turn every fetch against
// it from a wiring diagnostic into a nil-pointer panic.
func TestARefusedRegistrationLeavesNoSlotBehind(t *testing.T) {
	reg := NewAttributeRegistry()
	if err := reg.RegisterLocal(AttributeSlotUser, nil); err == nil {
		t.Fatal("a nil provider was accepted")
	}
	if reg.Has(AttributeSlotUser) {
		t.Fatal("a refused registration left the slot reporting Has")
	}
	if slots := reg.RegisteredSlots(); len(slots) != 0 {
		t.Fatalf("RegisteredSlots() = %v, want none", slots)
	}
	if _, err := reg.Fetch(context.Background(), AttributeSlotUser, "u-1"); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", aerr.CodeOf(err))
	}
}

// ---------------------------------------------------------------------------
// What each layer's failure means
// ---------------------------------------------------------------------------

// TestALayerThatDoesNotKnowAKeyIsNotTheSlotRefusingIt: one layer's
// APERTURE_NOT_FOUND means that layer has no record, and the other layer answers.
// A shared directory that does not carry a subject the local block declares — or
// the reverse — is the ordinary case for a layered slot, and it is the case the old
// whole-slot discard could not express.
func TestALayerThatDoesNotKnowAKeyIsNotTheSlotRefusingIt(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{bags: map[string]Metadata{
		"only-shared": {"department": "eng"},
	}})
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{bags: map[string]Metadata{
		"only-local": {"team": "atlas"},
	}})

	for _, tc := range []struct {
		key  string
		want Metadata
	}{
		{key: "only-shared", want: Metadata{"department": "eng"}},
		{key: "only-local", want: Metadata{"team": "atlas"}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			md, err := reg.Fetch(ctx, AttributeSlotUser, tc.key)
			if err != nil {
				t.Fatalf("Fetch(%s): %v", tc.key, err)
			}
			if !reflect.DeepEqual(md, tc.want) {
				t.Fatalf("bag = %#v, want %#v", md, tc.want)
			}
		})
	}
}

// TestNoLayerKnowingAKeyIsStillNotFound: the leniency contract is not widened. A
// key no layer serves reports APERTURE_NOT_FOUND exactly as a single-layer slot
// does, so the two codes Attributes and AccountAttributes collapse to a nil bag are
// the two they were — reached through the resolver seams, because that is where the
// collapse lives.
func TestNoLayerKnowingAKeyIsStillNotFound(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	for _, slot := range AttributeSlots() {
		reg.MustRegister(slot, &countingAttributes{bags: map[string]Metadata{"known": {"a": 1}}})
		reg.MustRegisterLocal(slot, &countingAttributes{bags: map[string]Metadata{"known": {"b": 2}}})
	}

	err := func() error {
		_, err := reg.Fetch(ctx, AttributeSlotUser, "nobody")
		return err
	}()
	if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND (err=%v)", code, err)
	}
	if d := codedDepth(err); d != 1 {
		t.Fatalf("coded chain depth = %d, want 1 — a layer's own coded refusal is not re-stamped: %v", d, err)
	}

	bag, err := reg.Attributes(ctx, "user", "nobody")
	if err != nil || bag != nil {
		t.Fatalf("Attributes = %#v, %v; want a nil bag and no error", bag, err)
	}
	acct, err := reg.AccountAttributes(ctx, "nobody")
	if err != nil || acct != nil {
		t.Fatalf("AccountAttributes = %#v, %v; want a nil bag and no error", acct, err)
	}
}

// TestAnUnreachableLayerIsNeverAnsweredOutOfTheOther is the one that decides
// whether layering is safe at all.
//
// An outage in the shared directory must surface as an outage. If a failed shared
// fetch fell back to the local layer, a deployment would keep deciding — against
// one machine's file, with a bag that is a legal bag, on a decision that looks
// exactly like a correct one. That is an authorization change wearing an
// infrastructure failure's clothes, which is the distinction this seam's NOT_FOUND
// rule exists to preserve, and it must hold in BOTH directions: a broken local
// layer is not silently answered out of the shared directory either, because a
// deployment cannot be allowed to half-read a slot it believes it read whole.
func TestAnUnreachableLayerIsNeverAnsweredOutOfTheOther(t *testing.T) {
	ctx := context.Background()
	boom := aerr.New(aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH, "test: the directory is unreachable")

	for _, tc := range []struct {
		name  string
		wire  func(*AttributeRegistry, AttributeProvider)
		other *countingAttributes
	}{
		{
			name: "the shared layer is down",
			wire: func(r *AttributeRegistry, broken AttributeProvider) {
				r.MustRegister(AttributeSlotUser, broken)
				r.MustRegisterLocal(AttributeSlotUser, &countingAttributes{
					bags: map[string]Metadata{"u-1": {"department": "local"}},
				})
			},
		},
		{
			name: "the local layer is down",
			wire: func(r *AttributeRegistry, broken AttributeProvider) {
				r.MustRegister(AttributeSlotUser, &countingAttributes{
					bags: map[string]Metadata{"u-1": {"department": "shared"}},
				})
				r.MustRegisterLocal(AttributeSlotUser, broken)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAttributeRegistry()
			tc.wire(reg, &countingAttributes{err: boom})

			md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
			if err == nil {
				t.Fatalf("Fetch succeeded with %#v; an unreachable layer was answered out of the other one", md)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
				t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH (err=%v)", code, err)
			}
			// Verbatim: an already-coded provider error keeps its own code and its
			// registry fixups, so the chain holds exactly one Aperture error.
			if d := codedDepth(err); d != 1 {
				t.Fatalf("coded chain depth = %d, want 1: %v", d, err)
			}
			// The resolver seams must not collapse it either: a non-decision is
			// the correct outcome and a nil bag would be an authorization change.
			if _, err := reg.Attributes(ctx, "user", "u-1"); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
				t.Fatalf("Attributes collapsed an outage: %v", err)
			}
		})
	}
}

// TestAPlainLayerErrorIsStillWrappedOnce: a provider that returns an uncoded error
// is classified APERTURE_ATTRIBUTE_PROVIDER_FETCH, once, on either layer. The
// merge path must go through the same attributeError both single-layer fetches do,
// or a layered slot would report a bare error across a package boundary.
func TestAPlainLayerErrorIsStillWrappedOnce(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{err: errors.New("dial tcp: refused")})
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{bags: map[string]Metadata{"u-1": {"a": 1}}})

	_, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
		t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH (err=%v)", code, err)
	}
	if d := codedDepth(err); d != 1 {
		t.Fatalf("coded chain depth = %d, want 1: %v", d, err)
	}
}

// ---------------------------------------------------------------------------
// Caching, per layer
// ---------------------------------------------------------------------------

// TestEachLayerKeepsItsOwnCacheAndItsOwnTTL: a layer's ttl: is its own revocation
// window, declared per source and honoured per source.
//
// One merged cache per slot could honour at most one of two declarations, and both
// choices are wrong: the longer window silently LENGTHENS the time a revoked shared
// attribute keeps authorizing, and the shorter one silently ignores a declaration an
// operator made. The inline attributes: block really does register with a ttl of 0,
// so a slot with an external source beside it is exactly the case where a pooled
// window would have made the directory's five minutes into forever.
func TestEachLayerKeepsItsOwnCacheAndItsOwnTTL(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	shared := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
	local := &countingAttributes{bags: map[string]Metadata{"u-1": {"team": "atlas"}}}

	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, shared, WithTTL(time.Minute), WithMaxSize(4), WithClock(clock.Now))
	reg.MustRegisterLocal(AttributeSlotUser, local, WithTTL(0), WithClock(clock.Now))

	sharedCfg, ok := reg.CacheConfigForLayer(AttributeSlotUser, AttributeLayerShared)
	if !ok || sharedCfg.TTL != time.Minute || sharedCfg.MaxSize != 4 {
		t.Fatalf("shared cfg = %+v (ok=%v), want TTL 1m / MaxSize 4", sharedCfg, ok)
	}
	localCfg, ok := reg.CacheConfigForLayer(AttributeSlotUser, AttributeLayerLocal)
	if !ok || localCfg.TTL != 0 || localCfg.MaxSize != DefaultMaxSize {
		t.Fatalf("local cfg = %+v (ok=%v), want TTL 0 / default MaxSize", localCfg, ok)
	}
	// CacheConfigFor reports the GOVERNING layer, which is the shared one: its ttl
	// is the window in which a revoked shared attribute keeps authorizing, and that
	// is the number an operator surface exists to show.
	if cfg, _ := reg.CacheConfigFor(AttributeSlotUser); cfg.TTL != time.Minute {
		t.Fatalf("CacheConfigFor(user).TTL = %s, want the shared layer's 1m", cfg.TTL)
	}

	if _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	clock.Advance(30 * time.Minute)
	md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if md["department"] != "eng" || md["team"] != "atlas" {
		t.Fatalf("bag after expiry = %#v; want both layers still merged", md)
	}
	if n := shared.fetches.Load(); n != 2 {
		t.Errorf("shared provider fetched %d times, want 2 — its minute TTL expired", n)
	}
	if n := local.fetches.Load(); n != 1 {
		t.Errorf("local provider fetched %d times, want 1 — a TTL of 0 never expires", n)
	}
	// Stats are summed across the slot's layers: one expiration and one hit is
	// exactly what two independently configured caches did.
	st, _ := reg.Stats(AttributeSlotUser)
	if st.Expirations != 1 || st.Hits != 1 || st.Entries != 2 {
		t.Fatalf("stats = %+v; want 1 expiration, 1 hit, 2 entries (one bag per layer)", st)
	}
}

// TestInvalidationClearsEveryLayer: all three Invalidate* methods, against a
// layered slot.
//
// A layer left warm is the whole hazard: the operator has been told the window is
// closed, the revoked attribute is gone from one cache, and the merged bag is still
// being assembled out of the other. Each method is asserted by the PROVIDER FETCH
// COUNT on both layers, not by the bool, because the bool is what a wrong
// implementation would still get right.
func TestInvalidationClearsEveryLayer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		drop func(*AttributeRegistry) error
	}{
		{name: "Invalidate", drop: func(r *AttributeRegistry) error {
			dropped, err := r.Invalidate(AttributeSlotUser, "u-1")
			if err == nil && !dropped {
				return errors.New("Invalidate reported nothing dropped")
			}
			return err
		}},
		{name: "InvalidateSlot", drop: func(r *AttributeRegistry) error {
			return r.InvalidateSlot(AttributeSlotUser)
		}},
		{name: "InvalidateAll", drop: func(r *AttributeRegistry) error {
			r.InvalidateAll()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
			local := &countingAttributes{bags: map[string]Metadata{"u-1": {"team": "atlas"}}}
			reg := NewAttributeRegistry()
			reg.MustRegister(AttributeSlotUser, shared)
			reg.MustRegisterLocal(AttributeSlotUser, local)

			if _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); err != nil {
				t.Fatalf("warm: %v", err)
			}
			if st, _ := reg.Stats(AttributeSlotUser); st.Entries != 2 {
				t.Fatalf("entries after warm = %d, want 2 (one per layer)", st.Entries)
			}
			if err := tc.drop(reg); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if st, _ := reg.Stats(AttributeSlotUser); st.Entries != 0 {
				t.Fatalf("entries after %s = %d, want 0 — a layer was left warm", tc.name, st.Entries)
			}
			if _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); err != nil {
				t.Fatalf("re-fetch: %v", err)
			}
			if n := shared.fetches.Load(); n != 2 {
				t.Errorf("shared provider fetched %d times, want 2 — its cache was not cleared", n)
			}
			if n := local.fetches.Load(); n != 2 {
				t.Errorf("local provider fetched %d times, want 2 — its cache was not cleared", n)
			}
		})
	}
}

// TestInvalidateRefusesTheSameKeysOnALayeredSlot: the key guard is the slot's, not
// a layer's, so the empty string and the account wildcard are refused before any
// cache is touched — one definition, shared with Fetch.
func TestInvalidateRefusesTheSameKeysOnALayeredSlot(t *testing.T) {
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotAccount, &countingAttributes{bags: map[string]Metadata{"acme": {"plan": "gold"}}})
	reg.MustRegisterLocal(AttributeSlotAccount, &countingAttributes{bags: map[string]Metadata{"acme": {"region": "eu"}}})

	for _, key := range []string{"", "*"} {
		dropped, err := reg.Invalidate(AttributeSlotAccount, key)
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("Invalidate(%q) code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", key, code)
		}
		if dropped {
			t.Fatalf("Invalidate(%q) reported a drop", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Enumerate
// ---------------------------------------------------------------------------

// TestEnumerateMergesBothLayersAndStillWritesNoCache is the E3-S5 guard carried
// forward to two layers.
//
// The merge is the easy half: a listing must show the bag a Fetch of that key would
// return, or an operator diagnosing a decision is reading a different bag from the
// one the decision read. The CACHE is the load-bearing half. Enumerate must write
// NEITHER layer's cache: an attribute ListQuery need only select a bare id, so a
// narrower get_all substituted for the authoritative bag makes a key ABSENT, every
// predicate over it false, and an EXCLUSIVE grant stop excluding for the whole of
// the slot's ttl. Two layers is two caches and therefore two ways to reintroduce
// that, so the entry count is asserted per slot after a full enumeration.
func TestEnumerateMergesBothLayersAndStillWritesNoCache(t *testing.T) {
	ctx := context.Background()
	shared := &countingAttributes{records: []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng", "clearance": int64(5)}},
		{ID: "u-2", Attributes: Metadata{"department": "sales"}},
	}}
	local := &countingAttributes{records: []AttributeRecord{
		// u-1 collides on department and adds team; u-3 is the local layer's own.
		{ID: "u-1", Attributes: Metadata{"department": "LOCAL", "team": "atlas"}},
		{ID: "u-3", Attributes: Metadata{"department": "ops"}},
	}}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, shared)
	reg.MustRegisterLocal(AttributeSlotUser, local)

	got, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	want := []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng", "clearance": int64(5), "team": "atlas"}},
		{ID: "u-2", Attributes: Metadata{"department": "sales"}},
		{ID: "u-3", Attributes: Metadata{"department": "ops"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Enumerate =\n%#v\nwant\n%#v", got, want)
	}
	// Shared-first order, then the keys only the local layer knows, so a
	// provider's own ORDER BY stays visible instead of being replaced by a sort.
	if got[0].ID != "u-1" || got[2].ID != "u-3" {
		t.Errorf("order = %v; want the shared layer's records first", []string{got[0].ID, got[1].ID, got[2].ID})
	}
	if st, ok := reg.Stats(AttributeSlotUser); !ok || st.Entries != 0 {
		t.Fatalf("stats = %+v; an enumeration warmed a layer's FETCH cache — see Enumerate's doc", st)
	}
	// Nothing about Query is allowed to reach a provider's Fetch, on either layer.
	if shared.fetches.Load() != 0 || local.fetches.Load() != 0 {
		t.Fatalf("Enumerate called Fetch (shared=%d local=%d)", shared.fetches.Load(), local.fetches.Load())
	}
}

// TestEnumerateAppliesFieldsToTheMergedBag: the predicate runs on what a Fetch
// would return, not on either layer's contribution.
//
// Both directions are wrong if it does not. Filtering per layer would ADMIT a
// record on a local value the shared layer overrides — shown alongside a bag that
// does not match it — and DROP a record whose only matching value comes from the
// other layer.
func TestEnumerateAppliesFieldsToTheMergedBag(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "overridden", Attributes: Metadata{"department": "eng"}},
		{ID: "needs-local", Attributes: Metadata{"department": "eng"}},
	}})
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		// The shared layer overrides this department, so the record must NOT match.
		{ID: "overridden", Attributes: Metadata{"department": "ops"}},
		// Only the local layer serves team, so the record matches only after merging.
		{ID: "needs-local", Attributes: Metadata{"team": "atlas"}},
	}})

	byDept, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Fields: map[string]any{"department": "ops"}})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(byDept) != 0 {
		t.Fatalf("Enumerate matched %#v; the local value the shared layer overrides must not select", byDept)
	}
	byTeam, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Fields: map[string]any{"team": "atlas"}})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(byTeam) != 1 || byTeam[0].ID != "needs-local" {
		t.Fatalf("Enumerate = %#v; want the record the merge makes match", byTeam)
	}
}

// TestEnumerateBoundsTheMergedSet: the limit is re-enforced on the merge, so a
// layered slot's listing is bounded exactly as a single layer's is.
func TestEnumerateBoundsTheMergedSet(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"a": 1}},
		{ID: "u-2", Attributes: Metadata{"a": 1}},
	}})
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "u-3", Attributes: Metadata{"a": 1}},
		{ID: "u-4", Attributes: Metadata{"a": 1}},
	}})
	got, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Limit: 3})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Enumerate returned %d records, want 3", len(got))
	}
}

// TestEnumerateSurfacesEitherLayersFailure: a listing over a slot one of whose
// layers is unreachable is a failed listing, not a partial one silently rendered as
// the whole directory.
func TestEnumerateSurfacesEitherLayersFailure(t *testing.T) {
	ctx := context.Background()
	boom := aerr.New(aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH, "test: the directory is unreachable")
	healthy := []AttributeRecord{{ID: "u-1", Attributes: Metadata{"a": 1}}}

	for _, tc := range []struct {
		name string
		wire func(*AttributeRegistry)
	}{
		{name: "shared down", wire: func(r *AttributeRegistry) {
			r.MustRegister(AttributeSlotUser, &countingAttributes{err: boom})
			r.MustRegisterLocal(AttributeSlotUser, &countingAttributes{records: healthy})
		}},
		{name: "local down", wire: func(r *AttributeRegistry) {
			r.MustRegister(AttributeSlotUser, &countingAttributes{records: healthy})
			r.MustRegisterLocal(AttributeSlotUser, &countingAttributes{err: boom})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewAttributeRegistry()
			tc.wire(reg)
			recs, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{})
			if err == nil {
				t.Fatalf("Enumerate returned %#v; a half-read directory must not render as the whole one", recs)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
				t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH", code)
			}
			if d := codedDepth(err); d != 1 {
				t.Fatalf("coded chain depth = %d, want 1: %v", d, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The layer set
// ---------------------------------------------------------------------------

// TestTheLayerSetIsClosedAtTwoInPrecedenceOrder: the set is exactly shared and
// local, IN precedence order. The order is the contract — a caller that walks
// AttributeLayers() and takes the first answer must get the precedence Fetch
// applies — so reversing it is an authorization change, not a cosmetic one.
func TestTheLayerSetIsClosedAtTwoInPrecedenceOrder(t *testing.T) {
	got := AttributeLayers()
	want := []AttributeLayer{AttributeLayerShared, AttributeLayerLocal}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttributeLayers() = %v, want %v (highest precedence FIRST)", got, want)
	}
	for _, l := range want {
		if !l.Valid() || l.String() != string(l) {
			t.Errorf("%q: Valid()=%v String()=%q", l, l.Valid(), l.String())
		}
	}
	if AttributeLayerShared.String() != "shared" || AttributeLayerLocal.String() != "local" {
		t.Error("the layer keys are what a listing prints; they must stay shared/local")
	}
	if AttributeLayer("").Valid() || AttributeLayer("Shared").Valid() || AttributeLayer("host").Valid() {
		t.Error("the layer set is not closed")
	}
}

// TestAnUnknownLayerIsRefusedRatherThanPanicking: every exported registration
// passes a constant, so this is the unexported path's backstop — a refusal instead
// of a silent no-op that would leave a provider registered nowhere.
func TestAnUnknownLayerIsRefusedRatherThanPanicking(t *testing.T) {
	reg := NewAttributeRegistry()
	err := reg.register(AttributeSlotUser, AttributeLayer("host"),
		&countingAttributes{bags: map[string]Metadata{"u-1": {"a": 1}}})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
		t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID (err=%v)", code, err)
	}
	if reg.Has(AttributeSlotUser) {
		t.Fatal("a refused layer left the slot wired")
	}
}

// ---------------------------------------------------------------------------
// The three tiers compose
// ---------------------------------------------------------------------------

// TestTheLayersComposeUnderTheFloorThroughTheResolverSeams asserts the whole stack
// in the direction it is claimed: floor over shared over local.
//
// It goes through Attributes and AccountAttributes — the two methods rules wires as
// its PrincipalResolver and AccountResolver — because that is the shape the engine
// sees, and the two floor keys (`principal.id` / `principal.kind`, `account.id`) are
// stamped one tier ABOVE this package by rules.principalBag. What this asserts here
// is the half provider owns: the merged bag a resolver hands up carries the SHARED
// layer's values, so the engine's floor lands on top of a bag whose contested keys
// have already been decided the same way on every instance.
func TestTheLayersComposeUnderTheFloorThroughTheResolverSeams(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	// Both providers spell an `id` and a `kind` key, which is the innocent
	// collision rules.principalBag's floor exists for. Neither layer may win it
	// there; here, the shared layer must win it over the local one.
	reg.MustRegister(AttributeSlotUser, &countingAttributes{bags: map[string]Metadata{
		"alice": {"id": "shared-surrogate", "kind": "shared-kind", "clearance": int64(5)},
	}})
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{bags: map[string]Metadata{
		"alice": {"id": "local-surrogate", "kind": "local-kind", "team": "atlas"},
	}})
	reg.MustRegister(AttributeSlotAccount, &countingAttributes{bags: map[string]Metadata{
		"acme": {"id": "shared-account", "plan": "enterprise"},
	}})
	reg.MustRegisterLocal(AttributeSlotAccount, &countingAttributes{bags: map[string]Metadata{
		"acme": {"id": "local-account", "plan": "free", "region": "eu"},
	}})

	principal, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if principal["id"] != "shared-surrogate" || principal["kind"] != "shared-kind" {
		t.Errorf("principal bag = %#v; the shared layer must win even the keys the floor later replaces", principal)
	}
	if principal["clearance"] != int64(5) || principal["team"] != "atlas" {
		t.Errorf("principal bag = %#v; want the union of the two layers", principal)
	}

	account, err := reg.AccountAttributes(ctx, "acme")
	if err != nil {
		t.Fatalf("AccountAttributes: %v", err)
	}
	if account["id"] != "shared-account" || account["plan"] != "enterprise" {
		t.Errorf("account bag = %#v; the shared layer must win a contested key", account)
	}
	if account["region"] != "eu" {
		t.Errorf("account bag = %#v; want the local layer's own key", account)
	}

	// The wildcard is still refused at this seam, on a layered slot, before any
	// layer is consulted: rules short-circuits it to the floor, and this is the
	// backstop that keeps that a backstop.
	if _, err := reg.AccountAttributes(ctx, "*"); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
		t.Fatalf("AccountAttributes(\"*\") code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", aerr.CodeOf(err))
	}
}
