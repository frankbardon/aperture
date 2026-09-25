package cli

import (
	aerr "github.com/frankbardon/aperture/errors"
)

// LAST-GOOD WIRING, AND THE ALARM THAT KEEPS THE STALENESS FROM BEING SILENT.
//
// A failed refresh does not stop this process deciding. Nothing here rolls the
// registries back, drops the pools, fails a request or exits: the instance keeps
// the wiring it already has and carries on, which is the only survivable
// behaviour for a library that is embedded in-process in its first real host —
// an access engine that stops answering takes the whole host down with it, and a
// fleet that stops answering because one operator pushed a row it cannot read is
// a worse outcome than a fleet answering from wiring one push behind.
//
// Keeping last-good was already the behaviour before this file existed, because
// the poller rebuilds nothing on a failure and the digest does not advance
// (wiring_poll.go's tick, and
// TestAFailedPollKeepsTheWiringItHasAndDoesNotBuryTheCode). What was missing is
// that the price of keeping it — STALENESS — was announced only as a line on
// stderr. Staleness is the window an already-replaced configuration keeps being
// enforced in, the same class of hazard as an attribute slot's ttl:, and silent
// staleness was explicitly rejected. So a failure now also:
//
//   - records the alarm on a service.WiringHealth, which the system-admin-gated
//     service.WiringPosture read (and its WiringPosture RPC) answers from with
//     the code, the failure count and — the half an operator escalates on — HOW
//     LONG; and
//   - keeps writing the stderr line, because a process whose facade nobody is
//     polling still has to say something.
//
// # One reporter, and the seam the rest of the epic attaches to
//
// wiringPoll.alarm is the ONE way a tick reports that a refresh did not
// complete, and wiringPoll.refreshed the one way it reports that one did. There
// is deliberately no per-failure-mode variant. The four origins — an unreachable
// store, a digest that could not be computed, wiring this instance cannot turn
// into a working registry (a connection name it has no route for, a kind it
// cannot construct, a statement set a builder refuses), and a rebuild or swap
// that fails once the hot-rebuild story lands — are four ways of arriving at one
// fact: this process did not adopt what the tables say and is still deciding
// from what it had. An operator needs the fact, its age and its code; a taxonomy
// of which internal step declined would be four alarms to wire, four to
// document, and three to forget.
//
// So a rebuild's failure path is `p.alarm(err)` and its success path is advancing
// p.digest and calling `p.refreshed()`. A rebuild that fails must NOT advance
// p.digest — see service.WiringHealth.Refreshed for why the digest it is handed
// is the one the instance RUNS and never the one the tables hold — and must not
// clear the alarm either, because staleness that began at the first refusal is
// CONTINUOUS until an adoption succeeds. See refreshed for why "the read
// succeeded" is not the same event as "a refresh completed".
//
// # The pass-through guard is the load-bearing half
//
// aerr.Wrap RE-STAMPS. A refresh usually fails for a reason that already carries
// a code and its own registry fixups — the store's
// APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, E2-S3's
// APERTURE_WIRING_CONNECTION_UNROUTED, whose fixups name the exact environment
// variable to export — and wrapping it in APERTURE_WIRING_REFRESH_FAILED would
// replace the code CodeOf reports and hand the operator a generic alarm instead
// of the remedy. So the alarm CLASSIFIES ONLY what nothing else did, and the
// tests assert chain depth: exactly one Aperture-coded error in the chain.

// wiringRefreshAlarm is the poll's classifier for a failed refresh: an
// already-coded failure passes through untouched, and only an uncoded one is
// stamped APERTURE_WIRING_REFRESH_FAILED.
//
// It is the call-site idiom CLAUDE.md requires rather than a bare Wrap, and this
// is exactly the situation the idiom exists for: every error reaching it comes
// from readSharedWiring, wiringDigest, or (later) the registry builders, all of
// which already speak in coded errors.
//
// The uncoded branch is not dead code kept for tidiness. An alarm that is the
// only thing standing between a stale instance and silence must name SOME
// APERTURE_* code whatever it is handed, or a future failure origin that returns
// a bare error — a context deadline, a driver's own error escaping a wrapper —
// would raise an alarm with no code, no registry Message and no fixups, which is
// the operator being told that something is wrong and nothing about what.
func wiringRefreshAlarm(err error) error {
	if err == nil {
		return nil
	}
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_WIRING_REFRESH_FAILED,
		"cli: re-reading the shared wiring failed, so this instance keeps the wiring it has", err)
}

// alarm records and reports a failed refresh. It is the seam described in this
// file's header: every failure origin in a tick routes through here, and nothing
// else writes a failure to the health recorder.
//
// The order is record-then-report on purpose. The recorder is what an operator
// reads without logs, and the stderr line is best-effort narration; a panic or a
// short write in the writer must not be able to lose the alarm.
//
// It returns nothing, because there is nothing a caller can do differently: a
// failed refresh always means "keep what we have and carry on", and giving the
// caller a value to branch on would invite a second policy.
func (p *wiringPoll) alarm(err error) {
	alarm := wiringRefreshAlarm(err)
	p.health.Failed(alarm)
	// Only the coded error's own text reaches the writer — no object type, id or
	// key — the same restriction every other line this poller writes carries.
	p.report("wiring poll: re-reading the shared wiring failed, so this instance keeps the wiring it has: %v", alarm)
}

// refreshed records that a refresh COMPLETED, which clears any standing alarm.
//
// It names p.digest — the digest of the wiring this process is DECIDING FROM —
// and never the digest just read, so the posture cannot claim an adoption that
// has not happened. tick therefore calls it only where those two are the same
// value: on the no-change branch, and after a successful swap has assigned
// p.digest.
//
// p.digest is read without a lock because the loop goroutine owns it (see
// wiringPoll.digest), and this method is only ever called from the same
// goroutine that drives tick.
//
// # A completed refresh is not the same as a successful read
//
// There are exactly two of them and tick names both. A tick that observed NO
// CHANGE completed one — that is almost every tick of almost every deployment,
// and an alarm that needed a change to clear would latch forever on a fleet
// whose wiring is stable, which is most of them. A tick that ADOPTED a change
// completed one too.
//
// A tick that read the tables, found a change and could NOT adopt it has
// completed nothing, and must not reach this method. It is the worst of the
// three postures — the instance knows the wiring changed and is knowingly
// running superseded wiring — and clearing on the strength of the read alone
// would leave the alarm firing with useless NUMBERS: Refreshed zeroes the window
// and the count, so a push refused for four hours would report one failure and
// an age of one tick, forever. The age is the half an operator escalates on.
func (p *wiringPoll) refreshed() {
	p.health.Refreshed(p.digest)
}
