package seed

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/rules"
)

// E3-S6's regression at the seed layer and over the FAKE driver, so plain
// `make test` catches it with no database present.
//
// It is the object-side mirror of
// TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag, and the same
// order is what makes it a regression test rather than a wiring test: an
// enumeration runs, and only then does a rule read the object it enumerated. Before
// the fix provider.Registry warmed its per-type metadata cache from every listing
// unconditionally, so the listing's projection became the decision path's view of
// the object for the whole of the type's TTL.
//
// The statements below are a legal pair — Config.FetchQuery and Config.ListQuery are
// independent and nothing makes their SELECT lists agree — and this is the shape a
// host writes on purpose: a wide get_one for decisions and a narrow get_all for a
// listing over a wide table.
const (
	narrowObjFetch = `SELECT tier, seats, classification FROM brands WHERE id = $1`
	narrowObjList  = `SELECT 'brand:' || id AS id, tier FROM brands`
)

func TestBuildRegistry_SQLAnEnumerationDoesNotNarrowTheObjectBag(t *testing.T) {
	ctx := context.Background()
	db := &fakeDB{tables: map[string]fakeTable{
		narrowObjFetch: {
			cols: []string{"tier", "seats", "classification"},
			rows: [][]driver.Value{{"gold", int64(12), "public"}},
		},
		narrowObjList: {
			cols: []string{"id", "tier"},
			rows: [][]driver.Value{
				{"brand:1", "gold"},
				{"brand:2", "silver"},
			},
		},
	}}
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc, err := Parse([]byte(`
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: "`+narrowObjFetch+`"
    get_all: "`+narrowObjList+`"
    ttl: "0"
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	// One fetch of a DIFFERENT object first, so the provider has observed its own
	// get_one projection. Without it the refusal to warm would be an "I do not know
	// these statements yet" rather than the comparison this test is about.
	if _, err := reg.Fetch(ctx, identity.MustParse("brand:2")); err != nil {
		t.Fatalf("priming Fetch: %v", err)
	}

	// The enumeration a rule-backed inclusive scope runs to gather its candidates.
	ids, err := reg.List(ctx, "brand", identity.MustParsePattern("brand:*"), 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("List returned %d ids, want 2", len(ids))
	}

	// The decision path's bag, AFTER the listing. Every column get_one projects is
	// still there, with the Go type the driver-value mapping table promises.
	md, err := reg.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch after List: %v", err)
	}
	if got, ok := md["tier"].(string); !ok || got != "gold" {
		t.Errorf("tier = %#v (%T), want the string \"gold\"", md["tier"], md["tier"])
	}
	if got, ok := md["seats"].(int64); !ok || got != 12 {
		t.Errorf("seats = %#v (%T), want int64(12) — a listing's projection reached "+
			"the decision path", md["seats"], md["seats"])
	}
	if got, ok := md["classification"].(string); !ok || got != "public" {
		t.Errorf("classification = %#v (%T), want the string \"public\" — the get_all "+
			"does not project it, so this is the field a listing drops",
			md["classification"], md["classification"])
	}

	// And the verdict, through the production seam: the registry is the rules
	// engine's metadata source, exactly as engine.WithMetadata wires it.
	eng := rules.NewEngine(rules.MapSource{
		// The rule that reads what the listing dropped. It must still select.
		"public-brands": {AST: rules.Compare(rules.OpEq,
			rules.Var("object.classification"), rules.Lit("public"))},
		// Two rules that must be FALSE. An EQUALITY against an absent field is
		// false, so "public-brands" selecting proves nothing on its own — a rule
		// that cannot deny is not evidence of anything. These ask the same
		// questions of the same bag and must deny. Both are equalities on purpose:
		// an ORDERING comparison against an absent field is an APERTURE_RULE_EVAL
		// error rather than a false, which would make the assertion about the
		// evaluator's nil handling instead of about the bag.
		"secret-brands": {AST: rules.Compare(rules.OpEq,
			rules.Var("object.classification"), rules.Lit("secret"))},
		"tiny-brands": {AST: rules.Compare(rules.OpEq,
			rules.Var("object.seats"), rules.Lit(3))},
		// And one the narrowing leaves intact, because the listing happens to carry
		// its column. It is what makes the bug look like a rule problem rather than
		// an outage: most predicates keep working.
		"gold-brands": {AST: rules.Compare(rules.OpEq,
			rules.Var("object.tier"), rules.Lit("gold"))},
	}, reg)

	brand := identity.MustParse("brand:1")
	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"public-brands", true},
		{"secret-brands", false},
		{"tiny-brands", false},
		{"gold-brands", true},
	} {
		selected, err := eng.Selected(ctx, tc.rule, brand, "acme", "user", "alice", "read")
		if err != nil {
			t.Fatalf("Selected(%s): %v", tc.rule, err)
		}
		if selected != tc.want {
			t.Errorf("Selected(%s) = %v, want %v — an enumeration changed what a rule "+
				"reads off the object it enumerated", tc.rule, selected, tc.want)
		}
	}
}
