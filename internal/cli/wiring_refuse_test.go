package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// E2-S3: an instance refuses to start on wiring it cannot construct or route.
//
// This is the effort's most important safety property, and the reason it is a
// BOOT contract rather than best-effort degradation is that degradation here is
// silent in the widening direction:
//
//   - An object type with no working provider does not error on a decision. A rule
//     reading object.tier reads a MISSING PATH, and a comparison against a missing
//     path is not a refusal.
//   - An attribute slot with no working provider is worse. The leniency contract
//     collapses a provider failure to a NIL BAG, and a rule that excluded on an
//     attribute it can no longer see stops excluding — the grant WIDENS
//     (rules/attribute_leniency_test.go, TestAMissingBagWidensAnExclusiveGrant).
//
// Neither appears in a verdict, a trace or a note. So every case below asserts two
// things, never one: that the boot REFUSED, and — for the headline cases — that the
// process then answered no decision at all. A test that only checked the refusal
// would stay green if a later edit turned the refusal into a warning.
//
// Each refusal is also asserted at chain DEPTH 1 (mustRefuse), because aerr.Wrap
// re-stamps: a wrap site without the `if aerr.CodeOf(err) != "" { return err }`
// guard would bury APERTURE_WIRING_KIND_UNSHAREABLE or
// APERTURE_WIRING_CONNECTION_UNROUTED — and their fixups, which are the whole
// remedy — under APERTURE_BOOT's "check your environment variables".
//
// # What is asserted here and what is deliberately NOT
//
// The refusals this layer owns are the ones no builder below it can give: a kind
// that cannot be SHARED wiring (the stored row carries no path and the schema has
// no column for one), and a connection NAME this instance has no route for. The
// statement contract, the ttl vocabulary, the reference targets and the field-type
// words belong to seed's own builders, which already name the entry — restating
// them here would be the rule-in-two-places hazard, and E2-S1's
// TestAnIncompleteStatementSetFailsTheBootLoudly already pins that those arrive
// unburied.

// routeSharedMain gives this instance the conventional route for the shared
// manifest's "main" connection. The DSN is never dialled (see unroutedDSN).
func routeSharedMain(t *testing.T) {
	t.Helper()
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)
}

// csvWiringSet is the shared wiring an operator would have if a kind: csv row had
// reached the tables anyway — hand-written SQL, or a push from a build that
// predates the check. model.ValidateWiringProvider records a Kind verbatim and
// does not police the vocabulary (model/wiring.go says so, and says the question is
// answered where the wiring is BUILT), so this set is storable.
func csvWiringSet(now time.Time) model.WiringSet {
	set := sharedWiringSet(now)
	set.Providers[0].Kind = "csv"
	return set
}

// runDecisionCLI runs one real `aperture <cmd>` invocation against a store and
// returns everything the process wrote plus the error its main would exit on.
//
// It takes no --seed: a DB-wired instance is exactly the deployment that has no
// seed file, which is the shape this story is about.
//
// The no-op ExitErrHandler is what makes assertAnsweredNoDecision an assertion
// rather than an accident. `check` ends a clean deny with ucli.Exit, which
// urfave/cli turns into os.Exit — so without it a boot that STOPPED refusing would
// kill the test binary on the way to printing "deny", and the failure would arrive
// as a dead process rather than as this case's own message.
func runDecisionCLI(t *testing.T, storeDSN string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	app.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	argv := append([]string{"aperture", args[0], "--store", storeDSN}, args[1:]...)
	err := app.Run(context.Background(), argv)
	return out.String(), err
}

// assertAnsweredNoDecision is the half of the acceptance a refusal assertion does
// not cover: the process must not have printed a verdict.
//
// `check` prints "allow" or "deny" and then returns ucli.Exit for a clean deny, so
// a deny is ALSO a non-zero exit — which means "the command failed" proves nothing
// on its own. The output is what tells the two apart: a refused boot never reaches
// the facade, so neither word is ever written.
func assertAnsweredNoDecision(t *testing.T, out string) {
	t.Helper()
	for _, verdict := range []string{"allow", "deny"} {
		if strings.Contains(out, verdict) {
			t.Fatalf("the process printed %q, so it decided on wiring it could not construct; "+
				"an instance missing a provider its peer has must not serve the types it "+
				"happened to have. Output:\n%s", verdict, out)
		}
	}
}

// TestConstructibleSharedWiringStillAnswersADecision is the positive control every
// refusal case above rests on, and without it they are all vacuous.
//
// assertAnsweredNoDecision proves a negative — "the process printed no verdict" —
// which is trivially true of a command that prints nothing for any reason at all: a
// renamed flag, a store that never opened, an output buffer wired to the wrong
// writer. This case runs the SAME store, the SAME command and the SAME argv with
// one thing changed (the provider row is constructible) and requires a verdict, so
// the difference between the two is the wiring and nothing else.
//
// The verdict itself is "deny" and the reason does not matter: the assertion is
// that the process reached the facade and answered, which is precisely what an
// instance on unconstructible wiring must never do.
func TestConstructibleSharedWiringStillAnswersADecision(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "healthy.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	out, _ := runDecisionCLI(t, dsn, "check", "user:alice", "read", "account:acme/document:42")
	if !strings.Contains(out, "deny") && !strings.Contains(out, "allow") {
		t.Fatalf("a boot on CONSTRUCTIBLE shared wiring printed no verdict either, so every "+
			"\"it answered no decision\" assertion in this file proves nothing. Output:\n%s", out)
	}
}

// TestADeclaredObjectTypeThisInstanceCannotConstructRefusesTheBoot is the story's
// headline case, and it is Aperture's whole side of Arc's wave/metric gap:
// materializing those types into a shared database is the host's work, and refusing
// to pretend they exist is ours.
//
// The row declares object type "document" perfectly legibly and names a kind whose
// only data source is a filesystem path — which the shared tables have no column
// for, so the projection can never supply one. Left to seed's own builder it would
// have been refused for the MISSING PATH: true, and a remedy ("add a path:") an
// operator cannot carry out.
func TestADeclaredObjectTypeThisInstanceCannotConstructRefusesTheBoot(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "unconstructible.db")
	pushWiring(t, dsn, csvWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	err := bootStackError(t, dsn, "")
	mustRefuse(t, "a shared providers: row this instance cannot construct", err,
		aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
		"document", "shared providers:")
	if strings.Contains(err.Error(), "missing path") {
		t.Errorf("the refusal blames a missing path, which is a remedy with nowhere to go: "+
			"the shared-wiring schema has no path column to put one in. Got: %v", err)
	}

	// And it does not answer a Check. The store holds acme, both object types and
	// an inline object, so a healthy boot would have reached the facade and printed
	// a verdict — there is no grant, so it would have printed "deny".
	out, err := runDecisionCLI(t, dsn, "check", "user:alice", "read", "account:acme/document:42")
	if err == nil {
		t.Fatal("`aperture check` succeeded against wiring it could not construct; the " +
			"process must exit non-zero rather than serve decisions on absent metadata")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_WIRING_KIND_UNSHAREABLE {
		t.Errorf("the command exited with code %q, not the boot refusal's own %q", got,
			aerr.APERTURE_WIRING_KIND_UNSHAREABLE)
	}
	assertAnsweredNoDecision(t, out)
}

// TestADeclaredAttributeSlotThisInstanceCannotConstructRefusesTheBoot is the same
// refusal on the axis where degradation is worst.
//
// An unconstructible object provider costs a rule its object metadata; an
// unconstructible attribute provider costs it the SLOT, and a slot that answers
// with a nil bag does not deny — a rule that excluded on an attribute it can no
// longer see stops excluding, and the grant widens with nothing in the verdict
// saying a bag went missing. So the slot is named, at boot, before a registry
// exists.
func TestADeclaredAttributeSlotThisInstanceCannotConstructRefusesTheBoot(t *testing.T) {
	now := time.Now().UTC()
	set := sharedWiringSet(now)
	set.AttributeProviders[0].Kind = "csv"

	dsn := "file:" + filepath.Join(t.TempDir(), "unconstructible-slot.db")
	pushWiring(t, dsn, set)
	routeSharedMain(t)

	mustRefuse(t, "a shared attribute_providers: row this instance cannot construct",
		bootStackError(t, dsn, ""),
		aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
		"user", "shared attribute_providers:")

	out, err := runDecisionCLI(t, dsn, "check", "user:alice", "read", "account:acme/document:42")
	if err == nil {
		t.Fatal("`aperture check` succeeded with an attribute slot it could not construct; " +
			"an empty bag WIDENS an exclusive grant, so serving decisions here over-grants")
	}
	assertAnsweredNoDecision(t, out)
}

// TestAnUnknownProviderKindInTheDatabaseRefusesTheBoot separates the two refusals
// checkWiringKind makes, because their remedies differ. kind: csv is IMPLEMENTED
// and works perfectly in a local seed — it is unshareable. A kind nothing
// implements is a typo or a vocabulary this build does not have, which is the
// ordinary APERTURE_CONFIG_INVALID.
func TestAnUnknownProviderKindInTheDatabaseRefusesTheBoot(t *testing.T) {
	now := time.Now().UTC()
	set := sharedWiringSet(now)
	set.Providers[0].Kind = "parquet"

	dsn := "file:" + filepath.Join(t.TempDir(), "unknown-kind.db")
	pushWiring(t, dsn, set)
	routeSharedMain(t)

	mustRefuse(t, "a shared providers: row with a kind nothing implements",
		bootStackError(t, dsn, ""),
		aerr.APERTURE_CONFIG_INVALID,
		"document", "parquet")
}

// TestASharedConnectionNameWithNoRouteRefusesTheBoot is FR-19.
//
// The shared tables carry a connection's NAME and nothing else, because which
// server, which credential and how big a pool are per-instance facts. An instance
// that answers for none of the three routes has no way to reach the database its
// providers read through, and every one of those providers would then fail under a
// decision — as absent metadata and as a nil bag, neither of which denies.
func TestASharedConnectionNameWithNoRouteRefusesTheBoot(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "unrouted.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	// The conventional variable is explicitly cleared rather than merely left
	// unset, so the case is the same whatever the developer's shell holds.
	t.Setenv(connectionDSNEnvVar("main"), "")

	err := bootStackError(t, dsn, "")
	mustRefuse(t, "a shared connection name this instance has no route for", err,
		aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		"main", connectionDSNEnvVar("main"))

	out, err := runDecisionCLI(t, dsn, "check", "user:alice", "read", "account:acme/document:42")
	if err == nil {
		t.Fatal("`aperture check` succeeded with no route to the database its providers " +
			"read through")
	}
	assertAnsweredNoDecision(t, out)
}

// TestEveryUnroutedNameIsNamedInOneRefusal keeps the refusal from becoming a
// restart loop. An operator deploying a new instance typically has none of the
// variables exported, and discovering the next missing one on each boot turns a
// five-minute job into as many restarts as the fleet has connections.
func TestEveryUnroutedNameIsNamedInOneRefusal(t *testing.T) {
	now := time.Now().UTC()
	set := model.WiringSet{Connections: []model.WiringConnection{
		{Name: "main", CreatedAt: now, UpdatedAt: now},
		{Name: "reporting", CreatedAt: now, UpdatedAt: now},
		{Name: "warehouse", CreatedAt: now, UpdatedAt: now},
	}}
	for _, name := range []string{"main", "reporting", "warehouse"} {
		t.Setenv(connectionDSNEnvVar(name), "")
	}

	_, err := wiringDocument(set, nil)
	mustRefuse(t, "three unrouted names", err, aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		"main", "reporting", "warehouse",
		connectionDSNEnvVar("main"), connectionDSNEnvVar("reporting"),
		connectionDSNEnvVar("warehouse"))

	// A route for one of them leaves exactly the other two, so the refusal shrinks
	// as the operator works rather than repeating itself.
	t.Setenv(connectionDSNEnvVar("reporting"), unroutedDSN)
	_, err = wiringDocument(set, nil)
	mustRefuse(t, "two unrouted names", err, aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		"main", "warehouse")
	if strings.Contains(err.Error(), `"reporting"`) {
		t.Errorf("the refusal still names a connection this instance now routes: %v", err)
	}
}

// TestALocalCSVProviderIsNotJudgedByTheSharedVocabulary is the negative control,
// and it is the one that stops the kind check from being tightened into a
// regression.
//
// The two sections are held to DIFFERENT vocabularies on purpose. A local file may
// say anything seed's builder implements, including kind: csv — a filesystem path
// belongs to the instance that reads the file, and the local file is the ONLY place
// a csv provider can ever be declared, since it cannot be shared wiring at all. If
// the check ever applied to the whole assembled document, pushing any wiring would
// silently become a loss of every csv-backed type on every instance.
func TestALocalCSVProviderIsNotJudgedByTheSharedVocabulary(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "reports.csv")
	if err := os.WriteFile(csvPath, []byte("id,title\naccount:acme/report:1,Q3\n"), 0o600); err != nil {
		t.Fatalf("write the csv: %v", err)
	}
	local := &seed.Document{Providers: []seed.Provider{{
		// A type the shared wiring never declares: the local file may only ADD.
		ObjectType: "report",
		Kind:       "csv",
		Path:       csvPath,
	}}}
	routeSharedMain(t)

	doc, err := wiringDocument(sharedWiringSet(time.Now().UTC()), local)
	if err != nil {
		t.Fatalf("a LOCAL kind: csv provider was refused by the SHARED vocabulary; a "+
			"filesystem path is machine-local, so the local file is the only place a csv "+
			"provider can ever be declared: %v", err)
	}

	reg, conns, err := doc.BuildRegistryWithConnections(dir,
		seed.WithConnectionOpener(func(string, seed.ConnectionSettings) (seed.Pool, error) {
			return fakePool{}, nil
		}))
	if err != nil {
		t.Fatalf("building the assembled document: %v", err)
	}
	defer func() { _ = conns.Close() }()

	for _, want := range []string{"document", "report"} {
		if !reg.Has(want) {
			t.Errorf("the registry serves %v, and not %q — a DB-wired boot must keep every "+
				"csv-backed type the local file adds", reg.Keys(), want)
		}
	}
}

// TestTheBootRefusalsCarryTheirOwnFixups is the operator half of the acceptance,
// asserted against the registry rather than the prose: a refusal whose code has no
// Message or no Fixup tells an operator that something is wrong and nothing about
// what to supply, which is exactly the outcome bootError's pass-through guard
// exists to prevent.
//
// TestCodesHaveFixups already gates this for every code in AllCodes; this case
// names the two THIS story raises, so a future edit that empties one of them fails
// beside the behaviour it breaks.
func TestTheBootRefusalsCarryTheirOwnFixups(t *testing.T) {
	for _, code := range []aerr.Code{
		aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
		aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
	} {
		meta, ok := aerr.Registry[code]
		if !ok {
			t.Fatalf("%s has no Registry entry", code)
		}
		if strings.TrimSpace(meta.Message) == "" {
			t.Errorf("%s has an empty Message", code)
		}
		if len(meta.Fixups) == 0 && !meta.FixupNotApplicable {
			t.Errorf("%s offers the operator no fixup, so a refused boot says what is wrong "+
				"and nothing about what to supply", code)
		}
	}
}

// TestAWiringRefusalIsNotBuriedOnTheServeCommandEither checks the property on a
// second surface, because the acceptance is about the PROCESS and not about one
// command. `serve` assembles the same stack through the same builder, so a boot
// refusal must reach its exit the same way — and the reason to assert it separately
// is that `serve` is the surface a fleet actually runs.
func TestAWiringRefusalIsNotBuriedOnTheServeCommandEither(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "serve-refusal.db")
	pushWiring(t, dsn, csvWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	// --addr with port 0 WOULD bind, so the refusal has to land before the
	// listener: that is the point. `serve` announces itself with "aperture serving
	// on <addr>" the moment it starts listening, so the absence of that line is what
	// proves nothing was ever bound — and asserting on it rather than on the error
	// alone matters because a listening server that refuses every decision is still
	// a process a deployment's health check calls up.
	out, err := runDecisionCLI(t, dsn, "serve", "--addr", "127.0.0.1:0")
	mustRefuse(t, "`aperture serve` on unconstructible wiring", err,
		aerr.APERTURE_WIRING_KIND_UNSHAREABLE, "document")
	if strings.Contains(out, "aperture serving on") {
		t.Fatalf("the server announced that it was serving, so it started on wiring it "+
			"could not construct. Output:\n%s", out)
	}
}

// ---- The gated live-Postgres proof ----
//
// The refusals above are decided entirely in internal/cli, so SQLite proves them.
// What SQLite cannot prove is that the ROW survives the round trip through the other
// dialect unchanged: a Kind is stored verbatim precisely so the vocabulary question
// is answered at the boot, and a backend that normalised, trimmed or dropped it
// would make an unconstructible row read back as a constructible one — which is a
// boot that succeeds and a fleet that answers two ways.
//
// Same gate as the rest of this package's live suite (see wiring_pull_test.go for
// the contract and the helpers): ungated it SKIPS, gated with an empty
// APERTURE_PG_DSN it FAILS.
//
//	APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/

// TestPostgresLiveAnUnconstructibleObjectTypeRefusesTheBoot runs the headline
// refusal against a real server, in its own schema, reading the row back out of
// Postgres rather than SQLite.
func TestPostgresLiveAnUnconstructibleObjectTypeRefusesTheBoot(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// livePostgresWiringStore creates the scratch schema, runs Setup and applies
	// wiringModelSeed — which declares the `document` object type the provider row's
	// foreign key points at.
	dsn = livePostgresWiringStore(t, ctx, dsn)
	routeSharedMain(t)

	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	if err := store.ReplaceWiring(ctx, csvWiringSet(time.Now().UTC())); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}

	mustRefuse(t, "a live-Postgres row this instance cannot construct",
		bootStackError(t, dsn, ""),
		aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
		"document", "shared providers:")
}

// TestPostgresLiveASharedConnectionNameWithNoRouteRefusesTheBoot is FR-19 against a
// real server. The refusal is decided before any pool is opened, which is why it
// needs no second database to point the connection at.
func TestPostgresLiveASharedConnectionNameWithNoRouteRefusesTheBoot(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn = livePostgresWiringStore(t, ctx, dsn)
	t.Setenv(connectionDSNEnvVar("main"), "")

	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	if err := store.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC())); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}

	mustRefuse(t, "a live-Postgres manifest name this instance has no route for",
		bootStackError(t, dsn, ""),
		aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		"main", connectionDSNEnvVar("main"))
}
