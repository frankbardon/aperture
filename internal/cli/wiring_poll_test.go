package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"

	ucli "github.com/urfave/cli/v3"
)

// E4-S1: an instance can be TOLD to poll for wiring changes.
//
// The two properties this file exists to hold apart, and the first one is the
// load-bearing one:
//
//   - UNSET -> boot-only. No goroutine, and no periodic store read. Every
//     deployment that exists today is in this case and none of them opted into
//     anything, so the assertion is on OBSERVED STORE READS through a counting
//     fake — never on the configuration, which would pass just as well if the
//     loop read the database and threw the answer away.
//   - CONFIGURED -> the deployed wiring is re-read on the interval and a CHANGE is
//     detected, without rebuilding anything. The rebuild is E4-S2; a test here
//     that asserted a swapped registry would be asserting a story that has not
//     landed.
//
// The digest is tested harder than the loop is, deliberately. A loop that stops
// ticking is a visible failure — nothing is ever reported again. A digest that
// misses a field is invisible: the loop ticks forever, reports nothing, and the
// instance is silently stale while looking healthy, which is the exact hazard the
// epic exists to close. So TestTheDigestCoversEveryContentFieldAndNoStamp walks
// the wiring model by REFLECTION rather than naming fields, and fails on a field
// it has not been taught to vary.

// pollingWiringReads counts which wiring read an instance took, in the shape
// E5-S1's countingWiringReads established (embed a real model.Storage rather than
// fake 60-odd methods, so the reads it does not count still behave) with one
// difference that matters here: the counters are ATOMICS.
//
// The difference is not tidiness. In E5-S1 one goroutine did the reading; here the
// poll loop reads while the test asserts, so plain ints are a data race — and a
// race detector failure in the one suite that exercises a background goroutine
// would be this story's own bug reported as somebody else's flake. It is a separate
// type rather than an edit to that one so the two stories' suites stay independent.
type pollingWiringReads struct {
	model.Storage
	getWiring atomic.Int64
	listCalls atomic.Int64
}

func (c *pollingWiringReads) GetWiring(ctx context.Context) (model.WiringSet, error) {
	c.getWiring.Add(1)
	return c.Storage.GetWiring(ctx)
}

func (c *pollingWiringReads) ListWiringConnections(ctx context.Context) ([]model.WiringConnection, error) {
	c.listCalls.Add(1)
	return c.Storage.ListWiringConnections(ctx)
}

func (c *pollingWiringReads) ListWiringProviders(ctx context.Context) ([]model.WiringProvider, error) {
	c.listCalls.Add(1)
	return c.Storage.ListWiringProviders(ctx)
}

func (c *pollingWiringReads) ListWiringFieldTypes(ctx context.Context) ([]model.WiringFieldType, error) {
	c.listCalls.Add(1)
	return c.Storage.ListWiringFieldTypes(ctx)
}

func (c *pollingWiringReads) ListWiringAttributeProviders(ctx context.Context) ([]model.WiringAttributeProvider, error) {
	c.listCalls.Add(1)
	return c.Storage.ListWiringAttributeProviders(ctx)
}

// pollProbe is one instance's worth of this story: a real store behind a read
// counter, a stack booted through the real builder, and the poller the resolved
// flag produced (nil when polling is off).
type pollProbe struct {
	counting *pollingWiringReads
	stack    decisionStack
	poll     *wiringPoll
	out      *bytes.Buffer
}

// newPollProbe boots a stack and starts the poll exactly as runServe does: parse
// the interval from a PARSED command, build the stack, then start the loop from
// the stack's own baseline digest.
//
// The store is wrapped in pollingWiringReads so every case here can assert on the
// reads the instance actually made.
//
// ctx governs the loop; the caller cancels it to test shutdown, and t.Cleanup
// closes the poller either way so a failing case cannot leak the goroutine into
// the rest of the package's tests.
func newPollProbe(t *testing.T, ctx context.Context, storeDSN, seedPath string, argv ...string) *pollProbe {
	t.Helper()

	inner, err := buildStore(ctx, storeDSN, seedPath)
	if err != nil {
		t.Fatalf("buildStore(%q, %q): %v", storeDSN, seedPath, err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	probe := &pollProbe{counting: &pollingWiringReads{Storage: inner}, out: &bytes.Buffer{}}
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: append(storeFlags(), wiringPollFlag()),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			every, _, err := wiringPollInterval(cmd)
			if err != nil {
				return err
			}
			stack, err := buildDecisionStack(ctx, cmd, probe.counting, cmd.String("seed"))
			if err != nil {
				return err
			}
			probe.stack = stack
			probe.poll = startWiringPoll(ctx, probe.counting, every, stack.wiringDigest, probe.out)
			return nil
		},
	}
	args := []string{"probe", "--store", storeDSN}
	if seedPath != "" {
		args = append(args, "--seed", seedPath)
	}
	args = append(args, argv...)
	if err := cmd.Run(ctx, args); err != nil {
		t.Fatalf("booting the probe (%v): %v", args, err)
	}
	t.Cleanup(func() {
		_ = probe.poll.Close()
		_ = probe.stack.Close()
	})
	return probe
}

// waitFor polls cond until it holds or the deadline passes. The loop is here
// rather than a sleep because these cases are about a background goroutine: a
// fixed sleep is either slower than it needs to be or flaky on a loaded runner,
// and this is neither.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// TestPollingIsOffUnlessConfiguredAndTheStoreReadsProveIt is the story's
// load-bearing criterion, asserted the way the acceptance demands: by COUNTING
// STORE READS.
//
// Reading the configuration back would prove nothing. "Polling is off" is a claim
// about what the process does to the database, and a loop that read five tables on
// a timer and discarded the answer would satisfy every configuration assertion
// while costing every single-instance deployment in existence a periodic read it
// never asked for.
//
// The boot's own GetWiring is expected — exactly one, the read that wires the
// instance (readSharedWiring). Anything past one is a read this story promised not
// to make.
func TestPollingIsOffUnlessConfiguredAndTheStoreReadsProveIt(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "off.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "")

	if probe.poll != nil {
		t.Fatalf("polling is unconfigured and a poller was started anyway: an instance that cannot use " +
			"the feature must not pay a goroutine for it")
	}
	// Generous next to any interval this package's own cases drive, so a loop that
	// HAD started would have ticked many times by now.
	time.Sleep(120 * time.Millisecond)

	if got := probe.counting.getWiring.Load(); got != 1 {
		t.Errorf("GetWiring was called %d times with polling unconfigured, want exactly 1 (the boot read). "+
			"Unset must mean BOOT-ONLY: no goroutine and no periodic read", got)
	}
	if got := probe.counting.listCalls.Load(); got != 0 {
		t.Errorf("the boot made %d per-section List reads; the wiring read is ONE atomic snapshot through "+
			"GetWiring, at the boot and nowhere else", got)
	}
	if probe.out.Len() != 0 {
		t.Errorf("an unconfigured instance narrated something about polling: %q", probe.out.String())
	}
}

// TestAConfiguredIntervalReReadsTheDeployedWiring is the other half: told to poll,
// the instance reads the wiring again, on its own, with nothing else happening.
func TestAConfiguredIntervalReReadsTheDeployedWiring(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "on.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "", "--wiring-poll", "5ms")

	if probe.poll == nil {
		t.Fatal("--wiring-poll 5ms started no poller")
	}
	waitFor(t, "the poller's third re-read", 5*time.Second, func() bool {
		return probe.counting.getWiring.Load() >= 4 // 1 boot + 3 ticks
	})
	if got := probe.poll.changes.Load(); got != 0 {
		t.Errorf("the poller reported %d changes against wiring nobody touched; a quiet store must be quiet", got)
	}
	if !strings.Contains(probe.out.String(), "every 5ms") {
		t.Errorf("the instance did not say that polling was on, or at what interval: %q", probe.out.String())
	}
}

// TestTheEnvironmentConfiguresTheSameLoop: APERTURE_WIRING_POLL is the spelling a
// DEPLOYMENT uses (the property is the deployment's, not the invocation's — the
// same argument CLAUDE.md makes for APERTURE_POSTGRES_SCHEMA), so it has to reach
// the same loop the flag does and not a second code path.
func TestTheEnvironmentConfiguresTheSameLoop(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "env.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)
	t.Setenv(envWiringPoll, "5ms")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "")

	if probe.poll == nil {
		t.Fatalf("%s=5ms started no poller", envWiringPoll)
	}
	waitFor(t, "a re-read driven by the environment", 5*time.Second, func() bool {
		return probe.counting.getWiring.Load() >= 2
	})
}

// TestTheIntervalVocabularyIsClosedAndTheFlagBeatsTheEnvironment drives the reader
// through a real parsed command, which is the only way precedence is actually
// tested: resolving it by hand here would test a resolution order this package does
// not use. urfave applies an EnvVars source only when the flag was not set on the
// command line, so flag > env > off is the library's own order and not one written
// out again in wiringPollInterval.
func TestTheIntervalVocabularyIsClosedAndTheFlagBeatsTheEnvironment(t *testing.T) {
	resolve := func(t *testing.T, env string, argv ...string) (time.Duration, bool, error) {
		t.Helper()
		if env != "" {
			t.Setenv(envWiringPoll, env)
		}
		var (
			every time.Duration
			on    bool
			perr  error
		)
		cmd := &ucli.Command{
			Name:   "probe",
			Flags:  []ucli.Flag{wiringPollFlag()},
			Action: func(context.Context, *ucli.Command) error { return nil },
		}
		cmd.Action = func(_ context.Context, c *ucli.Command) error {
			every, on, perr = wiringPollInterval(c)
			return nil
		}
		if err := cmd.Run(context.Background(), append([]string{"probe"}, argv...)); err != nil {
			t.Fatalf("running the probe: %v", err)
		}
		return every, on, perr
	}

	t.Run("unset is off", func(t *testing.T) {
		if _, on, err := resolve(t, ""); on || err != nil {
			t.Errorf("unset resolved to on=%v err=%v; the default is OFF", on, err)
		}
	})
	t.Run("an empty variable is off", func(t *testing.T) {
		// A variable that EXISTS and is empty is how a child environment says "not
		// here" when it cannot unset what it inherited. Set here rather than through
		// resolve, whose "" means "leave the variable alone" — the two cases have to be
		// distinguishable or one of them is not being tested.
		t.Setenv(envWiringPoll, "")
		if _, on, err := resolve(t, ""); on || err != nil {
			t.Errorf("an empty %s resolved to on=%v err=%v", envWiringPoll, on, err)
		}
	})
	t.Run("off is off", func(t *testing.T) {
		if _, on, err := resolve(t, "", "--wiring-poll", "off"); on || err != nil {
			t.Errorf("off resolved to on=%v err=%v", on, err)
		}
	})
	t.Run("zero is off and not a refusal", func(t *testing.T) {
		for _, raw := range []string{"0", "0s"} {
			if _, on, err := resolve(t, "", "--wiring-poll", raw); on || err != nil {
				t.Errorf("%q resolved to on=%v err=%v; a zero interval IS never, and an operator "+
					"who wrote it gets what they wrote", raw, on, err)
			}
		}
	})
	t.Run("on is the documented default", func(t *testing.T) {
		every, on, err := resolve(t, "", "--wiring-poll", "on")
		if err != nil || !on {
			t.Fatalf("on resolved to on=%v err=%v", on, err)
		}
		if every != defaultWiringPollInterval {
			t.Errorf("on resolved to %s, want the documented default %s", every, defaultWiringPollInterval)
		}
	})
	t.Run("a duration is taken verbatim", func(t *testing.T) {
		every, on, err := resolve(t, "", "--wiring-poll", "90s")
		if err != nil || !on || every != 90*time.Second {
			t.Errorf("90s resolved to %s on=%v err=%v", every, on, err)
		}
	})
	t.Run("the flag beats the environment", func(t *testing.T) {
		every, on, err := resolve(t, "10m", "--wiring-poll", "45s")
		if err != nil || !on {
			t.Fatalf("resolved to on=%v err=%v", on, err)
		}
		if every != 45*time.Second {
			t.Errorf("the flag resolved to %s while %s said 10m; the flag wins", every, envWiringPoll)
		}
	})
	t.Run("the flag can switch off what the environment switched on", func(t *testing.T) {
		if _, on, err := resolve(t, "30s", "--wiring-poll", "off"); on || err != nil {
			t.Errorf("--wiring-poll off against %s=30s resolved to on=%v err=%v", envWiringPoll, on, err)
		}
	})
	t.Run("a malformed value is refused", func(t *testing.T) {
		_, _, err := resolve(t, "", "--wiring-poll", "banana")
		mustRefuse(t, "an unparseable --wiring-poll", err, aerr.APERTURE_CONFIG_INVALID,
			"--"+wiringPollFlagName, envWiringPoll, "banana")
	})
	t.Run("a negative interval is refused rather than read as off", func(t *testing.T) {
		_, _, err := resolve(t, "", "--wiring-poll", "-5m")
		mustRefuse(t, "a negative --wiring-poll", err, aerr.APERTURE_CONFIG_INVALID, "-5m")
	})
	t.Run("the malformed environment is refused identically", func(t *testing.T) {
		_, _, err := resolve(t, "every friday")
		mustRefuse(t, "an unparseable "+envWiringPoll, err, aerr.APERTURE_CONFIG_INVALID, "every friday")
	})
}

// TestTheWiringPollFlagIsNotADurationFlag pins the shape choice, not the
// behaviour, exactly as TestEnumerateLimitIsNotAnIntFlag does for the bound: a
// ucli.DurationFlag carrying the same EnvVars source parses the environment inside
// urfave and fails the command with its own UNCODED error before the action runs,
// so APERTURE_WIRING_POLL=banana would report something other than
// APERTURE_CONFIG_INVALID. The StringFlag is what keeps the parse — and therefore
// the coded error and its fixups — on this side of the boundary.
func TestTheWiringPollFlagIsNotADurationFlag(t *testing.T) {
	for _, f := range serveCommand().Flags {
		if f.Names()[0] != wiringPollFlagName {
			continue
		}
		if _, ok := f.(*ucli.StringFlag); !ok {
			t.Fatalf("--%s is a %T; it must stay a *ucli.StringFlag so a malformed value is APERTURE_CONFIG_INVALID",
				wiringPollFlagName, f)
		}
		if !strings.Contains(f.(*ucli.StringFlag).Usage, envWiringPoll) {
			t.Errorf("--%s usage does not name %s; the usage string is the only shipped explanation, so an "+
				"operator reading `aperture serve --help` must be able to find the variable from it",
				wiringPollFlagName, envWiringPoll)
		}
		return
	}
	t.Fatalf("serve has no --%s flag", wiringPollFlagName)
}

// TestAMalformedIntervalIsRefusedBeforeAnyConnectionIsMade is the acceptance
// criterion about ORDER, and it is asserted on the filesystem rather than on the
// error alone.
//
// The code on its own proves little: `serve` would refuse a malformed interval
// whether it parsed it first or last. What must be true is that nothing was
// OPENED first — the same posture postgres.Open takes for a bad
// APERTURE_POSTGRES_SCHEMA, and the same one runServe already takes for
// APERTURE_MANAGE_* and --enumerate-limit. A SQLite --store is what makes it
// observable: buildStore CREATES the file, so a database left behind is proof the
// parse happened too late, and a refused configuration must not leave a store
// behind.
func TestAMalformedIntervalIsRefusedBeforeAnyConnectionIsMade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "never-created.db")

	out, err := runArgv(t, "serve", "--addr", "127.0.0.1:0", "--store", dbPath, "--wiring-poll", "banana")
	mustRefuse(t, "`aperture serve --wiring-poll banana`", err, aerr.APERTURE_CONFIG_INVALID,
		"--"+wiringPollFlagName, "banana")
	if strings.Contains(out, "aperture serving on") {
		t.Fatalf("the server announced that it was serving on a refused configuration. Output:\n%s", out)
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Fatalf("a refused --%s left a database behind at %s: the interval must be parsed BEFORE any "+
			"connection is made, so a typo costs nothing but the typo", wiringPollFlagName, dbPath)
	}
}

// TestTheLoopStopsOnContextCancellationAndLeaksNoGoroutine proves the shutdown
// half four ways, because each one alone is weak: a Close that returns proves only
// that Close returned, a read count that stops could be a store that stopped
// answering, and a goroutine count is noisy on a shared test binary.
//
// The poller is started by hand here rather than through the flag, which is what
// makes the goroutine count usable at all: newPollProbe OPENS A STORE, and a
// SQLite pool brings goroutines of its own, so a baseline taken before the boot
// could never be returned to and one taken after it is the only honest place to
// stand. The interval and the baseline digest are the same values the flag path
// would have produced.
func TestTheLoopStopsOnContextCancellationAndLeaksNoGoroutine(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "shutdown.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	probe := newPollProbe(t, ctx, dsn, "")
	if probe.poll != nil {
		t.Fatal("the unconfigured probe started a poller")
	}

	before := runtime.NumGoroutine()
	poll := startWiringPoll(ctx, probe.counting, 5*time.Millisecond, probe.stack.wiringDigest, probe.out)
	if poll == nil {
		t.Fatal("a 5ms interval started no poller")
	}
	waitFor(t, "the loop to be running", 5*time.Second, func() bool {
		return poll.ticks.Load() >= 2
	})
	if runtime.NumGoroutine() <= before {
		t.Fatalf("the goroutine count did not rise when the loop started (%d -> %d), so the count below "+
			"proves nothing", before, runtime.NumGoroutine())
	}

	// 1. Cancelling the CONTEXT is enough on its own — Close is the caller's
	//    convenience, not the only way out. `serve` derives the loop from its signal
	//    context precisely so a SIGINT stops the reader without waiting on a defer.
	cancel()

	closed := make(chan struct{})
	go func() {
		_ = poll.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the context was cancelled: the reader goroutine is still " +
			"running, and a returned Close must be a proof it is gone rather than a request that it go")
	}

	// 2. And the reads stop. A goroutine that returned cannot read, but a loop that
	//    kept a second timer alive somewhere would show up here and nowhere else.
	settled := probe.counting.getWiring.Load()
	time.Sleep(60 * time.Millisecond) // a dozen intervals
	if now := probe.counting.getWiring.Load(); now != settled {
		t.Errorf("the store was read %d more times after shutdown; the loop is still running",
			now-settled)
	}

	// 3. Idempotent, so `serve` may both cancel and defer Close.
	if err := poll.Close(); err != nil {
		t.Errorf("a second Close returned %v; it must be idempotent", err)
	}

	// 4. And the goroutine is actually gone, against the baseline taken after the
	//    store was open — the rise asserted above is what makes this the same
	//    goroutine and not an accounting coincidence.
	waitFor(t, "the goroutine count to return to its baseline", 5*time.Second, func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// TestAClosedPollerIsNilSafe: "polling is off" is a nil *wiringPoll, so every
// caller wires Close unconditionally. A nil check at each call site would be one
// more thing a later surface could forget, and forgetting it is a panic at
// shutdown.
func TestAClosedPollerIsNilSafe(t *testing.T) {
	var p *wiringPoll
	if err := p.Close(); err != nil {
		t.Errorf("(*wiringPoll)(nil).Close() = %v, want nil", err)
	}
	if got := startWiringPoll(context.Background(), nil, 0, "", nil); got != nil {
		t.Errorf("startWiringPoll with a zero interval returned %v, want nil — off means no goroutine", got)
	}
	if got := startWiringPoll(context.Background(), nil, -time.Second, "", nil); got != nil {
		t.Errorf("startWiringPoll with a negative interval returned %v, want nil", got)
	}
}

// TestAWiringChangeIsDetectedOnceAndNothingIsRebuilt is the story's last criterion
// and the boundary with E4-S2: the loop notices, says so, and swaps NOTHING.
//
// tick is driven directly rather than through the ticker, which is what makes the
// assertions about WHAT was detected deterministic instead of a race with a timer.
// The loop itself is covered by the cases above.
func TestAWiringChangeIsDetectedOnceAndNothingIsRebuilt(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "changed.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "", "--wiring-poll", "1h") // long: this case drives tick itself
	if probe.poll == nil {
		t.Fatal("--wiring-poll 1h started no poller")
	}
	registryBefore := probe.stack.registry

	if probe.poll.tick(ctx) {
		t.Fatal("the first tick against untouched wiring reported a change")
	}

	// Another host pushes: the same shape of set with one statement re-pointed, which
	// is the change a stamp-only or a row-count probe would both miss.
	changed := sharedWiringSet(time.Now().UTC())
	changed.Providers[0].GetOne = "SELECT tier FROM documents_v2 WHERE id = $1"
	if err := probe.counting.ReplaceWiring(ctx, changed); err != nil {
		t.Fatalf("the second push failed: %v", err)
	}

	if !probe.poll.tick(ctx) {
		t.Fatal("a re-pointed statement was not detected as a change. A change this instance does not " +
			"see is a silently stale instance, which is the whole hazard this epic closes")
	}
	// Reported once, and not once per tick from then on.
	if probe.poll.tick(ctx) {
		t.Error("the same change was reported twice; the digest must advance when a change is seen, " +
			"or every tick after a push narrates it again")
	}
	if got := probe.poll.changes.Load(); got != 1 {
		t.Errorf("%d changes reported for one push, want 1", got)
	}

	// The E4-S2 boundary, asserted rather than assumed: this story DETECTS.
	if probe.stack.registry != registryBefore {
		t.Error("the stack's object registry was replaced. E4-S1 detects and reports; the rebuild and the " +
			"swap are E4-S2, and a swap that arrives early arrives without the frozen connection-name " +
			"check (E4-S3) or the last-good handling (E4-S4)")
	}
	report := probe.out.String()
	for _, want := range []string{"CHANGED", "still running the wiring", "restart"} {
		if !strings.Contains(report, want) {
			t.Errorf("the change report does not mention %q: an operator told the wiring changed who "+
				"assumes it was adopted is worse off than one told nothing. Report:\n%s", want, report)
		}
	}
}

// TestAnIdenticalRePushIsNotAChange is why the stamps are out of the digest.
// ReplaceWiring is wholesale — every push rewrites every row and re-stamps it — so
// a digest over the stamps would call a re-run of the same pipeline a change, and
// E4-S2 would rebuild registries and reopen pools to arrive at what it already had.
func TestAnIdenticalRePushIsNotAChange(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "repush.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC().Add(-time.Hour)))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "", "--wiring-poll", "1h")

	// Same wiring, later stamps — the pipeline ran again.
	if err := probe.counting.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC())); err != nil {
		t.Fatalf("the re-push failed: %v", err)
	}
	if probe.poll.tick(ctx) {
		t.Error("a byte-identical re-push was reported as a change. The question a tick asks is " +
			"\"would this instance be wired differently?\", and a stamp cannot change that answer")
	}
}

// TestTheFirstEverPushToAnUnwiredDatabaseIsAChange is the transition a "" sentinel
// for "no wiring" would have made the one nobody noticed — and it is the single
// most consequential one there is, because it is the moment a file-wired fleet
// becomes a shared-wiring fleet.
func TestTheFirstEverPushToAnUnwiredDatabaseIsAChange(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "first-push.db")
	seedPath := writeSeed(t, "boot-wiring.yaml", bootWiringSeed)
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Booted on EMPTY wiring tables: the local seed file is in charge, which is every
	// deployment that has never run `aperture wiring push`.
	probe := newPollProbe(t, ctx, dsn, seedPath, "--wiring-poll", "1h")
	if probe.stack.wiringDigest == "" {
		t.Fatal("an empty wiring set produced an empty digest. The empty set's digest must be an " +
			"ordinary value, or the first push to a database is the one change nothing detects")
	}
	if probe.poll.tick(ctx) {
		t.Fatal("a tick against still-empty tables reported a change")
	}

	if err := probe.counting.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC())); err != nil {
		t.Fatalf("the first push failed: %v", err)
	}
	if !probe.poll.tick(ctx) {
		t.Error("the first ever push to an unwired database was not detected")
	}
}

// TestAFailedPollKeepsTheWiringItHasAndDoesNotBuryTheCode covers the branch E4-S4
// owns. Two things must be true of it already: last-good is the behaviour (nothing
// is swapped, so the instance decides on with what it has), and the digest does NOT
// advance, so the next successful read still sees the change a failed read could
// not confirm.
func TestAFailedPollKeepsTheWiringItHasAndDoesNotBuryTheCode(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "failing.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "", "--wiring-poll", "1h")
	baseline := probe.poll.digest

	// The store stops answering, and the wiring changes while it is unreadable.
	coded := aerr.New(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, "the wiring tables are from an older build")
	probe.poll.store = wiringReadFails{Storage: probe.counting, err: coded}
	if probe.poll.tick(ctx) {
		t.Fatal("a failed read reported a change")
	}
	if probe.poll.digest != baseline {
		t.Error("a failed read advanced the digest: the next successful read would then miss the change " +
			"the failed one could not confirm")
	}
	if !strings.Contains(probe.out.String(), "keeps the wiring it has") {
		t.Errorf("a failed poll did not say that the instance kept its wiring: %q", probe.out.String())
	}
	// The store's OWN code reaches the report, because readSharedWiring's
	// pass-through guard is what a poll inherits from the boot: an operator told
	// "aperture failed to start" instead of APERTURE_STORAGE_SCHEMA_INCOMPATIBLE has
	// lost the remedy and the registry fixups with it.
	if !strings.Contains(probe.out.String(), string(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE)) {
		t.Errorf("the store's own code did not reach the poll report: %q", probe.out.String())
	}

	// And a recovered store still sees the change.
	changed := sharedWiringSet(time.Now().UTC())
	changed.FieldTypes[0].DeclaredType = "datetime"
	if err := probe.counting.ReplaceWiring(ctx, changed); err != nil {
		t.Fatalf("push while unreadable: %v", err)
	}
	probe.poll.store = probe.counting
	if !probe.poll.tick(ctx) {
		t.Error("the change was lost across a failed read")
	}
}

// ---- The digest itself ----

// digestFixture is a wiring set with EVERY field of every section populated, so a
// case that varies one field varies it against a value that was already there.
// A fixture with zero-valued fields would let a digest that ignored a field pass
// half the time by accident.
func digestFixture() model.WiringSet {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main", CreatedAt: now, UpdatedAt: now}},
		Providers: []model.WiringProvider{{
			ObjectType: "document",
			Kind:       "sql",
			Connection: "main",
			GetOne:     "SELECT tier FROM documents WHERE id = $1",
			GetAll:     "SELECT 'document:' || d.id AS id, d.tier FROM documents d",
			IDColumn:   "id",
			TTL:        "30s",
			MaxSize:    256,
			References: []model.WiringReference{{Field: "project_id", TargetType: "project"}},
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		FieldTypes: []model.WiringFieldType{{
			ObjectType:   "document",
			Field:        "reviewed_on",
			DeclaredType: "date",
			CreatedAt:    now,
			UpdatedAt:    now,
		}},
		AttributeProviders: []model.WiringAttributeProvider{{
			Subject:      "user",
			Kind:         "sql",
			Connection:   "main",
			GetOne:       "SELECT department FROM users WHERE id = $1",
			GetAll:       "SELECT id FROM users",
			IDColumn:     "id",
			TTL:          "1m",
			MaxSize:      64,
			DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{"department"}},
			CreatedAt:    now,
			UpdatedAt:    now,
		}},
	}
}

func mustDigest(t *testing.T, set model.WiringSet) string {
	t.Helper()
	d, err := wiringDigest(set)
	if err != nil {
		t.Fatalf("wiringDigest: %v", err)
	}
	if d == "" {
		t.Fatal("wiringDigest returned an empty digest")
	}
	return d
}

// varyField changes v to a different value of the same type, reporting whether it
// knew how. It is reflective on purpose: the point of the gate it serves is that a
// field ADDED to the wiring model is covered without anybody remembering, so a
// type this function cannot vary has to FAIL rather than be skipped.
func varyField(v reflect.Value) bool {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
		return true
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-varied")
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 7)
		return true
	case reflect.Bool:
		v.SetBool(!v.Bool())
		return true
	case reflect.Slice:
		// Appending a zero element is enough: it changes the rendered array whatever
		// the element type is, which keeps this generic over a section that grows an
		// owned child table.
		v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
		return true
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && varyField(v.Field(i)) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// TestTheDigestCoversEveryContentFieldAndNoStamp is the gate the whole change
// check rests on, and it is written by REFLECTION rather than as a list of fields
// for the reason the digest itself marshals by reflection: a wiring column added
// later must be in the digest without anyone remembering to add it here.
//
// Both directions are failures, and they fail differently:
//
//   - A CONTENT field outside the digest is a change this instance never sees. The
//     loop ticks forever, reports nothing, and the instance is silently stale while
//     every health check passes. That is the hazard the epic exists to close.
//   - A STAMP inside the digest is a change reported for a push that deployed
//     identical wiring, which costs E4-S2 a rebuild and a pool reopen to arrive at
//     what it already had.
//
// A field of a type varyField does not know how to change fails the test rather
// than being skipped. A silent skip here is exactly how the first direction gets
// shipped.
func TestTheDigestCoversEveryContentFieldAndNoStamp(t *testing.T) {
	base := mustDigest(t, digestFixture())

	sections := []string{"Connections", "Providers", "FieldTypes", "AttributeProviders"}
	for _, section := range sections {
		elem := reflect.ValueOf(digestFixture()).FieldByName(section).Type().Elem()
		if elem.Kind() != reflect.Struct {
			t.Fatalf("section %s is a slice of %s, not of a struct: the gate below cannot walk it", section, elem.Kind())
		}
		for i := 0; i < elem.NumField(); i++ {
			field := elem.Field(i)
			if !field.IsExported() {
				continue
			}
			name := section + "." + field.Name
			t.Run(name, func(t *testing.T) {
				set := digestFixture()
				target := reflect.ValueOf(&set).Elem().FieldByName(section).Index(0).Field(i)
				if !varyField(target) {
					t.Fatalf("this gate does not know how to vary %s (a %s), so it cannot say whether the "+
						"digest covers it. Teach varyField the type — do NOT skip the field: a field the "+
						"digest ignores is a wiring change no instance ever notices", name, field.Type)
				}
				got := mustDigest(t, set)
				stamp := field.Name == "CreatedAt" || field.Name == "UpdatedAt"
				if stamp && got != base {
					t.Errorf("changing %s changed the digest. Stamps are OUT: ReplaceWiring re-stamps every "+
						"row on every push, so a stamp in the digest makes an identical re-push read as a "+
						"change and costs a needless rebuild", name)
				}
				if !stamp && got == base {
					t.Errorf("changing %s did NOT change the digest, so a push that changes it is a change "+
						"this instance never sees — a silently stale instance that reports itself healthy", name)
				}
			})
		}
	}

	// The owned child table too. WiringReference carries no stamps of its own (its
	// history is its provider entry's), so every field of it is content.
	refType := reflect.TypeOf(model.WiringReference{})
	for i := 0; i < refType.NumField(); i++ {
		field := refType.Field(i)
		if !field.IsExported() {
			continue
		}
		t.Run("WiringReference."+field.Name, func(t *testing.T) {
			set := digestFixture()
			target := reflect.ValueOf(&set).Elem().FieldByName("Providers").Index(0).
				FieldByName("References").Index(0).Field(i)
			if !varyField(target) {
				t.Fatalf("this gate does not know how to vary WiringReference.%s (a %s)", field.Name, field.Type)
			}
			if mustDigest(t, set) == base {
				t.Errorf("changing WiringReference.%s did not change the digest: a re-pointed declared "+
					"reference is a wiring change", field.Name)
			}
		})
	}
}

// TestTheDigestIsAPropertyOfTheWiringAndNotOfTheRead: two instances, or one
// instance across two ticks, must not disagree because a backend handed the same
// rows back in a different order. The digest sorts first, so the order a set was
// assembled in cannot reach it.
func TestTheDigestIsAPropertyOfTheWiringAndNotOfTheRead(t *testing.T) {
	forwards := digestFixture()
	forwards.Providers = append(forwards.Providers, model.WiringProvider{
		ObjectType: "project", Kind: "sql", Connection: "main",
		GetOne: "SELECT 1 WHERE id = $1", GetAll: "SELECT 'project:1' AS id",
		References: []model.WiringReference{{Field: "a", TargetType: "document"}, {Field: "b", TargetType: "document"}},
	})
	backwards := digestFixture()
	backwards.Providers = append([]model.WiringProvider{{
		ObjectType: "project", Kind: "sql", Connection: "main",
		GetOne: "SELECT 1 WHERE id = $1", GetAll: "SELECT 'project:1' AS id",
		References: []model.WiringReference{{Field: "b", TargetType: "document"}, {Field: "a", TargetType: "document"}},
	}}, backwards.Providers...)

	if mustDigest(t, forwards) != mustDigest(t, backwards) {
		t.Error("the same wiring listed in a different order digested differently")
	}
}

// TestDigestingDoesNotMutateTheCallersWiring: the set a digest is taken over is
// the one buildDecisionStack goes on to project into a document, so a helper that
// sorted or zeroed in place would change what the instance is WIRED with. The
// reference rows are the sharp edge — the slice header is shared, and Sort reorders
// in place.
func TestDigestingDoesNotMutateTheCallersWiring(t *testing.T) {
	set := digestFixture()
	set.Providers[0].References = []model.WiringReference{
		{Field: "zeta", TargetType: "project"},
		{Field: "alpha", TargetType: "project"},
	}
	stamp := set.Connections[0].UpdatedAt

	if _, err := wiringDigest(set); err != nil {
		t.Fatalf("wiringDigest: %v", err)
	}
	if set.Providers[0].References[0].Field != "zeta" {
		t.Errorf("digesting reordered the caller's reference rows: %+v", set.Providers[0].References)
	}
	if !set.Connections[0].UpdatedAt.Equal(stamp) {
		t.Error("digesting zeroed the caller's stamps in place")
	}
}

// ---- The gated live-Postgres proof ----
//
// Everything above is decided in internal/cli, so SQLite proves it. What SQLite
// cannot prove is that a wiring change survives the OTHER dialect's round trip as a
// change: the digest is taken over what GetWiring returns, so a backend that
// dropped a column, normalised a statement or collapsed a declared-empty key set to
// nil would make two different sets digest IDENTICALLY — and a poll that can never
// report a change is indistinguishable from a quiet store.
//
// Same gate as the rest of this package's live suite (see wiring_pull_test.go for
// the contract and the helpers): ungated it SKIPS, gated with an empty
// APERTURE_PG_DSN it FAILS.
//
//	APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/

// TestPostgresLiveAWiringChangeIsDetectedThroughTheOtherDialect pushes, digests,
// pushes a changed set and digests again — against a real server, in its own
// schema.
func TestPostgresLiveAWiringChangeIsDetectedThroughTheOtherDialect(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn = livePostgresWiringStore(t, ctx, dsn)
	routeSharedMain(t)

	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	defer func() { _ = store.Close() }()

	digestOf := func(what string) string {
		t.Helper()
		set, err := store.GetWiring(ctx)
		if err != nil {
			t.Fatalf("GetWiring after %s: %v", what, err)
		}
		return mustDigest(t, set)
	}

	empty := digestOf("Setup")

	if err := store.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC())); err != nil {
		t.Fatalf("the first push failed: %v", err)
	}
	first := digestOf("the first push")
	if first == empty {
		t.Fatal("a live-Postgres first push did not change the digest")
	}

	// An identical re-push, later stamps: not a change.
	if err := store.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC().Add(time.Minute))); err != nil {
		t.Fatalf("the re-push failed: %v", err)
	}
	if again := digestOf("the identical re-push"); again != first {
		t.Errorf("an identical re-push through Postgres read back as a change: %s -> %s",
			shortDigest(first), shortDigest(again))
	}

	// One statement re-pointed AND a declared key set added: the two shapes a
	// backend is most likely to normalise on the way through.
	changed := sharedWiringSet(time.Now().UTC())
	changed.Providers[0].GetOne = "SELECT tier FROM documents_v2 WHERE id = $1"
	changed.AttributeProviders[0].DeclaredKeys = model.DeclaredKeys{Declared: true, Keys: []string{}}
	if err := store.ReplaceWiring(ctx, changed); err != nil {
		t.Fatalf("the changed push failed: %v", err)
	}
	if got := digestOf("the changed push"); got == first {
		t.Error("a re-pointed statement and a declared-empty key set read back out of Postgres with an " +
			"unchanged digest: a poll that can never report a change is indistinguishable from a quiet store")
	}
}

// TestPostgresLiveTheDigestAgreesAcrossTheTwoDialects is the parity half, and it is
// the one the schema-parity gates cannot reach: they prove the two schemas DESCRIBE
// the same database, not that a READ of one digests to the same value as a read of
// the other. If they disagree, two identically-wired instances on different backends
// hold different baselines — and neither is wrong, so nothing ever reports it.
func TestPostgresLiveTheDigestAgreesAcrossTheTwoDialects(t *testing.T) {
	live := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	set := sharedWiringSet(time.Now().UTC())
	digestFrom := func(dsn, name string) string {
		t.Helper()
		store, err := buildStore(ctx, dsn, "")
		if err != nil {
			t.Fatalf("open the %s store: %v", name, err)
		}
		defer func() { _ = store.Close() }()
		if err := store.ReplaceWiring(ctx, set); err != nil {
			t.Fatalf("push to %s: %v", name, err)
		}
		read, err := store.GetWiring(ctx)
		if err != nil {
			t.Fatalf("GetWiring from %s: %v", name, err)
		}
		return mustDigest(t, read)
	}

	// The SQLite store first: t.Setenv inside livePostgresWiringStore applies to the
	// whole test, and only the Postgres backend reads it.
	fromSQLite := digestFrom(newWiringStore(t, wiringModelSeed), "sqlite")
	fromPostgres := digestFrom(livePostgresWiringStore(t, ctx, live), "postgres")
	if fromSQLite != fromPostgres {
		t.Errorf("one wiring set digests to %s out of SQLite and %s out of Postgres; two identically-wired "+
			"instances would hold different baselines and nothing would report it",
			shortDigest(fromSQLite), shortDigest(fromPostgres))
	}
}
