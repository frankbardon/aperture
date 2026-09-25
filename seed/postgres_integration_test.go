package seed

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// The one test in this repo that talks to a real Postgres.
//
// It is GATED, and it must stay gated: CI runs on ubuntu with no service
// containers, so `make test` has to pass with no database present. The gate
// mirrors the bench NFR convention (APERTURE_BENCH_ASSERT=1):
//
//	APERTURE_PG_INTEGRATION=1 \
//	APERTURE_PG_DSN='postgres://aperture:aperture@localhost:5432/aperture?sslmode=disable' \
//	go test -run TestPostgresIntegration ./seed/
//
// Ungated it SKIPS. Gated without a DSN it FAILS — asking for the integration
// test and silently not getting one is the outcome a gate must never produce.
//
// It exists because everything else about the SQL path is proved against a fake
// driver, and a fake cannot prove the two things that are only true of the real
// one: that pgx is linked into a CGO_ENABLED=0 binary and actually connects, and
// that a real Postgres result set lands in the value model as the mapping table
// in sqlprovider/values.go claims. E2-S1 could not write this test — that story
// forbade the driver import — and this is the story that takes the driver on.
const (
	pgGateEnv = "APERTURE_PG_INTEGRATION"
	pgDSNEnv  = "APERTURE_PG_DSN"
)

// requirePostgres skips unless the gate is set, and returns the DSN.
func requirePostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv(pgGateEnv) != "1" {
		t.Skipf("skipping: set %s=1 and %s=<dsn> to run the real-Postgres integration test", pgGateEnv, pgDSNEnv)
	}
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Fatalf("%s=1 but %s is empty: the gate is on and there is no database to run against", pgGateEnv, pgDSNEnv)
	}
	return dsn
}

// TestPostgresIntegration_SeedFileAloneServesRealObjects drives the whole story
// end to end against a live server: a YAML document with a connections: block
// and a kind: sql provider entry, the DEFAULT opener (a real pgx pool), and no
// Go wiring beyond reading the file.
func TestPostgresIntegration_SeedFileAloneServesRealObjects(t *testing.T) {
	dsn := requirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A scratch table, dropped on the way out, so the test leaves no residue in
	// whatever database the operator pointed it at.
	table := fmt.Sprintf("aperture_e2e_%d", time.Now().UnixNano())
	admin, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// The pool is closed by a CLEANUP, not by a defer. Cleanups run after the
	// deferred calls of the test function and in reverse registration order, so a
	// `defer admin.Close()` closes the pool BEFORE the DROP below ever runs — the
	// DROP then fails with sql.ErrConnDone, the `_, _ =` swallows it, and the
	// scratch table survives every run. That is exactly what happened: this test
	// left one table (and its index) behind per run in whatever database it was
	// pointed at. Registering Close FIRST makes it run LAST.
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pgDSNEnv, err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id text PRIMARY KEY, tier text NOT NULL, seats int NOT NULL, renews_on date NOT NULL)`,
		table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		// Reported, not discarded. A cleanup that cannot fail is a cleanup nobody
		// finds out has stopped working.
		if _, err := admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			t.Errorf("the scratch table %s was not dropped: %v — this test must leave no residue in "+
				"the database the operator pointed it at", table, err)
		}
	})
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s VALUES ('1','gold',12,'2026-03-01'), ('2','silver',3,'2026-09-15')`, table)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Setenv(pgDSNEnv, dsn)
	doc, err := Parse([]byte(fmt.Sprintf(`
connections:
  main:
    dsn_env: %s
    max_open_conns: 4
    query_timeout: 10s
providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: "SELECT tier, seats, renews_on FROM %s WHERE id = $1"
    get_all: "SELECT 'brand:' || id AS id, tier, seats, renews_on FROM %s"
    ttl: "0"
`, pgDSNEnv, table, table)), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// No WithConnectionOpener: this is the DEFAULT pgx path.
	reg, conns, err := doc.BuildRegistryWithConnections("")
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	if conns.Len() != 1 {
		t.Fatalf("Connections.Len() = %d, want 1", conns.Len())
	}

	md, err := reg.Fetch(ctx, identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The value model, against a real driver: an int column arrives as a Go
	// integer, and a date arrives as the canonical UTC text form — never as a
	// time.Time restated in the reader's zone. sqlprovider always formats a
	// timestamp at datetime granularity, so a DATE column reads back as midnight
	// Z rather than as a bare day.
	//
	// Each assertion pins the Go TYPE with a type assertion, for the reason spelled
	// out at the attribute test below: a fmt.Sprint comparison — which the seats
	// case used to be — cannot tell int64(12) from the string "12", and this test
	// is the only thing in the repo that watches a real driver hand over a real
	// result set. A silent stringification is exactly what it exists to catch.
	if got, ok := md["tier"].(string); !ok || got != "gold" {
		t.Errorf("tier = %#v (%T), want the string \"gold\"", md["tier"], md["tier"])
	}
	if got, ok := md["seats"].(int64); !ok || got != 12 {
		t.Errorf("seats = %#v (%T), want int64(12) — a stringified number compares "+
			"against nothing a rule can write", md["seats"], md["seats"])
	}
	if got, ok := md["renews_on"].(string); !ok || got != "2026-03-01T00:00:00Z" {
		t.Errorf("renews_on = %#v (%T), want the string \"2026-03-01T00:00:00Z\"",
			md["renews_on"], md["renews_on"])
	}

	ids, err := reg.Identifiers(ctx, "brand")
	if err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Identifiers = %v, want 2 objects", ids)
	}

	// An absent object is NOT FOUND, not an operational failure — the distinction
	// the whole error taxonomy rests on.
	if _, err := reg.Fetch(ctx, identity.MustParse("brand:missing")); err == nil {
		t.Error("Fetch of an absent id succeeded")
	}
}

// TestPostgresIntegration_UnsetDSNIsAHardError proves the environment rule
// against the real opener: the build fails at BuildRegistryWithConnections, not
// lazily on the first query.
func TestPostgresIntegration_UnsetDSNIsAHardError(t *testing.T) {
	_ = requirePostgres(t)
	t.Setenv("APERTURE_PG_ABSENT_DSN", "")
	doc := &Document{Connections: map[string]Connection{
		"main": {DSNEnv: "APERTURE_PG_ABSENT_DSN"},
	}}
	if _, conns, err := doc.BuildRegistryWithConnections(""); err == nil {
		_ = conns.Close()
		t.Fatal("build succeeded with an unset DSN environment variable")
	}
}

// TestPostgresIntegration_AttributeProviderServesASlot is E3's headline against a
// live server: a host points Aperture at its existing users table and a rule
// reads principal.department, with no Go written.
//
// It is gated exactly as the object-provider test above is — same environment
// variables, same skip-when-ungated, same FAIL when gated with an empty DSN —
// because it proves the two things a fake driver cannot: that pgx is linked into
// a CGO_ENABLED=0 binary and really connects, and that a real Postgres result set
// lands in the attribute bag as the mapping table in sqlprovider/values.go
// claims. The fake-driver proof of the same wiring is
// TestAttributeProviders_SQLServesASlot.
//
// The get_all statement selects a BARE id — u.id AS id, never 'user:' || u.id —
// which is the contract difference with no error attached: the wrong spelling
// enumerates and caches perfectly happily and then matches no principal id a
// decision ever presents. Writing the fixture the correct way is what makes the
// enumeration assertion mean something.
func TestPostgresIntegration_AttributeProviderServesASlot(t *testing.T) {
	dsn := requirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	table := fmt.Sprintf("aperture_e2e_users_%d", time.Now().UnixNano())
	admin, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Cleanup, never defer: cleanups run AFTER the deferred calls, so a deferred
	// Close would shut the pool before the DROP below could use it and leave one
	// scratch table behind per run. Registering Close FIRST makes it run LAST.
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pgDSNEnv, err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s (
			id text PRIMARY KEY,
			department text NOT NULL,
			clearance int NOT NULL,
			teams text[] NOT NULL,
			hired_on date NOT NULL
		)`, table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		// Reported, not discarded. A cleanup that cannot fail is a cleanup nobody
		// finds out has stopped working.
		if _, err := admin.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			t.Errorf("the scratch table %s was not dropped: %v — this test must leave no residue in "+
				"the database the operator pointed it at", table, err)
		}
	})
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s VALUES
			('alice','eng',3,ARRAY['platform','oncall'],'2024-03-04'),
			('bob','sales',1,ARRAY['crm'],'2023-07-01')`, table)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Setenv(pgDSNEnv, dsn)
	// to_jsonb(teams) is the cast that makes a text[] arrive as a LIST. Selecting
	// it uncast would yield the raw array literal "{platform,oncall}" — a
	// perfectly valid metadata string that every membership predicate silently
	// fails to match — which is the trap sqlprovider's package doc records and
	// cannot detect. hired_on::text is the other half of the same rule: a date
	// and a timestamp are one Go type, so day granularity is the statement's job.
	doc, err := Parse([]byte(fmt.Sprintf(`
connections:
  main:
    dsn_env: %s
    max_open_conns: 4
    query_timeout: 10s
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: "SELECT department, clearance, to_jsonb(teams) AS teams, hired_on::text AS hired_on FROM %s WHERE id = $1"
    get_all: "SELECT u.id AS id, u.department, u.clearance FROM %s u"
    ttl: "0"
`, pgDSNEnv, table, table)), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// No WithConnectionOpener: this is the DEFAULT pgx path. The object registry
	// is what opens the document's pools, and the attribute registry is HANDED
	// them — one connections: entry is one pool, whichever sections name it.
	_, conns, err := doc.BuildRegistryWithConnections("")
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("BuildAttributeRegistryWithConnections: %v", err)
	}

	// The decision path's fetch. The value model against a real driver: an int
	// column is a Go integer, a cast array is a real list, and a ::text date is
	// the bare day it was written as.
	//
	// Every assertion pins the Go TYPE as well as the value, with a type
	// assertion rather than ==, and that is the point of the shape they are
	// written in. `bag["clearance"] != 3` would compare an any against an
	// untyped constant and be false for the correct int64(3); a
	// fmt.Sprint(...) == "3" — which this test used to do — cannot tell int64(3)
	// from the string "3" at all, so a driver that silently stringified a numeric
	// column would pass here and then fail `principal.clearance >= 3` with
	// nothing to point at. In an authorization engine "5" != 5 is load-bearing
	// (it is the whole argument for pgx over lib/pq, which returns []byte for
	// numeric and uuid), so the type is the assertion, not a detail of it.
	bag, err := attrs.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	if got, ok := bag["department"].(string); !ok || got != "eng" {
		t.Errorf("department = %#v (%T), want the string \"eng\"", bag["department"], bag["department"])
	}
	if got, ok := bag["clearance"].(int64); !ok || got != 3 {
		t.Errorf("clearance = %#v (%T), want int64(3) — a stringified number compares "+
			"against nothing a rule can write", bag["clearance"], bag["clearance"])
	}
	if got, want := bag["teams"], []any{"platform", "oncall"}; !reflect.DeepEqual(got, want) {
		t.Errorf("teams = %#v (%T), want %#v — check the to_jsonb cast", got, got, want)
	}
	if got, ok := bag["hired_on"].(string); !ok || got != "2024-03-04" {
		t.Errorf("hired_on = %#v (%T), want the string \"2024-03-04\" — a ::text date is "+
			"stored as-is, never routed through the datetime form", bag["hired_on"], bag["hired_on"])
	}

	// A subject the table does not hold is not a failure: it decides against the
	// floor bag, which is the safe direction.
	if bag, err := attrs.Attributes(ctx, "user", "mallory"); err != nil || bag != nil {
		t.Errorf("unknown subject = %#v, %v; want a nil bag and no error", bag, err)
	}

	// The admin read, keyed by BARE ids.
	//
	// Its position in this test is load-bearing and must not be moved below the
	// verdict. This document's get_all projects three columns where get_one
	// projects four — the legal pairing, since AttributeConfig.ListQuery is
	// optional and need only select a bare id — so an Enumerate that wrote the
	// slot's FETCH cache would replace alice's decision bag with the listing's
	// narrower one, `principal.teams` would be absent, and the verdict below would
	// be false. That is the bug this ordering found (E3-S5); the fix is in
	// provider.AttributeRegistry.Enumerate, and the make-test-catchable version of
	// this case is TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag.
	recs, err := attrs.Enumerate(ctx, provider.AttributeSlotUser, provider.AttributeFilter{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	keys := make([]string, 0, len(recs))
	for _, r := range recs {
		keys = append(keys, r.ID)
	}
	sort.Strings(keys)
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("Enumerate keys = %v, want %v — a bare id, never 'user:' || u.id", keys, want)
	}

	// The verdict. This is the sentence the epic promised: a rule over a column of
	// the host's own users table decides, and the only thing anyone wrote was
	// YAML. It goes through rules.Engine with the attribute registry wired as the
	// principal resolver, which is exactly what internal/cli's buildDecisionStack
	// does.
	eng := rules.NewEngine(
		rules.MapSource{"engineering": {AST: rules.And(
			rules.Compare(rules.OpEq, rules.Var("principal.department"), rules.Lit("eng")),
			rules.Compare(rules.OpGe, rules.Var("principal.clearance"), rules.Lit(3)),
			// Membership over the cast array — the assertion that to_jsonb(teams)
			// really produced a list, not the raw "{platform,oncall}" literal that
			// every membership predicate silently fails to match.
			rules.Compare(rules.OpHasAny, rules.Var("principal.teams"), rules.List(rules.Lit("oncall"))),
		)}},
		nil, // the rule reads only the principal
		rules.WithPrincipalResolver(attrs),
	)
	for _, tc := range []struct {
		principal string
		want      bool
	}{
		{"alice", true},
		{"bob", false},
		// Not in the table at all: the floor bag has no department, so the rule
		// is false rather than errored. An unknown subject denies; it does not
		// throw.
		{"mallory", false},
	} {
		selected, err := eng.Selected(ctx, "engineering", identity.Identity{}, "acme", "user", tc.principal, "read")
		if err != nil {
			t.Fatalf("Selected(%s): %v", tc.principal, err)
		}
		if selected != tc.want {
			t.Errorf("Selected(%s) = %v, want %v", tc.principal, selected, tc.want)
		}
	}
}
