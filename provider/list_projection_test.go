package provider

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// E3-S6: an object listing cannot poison the per-type object metadata cache.
//
// Registry.List and Registry.Identifiers warm the cache Fetch reads. Nothing in
// ObjectProvider makes Query's bag and Fetch's bag equal, and sqlprovider's two
// independent statements make the inequality legal, so an unconditional warm
// substitutes the LISTING's projection for the authoritative one for the whole of
// the type's TTL. The fix is provider.FetchCompleteLister: a provider that does not
// promise the two bags are the same bag gets no warm.
//
// The tests below are paired on purpose. The narrowing and widening cases are the
// bug; the declared case is the NFR half, and without it "stop warming
// unconditionally" would pass every correctness test in this file while putting a
// round trip per candidate into engine.walkAllowed.

// projectingProvider is a host ObjectProvider whose LISTING bag is deliberately a
// different projection of the same object from its FETCH bag — the shape two
// independent SQL statements produce. It counts fetches so a test can tell a cache
// hit from a provider call, and whether it makes the FetchCompleteLister promise is
// a field rather than a type, so one fixture covers both sides of the condition.
type projectingProvider struct {
	ids []identity.Identity
	// fetched is the authoritative bag Fetch returns for every id.
	fetched Metadata
	// listed is the bag List/Query return for every id.
	listed  Metadata
	fetches int64 // atomic
}

func (p *projectingProvider) Fetch(_ context.Context, id identity.Identity) (Metadata, error) {
	atomic.AddInt64(&p.fetches, 1)
	for _, known := range p.ids {
		if known.String() == id.String() {
			return p.fetched, nil
		}
	}
	return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND, "no such object",
		map[string]any{"id": id.String()})
}

func (p *projectingProvider) List(ctx context.Context) ([]Object, error) {
	return p.Query(ctx, Filter{})
}

func (p *projectingProvider) Query(_ context.Context, f Filter) ([]Object, error) {
	out := make([]Object, 0, len(p.ids))
	for _, id := range p.ids {
		if f.Pattern != nil && !f.Pattern.Matches(id) {
			continue
		}
		out = append(out, Object{ID: id, Metadata: p.listed})
	}
	return out, nil
}

func (p *projectingProvider) fetchCount() int64 { return atomic.LoadInt64(&p.fetches) }

// promisingProvider is projectingProvider plus the FetchCompleteLister promise. It
// is a separate type rather than a bool field because the Registry decides by TYPE
// ASSERTION, and a bool would have tested a branch the production code does not
// take.
type promisingProvider struct{ projectingProvider }

func (p *promisingProvider) ListedMetadataMatchesFetch() bool { return true }

var _ FetchCompleteLister = (*promisingProvider)(nil)

func projectionFixture(count int) ([]identity.Identity, Metadata, Metadata) {
	ids := make([]identity.Identity, 0, count)
	for i := 1; i <= count; i++ {
		ids = append(ids, identity.MustParse(fmt.Sprintf("account:acme/document:%d", i)))
	}
	// The authoritative bag a get_one projects, and the narrower bag a get_all
	// projects. "classification" is the field a rule reads and the listing drops.
	return ids,
		Metadata{"title": "Q3 plan", "classification": "restricted", "seats": int64(12)},
		Metadata{"title": "Q3 plan"}
}

// A listing through a provider that makes no promise must leave the cache alone, so
// the Fetch that follows it inside the same candidate walk still returns the
// authoritative bag.
//
// Reverting the fix (warming unconditionally again) fails this in the assertion
// that matters: md["classification"] comes back absent, which is the shape that
// silently denies an inclusive grant and silently WIDENS an exclusive one.
func TestAListingDoesNotNarrowTheObjectBag(t *testing.T) {
	ctx := context.Background()
	ids, fetched, listed := projectionFixture(3)
	p := &projectingProvider{ids: ids, fetched: fetched, listed: listed}

	reg := NewRegistry()
	reg.MustRegister("document", p)

	pat := identity.MustParsePattern("account:acme/document:*")
	got, err := reg.List(ctx, "document", pat, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d ids, want 3", len(got))
	}

	// The cache holds NOTHING. Asserted as an entry count as well as through a
	// value, so a reintroduced warm fails even if this fixture were one day
	// changed to make the two bags agree.
	if s, _ := reg.Stats("document"); s.Entries != 0 {
		t.Errorf("the listing wrote %d cache entries; a provider that makes no "+
			"FetchCompleteLister promise must warm nothing", s.Entries)
	}

	md, err := reg.Fetch(ctx, ids[0])
	if err != nil {
		t.Fatalf("Fetch after List: %v", err)
	}
	if p.fetchCount() != 1 {
		t.Errorf("provider fetches = %d, want 1: the Fetch was served from a "+
			"listing-warmed entry", p.fetchCount())
	}
	if !reflect.DeepEqual(md, fetched) {
		t.Fatalf("Fetch after List returned %#v, want the authoritative bag %#v", md, fetched)
	}
	if _, ok := md["classification"]; !ok {
		t.Error("classification is ABSENT after a listing: every predicate over it " +
			"is now false, which denies an inclusive grant and stops an exclusive one excluding")
	}
}

// Identifiers is the other warming path — the unbounded enumeration an exclusive
// allowance expands through — and it is gated on the same promise. Tested
// separately because it is a separate loop over a separate provider method, and one
// of the two having been fixed is exactly how this bug survives.
func TestAnUnboundedListingDoesNotNarrowTheObjectBagEither(t *testing.T) {
	ctx := context.Background()
	ids, fetched, listed := projectionFixture(2)
	p := &projectingProvider{ids: ids, fetched: fetched, listed: listed}

	reg := NewRegistry()
	reg.MustRegister("document", p)

	if _, err := reg.Identifiers(ctx, "document"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if s, _ := reg.Stats("document"); s.Entries != 0 {
		t.Errorf("Identifiers wrote %d cache entries; it warms on exactly the same "+
			"condition List does", s.Entries)
	}
	md, err := reg.Fetch(ctx, ids[1])
	if err != nil {
		t.Fatalf("Fetch after Identifiers: %v", err)
	}
	if _, ok := md["classification"]; !ok {
		t.Errorf("classification is absent after Identifiers: bag = %#v", md)
	}
}

// The mirror image, and the reason the promise is EQUALITY rather than
// containment: a listing that carries a field the fetch statement does not project
// would cache a value Fetch never produces, so a predicate over it is true until
// the entry expires and false afterwards. That widens access, which is the
// direction that matters.
func TestAWiderListingDoesNotInventAField(t *testing.T) {
	ctx := context.Background()
	ids, _, _ := projectionFixture(1)
	p := &projectingProvider{
		ids:     ids,
		fetched: Metadata{"title": "Q3 plan"},
		listed:  Metadata{"title": "Q3 plan", "classification": "public"},
	}

	reg := NewRegistry()
	reg.MustRegister("document", p)

	if _, err := reg.Identifiers(ctx, "document"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	md, err := reg.Fetch(ctx, ids[0])
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, present := md["classification"]; present {
		t.Errorf("classification = %#v after a listing, but the fetch statement "+
			"projects no such field: a listing-warmed entry compares true for as "+
			"long as it lives", got)
	}
}

// The NFR half, and the reason the fix is a CONDITION rather than a removal.
// Registry.List is a decision-path call: a rule-backed inclusive scope lists its
// candidates and then Fetches every one of them inside the same candidate walk
// (engine.walkAllowed). A provider that promises the two bags are the same bag must
// still warm, or every candidate pays a provider round trip and
// bench.TestCheckNFREnumerateBound is measuring a different shape.
//
// Removing the warm outright passes every test above and fails this one.
func TestADeclaredFetchCompleteListerStillWarmsTheCache(t *testing.T) {
	ctx := context.Background()
	ids, fetched, _ := projectionFixture(3)
	p := &promisingProvider{projectingProvider{ids: ids, fetched: fetched, listed: fetched}}

	reg := NewRegistry()
	reg.MustRegister("document", p)

	pat := identity.MustParsePattern("account:acme/document:*")
	if _, err := reg.List(ctx, "document", pat, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
	if s, _ := reg.Stats("document"); s.Entries != 3 {
		t.Fatalf("the listing wrote %d cache entries, want 3: a promised listing "+
			"must still warm, or every enumerated candidate pays its own round trip", s.Entries)
	}
	for _, id := range ids {
		md, err := reg.Fetch(ctx, id)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", id, err)
		}
		if !reflect.DeepEqual(md, fetched) {
			t.Fatalf("Fetch(%s) = %#v, want %#v", id, md, fetched)
		}
	}
	if p.fetchCount() != 0 {
		t.Errorf("provider fetches = %d, want 0: the listing's warm was not used", p.fetchCount())
	}
}

// The promise is re-read per enumeration rather than settled at registration,
// because sqlprovider only learns its own projections by running both statements.
// A provider that starts out unable to answer and later can must start warming
// without being re-registered.
func TestThePromiseIsReReadOnEveryEnumeration(t *testing.T) {
	ctx := context.Background()
	ids, fetched, _ := projectionFixture(2)
	p := &learningProvider{projectingProvider: projectingProvider{
		ids: ids, fetched: fetched, listed: fetched,
	}}

	reg := NewRegistry()
	reg.MustRegister("document", p)

	if _, err := reg.Identifiers(ctx, "document"); err != nil {
		t.Fatalf("first Identifiers: %v", err)
	}
	if s, _ := reg.Stats("document"); s.Entries != 0 {
		t.Fatalf("a provider that cannot yet answer warmed %d entries", s.Entries)
	}

	p.knows.Store(true)
	if _, err := reg.Identifiers(ctx, "document"); err != nil {
		t.Fatalf("second Identifiers: %v", err)
	}
	if s, _ := reg.Stats("document"); s.Entries != 2 {
		t.Fatalf("the listing wrote %d cache entries once the provider could "+
			"answer, want 2: the answer was cached at registration", s.Entries)
	}
}

// learningProvider answers the promise only once it has been told it knows its own
// shape — sqlprovider's real behaviour, which is false until both statements have
// executed once.
type learningProvider struct {
	projectingProvider
	knows atomic.Bool
}

func (p *learningProvider) ListedMetadataMatchesFetch() bool { return p.knows.Load() }

var _ FetchCompleteLister = (*learningProvider)(nil)

// Static and csvprovider promise unconditionally because all three of their methods
// serve the same map per object. Static is asserted here (csvprovider cannot be, as
// this package is a strict leaf and importing it would be an import cycle), because
// it is what the seed document's inline objects: section and most of the repository's
// fixtures are built from — including the enumeration NFR benchmark's.
func TestStaticPromisesItsListingMatchesItsFetch(t *testing.T) {
	ctx := context.Background()
	id := identity.MustParse("account:acme/document:1")
	s, err := NewStatic([]Object{{ID: id, Metadata: Metadata{"classification": "public"}}})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	if !s.ListedMetadataMatchesFetch() {
		t.Fatal("Static does not promise its listing matches its fetch; it serves one map per object to all three methods")
	}

	reg := NewRegistry()
	reg.MustRegister("document", s)
	if _, err := reg.Identifiers(ctx, "document"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if st, _ := reg.Stats("document"); st.Entries != 1 {
		t.Fatalf("a Static listing wrote %d cache entries, want 1", st.Entries)
	}
}
