package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/storage/postgres"
)

// This file OWNS the live-PostgreSQL gate for internal/cli. Everything in this
// package that needs a real server goes through requireLivePostgres below, and the
// contract it implements is stated here once rather than restated per test file:
//
//		APERTURE_PG_INTEGRATION=1 \
//		APERTURE_PG_DSN='postgres://aperture@127.0.0.1:5432/aperture?sslmode=disable' \
//		go test -run TestPostgresLive ./internal/cli/
//
//	  - UNGATED it SKIPS. CI has no service containers, so `make test` has to pass
//	    with no database present.
//	  - GATED WITH AN EMPTY DSN it FAILS. Asking for the live run and silently not
//	    getting one is the outcome a gate must never produce.
//	  - GATED WITH A VALUE THAT IS NEITHER ON NOR OFF it FAILS. A bare `!= "1"` test
//	    turns APERTURE_PG_INTEGRATION=true into a silent skip, which is the same
//	    failure as an empty DSN wearing a different hat.
//
// The two environment variables are SHARED BY NAME with storage/postgres and seed,
// so one exported DSN drives every live suite in one shell; that sharing is itself
// a test below rather than a convention nobody rechecks.
//
// The decision is a pure function (decideLiveGate) precisely so the three outcomes
// above are assertable without a server, in `make test`, on every push. A gate
// whose own behaviour is only observable by running it is not a gate. The shape is
// storage/postgres/gate_test.go's, deliberately: two gates over the same two
// variables that behaved differently would be the drift both of them exist to stop.
//
// # What runs behind it
//
// Three suites, all of them claims about a DATABASE rather than about Go:
//
//   - the `aperture wiring push -> pull -> push` fixed point and the two-backend
//     parity of a pull (wiring_pull_test.go),
//   - `aperture wiring diff`, clean and drifted, and the same report from both
//     backends (wiring_diff_test.go),
//   - the DB-wired boot and the two-instance identical-decisions proof
//     (wiring_refuse_test.go, wiring_identical_test.go).
//
// Each one creates its OWN PostgreSQL schema and drops it afterwards, so a live run
// leaves no residue in whatever database the operator pointed it at and two agents
// can share one container.
//
// Never put a DSN in a file; pass it in the environment.
const (
	livePGGateEnv = "APERTURE_PG_INTEGRATION"
	livePGDSNEnv  = "APERTURE_PG_DSN"
)

// liveGateDecision is what the environment asked for. Exactly one of Skip, Fail, or
// a usable DSN is set.
type liveGateDecision struct {
	DSN  string // the database to run against; set only when the run proceeds
	Skip string // non-empty: the live run was not asked for, and why we say so
	Fail string // non-empty: the live run WAS asked for and cannot happen
}

// liveGateOn and liveGateOff are the values the gate recognises. Anything else is a
// typo, and a typo must not be indistinguishable from "off".
var (
	liveGateOn  = []string{"1", "true", "yes", "on"}
	liveGateOff = []string{"", "0", "false", "no", "off"}
)

// decideLiveGate maps the two environment values onto the contract. It touches no
// globals so the tests below can drive every branch directly.
func decideLiveGate(gate, dsn string) liveGateDecision {
	norm := strings.ToLower(strings.TrimSpace(gate))
	switch {
	case slices.Contains(liveGateOff, norm):
		return liveGateDecision{Skip: "skipping the live PostgreSQL proof: set " + livePGGateEnv +
			"=1 and " + livePGDSNEnv + "=<dsn> to run it"}
	case !slices.Contains(liveGateOn, norm):
		return liveGateDecision{Fail: livePGGateEnv + " is set to " + strconv.Quote(gate) +
			", which is neither on (" + strings.Join(liveGateOn, ", ") + ") nor off (" +
			strings.Join(liveGateOff[1:], ", ") + "). Refusing to guess: set " + livePGGateEnv +
			"=1 to run the live tests, or unset it to skip them."}
	case strings.TrimSpace(dsn) == "":
		return liveGateDecision{Fail: livePGGateEnv + "=" + gate + " but " + livePGDSNEnv +
			" is empty: the gate is on and there is no database to run against. Export " +
			livePGDSNEnv + "=<dsn> in the environment (never in a file), or unset " +
			livePGGateEnv + " to skip the live tests."}
	}
	return liveGateDecision{DSN: dsn}
}

// requireLivePostgres is the one entry point every live test in this package calls.
// It skips, fails, or returns the DSN, per decideLiveGate.
func requireLivePostgres(t *testing.T) string {
	t.Helper()
	d := decideLiveGate(os.Getenv(livePGGateEnv), os.Getenv(livePGDSNEnv))
	switch {
	case d.Skip != "":
		t.Skip(d.Skip)
	case d.Fail != "":
		t.Fatal(d.Fail)
	}
	return d.DSN
}

// liveScratchSchema gives ONE test its own PostgreSQL schema, points Aperture's
// backend at it, and drops it when the test ends. It returns the schema name, for a
// test that has to name it in SQL of its own.
//
// The schema is chosen through APERTURE_POSTGRES_SCHEMA rather than a flag because
// that variable IS the knob — there is no --store-schema — and buildStore reads it
// where it opens the backend. t.Setenv restores it, and it also fails the test if
// the package is ever made parallel, which is the right outcome: two tests sharing
// one process cannot each have their own value of it.
//
// The live container is shared — with other agents, and with whatever else the
// operator has in that database — so nothing here assumes an empty database and
// nothing here drops anything it did not create.
func liveScratchSchema(t *testing.T, ctx context.Context, dsn, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if err := postgres.ValidateSchemaName(name); err != nil {
		t.Fatalf("this test's own generated schema name is not one Aperture accepts: %v", err)
	}
	// "pgx" is registered by storage/postgres, which internal/cli already imports; the
	// admin handle is only here to create and drop the scratch schema.
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Registered FIRST so it runs LAST: cleanups run in reverse order, and a
	// `defer admin.Close()` would close the pool before the DROP below.
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", livePGDSNEnv, err)
	}
	t.Cleanup(func() {
		// Reported, not discarded. A cleanup that cannot fail is a cleanup nobody finds
		// out has stopped working, and this one is what keeps a live run residue-free.
		if _, err := admin.Exec(`DROP SCHEMA IF EXISTS "` + name + `" CASCADE`); err != nil {
			t.Errorf("dropping the scratch schema %s left residue behind: %v", name, err)
		}
	})
	t.Setenv(postgres.EnvSchema, name)
	return name
}

// ---- the gate's own behaviour, proved without a server ----

// liveDSNPlaceholder stands in for the operator's DSN in the tests below. It is
// deliberately not a connection string: a gate that compiled one into its own tests
// would be the first place a real DSN landed.
const liveDSNPlaceholder = "the-dsn-from-the-environment"

// TestTheLiveCLIGateSkipsWhenUngated is the criterion `make test` depends on: with
// nothing set, and with any spelling of "off", the live tests are skipped rather
// than attempted.
func TestTheLiveCLIGateSkipsWhenUngated(t *testing.T) {
	for _, off := range []string{"", "0", "false", "FALSE", "no", "off", "  ", "OFF"} {
		d := decideLiveGate(off, liveDSNPlaceholder)
		if d.Skip == "" {
			t.Errorf("%s=%q did not skip (fail=%q dsn=%q)", livePGGateEnv, off, d.Fail, d.DSN)
			continue
		}
		if d.Fail != "" || d.DSN != "" {
			t.Errorf("%s=%q skipped AND produced fail=%q dsn=%q", livePGGateEnv, off, d.Fail, d.DSN)
		}
		// The skip has to say how to turn the run on, or the operator learns only
		// that something did not happen.
		if !strings.Contains(d.Skip, livePGGateEnv) || !strings.Contains(d.Skip, livePGDSNEnv) {
			t.Errorf("%s=%q skipped without naming both variables: %q", livePGGateEnv, off, d.Skip)
		}
	}
}

// TestTheLiveCLIGateFailsWhenGatedWithAnEmptyDSN is the sharpest half of the
// contract: asking for the run and silently not getting it must be impossible. A
// whitespace-only DSN counts as empty — it is a shell accident, not a database.
func TestTheLiveCLIGateFailsWhenGatedWithAnEmptyDSN(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "on"} {
		for _, dsn := range []string{"", "   ", "\t"} {
			d := decideLiveGate(on, dsn)
			if d.Fail == "" {
				t.Fatalf("%s=%q with %s=%q did not fail (skip=%q dsn=%q)",
					livePGGateEnv, on, livePGDSNEnv, dsn, d.Skip, d.DSN)
			}
			if d.Skip != "" {
				t.Errorf("%s=%q with an empty DSN produced a SKIP as well as a failure: %q",
					livePGGateEnv, on, d.Skip)
			}
			// Actionable means: it names the variable to set, and it says the DSN
			// belongs in the environment rather than in a file.
			for _, want := range []string{livePGDSNEnv, livePGGateEnv, "environment"} {
				if !strings.Contains(d.Fail, want) {
					t.Errorf("the empty-DSN failure does not mention %q, so it does not tell the "+
						"operator what to do: %q", want, d.Fail)
				}
			}
		}
	}
}

// TestTheLiveCLIGateFailsOnAValueThatIsNeitherOnNorOff closes the silent-skip hole a
// bare `!= "1"` comparison leaves: APERTURE_PG_INTEGRATION=yolo is an operator who
// believes the live suite ran.
func TestTheLiveCLIGateFailsOnAValueThatIsNeitherOnNorOff(t *testing.T) {
	for _, v := range []string{"2", "yolo", "enabled", "-1"} {
		d := decideLiveGate(v, liveDSNPlaceholder)
		if d.Fail == "" {
			outcome := "run"
			if d.Skip != "" {
				outcome = "skip"
			}
			t.Errorf("%s=%q was treated as %q instead of being refused; a typo must not be "+
				"indistinguishable from switching the suite off", livePGGateEnv, v, outcome)
		}
	}
	// Surrounding whitespace is a shell accident, not a typo: "1 " is on.
	if d := decideLiveGate("1 ", liveDSNPlaceholder); d.Fail != "" || d.Skip != "" {
		t.Errorf(`%s="1 " was refused (skip=%q fail=%q); leading and trailing space is trimmed`,
			livePGGateEnv, d.Skip, d.Fail)
	}
}

// TestTheLiveCLIGateRunsWhenGatedWithADSN is the positive case, and the reason the
// three above are not vacuous.
func TestTheLiveCLIGateRunsWhenGatedWithADSN(t *testing.T) {
	d := decideLiveGate("1", liveDSNPlaceholder)
	if d.Skip != "" || d.Fail != "" {
		t.Fatalf("a fully gated environment did not run: skip=%q fail=%q", d.Skip, d.Fail)
	}
	if d.DSN != liveDSNPlaceholder {
		t.Errorf("DSN = %q, want the value from the environment verbatim", d.DSN)
	}
}

// TestTheLiveGateSharesItsVariablesWithTheOtherLiveSuites: one exported DSN has to
// drive every live suite in one shell, so the two variable names are asserted rather
// than left as a convention nobody rechecks.
func TestTheLiveGateSharesItsVariablesWithTheOtherLiveSuites(t *testing.T) {
	if livePGGateEnv != "APERTURE_PG_INTEGRATION" || livePGDSNEnv != "APERTURE_PG_DSN" {
		t.Errorf("this suite gates on %s/%s, which are not the variables storage/postgres and seed use; "+
			"one exported DSN must drive every live suite", livePGGateEnv, livePGDSNEnv)
	}
}
