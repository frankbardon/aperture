package seed

import (
	"context"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// The kind: sql arm, end to end at the seed layer.
//
// Everything here runs against the package's fake database/sql DRIVER, so it
// runs in plain `make test` with no database present — CI has no service
// containers. The live-Postgres proof of the same path is the gated
// TestPostgresIntegration_AttributeProviderServesASlot in
// postgres_integration_test.go; a fake cannot show that pgx links and connects,
// and a live server cannot run in CI, so both exist.

const (
	userFetch = `SELECT department, clearance FROM users WHERE id = $1`
	userList  = `SELECT u.id AS id, u.department, u.clearance FROM users u`
	acctFetch = `SELECT plan FROM accounts WHERE id = $1`
)

// userDB is the canned directory both statements read. The id column of the
// get_all result holds BARE keys — "alice", not "user:alice" — which is the
// asymmetry with an object provider's statement, and the fixture is written the
// correct way precisely so the assertions mean something: an identity-shaped
// result would enumerate just as happily and answer nothing.
func userDB() *fakeDB {
	return &fakeDB{tables: map[string]fakeTable{
		userFetch: {
			cols: []string{"department", "clearance"},
			rows: [][]driver.Value{{"eng", int64(3)}},
		},
		userList: {
			cols: []string{"id", "department", "clearance"},
			rows: [][]driver.Value{
				{"alice", "eng", int64(3)},
				{"bob", "sales", int64(1)},
			},
		},
		acctFetch: {
			cols: []string{"plan"},
			rows: [][]driver.Value{{"enterprise"}},
		},
	}}
}

// TestAttributeProviders_SQLServesASlot is the story's headline: a seed file
// ALONE — connections: plus a kind: sql attribute entry — produces a registry
// that answers the fetch the decision path makes and the enumeration the admin
// read makes, with no Go wiring.
//
// It also pins the FIRST contract difference from a providers: entry, which is
// invisible in the statement text and only observable in what was bound: the
// BARE subject id reaches the placeholder verbatim, because a subject key has no
// terminal segment to strip.
func TestAttributeProviders_SQLServesASlot(t *testing.T) {
	ctx := context.Background()
	db := userDB()
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
    get_all: `+userList+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	reg, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	// The decision path's fetch, through the principal resolver seam.
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	// The value model is the shared one, so a typed column arrives typed: an
	// untyped clearance would decide differently against `principal.clearance >= 3`.
	if bag["department"] != "eng" || bag["clearance"] != int64(3) {
		t.Fatalf("bag = %#v; want the columns typed as the driver-value table says", bag)
	}
	// The bare key, verbatim. "alice" — not a stripped segment, and not a
	// composed identity.
	if got := db.boundArgs(); len(got) != 1 || got[0] != any("alice") {
		t.Fatalf("bound %#v; want exactly the bare subject id", got)
	}

	// The admin read enumerates the directory, filtered by the Fields contract,
	// and every row is keyed by its BARE id.
	recs, err := reg.Enumerate(ctx, provider.AttributeSlotUser, provider.AttributeFilter{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		got = append(got, r.ID)
	}
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Enumerate keys = %v; want %v — a bare id, never 'user:' || u.id", got, want)
	}
	if _, leaked := recs[0].Attributes["id"]; leaked {
		t.Error("the id column leaked into the bag; it is the key, not a field")
	}

	filtered, err := reg.Enumerate(ctx, provider.AttributeSlotUser,
		provider.AttributeFilter{Fields: map[string]any{"department": "sales"}})
	if err != nil {
		t.Fatalf("Enumerate(filtered): %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "bob" {
		t.Fatalf("filtered = %+v; want bob alone", filtered)
	}
}

// TestAttributeProviders_SQLRuleReadsTheHostsTable is the effort's promise stated
// as the thing a host actually wants: a rule reads principal.department off the
// host's own users table, and nobody wrote any Go.
//
// It goes through rules.Engine — the same seam internal/cli wires with
// rules.WithPrincipalResolver(attrs) — so the path under test is the production
// one, not a re-implementation of it.
func TestAttributeProviders_SQLRuleReadsTheHostsTable(t *testing.T) {
	ctx := context.Background()
	dsn := newFakeDSN(t, userDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	eng := rules.NewEngine(
		rules.MapSource{
			// alice's row is department eng, clearance 3.
			"engineering": {AST: rules.And(
				rules.Compare(rules.OpEq, rules.Var("principal.department"), rules.Lit("eng")),
				rules.Compare(rules.OpGe, rules.Var("principal.clearance"), rules.Lit(3)),
			)},
			// The same bag, read the other way. Both rules are needed: one that
			// only ever selected would be satisfied by an empty bag just as well,
			// so the pair is what proves the COLUMN VALUES reached the evaluator
			// rather than the rule being trivially true.
			"sales": {AST: rules.Compare(rules.OpEq, rules.Var("principal.department"), rules.Lit("sales"))},
			// The floor is stamped over whatever the directory returned, so a rule
			// can always read principal.id even for a subject with no row.
			"is-alice": {AST: rules.Compare(rules.OpEq, rules.Var("principal.id"), rules.Lit("alice"))},
		},
		nil, // no object metadata: these rules read only the principal
		rules.WithPrincipalResolver(attrs),
	)

	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"engineering", true},
		{"sales", false},
		{"is-alice", true},
	} {
		selected, err := eng.Selected(ctx, tc.rule, noObject, "acme", "user", "alice", "read")
		if err != nil {
			t.Fatalf("Selected(%s): %v", tc.rule, err)
		}
		if selected != tc.want {
			t.Errorf("Selected(%s) = %v, want %v — the host table's columns did not reach the evaluator", tc.rule, selected, tc.want)
		}
	}
}

// TestAttributeProviders_SQLHonoursTheConnectionsBudget: query_timeout is the
// CONNECTION's, and it must reach the statement.
//
// An attribute bag is read by every rule against every object in a decision, so
// an unbounded statement is not one object type answering slowly — it is the
// whole decision hanging while it holds a connection. A budget that was parsed,
// carried, and then dropped by the loader would be a silently ignored knob.
func TestAttributeProviders_SQLHonoursTheConnectionsBudget(t *testing.T) {
	ctx := context.Background()
	db := userDB()
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
    query_timeout: 2s
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	reg, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}
	if _, err := reg.Attributes(ctx, "user", "alice"); err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	budget := db.budget()
	if budget <= 0 || budget > 2*time.Second {
		t.Fatalf("observed statement budget %v; want the connection's 2s", budget)
	}
}

// TestAttributeProviders_SQLSharesTheDocumentsPool: one connections: entry is ONE
// pool, however many providers of either kind name it. A second pool per name
// would double every deployment's connections, and half of them would be held by
// a registry nothing has a handle to close.
func TestAttributeProviders_SQLSharesTheDocumentsPool(t *testing.T) {
	ctx := context.Background()
	db := userDB()
	for stmt, tbl := range brandDB().tables {
		db.tables[stmt] = tbl
	}
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: `+brandFetch+`
    get_all: `+brandList+`
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
  - subject: account
    kind: sql
    connection: main
    get_one: `+acctFetch+`
`)
	objects, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}
	if got := opener.callsFor("main"); got != 1 {
		t.Fatalf("the opener ran %d times for \"main\"; want exactly 1 — one object provider and two attribute slots must SHARE the pool", got)
	}
	if len(objects.Keys()) != 1 {
		t.Fatalf("object registry has %d types, want 1", len(objects.Keys()))
	}
	// Both registries answer through that one pool.
	if _, err := attrs.Attributes(ctx, "user", "alice"); err != nil {
		t.Fatalf("Attributes(user): %v", err)
	}
	acct, err := attrs.AccountAttributes(ctx, "acme")
	if err != nil {
		t.Fatalf("AccountAttributes: %v", err)
	}
	if acct["plan"] != "enterprise" {
		t.Errorf("plan = %#v; want enterprise", acct["plan"])
	}
}

// TestAttributeProviders_SQLFetchOnlySlotDecidesButDoesNotEnumerate is the
// asymmetry with providers: proved against the REAL loader rather than a stub.
//
// Omitting get_all yields a fetch-only slot: every decision path works unchanged,
// and only List/Query refuse — with a code, not an empty page. It is a feature,
// not a tolerated gap: it lets a host expose the attributes of the principal
// currently being decided about without exposing its whole user table to an
// admin enumeration.
func TestAttributeProviders_SQLFetchOnlySlotDecidesButDoesNotEnumerate(t *testing.T) {
	ctx := context.Background()
	dsn := newFakeDSN(t, userDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	reg, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if bag["department"] != "eng" {
		t.Errorf("department = %#v; want eng", bag["department"])
	}
	_, err = reg.Enumerate(ctx, provider.AttributeSlotUser, provider.AttributeFilter{})
	if err == nil {
		t.Fatal("a fetch-only slot enumerated")
	}
	if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s; want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "get_all") {
		t.Errorf("the refusal does not name the statement to declare: %v", err)
	}
}

// TestAttributeProviders_SQLQueryFailureIsCodedNotSwallowed: a database that
// cannot answer must not read as "this subject has no attributes".
//
// The two mean opposite things for a decision — an empty bag decides against the
// floor and denies as though on purpose, while a coded operational error tells
// the operator the directory is down — so the failure surfaces with the code and
// the driver's cause intact.
func TestAttributeProviders_SQLQueryFailureIsCodedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	// A database with NO canned result for the statement: the fake driver refuses
	// it, which is a driver failure exactly as an unreachable server is.
	dsn := newFakeDSN(t, &fakeDB{tables: map[string]fakeTable{}})
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+userFetch+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	reg, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	bag, err := reg.Attributes(ctx, "user", "alice")
	if err == nil {
		t.Fatalf("a failing statement produced a bag %#v instead of an error", bag)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_QUERY {
		t.Fatalf("code = %s; want APERTURE_SQL_PROVIDER_QUERY", got)
	}
	if !strings.Contains(err.Error(), "no canned result") {
		t.Errorf("the driver's cause was not wrapped verbatim: %v", err)
	}
}

// noObject is the empty identity a principal-only rule is evaluated against. The
// rule under test reads nothing off `object`, so there is nothing for a fetcher
// to supply — and the engine is wired with a nil MetadataFetcher to match, which
// makes `object` an empty bag rather than a failed lookup.
var noObject = identity.Identity{}

// The statements for the narrower-get_all case below. get_one projects four
// columns and get_all projects two, which is the pairing the loaders' own
// contract permits (AttributeConfig.ListQuery is OPTIONAL and is only required to
// select a bare id) and the pairing the gated live-Postgres test uses.
const (
	wideFetch   = `SELECT department, clearance, to_jsonb(teams) AS teams, hired_on::text AS hired_on FROM users WHERE id = $1`
	narrowList  = `SELECT u.id AS id, u.department, u.clearance FROM users u`
	teamsColumn = `["platform","oncall"]`
)

// TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag is E3-S5's
// regression, at the seed layer and over the FAKE driver, so plain `make test`
// catches it with no database present.
//
// It is the gated TestPostgresIntegration_AttributeProviderServesASlot's exact
// shape and exact order: an operator's admin listing runs, and only then does a
// decision. The listing's projection has no `teams` column; the decision's rule
// asks about `principal.teams`. Before the fix the registry warmed the slot's
// FETCH cache from the listing's bags, so the decision read a bag with `teams`
// absent and `hasAny` went false — a verdict flipped by an administrative read,
// for the whole of the slot's ttl, with nothing anywhere to say so.
//
// The three predicates are deliberate. Two of them (a string equality and a
// numeric >=) survive the narrowing because the listing happens to carry those
// two columns; only the third fails. A single-predicate rule would have made the
// bug look like a total outage, which is the thing that would have been noticed.
func TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag(t *testing.T) {
	ctx := context.Background()
	db := &fakeDB{tables: map[string]fakeTable{
		wideFetch: {
			cols: []string{"department", "clearance", "teams", "hired_on"},
			// teams arrives as jsonb, which reaches the value model as []byte and
			// decodes to a LIST — the to_jsonb cast the package doc requires.
			rows: [][]driver.Value{{"eng", int64(3), []byte(teamsColumn), "2024-03-04"}},
		},
		narrowList: {
			cols: []string{"id", "department", "clearance"},
			rows: [][]driver.Value{
				{"alice", "eng", int64(3)},
				{"bob", "sales", int64(1)},
			},
		},
	}}
	dsn := newFakeDSN(t, db)
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := attributeDoc(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: `+wideFetch+`
    get_all: `+narrowList+`
`)
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	// The operator's system-tier admin read, first. This is the only step the bug
	// needed, and it is one an operator is meant to be able to run at any time.
	if _, err := attrs.Enumerate(ctx, provider.AttributeSlotUser, provider.AttributeFilter{}); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	// The decision path's bag, AFTER the listing. Every column get_one projects is
	// still there, with the Go type the driver-value table promises.
	bag, err := attrs.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	if got, ok := bag["department"].(string); !ok || got != "eng" {
		t.Errorf("department = %#v (%T), want the string \"eng\"", bag["department"], bag["department"])
	}
	if got, ok := bag["clearance"].(int64); !ok || got != 3 {
		t.Errorf("clearance = %#v (%T), want int64(3)", bag["clearance"], bag["clearance"])
	}
	if got, want := bag["teams"], []any{"platform", "oncall"}; !reflect.DeepEqual(got, want) {
		t.Errorf("teams = %#v, want %#v — a listing's projection reached the decision path", got, want)
	}
	if got, ok := bag["hired_on"].(string); !ok || got != "2024-03-04" {
		t.Errorf("hired_on = %#v (%T), want the string \"2024-03-04\"", bag["hired_on"], bag["hired_on"])
	}

	// And the verdict, through the production seam.
	eng := rules.NewEngine(
		rules.MapSource{
			"engineering": {AST: rules.And(
				rules.Compare(rules.OpEq, rules.Var("principal.department"), rules.Lit("eng")),
				rules.Compare(rules.OpGe, rules.Var("principal.clearance"), rules.Lit(3)),
				rules.Compare(rules.OpHasAny, rules.Var("principal.teams"), rules.List(rules.Lit("oncall"))),
			)},
			// The two rules that must be FALSE. A membership predicate over an
			// absent collection is false, so `engineering` selecting proves
			// nothing on its own — a rule that cannot deny is not evidence of
			// anything. `finance` asks the same question of the same key and must
			// deny, which is what makes the true verdict above a read of the
			// bag's real contents rather than of a shape.
			"finance": {AST: rules.Compare(rules.OpHasAny,
				rules.Var("principal.teams"), rules.List(rules.Lit("finance")))},
			"sales": {AST: rules.Compare(rules.OpEq,
				rules.Var("principal.department"), rules.Lit("sales"))},
		},
		nil, // the rules read only the principal
		rules.WithPrincipalResolver(attrs),
	)
	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"engineering", true},
		{"finance", false},
		{"sales", false},
	} {
		selected, err := eng.Selected(ctx, tc.rule, noObject, "acme", "user", "alice", "read")
		if err != nil {
			t.Fatalf("Selected(%s): %v", tc.rule, err)
		}
		if selected != tc.want {
			t.Errorf("Selected(%s) = %v, want %v — an admin enumeration changed a decision", tc.rule, selected, tc.want)
		}
	}
}
