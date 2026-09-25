package service

import (
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// E4-S4: stale shared wiring is loud, and it is not a capability.
//
// The two halves this file holds apart:
//
//   - the RECORDER: a failed refresh opens a staleness window whose AGE keeps
//     growing, and a successful one closes it completely. An alarm that latches
//     past its own remedy is a different bug from one that never fires, and a
//     worse one, so recovery is asserted as hard as the alarm is.
//   - the GATE: the answer is a system-tier read, in the same order as the
//     attribute directory's, and Capabilities did not absorb it.
//
// The clock is pinned throughout. StaleFor is the field this surface exists for,
// and a duration driven by time.Now can only be asserted as "more than nothing" —
// which would pass just as well for a recorder that reported one nanosecond
// forever.

// fixedClock is a hand-advanced clock. Nothing here runs concurrently, so it
// needs no lock; the CLI suite's equivalent does, and has one.
type fixedClock struct{ at time.Time }

func newFixedClock() *fixedClock {
	return &fixedClock{at: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) now() time.Time          { return c.at }
func (c *fixedClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// TestAFailedRefreshOpensAStalenessWindowThatAges is the recorder's headline: the
// alarm carries HOW LONG, and the duration grows with the clock rather than with
// the loop.
//
// The second half is the one that would be missed by a test asserting only that
// StaleFor is non-zero: it is computed at READ time. A recorder that accumulated
// the age on each Failed call would freeze at whatever it reached on the last
// tick — under-reporting exactly the failure that stopped the loop dead, which is
// the worst case this field exists to expose.
func TestAFailedRefreshOpensAStalenessWindowThatAges(t *testing.T) {
	clk := newFixedClock()
	h := NewWiringHealth(30*time.Second, "digest-boot", clk.now)

	if p := h.Posture(); p.Stale || !p.Healthy() {
		t.Fatalf("a fresh recorder reported stale: %+v", p)
	} else if !p.Polling || p.Every != 30*time.Second {
		t.Fatalf("a polling recorder did not report its interval: %+v", p)
	}

	coded := aerr.New(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, "the wiring tables are from an older build")
	h.Failed(coded)

	p := h.Posture()
	if !p.Stale || p.Healthy() {
		t.Fatalf("a failed refresh did not report stale: %+v", p)
	}
	if p.StaleFor != 0 {
		t.Errorf("StaleFor at the instant of the first failure = %s, want 0", p.StaleFor)
	}
	if p.Failures != 1 {
		t.Errorf("Failures = %d, want 1", p.Failures)
	}
	if p.Code != string(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE) {
		t.Errorf("Code = %q, want the STORE's own code — burying it costs the operator the registry fixups", p.Code)
	}
	if p.Digest != "digest-boot" {
		t.Errorf("Digest = %q, want the last-good digest the instance is still deciding from", p.Digest)
	}

	// Time passes and nothing else happens: the alarm must age on its own.
	clk.advance(4 * time.Hour)
	if got := h.Posture().StaleFor; got != 4*time.Hour {
		t.Errorf("StaleFor after four hours with no further tick = %s, want 4h0m0s — "+
			"an age that only advances when the broken thing manages to run under-reports the worst case", got)
	}

	// A second, DIFFERENT failure extends the window rather than restarting it,
	// and replaces the reported code: an operator needs the reason it is failing
	// now, over the age it has been failing for.
	h.Failed(aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "connection \"main\" has no route on this host"))
	p = h.Posture()
	if p.StaleFor != 4*time.Hour {
		t.Errorf("a second failure reset the window to %s; the AGE is the point", p.StaleFor)
	}
	if p.Failures != 2 {
		t.Errorf("Failures = %d, want 2", p.Failures)
	}
	if p.Code != string(aerr.APERTURE_WIRING_CONNECTION_UNROUTED) {
		t.Errorf("Code = %q, want the most recent failure's code", p.Code)
	}
}

// TestASuccessfulRefreshClearsTheAlarmCompletely is the recovery half, and it
// asserts every field rather than only the boolean. An alarm that keeps its code,
// its count or its age after recovering reads as a live incident on the next
// person to look at it, and trains them to ignore the channel.
func TestASuccessfulRefreshClearsTheAlarmCompletely(t *testing.T) {
	clk := newFixedClock()
	h := NewWiringHealth(time.Second, "digest-boot", clk.now)

	h.Failed(aerr.New(aerr.APERTURE_STORAGE, "no route to host"))
	clk.advance(90 * time.Second)
	if !h.Posture().Stale {
		t.Fatal("the alarm did not fire")
	}

	clk.advance(time.Second)
	h.Refreshed("digest-boot")

	p := h.Posture()
	switch {
	case p.Stale:
		t.Error("the alarm survived a successful refresh")
	case p.StaleFor != 0:
		t.Errorf("StaleFor after recovery = %s, want 0", p.StaleFor)
	case !p.Since.IsZero():
		t.Errorf("Since after recovery = %s, want the zero instant", p.Since)
	case p.Failures != 0:
		t.Errorf("Failures after recovery = %d, want 0", p.Failures)
	case p.Code != "":
		t.Errorf("Code after recovery = %q, want empty", p.Code)
	case p.Reason != "":
		t.Errorf("Reason after recovery = %q, want empty", p.Reason)
	case !p.LastRefresh.Equal(clk.at):
		t.Errorf("LastRefresh = %s, want %s", p.LastRefresh, clk.at)
	}

	// And it can fire AGAIN, from a fresh window. A recorder that cleared but
	// could not re-arm is silent staleness with extra steps.
	h.Failed(aerr.New(aerr.APERTURE_STORAGE, "no route to host"))
	clk.advance(5 * time.Second)
	if p := h.Posture(); !p.Stale || p.StaleFor != 5*time.Second || p.Failures != 1 {
		t.Errorf("the recorder did not re-arm cleanly: %+v", p)
	}
}

// TestARecorderThatIsNotPollingIsNotStale pins the honest answer for every
// deployment that has not opted in: boot-only wiring is the wiring the instance
// was told to run, and nothing has been observed to replace it.
//
// The nil recorder is the same answer by a different route, which is what lets a
// facade built without WithWiringHealth answer at all instead of refusing.
func TestARecorderThatIsNotPollingIsNotStale(t *testing.T) {
	off := NewWiringHealth(0, "digest-boot", nil)
	if p := off.Posture(); p.Polling || p.Stale || p.Every != 0 {
		t.Errorf("a non-polling recorder reported %+v, want not polling and not stale", p)
	}

	var absent *WiringHealth
	if p := absent.Posture(); p.Polling || p.Stale || p.Digest != "" {
		t.Errorf("the nil recorder reported %+v, want the zero posture", p)
	}
	// Nil-safe in both writing directions too, so no call site needs a condition.
	absent.Failed(aerr.New(aerr.APERTURE_BOOT, "x"))
	absent.Refreshed("y")
	if p := absent.Posture(); p.Stale {
		t.Error("the nil recorder recorded something")
	}

	// A nil error is not a failure. Inventing an alarm for a caller's bug would
	// hide the bug behind a permanently stale instance.
	live := NewWiringHealth(time.Second, "d", nil)
	live.Failed(nil)
	if live.Posture().Stale {
		t.Error("Failed(nil) raised an alarm")
	}
}

// TestTheWiringPostureIsASystemTierRead is the gate. Same call, two authenticated
// principals: the posture for the system-admin, a coded refusal for everybody
// else.
//
// Depth is asserted for the same reason ListAttributes asserts it: Wrap
// re-stamps, so a refusal re-classified here would hand the operator a generic
// code instead of APERTURE_AUTHZ_DENIED's fixups, and a same-code re-wrap is
// invisible to CodeOf.
func TestTheWiringPostureIsASystemTierRead(t *testing.T) {
	svc, _, ctx := attributeFixture(t)
	clk := newFixedClock()
	h := NewWiringHealth(30*time.Second, "digest-boot", clk.now)
	WithWiringHealth(h)(svc)

	h.Failed(aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "connection \"main\" has no route on this host"))
	clk.advance(2 * time.Hour)

	p, err := svc.WiringPosture(ctx, adminActor)
	if err != nil {
		t.Fatalf("a system-admin must be able to read the wiring posture: %v", err)
	}
	if !p.Stale || p.StaleFor != 2*time.Hour {
		t.Errorf("admin read %+v, want stale for 2h", p)
	}

	p, err = svc.WiringPosture(ctx, deniedActor)
	if code := aerr.CodeOf(err); code != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-admin read = %v (code %q), want APERTURE_AUTHZ_DENIED", err, code)
	}
	if p != (WiringPosture{}) {
		t.Errorf("a refused read returned %+v; it must return nothing at all", p)
	}
	if depth := codedAttributeDepth(err); depth != 1 {
		t.Errorf("refusal has %d Aperture-coded errors in its chain, want exactly 1 "+
			"(the gate's APERTURE_AUTHZ_DENIED, verbatim)", depth)
	}
}

// TestARefusedPostureReadDisclosesNothingAboutTheStaleness is the disclosure half
// of the same decision, and the reason the gate runs BEFORE the recorder is
// consulted: a refusal must be byte-identical whether this instance is healthy,
// not polling, or four hours stale. Otherwise the refusal is a probe for
// "is this instance degraded?", which is the fact the gate exists to withhold.
func TestARefusedPostureReadDisclosesNothingAboutTheStaleness(t *testing.T) {
	refusalFor := func(t *testing.T, configure func(*Service)) string {
		t.Helper()
		svc, _, ctx := attributeFixture(t)
		configure(svc)
		_, err := svc.WiringPosture(ctx, deniedActor)
		if err == nil {
			t.Fatal("a non-admin read was not refused")
		}
		return err.Error()
	}

	healthy := refusalFor(t, func(svc *Service) {
		WithWiringHealth(NewWiringHealth(30*time.Second, "digest-boot", nil))(svc)
	})
	stale := refusalFor(t, func(svc *Service) {
		h := NewWiringHealth(30*time.Second, "digest-boot", nil)
		h.Failed(aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "connection \"main\" has no route"))
		WithWiringHealth(h)(svc)
	})
	unwired := refusalFor(t, func(*Service) {})

	if healthy != stale || healthy != unwired {
		t.Errorf("a refusal distinguishes the instance's health:\n healthy: %s\n   stale: %s\n unwired: %s\n"+
			"the gate must run before the recorder is consulted", healthy, stale, unwired)
	}
	for _, leak := range []string{"stale", "UNROUTED", "main"} {
		if strings.Contains(healthy, leak) {
			t.Errorf("the refusal mentions %q: %s", leak, healthy)
		}
	}
}

// TestAnUnwiredRecorderIsAnAnswerAndNotARefusal pins the deliberate difference
// from ListAttributes, which refuses when no registry is wired.
//
// "This instance does not poll, and is therefore not stale" is true and useful.
// Refusing instead would make the read unusable as a fleet-wide probe: an
// operator sweeping ten instances would have to read a refusal as either "fine"
// or "broken" and would be wrong about one of them.
func TestAnUnwiredRecorderIsAnAnswerAndNotARefusal(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	p, err := svc.WiringPosture(ctx, adminActor)
	if err != nil {
		t.Fatalf("a facade with no recorder refused the read: %v", err)
	}
	if p != (WiringPosture{}) {
		t.Errorf("an unwired recorder reported %+v, want the zero posture", p)
	}

	// But it is still gated, and still needs a principal: the absence of a
	// recorder must not become a way around the door.
	if _, err := svc.WiringPosture(ctx, Actor{}); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("an unauthenticated read = %v, want APERTURE_UNAUTHENTICATED", err)
	}
	ungated := New(svc.eng, WithStorage(svc.store))
	if _, err := ungated.WiringPosture(ctx, adminActor); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("a facade with no gate = %v, want APERTURE_UNIMPLEMENTED — a system-tier read is never served ungated", err)
	}
}

// TestCapabilitiesDidNotAbsorbTheStaleness is the contract assertion behind the
// design decision, and it is deliberately a test and not only a comment.
//
// Capabilities documents itself as booleans of BOOT-TIME configuration, read
// without an actor and unable to fail, which is what licenses every surface to
// expose it unauthenticated. A staleness field there would break all three at
// once and — because the admin shell probes Capabilities once on page load — would
// also be permanently whatever it was at load. So the same process, four hours
// stale, must report exactly the Capabilities it reported healthy.
func TestCapabilitiesDidNotAbsorbTheStaleness(t *testing.T) {
	clk := newFixedClock()
	h := NewWiringHealth(30*time.Second, "digest-boot", clk.now)
	svc := New(nil, WithWiringHealth(h))

	before := svc.Capabilities()
	h.Failed(aerr.New(aerr.APERTURE_STORAGE, "no route to host"))
	clk.advance(4 * time.Hour)
	if after := svc.Capabilities(); after != before {
		t.Errorf("Capabilities changed when the wiring went stale (%+v -> %+v). "+
			"It promises immutable boot-time configuration, which is what lets every surface "+
			"serve it unauthenticated and lets a client cache it once on load; runtime fault "+
			"state belongs on WiringPosture, behind the gate", before, after)
	}
	if !h.Posture().Stale {
		t.Fatal("the fixture did not actually go stale, so this proves nothing")
	}
}
