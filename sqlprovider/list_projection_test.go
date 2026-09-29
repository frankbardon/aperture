package sqlprovider

import (
	"context"
	"database/sql/driver"
	"reflect"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// E3-S6, the sqlprovider half: whether a listing may warm the Registry's per-type
// metadata cache is DERIVED from the two statements' real column projections, never
// declared.
//
// The hazard is that Config carries two independent statements and nothing makes
// their SELECT lists agree. A get_all narrower than its get_one caches a bag with a
// field missing, and an absent field makes every predicate over it false — which
// denies under an inclusive grant and stops EXCLUDING under an exclusive one, for
// the whole of the type's TTL, with nothing in any verdict to say so.

// narrowScript is a correctly-wired-but-narrower pair: get_one projects three
// fields, get_all projects the id plus one of them. Both statements are legal and
// the pairing is one a host writes deliberately (a display projection over a wide
// table).
func narrowScript() *script {
	return &script{
		cols: []string{"id", "tier"},
		rows: [][]driver.Value{
			{"brand:1", "gold"},
			{"brand:2", "silver"},
		},
		fetchCols: []string{"tier", "seats", "renews_on"},
		fetchRows: [][]driver.Value{{"gold", int64(12), "2026-03-01"}},
	}
}

// A get_all narrower than its get_one must not warm the cache, so the Fetch that
// follows it inside the same candidate walk still returns every field the fetch
// statement projects.
//
// With the fix reverted this fails on seats and renews_on: both come back ABSENT,
// which is the shape that silently changes a verdict.
func TestAListingWithANarrowerProjectionDoesNotWarm(t *testing.T) {
	ctx := context.Background()
	s := narrowScript()
	p := newProvider(t, s, Config{})
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	// The fetch projection observed first, so the provider genuinely KNOWS both and
	// the false answer is a comparison rather than an "I do not know yet".
	if _, err := reg.Fetch(ctx, identity.MustParse("brand:1")); err != nil {
		t.Fatalf("priming Fetch: %v", err)
	}
	if p.ListedMetadataMatchesFetch() {
		t.Fatal("the provider promises the two bags match before the list statement has run")
	}
	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if p.ListedMetadataMatchesFetch() {
		fetch, list := p.observedProjections()
		t.Fatalf("the provider promises a narrower get_all matches its get_one: "+
			"fetch projects %v, the listing projects %v", fetch, list)
	}
	if st, _ := reg.Stats("brand"); st.Entries != 1 {
		// Exactly the one entry the priming Fetch wrote: the listing added none.
		t.Fatalf("cache holds %d entries after the listing, want only the 1 the "+
			"priming Fetch wrote", st.Entries)
	}

	if _, err := reg.Invalidate(identity.MustParse("brand:1")); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("second Identifiers: %v", err)
	}
	md, err := reg.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch after Identifiers: %v", err)
	}
	want := provider.Metadata{"tier": "gold", "seats": int64(12), "renews_on": "2026-03-01"}
	if !reflect.DeepEqual(md, want) {
		t.Fatalf("Fetch after a narrower listing returned %#v, want the fetch "+
			"statement's own bag %#v — a listing's projection reached the decision path", md, want)
	}
}

// Equal projections keep the warm, which is the half the enumeration NFR rests on:
// Registry.List is a decision-path call and a Fetch of every candidate follows it in
// the same walk.
func TestAListingWithTheSameProjectionStillWarms(t *testing.T) {
	ctx := context.Background()
	s := narrowScript()
	// The same three fields on both sides, the id column aside.
	s.cols = []string{"id", "tier", "seats", "renews_on"}
	s.rows = [][]driver.Value{
		{"brand:1", "gold", int64(12), "2026-03-01"},
		{"brand:2", "silver", int64(3), "2026-09-15"},
	}
	p := newProvider(t, s, Config{})
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	if _, err := reg.Fetch(ctx, identity.MustParse("brand:1")); err != nil {
		t.Fatalf("priming Fetch: %v", err)
	}
	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if !p.ListedMetadataMatchesFetch() {
		fetch, list := p.observedProjections()
		t.Fatalf("equal projections are not recognised: fetch %v, listing %v", fetch, list)
	}
	calls, _, _ := s.observed()
	md, err := reg.Fetch(ctx, identity.MustParse("brand:2"))
	if err != nil {
		t.Fatalf("Fetch of a listed object: %v", err)
	}
	if after, _, _ := s.observed(); after != calls {
		t.Fatalf("Fetch ran %d extra statement(s): an equally-projected listing must "+
			"still warm, or every enumerated candidate pays its own round trip", after-calls)
	}
	if md["seats"] != int64(3) {
		t.Fatalf("metadata = %#v", md)
	}
}

// A get_all WIDER than its get_one is refused too, which is why the comparison is
// equality rather than containment: a field the fetch statement never projects would
// compare true for as long as the warmed entry lives and false afterwards.
func TestAListingWithAWiderProjectionDoesNotWarm(t *testing.T) {
	ctx := context.Background()
	s := &script{
		cols: []string{"id", "tier", "seats"},
		rows: [][]driver.Value{{"brand:1", "gold", int64(12)}},
		// get_one projects tier alone.
		fetchCols: []string{"tier"},
		fetchRows: [][]driver.Value{{"gold"}},
	}
	p := newProvider(t, s, Config{})
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	if _, err := reg.Fetch(ctx, identity.MustParse("brand:1")); err != nil {
		t.Fatalf("priming Fetch: %v", err)
	}
	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if p.ListedMetadataMatchesFetch() {
		t.Fatal("a wider get_all is reported as matching its get_one")
	}
	if _, err := reg.Invalidate(identity.MustParse("brand:1")); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("second Identifiers: %v", err)
	}
	md, err := reg.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, present := md["seats"]; present {
		t.Errorf("seats = %#v, but the fetch statement projects no such column: a "+
			"listing invented a field the decision path then compared against", got)
	}
}

// Until both statements have run the answer is false, because a projection nobody
// has seen is not a projection anything may be assumed about. Order matters here:
// the listing runs FIRST, which is the ordinary shape of an enumeration in a cold
// process.
func TestAnUnobservedProjectionIsNotAPromise(t *testing.T) {
	ctx := context.Background()
	s := narrowScript()
	s.cols = []string{"id", "tier", "seats", "renews_on"}
	s.rows = [][]driver.Value{{"brand:1", "gold", int64(12), "2026-03-01"}}
	p := newProvider(t, s, Config{})

	if p.ListedMetadataMatchesFetch() {
		t.Fatal("a provider that has run no statement at all already promises")
	}
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	if _, err := reg.Identifiers(ctx, "brand"); err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if p.ListedMetadataMatchesFetch() {
		t.Fatal("the fetch projection is unknown, yet the provider promises the two match")
	}
	if st, _ := reg.Stats("brand"); st.Entries != 0 {
		t.Fatalf("the first, cold listing warmed %d entries", st.Entries)
	}

	// Which is the documented cost: the cold enumeration's candidates each fetch,
	// and that is what teaches the fetch projection. Every later enumeration warms.
	if _, err := reg.Fetch(ctx, identity.MustParse("brand:1")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !p.ListedMetadataMatchesFetch() {
		fetch, list := p.observedProjections()
		t.Fatalf("both statements have now run and their projections agree, but the "+
			"provider still refuses: fetch %v, listing %v", fetch, list)
	}
}

// A fetch that finds NOTHING still teaches the projection, because a column list is
// a property of the statement rather than of any row. It is worth pinning: the
// alternative — learning only from a row — would leave a type whose first lookup
// misses unable to warm for no reason a reader could work out.
func TestAFetchThatFindsNothingStillTeachesItsProjection(t *testing.T) {
	ctx := context.Background()
	s := &script{
		cols:      []string{"id", "tier"},
		rows:      [][]driver.Value{{"brand:1", "gold"}},
		fetchCols: []string{"tier"},
		fetchRows: nil, // no rows: APERTURE_NOT_FOUND
	}
	p := newProvider(t, s, Config{})
	if _, err := p.Fetch(ctx, identity.MustParse("brand:missing")); err == nil {
		t.Fatal("a fetch with no rows succeeded")
	}
	fetch, _ := p.observedProjections()
	if !reflect.DeepEqual(fetch, []string{"tier"}) {
		t.Fatalf("observed fetch projection = %v, want [tier] from a statement that returned no rows", fetch)
	}
	if _, err := p.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !p.ListedMetadataMatchesFetch() {
		t.Fatal("the projections agree but the provider does not say so")
	}
}

// The comparison is a SET comparison, not a SELECT-list-order one: two statements
// that project the same fields in a different order describe the same bag.
func TestTheProjectionComparisonIgnoresColumnOrder(t *testing.T) {
	ctx := context.Background()
	s := &script{
		cols:      []string{"seats", "id", "tier"},
		rows:      [][]driver.Value{{int64(12), "brand:1", "gold"}},
		fetchCols: []string{"tier", "seats"},
		fetchRows: [][]driver.Value{{"gold", int64(12)}},
	}
	p := newProvider(t, s, Config{})
	if _, err := p.Fetch(ctx, identity.MustParse("brand:1")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := p.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !p.ListedMetadataMatchesFetch() {
		fetch, list := p.observedProjections()
		t.Fatalf("the same fields in a different SELECT order are reported as "+
			"different projections: fetch %v, listing %v", fetch, list)
	}
}
