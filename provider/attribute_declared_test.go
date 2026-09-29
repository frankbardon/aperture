package provider

import (
	"context"
	"reflect"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// A declared key set reserves its keys to the layer that declared them.
//
// attribute_layer.go's file doc is the account; these are its two closed holes and
// the three things that must NOT change while they close. The holes were both
// "shared wins" read as a statement about keys the shared bag HAPPENS to carry:
//
//	(a) the shared layer serves a key and omits it for ONE ROW (a SQL NULL becomes
//	    an absent field), and the local file answered that row — inside the shared
//	    layer's own declared set, with the rule validating cleanly; and
//	(b) the shared layer has NO RECORD for a subject, so deleting a principal from
//	    the directory revoked nothing on any instance whose own file still listed
//	    them, forever, since the inline layer is registered with WithTTL(0).
//
// (b) is the security half and is why these tests exist at all: without it there is
// no instance-independent way to revoke an attribute.
//
// The three that must not change are asserted here beside them, because each one
// stays green when the others break: a layer that declares NOTHING must suppress
// NOTHING (the opt-in bar — every deployment that has never written the key), a
// local layer must still ADD keys outside the set (the reversed decision this
// layering replaced a discard with), and declared-EMPTY must stay a different STATE
// from not-declared even though the two suppress the same amount.

// declaredFixture wires a slot with a shared layer that declares `declared` and a
// local layer that does not, and returns the registry plus both providers.
//
// declared is a *[]string for the same reason seed.AttributeProvider.DeclaredKeys
// is: nil is NOT DECLARED (the option is never passed) and non-nil is DECLARED
// whatever it points at. A fixture that took a []string could not express the
// opt-in bar and the declared-empty case in the same table.
func declaredFixture(t *testing.T, slot AttributeSlot, declared *[]string, sharedBags, localBags map[string]Metadata) (*AttributeRegistry, *countingAttributes, *countingAttributes) {
	t.Helper()
	shared := &countingAttributes{bags: sharedBags}
	local := &countingAttributes{bags: localBags}
	reg := NewAttributeRegistry()
	var opts []AttributeRegistrationOption
	if declared != nil {
		opts = append(opts, WithDeclaredKeys(*declared))
	}
	reg.MustRegister(slot, shared, opts...)
	reg.MustRegisterLocal(slot, local)
	return reg, shared, local
}

func keys(names ...string) *[]string { return &names }

// ---------------------------------------------------------------------------
// The two holes
// ---------------------------------------------------------------------------

// TestADeclaredKeyTheSharedLayerOmitsIsNotAnsweredLocally is hole (a). The shared
// layer SERVES clearance — it declares it and carries it for u-2 — and simply has
// no clearance for u-1, which is what a NULL column looks like after
// metadataValue: an absent field, indistinguishable from "this layer has no
// opinion".
//
// Before the declared set was made authoritative, u-1's clearance was read out of
// the local file. The rule validated (the shared entry declares clearance), the
// verdict was correct-looking, and the value came from one machine.
func TestADeclaredKeyTheSharedLayerOmitsIsNotAnsweredLocally(t *testing.T) {
	ctx := context.Background()
	for _, slot := range AttributeSlots() {
		t.Run(slot.String(), func(t *testing.T) {
			reg, _, _ := declaredFixture(t, slot, keys("department", "clearance"),
				map[string]Metadata{
					// u-1's clearance is the hole: declared, served for u-2, absent here.
					"u-1": {"department": "eng"},
					"u-2": {"department": "ops", "clearance": int64(5)},
				},
				map[string]Metadata{
					"u-1": {"clearance": int64(9), "team": "atlas"},
				})

			md, err := reg.Fetch(ctx, slot, "u-1")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got, ok := md["clearance"]; ok {
				t.Errorf("clearance = %#v; the shared layer DECLARES clearance, so a row it omits "+
					"must not be answered out of the local file — that is one machine's value "+
					"inside the deployment's own declared set", got)
			}
			if md["department"] != "eng" {
				t.Errorf("department = %#v; the shared layer's own value must still stand", md["department"])
			}
			if md["team"] != "atlas" {
				t.Errorf("team = %#v; team is OUTSIDE the declared set, so the local layer must "+
					"still add it — suppression is scoped to the set, it is not a discard", md["team"])
			}
			// The row the shared layer does populate is unaffected.
			md2, err := reg.Fetch(ctx, slot, "u-2")
			if err != nil {
				t.Fatalf("Fetch(u-2): %v", err)
			}
			if md2["clearance"] != int64(5) {
				t.Errorf("u-2 clearance = %#v; want the shared layer's 5", md2["clearance"])
			}
		})
	}
}

// TestRemovingASubjectFromTheSharedDirectoryRevokesItsDeclaredKeys is hole (b),
// and it is the reason the whole change exists.
//
// The shared layer has NO RECORD for `revoked` — APERTURE_NOT_FOUND, which a fetch
// reads as "this layer has no record for this key", the ordinary and necessary
// case. The local file still lists them. Before this, that instance went on
// deciding `principal.clearance >= 3` against the file's 9 forever: the inline
// layer is registered with WithTTL(0), so nothing expired and an invalidation had
// nothing stale to drop. The delete was a revocation on every instance that did
// not happen to carry a local entry, and silently not one on the instances that
// did.
func TestRemovingASubjectFromTheSharedDirectoryRevokesItsDeclaredKeys(t *testing.T) {
	ctx := context.Background()
	for _, slot := range AttributeSlots() {
		t.Run(slot.String(), func(t *testing.T) {
			reg, shared, _ := declaredFixture(t, slot, keys("clearance"),
				// The subject has been deleted from the directory.
				map[string]Metadata{"still-there": {"clearance": int64(1)}},
				map[string]Metadata{"revoked": {"clearance": int64(9), "team": "atlas"}})

			md, err := reg.Fetch(ctx, slot, "revoked")
			if err != nil {
				t.Fatalf("a subject the LOCAL layer still knows must not become a failed "+
					"fetch; leniency is unchanged: %v", err)
			}
			if got, ok := md["clearance"]; ok {
				t.Errorf("clearance = %#v; the shared layer has no record for this subject and "+
					"DECLARES clearance, so the declared key is revoked — a local file must not "+
					"keep authorizing a subject the directory has dropped", got)
			}
			if md["team"] != "atlas" {
				t.Errorf("team = %#v; the undeclared remainder still answers, which is what keeps "+
					"the local layer a layer and not a candidate", md["team"])
			}
			// And the shared layer really was asked. A suppression that worked by
			// never consulting the directory would pass this test and be a different,
			// worse thing.
			if shared.fetches.Load() == 0 {
				t.Error("the shared layer was never fetched; suppression must follow a real read")
			}
		})
	}
}

// TestADeclaredKeyBothLayersServeStillReadsTheSharedValue is the case that was
// ALREADY right, kept because it is the one suppression must not overshoot into.
// The shared layer's value stands; it is not dropped along with the local one.
func TestADeclaredKeyBothLayersServeStillReadsTheSharedValue(t *testing.T) {
	ctx := context.Background()
	reg, _, _ := declaredFixture(t, AttributeSlotUser, keys("clearance"),
		map[string]Metadata{"u-1": {"clearance": int64(5)}},
		map[string]Metadata{"u-1": {"clearance": int64(9)}})

	md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["clearance"] != int64(5) {
		t.Fatalf("clearance = %#v; want the shared layer's 5 — reserving a key must not "+
			"suppress the value the declaring layer actually gave", md["clearance"])
	}
}

// ---------------------------------------------------------------------------
// What must not change
// ---------------------------------------------------------------------------

// TestALayerThatDeclaresNothingSuppressesNothing is the opt-in bar, and it is the
// non-negotiable half: every deployment that has never written a `declared_keys:`
// must merge exactly as it did. It is asserted at the SURFACE — through Fetch on a
// registry wired the way seed wires one — rather than on the merge helper, because
// a helper can keep its behaviour while a call site starts passing a set nobody
// declared.
func TestALayerThatDeclaresNothingSuppressesNothing(t *testing.T) {
	ctx := context.Background()
	for _, slot := range AttributeSlots() {
		t.Run(slot.String(), func(t *testing.T) {
			reg, _, _ := declaredFixture(t, slot, nil,
				map[string]Metadata{"u-1": {"department": "eng"}},
				map[string]Metadata{
					// The (a) shape and the (b) shape, both, with nothing declared.
					"u-1":     {"clearance": int64(9), "team": "atlas"},
					"unknown": {"clearance": int64(7)},
				})

			md, err := reg.Fetch(ctx, slot, "u-1")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			want := Metadata{"department": "eng", "clearance": int64(9), "team": "atlas"}
			if !reflect.DeepEqual(md, want) {
				t.Errorf("merged bag = %#v, want %#v — a slot that declares nothing reserves "+
					"nothing, and that is the whole backward-compatibility bar", md, want)
			}
			// The no-record-in-shared shape too: the local bag is the answer, entire.
			md2, err := reg.Fetch(ctx, slot, "unknown")
			if err != nil {
				t.Fatalf("Fetch(unknown): %v", err)
			}
			if md2["clearance"] != int64(7) {
				t.Errorf("bag = %#v; with nothing declared, a subject only the local layer knows "+
					"is answered from it in full", md2)
			}
		})
	}
}

// TestDeclaredEmptyIsADeclarationThatReservesNothing keeps the distinction the
// whole codebase is careful about from collapsing in the one place that reads the
// set on the decision path.
//
// `declared_keys: []` reserves no key, so it suppresses nothing — the same OUTCOME
// as not declaring. It is not the same STATE: it is opted IN to rule enforcement
// (that half lives on the facade) and a representation that inferred "declared"
// from a non-empty set would silently turn the two into one. So the outcome is
// asserted at the surface and the state is asserted on the registered entry, which
// is the only place both are visible at once.
func TestDeclaredEmptyIsADeclarationThatReservesNothing(t *testing.T) {
	ctx := context.Background()
	sharedBags := map[string]Metadata{"u-1": {"department": "eng"}}
	localBags := map[string]Metadata{"u-1": {"clearance": int64(9)}}

	for _, tc := range []struct {
		name         string
		declared     *[]string
		wantDeclared bool
	}{
		{name: "not declared", declared: nil, wantDeclared: false},
		{name: "declared empty", declared: &[]string{}, wantDeclared: true},
		{name: "declared empty from a nil slice", declared: keys(), wantDeclared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, _, _ := declaredFixture(t, AttributeSlotUser, tc.declared, sharedBags, localBags)

			set := reg.slots[AttributeSlotUser].shared.declared
			if set.declared != tc.wantDeclared {
				t.Errorf("declared = %v, want %v: DECLARED EMPTY and NOT DECLARED are "+
					"different answers everywhere else (model.DeclaredKeys, both dialects' "+
					"declared_keys column, `aperture wiring show`) and must not collapse here",
					set.declared, tc.wantDeclared)
			}
			if set.authoritative() {
				t.Error("an empty set must reserve nothing, whether or not it was declared")
			}
			md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if md["clearance"] != int64(9) {
				t.Errorf("clearance = %#v; an empty declared set names no key, so it reserves "+
					"none and the local layer answers exactly as before", md["clearance"])
			}
		})
	}
}

// TestOnlyTheSharedLayerMayDeclare: a declared set on the LOCAL layer would be one
// machine reserving keys against the deployment's own directory — the precedence
// inversion the layering exists to forbid, arrived at through an opt-in. It is
// refused at registration rather than ignored, because a silently-ignored security
// declaration is worse than a refused one.
func TestOnlyTheSharedLayerMayDeclare(t *testing.T) {
	reg := NewAttributeRegistry()
	p := &countingAttributes{bags: map[string]Metadata{"u-1": {"a": 1}}}

	err := reg.RegisterLocal(AttributeSlotUser, p, WithDeclaredKeys([]string{"a"}))
	if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
		t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID (err=%v)", code, err)
	}
	if d := codedDepth(err); d != 1 {
		t.Fatalf("coded chain depth = %d, want 1: %v", d, err)
	}
	if reg.Has(AttributeSlotUser) {
		t.Fatal("a refused registration left the slot wired")
	}
	// Declaring nothing on the local layer is of course still fine — that is how
	// seed registers every inline block.
	if err := reg.RegisterLocal(AttributeSlotUser, p, WithTTL(0)); err != nil {
		t.Fatalf("RegisterLocal with cache options only: %v", err)
	}
}

// TestARegisteredDeclaredSetCannotBeMutatedByItsCaller: the set is read on the
// decision path of every decision about every subject in the slot, so a caller
// holding the slice it passed must not be able to change what a layer reserves.
// The set's own MAP is the copy, and that is the point of it being a map rather
// than the slice it arrived as — a slice-backed set scanned with slices.Contains
// would compile, read identically, and let host state decide a decision.
func TestARegisteredDeclaredSetCannotBeMutatedByItsCaller(t *testing.T) {
	ctx := context.Background()
	declared := []string{"clearance"}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser,
		&countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}},
		WithDeclaredKeys(declared))
	reg.MustRegisterLocal(AttributeSlotUser,
		&countingAttributes{bags: map[string]Metadata{"u-1": {"clearance": int64(9), "team": "atlas"}}})

	// Rewrite the caller's slice to name a key it never declared.
	declared[0] = "team"

	md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := md["clearance"]; ok {
		t.Error("clearance stopped being reserved when the caller mutated its slice")
	}
	if md["team"] != "atlas" {
		t.Error("team became reserved when the caller mutated its slice; a registered set is " +
			"immutable, and an aliased one is host state deciding a decision")
	}
}

// ---------------------------------------------------------------------------
// The administrative listing
// ---------------------------------------------------------------------------

// TestEnumerateSuppressesWhatAFetchSuppresses: the listing's promise is that the
// bag it shows for a key is the bag a Fetch of that key would return, and the
// subjects that promise matters most for are exactly the revoked ones — an operator
// runs `aperture attributes query` to CHECK that a delete landed. A listing that
// still displayed the local value would be the one surface reporting the revoked
// attribute.
//
// `revoked` therefore appears with an EMPTY bag rather than vanishing: the local
// layer does have a record for it, and it now answers for nothing, which is exactly
// what a Fetch of it returns.
func TestEnumerateSuppressesWhatAFetchSuppresses(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng"}},
	}}, WithDeclaredKeys([]string{"clearance"}))
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		// The (a) shape: a contested subject whose declared key the shared layer omits.
		{ID: "u-1", Attributes: Metadata{"clearance": int64(9), "team": "atlas"}},
		// The (b) shape: a subject the shared layer no longer lists at all.
		{ID: "revoked", Attributes: Metadata{"clearance": int64(9)}},
	}})

	got, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	want := []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng", "team": "atlas"}},
		{ID: "revoked", Attributes: Metadata{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Enumerate =\n%#v\nwant\n%#v", got, want)
	}
	// Still no cache write: suppression only ever removes keys, and this path's
	// projection is allowed to be narrower than Fetch's — see Enumerate's doc.
	if st, ok := reg.Stats(AttributeSlotUser); !ok || st.Entries != 0 {
		t.Fatalf("stats = %+v; an enumeration warmed a layer's FETCH cache", st)
	}
}

// TestEnumerateFieldsRunOnTheSuppressedBag: the predicate runs on what a Fetch
// would return, which now means the SUPPRESSED merge. A record admitted on a
// declared value no decision could read would be a listing that answers a
// different question from the one the operator asked, in the direction that matters
// — "who still has clearance 9?" must not name a subject whose clearance was
// revoked.
func TestEnumerateFieldsRunOnTheSuppressedBag(t *testing.T) {
	ctx := context.Background()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng"}},
	}}, WithDeclaredKeys([]string{"clearance"}))
	reg.MustRegisterLocal(AttributeSlotUser, &countingAttributes{records: []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"clearance": int64(9)}},
		{ID: "revoked", Attributes: Metadata{"clearance": int64(9)}},
	}})

	got, err := reg.Enumerate(ctx, AttributeSlotUser,
		AttributeFilter{Fields: map[string]any{"clearance": int64(9)}})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Enumerate = %#v; no subject's clearance survives the shared layer's "+
			"declaration, so nothing may match on it", got)
	}
}

// ---------------------------------------------------------------------------
// The merge, directly
// ---------------------------------------------------------------------------

// TestMergeAttributeBagsFastPathsStayHonest pins the two shortcuts, because one of
// them stopped being safe. "The local bag is empty" is still a verbatim return —
// there is nothing to suppress. "The SHARED bag is empty" is NOT, once a set is
// declared: that is case (b), and returning the local bag verbatim there is exactly
// the bug.
func TestMergeAttributeBagsFastPathsStayHonest(t *testing.T) {
	shared := Metadata{"department": "eng"}
	local := Metadata{"clearance": int64(9), "team": "atlas"}
	none := declaredKeySet{}
	set := newDeclaredKeySet([]string{"clearance"})

	// No local bag: the shared map itself, not a copy.
	if got := mergeAttributeBags(shared, nil, set); !sameMap(got, shared) {
		t.Errorf("an empty local bag must return the shared map verbatim, got %#v", got)
	}
	// No shared bag and nothing declared: the local map itself, not a copy.
	if got := mergeAttributeBags(nil, local, none); !sameMap(got, local) {
		t.Errorf("with nothing declared, an empty shared bag must return the local map "+
			"verbatim, got %#v", got)
	}
	// No shared bag WITH a declaration: a filtered copy, and the input untouched.
	got := mergeAttributeBags(nil, local, set)
	if _, ok := got["clearance"]; ok {
		t.Error("an empty shared bag took the fast path past a declared set — that is case (b)")
	}
	if got["team"] != "atlas" {
		t.Errorf("merge = %#v; the undeclared remainder must survive", got)
	}
	if local["clearance"] != int64(9) {
		t.Error("the local provider's own cached bag was written through")
	}
}

// sameMap reports whether two Metadata values are the SAME map, not merely equal.
// The verbatim fast paths are an allocation contract as much as a value one.
func sameMap(a, b Metadata) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
