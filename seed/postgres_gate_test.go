package seed

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file owns the DECISION half of the live-PostgreSQL gate for package seed.
// The environment variable names and the narrative of what runs behind the gate
// stay in postgres_integration_test.go; what lives here is the pure function that
// maps the two environment values onto the gate's contract, and the tests that
// assert all three of its outcomes with no server present.
//
//   - UNGATED it SKIPS. CI has no service containers, so `make test` has to pass
//     with no database present.
//   - GATED WITH AN EMPTY DSN it FAILS. Asking for the integration run and
//     silently not getting one is the outcome a gate must never produce.
//   - GATED WITH A VALUE THAT IS NEITHER ON NOR OFF it FAILS. This is the half
//     this package used to leave open: a bare `!= "1"` test turned
//     APERTURE_PG_INTEGRATION=true into a silent skip, which is the same failure
//     as an empty DSN wearing a different hat. It was the last of the three live
//     suites where asking for the run could silently not get one.
//
// The shape is storage/postgres/gate_test.go's and internal/cli/live_gate_test.go's,
// deliberately and to the letter: three gates over the same two variables that
// behaved differently would be the drift all three of them exist to stop. The
// decision is a pure function precisely so the three outcomes are assertable in
// `make test`, on every push — a gate whose own behaviour is only observable by
// running it is not a gate.
type seedGateDecision struct {
	DSN  string // the database to run against; set only when the run proceeds
	Skip string // non-empty: the integration run was not asked for, and why we say so
	Fail string // non-empty: the run WAS asked for and cannot happen
}

// seedGateOn and seedGateOff are the values the gate recognises. Anything else is
// a typo, and a typo must not be indistinguishable from "off".
var (
	seedGateOn  = []string{"1", "true", "yes", "on"}
	seedGateOff = []string{"", "0", "false", "no", "off"}
)

// decideSeedGate maps the two environment values onto the contract. It touches no
// globals, so the tests below drive every branch directly.
func decideSeedGate(gate, dsn string) seedGateDecision {
	norm := strings.ToLower(strings.TrimSpace(gate))
	switch {
	case slices.Contains(seedGateOff, norm):
		return seedGateDecision{Skip: "skipping the real-Postgres integration tests: set " +
			pgGateEnv + "=1 and " + pgDSNEnv + "=<dsn> to run them"}
	case !slices.Contains(seedGateOn, norm):
		return seedGateDecision{Fail: pgGateEnv + " is set to " + strconv.Quote(gate) +
			", which is neither on (" + strings.Join(seedGateOn, ", ") + ") nor off (" +
			strings.Join(seedGateOff[1:], ", ") + "). Refusing to guess: set " + pgGateEnv +
			"=1 to run the integration tests, or unset it to skip them."}
	case strings.TrimSpace(dsn) == "":
		return seedGateDecision{Fail: pgGateEnv + "=" + gate + " but " + pgDSNEnv +
			" is empty: the gate is on and there is no database to run against. Export " +
			pgDSNEnv + "=<dsn> in the environment (never in a file), or unset " + pgGateEnv +
			" to skip the integration tests."}
	}
	return seedGateDecision{DSN: dsn}
}

// TestTheGateSkipsOnlyWhenTheRunWasNotAskedFor walks every recognised off value
// and asserts a skip, and every recognised on value with a DSN and asserts the run
// proceeds. The DSN is returned verbatim: the gate never edits it.
func TestTheGateSkipsOnlyWhenTheRunWasNotAskedFor(t *testing.T) {
	for _, off := range seedGateOff {
		got := decideSeedGate(off, "")
		if got.Skip == "" || got.Fail != "" || got.DSN != "" {
			t.Errorf("gate %q: want a skip, got %+v", off, got)
		}
	}
	// Case and surrounding whitespace are not a typo; " TRUE " is a person
	// exporting a value, not asking for a different behaviour.
	for _, on := range append(slices.Clone(seedGateOn), " TRUE ", "On") {
		got := decideSeedGate(on, "postgres://u@h/db")
		if got.DSN != "postgres://u@h/db" || got.Skip != "" || got.Fail != "" {
			t.Errorf("gate %q: want the run to proceed with the DSN verbatim, got %+v", on, got)
		}
	}
}

// TestTheGateFailsWhenItIsOnAndThereIsNoDatabase is the half a skip would hide.
func TestTheGateFailsWhenItIsOnAndThereIsNoDatabase(t *testing.T) {
	for _, dsn := range []string{"", "   "} {
		got := decideSeedGate("1", dsn)
		if got.Fail == "" || got.Skip != "" || got.DSN != "" {
			t.Errorf("DSN %q: want a failure, got %+v", dsn, got)
		}
		if !strings.Contains(got.Fail, pgDSNEnv) {
			t.Errorf("the failure must name %s so the remedy is in the message: %q", pgDSNEnv, got.Fail)
		}
	}
}

// TestAnUnrecognisedGateValueFailsRatherThanSkipping is the defect this file was
// added for. Before it, `APERTURE_PG_INTEGRATION=true` ran nothing and said
// nothing, and the only evidence was a suspiciously fast green.
func TestAnUnrecognisedGateValueFailsRatherThanSkipping(t *testing.T) {
	for _, gate := range []string{"yolo", "2", "TRUEISH", "1 "} {
		if gate == "1 " {
			// Trimmed, so this one is ON — the positive control for the
			// normalisation, proving the refusal below is about the VALUE and
			// not about whitespace.
			if got := decideSeedGate(gate, "postgres://u@h/db"); got.DSN == "" {
				t.Errorf("gate %q: whitespace is not a typo, want the run to proceed, got %+v", gate, got)
			}
			continue
		}
		got := decideSeedGate(gate, "postgres://u@h/db")
		if got.Fail == "" || got.Skip != "" || got.DSN != "" {
			t.Errorf("gate %q: want a failure, got %+v", gate, got)
		}
		if !strings.Contains(got.Fail, strconv.Quote(gate)) {
			t.Errorf("the failure must quote the offending value so a typo is visible: %q", got.Fail)
		}
	}
}

// TestTheThreeLiveSuitesShareTheGateVariablesByName pins the sharing that lets one
// exported DSN drive storage/postgres, internal/cli and this package in one shell.
// The names are the contract; a rename here that missed the other two would split
// one gate into three.
func TestTheThreeLiveSuitesShareTheGateVariablesByName(t *testing.T) {
	if pgGateEnv != "APERTURE_PG_INTEGRATION" {
		t.Errorf("gate variable renamed to %q: storage/postgres and internal/cli still read APERTURE_PG_INTEGRATION", pgGateEnv)
	}
	if pgDSNEnv != "APERTURE_PG_DSN" {
		t.Errorf("DSN variable renamed to %q: storage/postgres and internal/cli still read APERTURE_PG_DSN", pgDSNEnv)
	}
}
