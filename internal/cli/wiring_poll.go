package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// OPT-IN POLLING for a wiring change somebody else pushed.
//
// An instance is wired by the database it opens (wiring_boot.go), and the read
// that wires it happens exactly once, at the boot. That is the whole behaviour
// for every deployment that does not ask for more: a `aperture wiring push` from
// another host is picked up by restarting the instances, which is what a
// deployment with one instance and a deploy pipeline already does.
//
// # Why it is OFF unless configured, and what "off" costs
//
// Off means off: no goroutine is started and no periodic read is made. A
// single-instance deployment therefore pays nothing at all for a feature it
// cannot use — not a timer, not a read of five tables on a schedule, not a
// stderr line. That is asserted by COUNTING STORE READS with a fake
// (wiring_poll_test.go), not by reading the configuration back, because the
// property an operator cares about is the reads and not the flag.
//
// # What a tick does, and what it deliberately does not
//
// A tick re-reads the shared wiring (the same readSharedWiring a boot takes, for
// the same reason: ONE atomic snapshot, never five List* calls) and compares a
// DIGEST of it against the digest the running stack was built from. A change is
// ADOPTED: the tick hands the set to the swapper, which rebuilds every registry
// this process decides through and installs the result as one immutable version
// (wiring_swap.go). The digest advances only once that has SUCCEEDED, so a failed
// rebuild leaves the change outstanding and the next tick tries again.
//
// The seams the rest of the epic builds on are named where they are:
//
//   - The CONNECTION NAME SET is frozen for the life of a process, so a set whose
//     connections: manifest differs from the one this instance resolved routes for
//     at boot is refused WHOLE by liveWiring.swap before anything is rebuilt, in
//     either direction. The refusal reaches the swap branch below like any other
//     failed adoption and is recorded on the service.WiringHealth with its own
//     APERTURE_WIRING_RESTART_REQUIRED, so a posture reader sees the standing
//     condition without watching this writer. There is deliberately no second
//     latch beside it: one existed, could be cleared only by a successful swap,
//     and so went on reporting "restart required" after the operator withdrew the
//     push — where the health record clears on the very next no-change tick.
//   - E4-S4 (last-good on failure, and the alarm) owns tick's failure branches,
//     and has landed: a failure records an alarm on a service.WiringHealth and
//     keeps the wiring it has, and a successful refresh clears it. wiring_stale.go
//     holds the whole account. There is ONE recorder and one LINE PER CONDITION:
//     a failed adoption records through p.alarmf with the adoption's own sentence
//     and p.digest left exactly where it is, a read or digest failure through
//     p.alarm with the read's, and a successful swap advances p.digest and calls
//     p.refreshed().
//
// # Why the change check is a digest of a full read
//
// See wiringDigest. The short version is that a cheaper PROBE is not available
// without a new model.Storage method, and every probe that could be built out of
// what is there can be wrong in the direction that matters — a change this
// instance does not see is a silently stale instance, which is the hazard the
// whole epic exists to close. So the READ is full and the COMPARISON is cheap,
// which is the half that keeps a tick from reconstructing anything.

// wiringPollFlagName is the single spelling of the flag. The flag, its reader and
// the docs all name this constant, so a command cannot declare one spelling while
// wiringPollInterval reads another — which fails silently, as a knob nobody reads
// and a loop nobody started.
const wiringPollFlagName = "wiring-poll"

// envWiringPoll is the environment variable --wiring-poll reads when the operator
// did not type the flag. It is named in the flag's usage text (the convention
// TestServeFlagsNameTheirEnvVars pins), so this constant is the one spelling the
// flag, the help output and the tests share.
//
// Env-configurable for the reason CLAUDE.md gives for APERTURE_POSTGRES_SCHEMA:
// whether this instance polls, and how often, is a property of the DEPLOYMENT and
// not of an invocation. The flag exists alongside it so `aperture serve --help`
// can discover the variable at all, and so a single container can be run with an
// override without editing its environment.
const envWiringPoll = "APERTURE_WIRING_POLL"

// wiringPollOn and wiringPollOff are the two WORDS the interval accepts beside a
// Go duration.
//
// `on` is what gives defaultWiringPollInterval a job: an operator who wants their
// fleet to notice a push, and has no opinion about how often, should not have to
// invent a number, and a number invented under that pressure is the one nobody
// revisits. `off` is the other half of the same argument, from the other
// direction: an environment variable can be overridden but often cannot be
// UNSET — a child container inherits its parent's environment — so "off" has to
// be sayable, or a deployment that wanted polling in staging and not in
// production would have to express it by unsetting a variable it does not control.
const (
	wiringPollOn  = "on"
	wiringPollOff = "off"
)

// defaultWiringPollInterval is how often an instance re-reads the shared wiring
// when polling is switched on with no interval (`--wiring-poll on`).
//
// # What the number is
//
// It is the window a fleet is allowed to DISAGREE WITH ITSELF. From the moment
// one operator pushes wiring that removes a provider, narrows an attribute slot
// or re-points a connection, until this instance re-reads it, this instance is
// still answering from the wiring it booted on. That makes the interval the same
// KIND of number as an attribute slot's `ttl:` — a bound on how long a revoked
// thing keeps being honoured — and not a performance knob.
//
// # Why 30 seconds and not a minute, and not a second
//
// Longer is the more tempting mistake and the worse one. At five minutes a push
// reads as having had no effect: the operator pushes, checks `aperture wiring
// show`, sees the row, sees the instance still behaving the old way, and reaches
// for a rolling restart — which is exactly the act polling exists to remove, now
// performed with less confidence than before because two mechanisms are in play.
//
// Shorter buys nothing measurable. A push is a human act at human cadence, and no
// operator perceives half a minute as slow after typing one. A one-second interval
// would have every instance in the fleet read five tables every second, forever,
// against wiring that changes perhaps weekly — twenty instances is 100 reads a
// second of a table set nobody wrote to — and the instance still cannot answer a
// decision any more correctly for it.
//
// Thirty seconds also matches the cadence an operator already has a mental model
// for: it is where readiness and health probes sit, so "within half a minute of
// the push" needs no new unit of trust.
//
// # What happens when it is omitted
//
// Omitting the setting ENTIRELY is not this default — it is polling off, and the
// instance is boot-only. This default applies only when polling was asked for
// without an interval.
const defaultWiringPollInterval = 30 * time.Second

// wiringPollCloseGrace and wiringPollCloseWait bound how long a stopping process
// waits for a tick that is still running, and are the answer to a shutdown that
// used to be unbounded and silent.
//
// # What was wrong with waiting forever
//
// Close cancels the loop's context and waits for the goroutine, and NOTHING inside
// a rebuild is cancellable: seedDocument reads the --seed file, csvprovider opens
// every declared CSV, and the registries are constructed from what they hold. A
// SIGTERM landing while a tick rebuilds over a large CSV provider on a stalled
// network mount therefore let httpServer.Shutdown complete within shutdownTimeout
// and then parked the process inside Close for the rebuild's duration, with nothing
// on stderr, exiting only on SIGKILL. An orchestrator's termination grace period is
// sized against shutdownTimeout; that exceeded it and reported nothing.
//
// # Why two numbers
//
// The GRACE is silence. A loop parked in its own select returns within
// microseconds of the cancel, which is what happens on essentially every shutdown,
// and a line printed for that would be noise on every restart of every deployment
// — and noise is how the line that matters gets skipped.
//
// The WAIT is the bound, and 5s against shutdownTimeout's 10s is chosen so the
// worst case of the two together stays inside the 30s termination grace an
// orchestrator gives by default. Longer buys the rebuild nothing: it either
// finishes quickly or it is blocked on I/O that is not coming back. Shorter would
// abandon rebuilds that were about to finish, for no gain — nothing waits on this
// but the exit.
//
// Abandoning is safe in the direction that counts. A version is installed whole or
// not at all (wiring_swap.go), so a rebuild interrupted by the process exiting
// installs nothing; the pools it borrowed belong to the boot, which serve closes
// once; and whatever it was going to adopt is re-read by the next process to start.
const (
	wiringPollCloseGrace = 100 * time.Millisecond
	wiringPollCloseWait  = 5 * time.Second
)

// wiringPollFlag is the one declaration of --wiring-poll, constructed fresh per
// command because a ucli.Flag carries parse state and must not be shared between
// two commands in one tree.
//
// It is a ucli.StringFlag carrying an env source and parsed by hand
// (wiringPollInterval), NOT a ucli.DurationFlag, for the reason enumerate_limit.go
// spells out at length for --enumerate-limit: urfave parses a typed flag's env
// source itself and fails the command with its own UNCODED error before the action
// runs, so APERTURE_WIRING_POLL=banana would report something other than
// APERTURE_CONFIG_INVALID. A string flag has no parse to fail, so the malformed
// value reaches this package and becomes a coded error with fixups. Keeping the
// EnvVars source is what leaves precedence as urfave's native flag > env >
// default, rather than a second resolution order written out here that could drift
// from the one every other flag obeys.
//
// It is declared on `serve` and on `serve` only, which is the one deliberate
// difference from --enumerate-limit. The bound describes the deployment and every
// command that DECIDES must agree about it; a poll interval describes a PROCESS
// THAT OUTLIVES A DECISION, and there is no tick in the life of `aperture check`
// for one to happen on. Attaching it to the one-shot commands would put a knob in
// six help texts that moves nothing in five of them.
func wiringPollFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name: wiringPollFlagName,
		Usage: "re-read the shared wiring tables on an interval instead of only at startup, so a `wiring push` from another host is noticed without a restart. " +
			"A Go duration (\"30s\", \"5m\"), or " + wiringPollOn + " for the default of " + defaultWiringPollInterval.String() + ", or " + wiringPollOff + ". " +
			"Omitted means OFF — the instance is wired once, at boot, and starts no background reader (overrides " + envWiringPoll + ")",
		Sources: ucli.EnvVars(envWiringPoll),
	}
}

// wiringPollInterval resolves how often this process re-reads the shared wiring,
// in precedence order: off, overridden by APERTURE_WIRING_POLL, overridden by a
// --wiring-poll the operator actually typed.
//
// ok is false when polling is off, which is the default and the state every
// deployment that exists today is in: no goroutine, no periodic read, boot-only
// wiring. A cmd that does not declare the flag reads the empty string and
// therefore resolves to off, which is why the flag is attached per command rather
// than read from os.Getenv here — reading the environment directly would honour
// the variable on commands whose --help could not be used to discover it.
//
// The accepted vocabulary is small and deliberately closed:
//
//	""            -> off (the default; also what an empty variable says)
//	"off"         -> off, said out loud
//	"0", "0s"     -> off. A zero interval IS "never", and an operator who wrote it
//	                 gets exactly what they wrote. This is the one place the
//	                 --enumerate-limit precedent is not followed, and for its own
//	                 reason: there 0 had to be refused because the library would
//	                 have absorbed it into a DIFFERENT number (1000), leaving the
//	                 operator believing a bound they did not get. Here 0 is not
//	                 quietly turned into something else.
//	"on"          -> defaultWiringPollInterval
//	a Go duration -> that interval, verbatim
//
// A NEGATIVE duration is refused rather than read as off: "-5m" is not a statement
// anybody makes on purpose, and silently treating it as off would leave an
// operator who meant to switch polling on believing they had.
//
// There is deliberately no MINIMUM. A floor would be a second invented number,
// and it would refuse the millisecond intervals a test (and this package's own
// tests) legitimately drive the loop at. What a very short interval costs is
// written in the docs instead, where an operator can read it, rather than enforced
// as a refusal they cannot override.
func wiringPollInterval(cmd *ucli.Command) (time.Duration, bool, error) {
	raw := strings.TrimSpace(cmd.String(wiringPollFlagName))
	switch {
	case raw == "", strings.EqualFold(raw, wiringPollOff):
		return 0, false, nil
	case strings.EqualFold(raw, wiringPollOn):
		return defaultWiringPollInterval, true, nil
	}
	every, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, badWiringPoll(raw, "is not a Go duration")
	}
	if every < 0 {
		return 0, false, badWiringPoll(raw, "must not be negative")
	}
	if every == 0 {
		return 0, false, nil
	}
	return every, true, nil
}

// badWiringPoll builds the single refusal both --wiring-poll rejections share, so
// an unparseable value and a negative one read identically apart from the reason.
//
// The setting and the rejected value go in the MESSAGE and not only in the context
// map, for the reason badEnumerateLimit gives: nothing on the CLI path renders a
// CodedError's Context, so an operator who mistyped one of two spellings would be
// told which code failed but not which knob or what it read. The value is the
// operator's own input and carries no account data.
func badWiringPoll(raw, why string) error {
	return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
		fmt.Sprintf("cli: --%s / %s %s: %q", wiringPollFlagName, envWiringPoll, why, raw),
		map[string]any{
			"setting": "--" + wiringPollFlagName + " / " + envWiringPoll,
			"value":   raw,
			"valid": "a Go duration (\"30s\", \"5m\"), " + wiringPollOn + " for the default of " +
				defaultWiringPollInterval.String() + ", or " + wiringPollOff,
			"default": "off — the setting may simply be omitted, and the instance is then wired once at boot",
		})
}

// wiringDigest is the value a tick compares: a stable content digest of a whole
// wiring snapshot, with the STAMPS left out.
//
// # Why a digest of a full read, and not a cheaper probe
//
// The honest answer is that a full read is the only sound option available, and
// the cheap half is the COMPARISON — which is what keeps a tick from
// reconstructing five registries to find out whether it needed to.
//
// Everything cheaper was considered against one requirement: a probe MUST NOT MISS
// A CHANGE, because a missed change is an instance that is silently stale while
// reporting itself healthy, which is the hazard this epic exists to close. None of
// the available probes clears it:
//
//   - A max(updated_at) probe would need a new model.Storage method (there is no
//     stamp-only read), so it is a three-backend interface change plus a
//     conformance-suite case — and it would still be WRONG. The stamps are written
//     by whichever host ran the push, so a fleet's stamps are several machines'
//     clocks: a push from a host whose clock lags writes rows OLDER than the ones
//     it replaced, and a max-stamp probe reports "unchanged" on wiring that
//     changed. storage/storagetime makes the encoding exact; it does not make the
//     writers agree about what time it is.
//   - A row-count probe misses every change that keeps the count: a re-pointed
//     GetOne, a narrowed declared key set, a connection renamed in place.
//   - A per-section List* probe is four reads that can straddle a concurrent
//     ReplaceWiring and compose a set that never existed — the same reason
//     readSharedWiring and pullWiringSnapshot both take one GetWiring.
//
// So a tick pays one GetWiring, which is one snapshot of five tables against an
// index — the read a boot already makes once — and the comparison it feeds is a
// 32-byte string equality.
//
// # Why the stamps are excluded
//
// ReplaceWiring is wholesale: every push rewrites every row, stamps included. A
// digest that included them would report a CHANGE for a push that deployed
// byte-identical wiring — a re-run of the same pipeline, a re-apply of the same
// document — and E4-S2 would then rebuild registries and reopen pools to arrive
// at what it already had. The question a tick asks is "would this instance be
// wired differently?", and a stamp cannot change that answer.
//
// Excluding them is also the SAFE DIRECTION to be wrong in. Forgetting to exclude
// a future stamp-shaped field costs an unnecessary rebuild; forgetting to INCLUDE
// a content field costs a missed change. That is why the content half is not an
// explicit field list at all: the digest marshals the value with encoding/json,
// which walks every exported field by reflection, so a column added to the wiring
// model is in the digest without anyone remembering to add it. The stamp half IS
// an explicit list (wiringWithoutStamps), because that is the half whose failure
// mode is merely wasteful. TestTheDigestCoversEveryContentFieldAndNoStamp holds
// both halves.
//
// # What it does NOT cover, and why that stays true
//
// It is a digest of model.WiringSet: the five SHARED tables and nothing else. It
// covers no part of an instance's LOCAL --seed file, and a rebuild re-reads that
// file (buildWiredStack -> seedDocument -> seed.ParseFile), so the two local
// sections — inline objects: metadata and inline attributes: bags, plus the declared
// attribute-key sets taken from them — can differ between two instances reporting
// the same digest. That is stated in docs/src/cli/serve.md ("The digest covers the
// shared set only"), skills/shared-wiring.md and
// docs/src/operations/wiring-refresh.md, because a reader who assumes the digest is
// a whole-configuration fingerprint will build a fleet sweep on it that answers a
// question it cannot answer.
//
// Folding a local digest INTO this value is the tempting fix and the wrong one: this
// digest's job is to compare against what a push produced and what `aperture wiring
// diff` reports, and a value mixed with per-instance content matches neither. A
// separate local digest on service.WiringPosture is the shape that would work; it is
// not here because it is a wire-surface change.
//
// The snapshot is sorted first. GetWiring already returns canonical order, so this
// is belt-and-braces for a caller holding a set it assembled itself — but it is
// what makes the digest a property of the WIRING rather than of the read, so two
// instances, or one instance across two ticks, cannot disagree because a backend
// returned the same rows in a different order.
func wiringDigest(set model.WiringSet) (string, error) {
	clean := wiringWithoutStamps(set)
	clean.Sort()
	raw, err := json.Marshal(clean)
	if err != nil {
		// Unreachable for a model.WiringSet: encoding/json fails on channels,
		// functions, cyclic values and non-string-keyed maps, and the wiring model is
		// strings, integers, slices of the same and time.Time — the shapes the TEXT
		// and INTEGER columns behind it can hold. It is coded rather than returned
		// bare all the same, because a digest that could not be computed must never
		// be papered over with a constant: a constant digest freezes change detection
		// permanently ON or permanently OFF, and the second one is the silent stale
		// instance.
		return "", aerr.Wrap(aerr.APERTURE_BOOT, "cli: digesting the shared wiring failed", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// wiringWithoutStamps copies set with every CreatedAt/UpdatedAt zeroed, so a
// digest is taken over the wiring and not over when it was written.
//
// It COPIES rather than zeroing in place: the caller's set is the one
// buildDecisionStack goes on to project into a document, and a digest must not be
// able to change what an instance is wired with.
//
// This is the one place the stamp fields are named. See wiringDigest for why that
// is acceptable here and would not be for the content fields: a stamp this list
// misses costs a needless rebuild, and a content field an explicit list missed
// would cost a missed change.
func wiringWithoutStamps(set model.WiringSet) model.WiringSet {
	out := model.WiringSet{
		Connections:        make([]model.WiringConnection, len(set.Connections)),
		Providers:          make([]model.WiringProvider, len(set.Providers)),
		FieldTypes:         make([]model.WiringFieldType, len(set.FieldTypes)),
		AttributeProviders: make([]model.WiringAttributeProvider, len(set.AttributeProviders)),
	}
	copy(out.Connections, set.Connections)
	copy(out.Providers, set.Providers)
	copy(out.FieldTypes, set.FieldTypes)
	copy(out.AttributeProviders, set.AttributeProviders)

	for i := range out.Connections {
		out.Connections[i].CreatedAt, out.Connections[i].UpdatedAt = time.Time{}, time.Time{}
	}
	for i := range out.Providers {
		out.Providers[i].CreatedAt, out.Providers[i].UpdatedAt = time.Time{}, time.Time{}
		// The reference rows are OWNED by the entry and carry no stamps of their
		// own, but the slice header is shared with the caller's set and Sort
		// reorders it in place. Copy it too, or digesting somebody's wiring would
		// reorder their references: harmless today, and exactly the kind of
		// action-at-a-distance a read-only helper must not have.
		refs := make([]model.WiringReference, len(out.Providers[i].References))
		copy(refs, out.Providers[i].References)
		out.Providers[i].References = refs
	}
	for i := range out.FieldTypes {
		out.FieldTypes[i].CreatedAt, out.FieldTypes[i].UpdatedAt = time.Time{}, time.Time{}
	}
	for i := range out.AttributeProviders {
		out.AttributeProviders[i].CreatedAt, out.AttributeProviders[i].UpdatedAt = time.Time{}, time.Time{}
		// DeclaredKeys.Clone preserves the declared-but-empty state a plain slice
		// copy would collapse to nil — which is a DIFFERENT ANSWER (model.DeclaredKeys)
		// and therefore has to survive into the digest.
		out.AttributeProviders[i].DeclaredKeys = out.AttributeProviders[i].DeclaredKeys.Clone()
	}
	return out
}

// wiringPoll is the background re-reader: a goroutine, a ticker, and the digest of
// the wiring the running stack was built from.
//
// It is constructed by startWiringPoll and stopped by Close, both of which are
// nil-safe, so a caller wires it unconditionally and the "polling is off" path is
// a nil pointer rather than a branch at every call site.
type wiringPoll struct {
	store model.Storage
	every time.Duration
	// swap adopts a changed set: it rebuilds everything a decision reads and
	// installs it as one version, or installs nothing and says why
	// (liveWiring.swap). It is REQUIRED — a poller with nothing to swap into is a
	// periodic read of five tables whose answer is discarded, which is the one thing
	// the default-off design above exists to avoid.
	swap wiringSwapper
	// log is where a change, and a failure to look for one, are reported. It is
	// stderr under `serve` — the same writer reportCollisions uses — because this
	// is operational narration and stdout is a surface's output.
	log io.Writer

	// health is where a failed refresh is RECORDED, as distinct from merely
	// reported: it is what service.Service.WiringPosture answers from, so an
	// operator reads the staleness and — the half they escalate on — its DURATION
	// without tailing this poller's stderr. Every method on it is nil-safe, so a
	// caller that wires no recorder costs nothing and branches nowhere. See
	// wiring_stale.go for why the alarm exists at all and why it is not a
	// Capabilities boolean.
	health *service.WiringHealth

	// digest is the digest of the wiring THIS PROCESS IS RUNNING. It is owned by
	// the loop goroutine (and by whichever single goroutine drives tick in a test),
	// never read from outside, which is what keeps the type free of a mutex.
	digest string

	// cancel and done are the shutdown pair Close drives: cancel trips the loop's
	// context, done is closed by the goroutine as it returns. Close waits on it, so
	// a returned Close is normally a proof the goroutine is gone rather than a
	// request that it go — see Close for the one bounded exception.
	cancel context.CancelFunc
	done   chan struct{}

	// closeGrace and closeWait are wiringPollCloseGrace / wiringPollCloseWait, held
	// per poller so a test can assert the BOUND itself without spending the real one.
	// They are read by Close, on the caller's goroutine, and never by the loop. A
	// non-positive value falls back to the constant, so a poller built by hand cannot
	// accidentally have no bound at all.
	closeGrace time.Duration
	closeWait  time.Duration

	// ticks and changes are observability for the tests, and the reason they are
	// atomics is that the loop writes them while a test reads them. They are what
	// lets the default-off case be asserted on OBSERVED BEHAVIOUR rather than on
	// configuration, alongside the store-read count.
	//
	// changes counts ADOPTED changes, not detected ones. A change detected and then
	// refused by the rebuild is not a change this instance made, and counting it
	// would make the counter agree with the digest — which does not advance either —
	// rather than with what the instance is deciding through.
	ticks   atomic.Int64
	changes atomic.Int64
}

// wiringSwapper adopts a wiring set a tick has just read: rebuild everything a
// decision reads, install it as ONE immutable version, and report what stopped it
// if anything did. liveWiring.swap is the implementation; see wiring_swap.go for
// why the unit installed is a whole version and not a set of fields.
//
// digest is passed in rather than recomputed so that the value the tick compared,
// the set the version is built from and the digest the version records are one
// read of the database.
type wiringSwapper func(ctx context.Context, set model.WiringSet, digest string) error

// noWiringSwap is the swapper a poller falls back to when it was started without
// one. It refuses every change, which keeps the instance deciding through the
// wiring it has and keeps the digest from advancing, so the condition is reported
// on every tick rather than becoming a silently stale instance.
func noWiringSwap(context.Context, model.WiringSet, string) error {
	return aerr.New(aerr.APERTURE_BOOT,
		"cli: this process was started with no way to adopt a wiring change; restart it to pick one up")
}

// startWiringPoll starts the background re-reader, or returns nil when polling is
// off.
//
// Returning nil for "off" is the whole design of the off path: there is no
// goroutine to stop, no ticker to drain and no store read to count, and a caller
// cannot accidentally leave a disabled poller running because there is nothing
// running. Close is nil-safe, so the caller's defer needs no condition either.
//
// booted is the digest of the wiring the stack was built from, and it is taken
// from the BOOT (decisionStack.wiringDigest) rather than from the loop's own first
// read. The difference is not cosmetic: a push that lands between the boot read
// and the first tick would, on a self-baselining loop, become the baseline — the
// change would never be reported and the instance would be stale for its whole
// lifetime with nothing anywhere saying so. That is precisely the failure this
// epic exists to close, so the baseline is the one thing the loop is not allowed
// to decide for itself.
//
// swap is how a detected change is adopted. It is required whenever polling is on
// — see the field's comment — and is not consulted at all on the off path, which is
// why the nil-interval return above comes first.
//
// ctx governs the loop's lifetime as well as Close does: under `serve` it is the
// signal context, so a SIGINT stops the reader at once and the deferred Close then
// only waits for it.
// health is the recorder a failure is announced on, and the caller must hand the
// poller the SAME pointer it handed service.WithWiringHealth: a facade holding a
// recorder of its own would answer for a loop that never wrote to it, and report
// a permanently healthy instance no matter what the loop observed — the silent
// staleness this epic exists to close, reintroduced one layer up. It may be nil,
// which records nothing and reports only on stderr.
func startWiringPoll(ctx context.Context, store model.Storage, every time.Duration, booted string, swap wiringSwapper, log io.Writer, health *service.WiringHealth) *wiringPoll {
	if every <= 0 {
		return nil
	}
	if swap == nil {
		// Defaulted rather than left nil, because the alternative is a nil call in a
		// background goroutine the first time somebody pushes — and a panic there takes
		// the whole process down, which for an embedded access engine means the host
		// stops deciding. The fallback refuses loudly on every tick instead: E4-S4's
		// contract is that an instance keeps deciding and that staleness is never
		// silent, and both halves survive a caller's omission this way.
		swap = noWiringSwap
	}
	loopCtx, cancel := context.WithCancel(ctx)
	p := &wiringPoll{
		store:      store,
		every:      every,
		swap:       swap,
		log:        log,
		health:     health,
		digest:     booted,
		cancel:     cancel,
		done:       make(chan struct{}),
		closeGrace: wiringPollCloseGrace,
		closeWait:  wiringPollCloseWait,
	}
	p.report("wiring poll: re-reading the shared wiring every %s; a change will be adopted and reported here", every)
	go p.run(loopCtx)
	return p
}

// Close stops the reader and waits for it to be gone, for a BOUNDED time. It is
// nil-safe and idempotent, so `defer poll.Close()` is unconditional and a caller
// that stops explicitly on shutdown may also defer it — the same contract
// decisionStack.Close has, for the same reason.
//
// The bound is the fix for a shutdown that could hang. Cancelling the context stops
// the loop between ticks at once, but a tick already inside a REBUILD is not
// interruptible — see wiringPollCloseWait — so an unbounded wait here put the
// process's exit at the mercy of a file read on a stalled mount, silently, after
// httpServer.Shutdown had already returned. Now it waits, says so if the wait is
// long enough to notice, and stops waiting.
//
// Abandoning the goroutine does not break "a version is installed whole or not at
// all": the rebuild is in a background goroutine of a process that is exiting, it
// installs nothing it had not already installed, and its pools are borrowed from
// the boot. What it costs is that a returned Close is a proof the goroutine is gone
// only when it did not time out — and when it did, it said so on stderr, which is
// the one thing the old behaviour did not do.
//
// It returns an error only to fit the defer-and-ignore shape every other Close in
// this package has. A timed-out wait is deliberately NOT one: every caller ignores
// it, an error nobody reads is not a report, and failing a shutdown over a
// background rebuild would turn a bounded exit into a non-zero one.
func (p *wiringPoll) Close() error {
	if p == nil {
		return nil
	}
	p.cancel()

	grace, wait := p.closeGrace, p.closeWait
	if grace <= 0 {
		grace = wiringPollCloseGrace
	}
	if wait <= 0 {
		wait = wiringPollCloseWait
	}

	// The silent path, which is every ordinary shutdown: a loop parked in its own
	// select returns within microseconds of the cancel.
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}

	// Past the grace means a tick is in flight, and the only step in one long enough
	// to be noticed is a rebuild. Whichever way it ends, it is now legible: silence
	// was the worst part of the old behaviour, because the symptom was a process that
	// simply took minutes to exit.
	p.report("wiring poll: a wiring refresh is still in flight; waiting up to %s for it before this process exits", wait)
	select {
	case <-p.done:
		p.report("wiring poll: the refresh in flight finished, and this process is stopping")
		return nil
	case <-time.After(wait):
		p.report("wiring poll: the refresh in flight did not finish within %s, so this process stops without "+
			"waiting for it; a wiring version is installed whole or not at all, so it installed nothing", wait)
		return nil
	}
}

// run is the loop. It reads nothing before its first tick, because the boot has
// just read: a read here would be the second read of the same rows in the same
// second, on every instance, at every deploy.
func (p *wiringPoll) run(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The tick takes the loop's own context, so a shutdown cancels a read in
			// flight rather than waiting for it. What the tick then does with that
			// cancellation is the distinction abandonedDuringShutdown draws: "the
			// database went away" and "we are shutting down" must not become the same
			// SILENCE, and they must not become the same ALARM either.
			p.tick(ctx)
		}
	}
}

// tick performs one re-read and reports whether the deployed wiring changed AND
// was adopted.
//
// It returns the answer as well as reporting it, which is what lets a test assert
// the adoption synchronously instead of racing a ticker.
//
// The digest advances AFTER a successful swap, and only then. That ordering is the
// whole of last-good: a digest advanced before the rebuild would mean an instance
// whose rebuild failed forgets there was ever anything to pick up, and it would
// then sit on its old wiring for the rest of its life reporting nothing — the
// silently stale instance this epic exists to close. Advancing after means a
// refused change is re-detected and re-reported on every tick until it is adopted
// or the push is corrected, which is noisy in exactly the direction an operator
// needs. E4-S4 turns that repetition into an alarm with a staleness duration.
//
// The alarm follows the same rule, and there are exactly TWO places a tick declares
// a refresh COMPLETE: the no-change branch, and after a successful swap. Each
// failure branch records the alarm ONCE and prints ONE line, in the words that fit
// that branch — the read's are not the adoption's. A tick
// that read the tables, found a change and could not adopt it has not completed
// anything — it is the worst of the three postures — so nothing on that path
// clears, and the staleness window it opens runs continuously from the first
// refusal. Clearing on the strength of the READ alone (before the change branch)
// leaves the alarm firing but resets its age and its count on every tick, which
// reports a four-hour refusal as one failure a tick old.
func (p *wiringPoll) tick(ctx context.Context) bool {
	p.ticks.Add(1)

	set, err := readSharedWiring(ctx, p.store)
	if err != nil {
		if p.abandoned(ctx, err, "re-reading the shared wiring") {
			return false
		}
		// Last-good, and LOUD. Nothing is swapped, so the instance keeps deciding
		// with the wiring it has; the digest deliberately does not advance, so the
		// next successful read still sees a change this one could not confirm; and
		// the alarm is RECORDED as well as reported, so the staleness and its age
		// are readable without tailing stderr. alarm passes the store's own code
		// through untouched — see wiring_stale.go.
		p.alarm(err)
		return false
	}
	digest, err := wiringDigest(set)
	if err != nil {
		p.alarm(err)
		return false
	}
	if digest == p.digest {
		// The store answered, its wiring digested, and it says this process is running
		// what the deployment deployed. That is a completed refresh and it clears any
		// standing alarm — including one raised by a read that failed an hour ago, and
		// including on the overwhelmingly common tick where nothing changed at all,
		// which is the answer on almost every tick of almost every deployment.
		p.refreshed()
		return false
	}
	previous := p.digest
	// The rebuild happens HERE, on the poll goroutine, and not on any decision's
	// path: it reads the seed file, projects the set into a document and constructs
	// two registries, a rules engine, a decision engine and a facade, while every
	// decision in flight goes on answering through the version this process already
	// has. Only the final pointer store is visible to a reader, and it is atomic.
	if err := p.swap(ctx, set, digest); err != nil {
		if p.abandoned(ctx, err, "adopting the deployed wiring") {
			return false
		}
		// Last-good, and the digest deliberately does NOT advance — see the doc
		// comment.
		//
		// ONE line, and it is this branch's OWN sentence. The alarm is recorded first
		// and reported second (alarmf), and the report says what is true HERE: the
		// re-read succeeded and the ADOPTION did not. Routing this through the generic
		// alarm would emit a second line claiming the re-read failed, which sends an
		// operator to check store reachability and schema compatibility when the
		// remedy is the restart APERTURE_WIRING_RESTART_REQUIRED's fixups name — and a
		// refused connection-name change never clears by itself, so the wrong line
		// would print on every tick for the life of the process.
		//
		// The error's own text is interpolated verbatim because it is already an
		// APERTURE_*-coded refusal naming the entry to go and fix (an unconstructable
		// kind, a connection NAME SET this process cannot adopt, a seed file that has
		// since been edited into an invalid one).
		//
		// That it is RECORDED at all is the load-bearing half, and NOTHING has cleared
		// it on the way here. A failed adoption is the worst posture of the three: the
		// instance read the tables perfectly well, knows the wiring changed, and is
		// KNOWINGLY running superseded wiring — strictly worse than not having looked.
		// Without the record it would report on stderr and read as HEALTHY, which is
		// the one shape of silent staleness no amount of polling discovers, because
		// every subsequent tick reads fine, fails to adopt again, and says nothing.
		//
		// Which is also why refreshed() is NOT called before this branch. Clearing on
		// a successful READ and re-arming here would leave the alarm technically
		// correct and its NUMBERS useless: Refreshed zeroes the window and the count,
		// so a push refused for four hours would report one failure and an age of one
		// tick, forever — and the age is the half an operator escalates on. Staleness
		// that began at the first refusal is CONTINUOUS until an adoption succeeds, so
		// nothing on this path is allowed to reset it.
		p.alarmf(err, "wiring poll: the deployed wiring CHANGED (%s -> %s) but this instance could not adopt it, so it "+
			"keeps the wiring it has and goes on deciding: %v", shortDigest(previous), shortDigest(digest))
		return false
	}
	p.digest = digest
	p.changes.Add(1)
	// The adoption succeeded, so this is the completed refresh — and only now is
	// p.digest the wiring this process runs, which is what refreshed() must be told.
	// Clearing here rather than before the swap is what makes the posture's digest
	// right from the instant of the adoption instead of one tick later.
	p.refreshed()
	p.report("wiring poll: the deployed wiring CHANGED (%s -> %s) and this instance ADOPTED it; decisions already in "+
		"flight finish on the wiring they started with", shortDigest(previous), shortDigest(digest))
	return true
}

// abandoned distinguishes a step THIS LOOP cancelled from a step that failed, and
// says so on stderr without recording an alarm.
//
// A tick takes the loop's own context, so SIGTERM landing while readSharedWiring
// is in flight returns context.Canceled. Alarming on it made a CLEANLY TERMINATING
// instance report itself stale: service.WiringHealth would open a staleness window
// with Reason "context canceled", and httpServer.Shutdown then drains for up to
// shutdownTimeout, during which every WiringPosture read answers Stale=true. A
// fleet sweep taken across a rolling restart would see every instance being
// replaced reported as degraded, which is the false positive that trains an
// operator to stop reading the channel — silent staleness by the longest route
// there is.
//
// BOTH conditions are required, and each rules out a different mistake:
//
//   - ctx.Err() != nil, so only OUR OWN cancellation qualifies. A driver that
//     surfaces a context error for a reason of its own, while this loop's context
//     is alive, is a fault and is alarmed.
//   - the error really IS a context error, so a read that failed for a REAL reason
//     and only then noticed the shutdown is still alarmed. That ordering is the one
//     a store that is going away at the same moment as the process produces, and
//     losing it would make "the database died during a deploy" the one failure
//     nothing anywhere records.
//
// It is NOT silent. Nothing is recorded, because there is nothing for an operator
// to act on and no window to open — the instance is stopping, not deciding from
// superseded wiring — but the line is written, so a shutdown that abandoned a
// refresh is legible in the same place the refresh itself would have been.
func (p *wiringPoll) abandoned(ctx context.Context, err error, what string) bool {
	if ctx.Err() == nil {
		return false
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	p.report("wiring poll: %s was abandoned because this process is shutting down; nothing is stale and no alarm "+
		"is recorded", what)
	return true
}

// report writes one operational line, and is a no-op when the caller wired no
// writer. Only digests, durations and the store's own coded errors reach it —
// never an object type, an id or a key — so a poll line cannot become a channel
// for cross-account data.
func (p *wiringPoll) report(format string, args ...any) {
	if p.log == nil {
		return
	}
	fmt.Fprintf(p.log, format+"\n", args...)
}

// shortDigest is the prefix a report names a digest by. A digest is not a secret —
// it is a hash of wiring the operator pushed — but 64 hex characters twice on one
// line is unreadable, and 8 is plenty to tell two of them apart by eye.
func shortDigest(d string) string {
	if len(d) <= 8 {
		return d
	}
	return d[:8]
}
