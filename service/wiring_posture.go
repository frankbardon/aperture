package service

import (
	"context"
	"sync"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// STALE SHARED WIRING, AND WHY IT IS NOT A CAPABILITY.
//
// An instance that polls the shared wiring tables (--wiring-poll) can fail to
// re-read them: the store is unreachable, the rows it returns cannot be turned
// into a working registry, or a connection name in them is one this host has no
// route for. When that happens the instance KEEPS THE WIRING IT ALREADY HAS and
// keeps deciding. It has to: Aperture is embedded in-process in its first real
// host, so an access engine that stops answering takes the whole host down with
// it, and a fleet that stops deciding because one operator pushed a bad wiring
// row is a worse outcome than a fleet deciding from wiring one push behind.
//
// The price of that choice is STALENESS, and staleness must never be silent. A
// slot's ttl: is already the window a revoked clearance keeps authorizing for;
// stale wiring is the same class of hazard — an instance enforcing configuration
// its operator has already replaced — and it gets the same treatment: loud, and
// visible somewhere other than a log line nobody tails.
//
// # Why this is a separate type and Capabilities did not absorb it
//
// The obvious place to put "this instance's wiring is stale" is Capabilities,
// because that is the existing posture read and every surface already exposes
// it. It is the wrong place, on three counts, and all three are in
// Capabilities' own doc comment:
//
//   - "It carries booleans and nothing else — no ids, no counts, no model
//     data." The load-bearing half of this fact is HOW LONG, which is a
//     duration, and the supporting half is a failure COUNT. Neither is a
//     boolean, and reducing the answer to one throws away the half an operator
//     acts on: "stale" is a question, "stale for four hours" is an incident.
//   - "It reads immutable boot-time configuration." This is mutable runtime
//     state. It changes between two reads of the same process, which is the one
//     property Capabilities promises it does not have — and a client that cached
//     Capabilities once on page load, exactly as the admin shell does, would
//     hold a staleness answer that is permanently whatever it was at load.
//   - "which is what lets every surface expose it as an open, unauthenticated
//     call." "This instance has been running configuration its operator already
//     replaced, for four hours" is operational intelligence: it tells an
//     anonymous caller that the enforced policy is not the intended policy, and
//     how long the window has been open. That is a fact about a fault, not about
//     deployment posture, and it is not knowingly public the way "this Aperture
//     does not master its own accounts" is.
//
// Splitting the difference — a bare boolean left open, the duration behind
// authentication — was considered and rejected as theatre: the boolean carries
// the disclosure ("act now, the enforced policy is not the intended one") and
// the duration only sharpens it. So the whole answer moves behind the same
// system-admin gate the attribute directory read uses, and Capabilities keeps
// its contract intact. See Service.WiringPosture.
//
// This is a deliberate departure from E4-S4's literal wording, which named
// Capabilities. The requirement that wording exists to serve — an operator can
// see the staleness AND its duration without reading logs — is met in full by an
// authenticated read on the facade and its Twirp translation; the operator is
// the party holding credentials, and the anonymous caller is not the operator.

// WiringPosture is what this process will say about the shared wiring it is
// deciding from: whether it re-reads it at all, whether the last re-read
// FAILED, and — the half that matters — how long ago that started.
//
// It is a snapshot, not a handle. Every field is a value read under one lock, so
// two fields of one WiringPosture always describe the same instant; a caller
// that wants a later answer reads again.
//
// The zero value is the honest answer for a process that does not poll: not
// polling, not stale. Boot-only wiring is not stale — it is the wiring the
// instance was told to run, and nothing has been observed to replace it.
type WiringPosture struct {
	// Polling reports whether this process re-reads the shared wiring on an
	// interval. False is the default and the state of every deployment that has
	// not set --wiring-poll / APERTURE_WIRING_POLL: such an instance is wired
	// once, at boot, and can never become stale in this sense because it never
	// looks again.
	Polling bool
	// Every is the poll interval, zero when Polling is false. It is the window
	// the fleet is allowed to disagree with itself in even when nothing is
	// failing, so an operator reading a staleness alarm needs it to know whether
	// "stale for 40s" is one missed tick or thirteen.
	Every time.Duration
	// Digest is the content digest of the shared wiring THIS PROCESS IS DECIDING
	// FROM — the last-good set, when the alarm is raised. It is a hash of wiring
	// an operator pushed and carries no account, principal or object data.
	Digest string
	// Stale reports that the most recent refresh attempt FAILED and this process
	// is therefore running the wiring it last succeeded with. It says nothing
	// about whether the deployed wiring has actually changed: a failed read
	// cannot know, which is precisely why the instance keeps what it has.
	Stale bool
	// StaleFor is how long the CURRENT run of failures has lasted — from the
	// first failure after the last success to now — and is zero when Stale is
	// false. It is the number an operator escalates on. A single missed tick on a
	// restarting database is ordinary; the same alarm four hours old is a fleet
	// enforcing policy somebody already retired.
	StaleFor time.Duration
	// Since is the instant the current run of failures began, zero when Stale is
	// false. StaleFor is derived from it, and both are reported because a
	// duration is what an operator judges and an instant is what they correlate
	// against a deploy.
	Since time.Time
	// Failures counts the consecutive failed refreshes in the current run, and
	// is zero when Stale is false. It is reported alongside StaleFor because the
	// two answer different questions: a long duration with one failure is a
	// process that gave up looking, and a long duration with many is a store that
	// keeps refusing.
	Failures int
	// Code is the APERTURE_* code of the most recent failure, empty when Stale is
	// false. It is the code the underlying failure carried — the store's own
	// APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, E2-S3's
	// APERTURE_WIRING_CONNECTION_UNROUTED — and only APERTURE_WIRING_REFRESH_FAILED
	// when nothing beneath it was coded, so the operator reaches the registry
	// fixups that name the actual remedy rather than a generic one.
	Code string
	// Reason is that failure's message. It carries the failing subsystem's own
	// words (a DSN-less connection name, a store error, a refused kind) and never
	// an account, principal or object id — the same restriction the poll's stderr
	// reports carry, for the same reason.
	Reason string
	// LastRefresh is when a refresh last succeeded, zero when none has. On a
	// healthy poller it is within Every of now; on a stale one it is the far end
	// of the window Since opens.
	LastRefresh time.Time
}

// Healthy reports whether this process's shared wiring is the wiring it last
// successfully read. It is the negation of Stale, named positively so a caller
// rendering a light does not have to invert a boolean.
func (p WiringPosture) Healthy() bool { return !p.Stale }

// WiringHealth is the ONE recorder a background wiring refresher reports to, and
// the seam between the refresher and the facade that answers for it.
//
// There is deliberately one recorder and not one per failure mode. An
// unreachable store, wiring the instance cannot build, a connection name it
// cannot route and — once the hot rebuild lands — a failed rebuild are four
// origins of a single fact: this process did not adopt what the tables say, and
// is still deciding from what it had. An operator needs that fact, its age and
// its code; a taxonomy of which internal step declined would be four alarms to
// wire up, four to document, and three to forget.
//
// Every method is nil-safe, so a caller wires a recorder unconditionally and the
// "polling is off" path is a nil pointer rather than a branch at each call site
// — the same contract the poller itself has.
//
// It is safe for concurrent use: the refresher writes from its own goroutine
// while a surface reads from a request's.
type WiringHealth struct {
	mu sync.Mutex
	// now is the clock, time.Now unless a caller supplied one. It exists for the
	// same reason rules.WithClock does: StaleFor is the field this type is FOR,
	// and a duration nothing can pin is a duration no test can assert an
	// operator will ever read correctly.
	now func() time.Time

	polling bool
	every   time.Duration
	digest  string

	// since is the start of the current run of failures, and the single source of
	// "is it stale". Zero means healthy, which is what makes recovery one
	// assignment and makes a latched alarm structurally hard: there is no
	// separate boolean that can stay true after since is cleared.
	since       time.Time
	failures    int
	code        string
	reason      string
	lastRefresh time.Time
}

// NewWiringHealth builds the recorder for a process that polls the shared wiring
// every `every`, having booted on `digest`.
//
// A non-positive `every` records a process that does NOT poll: Posture reports
// Polling false and can never report Stale, because nothing will ever attempt a
// refresh to fail. That makes the "polling is off" configuration expressible
// without a nil check at the construction site.
//
// clock may be nil, which means time.Now. A caller supplies one only to pin
// StaleFor.
func NewWiringHealth(every time.Duration, digest string, clock func() time.Time) *WiringHealth {
	if clock == nil {
		clock = time.Now
	}
	return &WiringHealth{
		now:     clock,
		polling: every > 0,
		every:   every,
		digest:  digest,
	}
}

// Refreshed records that a refresh SUCCEEDED and clears the alarm, naming the
// digest this process is now deciding from.
//
// runningDigest is what the instance RUNS, never what the tables hold. On a tick
// that found no change the two are the same value; on a tick that found a change
// the instance keeps running the old one until the rebuild adopts it, and this
// method must be told the old one or the posture would claim an adoption that
// has not happened. That argument is the seam the hot-rebuild story attaches to:
// it advances the digest and calls this again once the swap has actually
// succeeded, and calls Failed instead when it has not.
//
// Clearing is unconditional. An alarm that survives its own remedy is a
// different bug from an alarm that never fired, and a worse one — it trains an
// operator to ignore the channel.
func (h *WiringHealth) Refreshed(runningDigest string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.digest = runningDigest
	h.lastRefresh = h.now()
	h.since = time.Time{}
	h.failures = 0
	h.code = ""
	h.reason = ""
}

// Failed records that a refresh did NOT complete, so this process is still
// deciding from the wiring it has.
//
// The first failure after a success opens the staleness window; every later one
// extends it and replaces the reported code, so an operator reads the reason the
// refresh is failing NOW rather than the one it started failing with. The window
// itself is never re-opened, because its age is the whole point.
//
// err's own code is recorded verbatim. It is NOT re-stamped here: aerr.Wrap
// replaces a code, and burying APERTURE_WIRING_CONNECTION_UNROUTED (whose
// fixups name the environment variable to export) under a generic alarm code
// costs the operator the remedy. Classifying an UNCODED error is the caller's
// job, at the call site, where the pass-through guard belongs.
//
// A nil err is ignored rather than treated as a failure: a caller that reached
// this method with nothing wrong has a bug, and inventing an alarm for it would
// hide that bug behind a permanently stale instance.
func (h *WiringHealth) Failed(err error) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.since.IsZero() {
		h.since = h.now()
	}
	h.failures++
	h.code = string(aerr.CodeOf(err))
	h.reason = err.Error()
}

// Posture takes one consistent snapshot of what this process would say about its
// shared wiring.
//
// StaleFor is computed at read time rather than accumulated at write time, so it
// keeps growing while the refresher is failing instead of freezing at whatever
// it was on the last tick — an alarm whose age only advanced when the thing that
// is broken managed to run would under-report exactly the failure that stopped
// the loop dead. It is clamped at zero so a clock that went backwards reports a
// young alarm rather than a negative one.
func (h *WiringHealth) Posture() WiringPosture {
	if h == nil {
		return WiringPosture{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := WiringPosture{
		Polling:     h.polling,
		Every:       h.every,
		Digest:      h.digest,
		LastRefresh: h.lastRefresh,
	}
	if h.since.IsZero() {
		return p
	}
	p.Stale = true
	p.Since = h.since
	p.Failures = h.failures
	p.Code = h.code
	p.Reason = h.reason
	if d := h.now().Sub(h.since); d > 0 {
		p.StaleFor = d
	}
	return p
}

// WithWiringHealth gives the facade the recorder a background wiring refresher
// reports to, so Service.WiringPosture can answer for it.
//
// It is the seam and not a constructor: the recorder is created by whatever
// starts the refresher (under `aperture serve`, runServe), because the two must
// share one pointer — a facade holding its own copy would answer for a poller
// that never wrote to it, and report a permanently healthy instance no matter
// what the loop observed. Left unwired, WiringPosture reports the zero posture:
// not polling, not stale, which is the truth for a process with no refresher.
func WithWiringHealth(h *WiringHealth) Option {
	return func(s *Service) { s.wiring = h }
}

// WiringPosture reports whether this process's shared wiring is stale, and for
// how long — a SYSTEM-TIER read, gated exactly like the attribute directory.
//
// # Why it is gated when Capabilities is not
//
// See the file header. The short version: this is mutable runtime state about a
// FAULT, and its useful half is a duration. "This instance has been enforcing
// configuration its operator already replaced, for four hours" is an answer only
// the operator should get, where "this deployment does not master its own
// accounts" is knowingly public.
//
// # The order of the checks is the contract
//
// The gate runs BEFORE the recorder is consulted, so a refused caller's error is
// identical for an instance that polls, one that does not, and one that is four
// hours stale. Checking the recorder first would turn the refusal into a probe
// for whether this instance is degraded, which is the fact the gate exists to
// withhold — the same reasoning, and the same ordering, as
// requireAttributeAdmin.
//
// # An unwired recorder is an ANSWER, not a failure
//
// A process with no refresher reports the zero posture rather than
// APERTURE_UNIMPLEMENTED, because "this instance does not poll, and is therefore
// not stale" is true, useful, and the state of every deployment that has not
// opted in. Refusing instead would make the read unusable as a fleet-wide health
// probe: an operator sweeping ten instances would have to treat a refusal as
// either "fine" or "broken" and would be wrong on one of them.
func (s *Service) WiringPosture(ctx context.Context, actor Actor) (WiringPosture, error) {
	if err := s.requireWiringAdmin(ctx, actor); err != nil {
		return WiringPosture{}, err
	}
	return s.wiring.Posture(), nil
}

// requireWiringAdmin is the single definition of "may this actor read this
// instance's wiring posture": the gate is wired, the caller is authenticated,
// and it holds system-admin authority in its active account.
//
// It is the same three conditions in the same order as requireAttributeAdmin,
// minus the registry check, which moved into the answer for the reason
// WiringPosture gives.
func (s *Service) requireWiringAdmin(ctx context.Context, actor Actor) error {
	if s.gate == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: the admin-authority gate is not wired; the wiring posture is a system-tier read and is never served ungated")
	}
	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: reading the wiring posture requires an authenticated principal")
	}
	// Verbatim: Wrap re-stamps, and APERTURE_AUTHZ_DENIED's registry fixups are
	// the operator's remedy.
	return s.gate.RequireSystemAdmin(ctx, actor.gateActor())
}
