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
// # One RECORDER, one LINE per condition
//
// There is ONE recorder. Every failure origin in a tick records through
// wiringPoll.alarm / wiringPoll.alarmf, and wiringPoll.refreshed is the one way a
// tick records that a refresh completed. There is deliberately no
// per-failure-mode alarm. The origins — an unreachable store, a digest that could
// not be computed, and a change this instance read perfectly well and could not
// ADOPT (a connection name set it cannot renegotiate, a kind it cannot construct,
// a statement set a builder refuses, a seed file edited into an invalid one) — are
// ways of arriving at one fact: this process did not adopt what the tables say and
// is still deciding from what it had. An operator needs the fact, its age and its
// code; a taxonomy of which internal step declined would be several alarms to
// wire, several to document, and most to forget.
//
// The stderr LINE is the other half, and it is NOT one sentence. A recorded fact
// and a printed remedy are different things: the posture answers "is this instance
// behind, and for how long", where a line has to tell the operator what to go and
// do. "Re-reading the shared wiring failed" is exactly true of the read and the
// digest branches and FALSE of a refused adoption — there the re-read succeeded
// and the remedy is APERTURE_WIRING_RESTART_REQUIRED's restart, not a check of
// store reachability — so the adoption branch reports its own sentence through
// alarmf. What is fixed is that each condition emits EXACTLY ONE line: two lines
// per tick for one fact is how the wrong one gets read, and a refused
// connection-name change never clears by itself, so the pair would print forever.
//
// What does NOT come through here at all is a step this process's own shutdown
// cancelled. A tick takes the loop's context, so SIGTERM mid-read returns
// context.Canceled, and recording that made a cleanly terminating instance report
// itself stale for the whole of its Shutdown drain. That is a fault report about an
// orderly exit, and it is the false positive that teaches an operator to stop
// reading the channel. wiringPoll.abandoned is the discriminator, and it still
// writes a line — abandoning a refresh silently would be the other mistake.
//
// So a rebuild's failure path records through alarmf and its success path advances
// p.digest and calls p.refreshed(). A rebuild that fails must NOT advance
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

// alarm records and reports a refresh that could not even LOOK: the read failed,
// or the set it returned could not be digested. Its sentence names that condition
// and no other, which is why an adoption failure does not come through here — see
// alarmf.
//
// It returns nothing, because there is nothing a caller can do differently: a
// failed refresh always means "keep what we have and carry on", and giving the
// caller a value to branch on would invite a second policy.
func (p *wiringPoll) alarm(err error) {
	p.alarmf(err, "wiring poll: re-reading the shared wiring failed, so this instance keeps the wiring it has: %v")
}

// alarmf records a failed refresh and then reports it in the CALLER's words. It is
// the seam described in this file's header: every failure origin in a tick routes
// through here, and nothing else writes a failure to the health recorder.
//
// The order is record-then-report on purpose, and it is why the recording is not
// left to the caller beside its own p.report: the recorder is what an operator
// reads without logs, and the stderr line is best-effort narration, so a panic or
// a short write in the writer must not be able to lose the alarm. A branch that
// printed first and recorded second would invert that on the one path where it
// matters most — a refused push is the posture nothing else discovers.
//
// format's LAST verb is handed the CLASSIFIED alarm, so a caller cannot report a
// code the recorder did not record; any earlier verbs take args. Only digests,
// durations and the coded error's own text may reach the writer — never an object
// type, an id or a key — the same restriction every other line this poller writes
// carries.
func (p *wiringPoll) alarmf(err error, format string, args ...any) {
	alarm := wiringRefreshAlarm(err)
	p.health.Failed(alarm)
	p.report(format, append(args, alarm)...)
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
