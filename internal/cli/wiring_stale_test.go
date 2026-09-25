package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"
)

// E4-S4: a failed refresh keeps last-good wiring, keeps deciding, and alarms.
//
// The three things a case here has to prove TOGETHER, because each one passes on
// its own while the story is broken:
//
//   - the instance KEEPS DECIDING, and decides the SAME WAY it did before the
//     failure. Asserting only that the poller returned is not enough — the whole
//     reason last-good is the policy is that Aperture is embedded in-process, so
//     "did not crash" is the floor and "still answers correctly from the wiring it
//     had" is the requirement.
//   - the failure is ALARMED, with the underlying code intact. An alarm that
//     buries APERTURE_WIRING_CONNECTION_UNROUTED under a generic code is an
//     operator without a remedy, and CLAUDE.md's chain-depth rule is how a
//     re-stamp is caught.
//   - RECOVERY CLEARS IT. An alarm that latches forever is a different bug from no
//     alarm at all, and it passes any test that only checks that it fired.
//
// staleDecisionSeed is deliberately rule-backed over an inline `objects:` block:
// the decision reads the object registry this instance BUILT, so a case that
// keeps getting the right verdict through a failing refresh is evidence the
// last-good wiring is still in use, not merely that the engine is alive.
const staleDecisionSeed = `
accounts:
  - {id: acme, name: Acme Corp}
memberships:
  - {principal: alice, account: acme}
object_types:
  - name: project
    description: The type the local file serves inline.
    actions: [read]
  - name: document
    description: The type the shared wiring serves.
    actions: [read]
permissions:
  - id: perm-project-read
    object_type: project
    action: read
    scope_strategy: "inclusive;rule=gold-projects"
    description: Read a project whose tier is gold.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice}
grants:
  - id: g-alice-read
    account: acme
    subject: {kind: principal, id: alice}
    permission: perm-project-read
    object: "account:acme/**"
    effect: allow
rules:
  - name: gold-projects
    description: A project is readable when its tier is gold.
    ast:
      type: compare
      op: eq
      left: {type: var, name: object.tier}
      right: {type: literal, value: "gold"}
objects:
  - id: "account:acme/project:atlas"
    metadata: {tier: gold}
  - id: "account:acme/project:bronze"
    metadata: {tier: bronze}
`

// staleWiringSet is sharedWiringSet with the `user` attribute slot removed.
//
// The slot is dropped for a reason worth stating: the shared manifest's one
// connection points at a DSN nothing ever answers (unroutedDSN), and a `user`
// slot over it makes EVERY decision reach that connection —
// APERTURE_SQL_PROVIDER_QUERY is outside the leniency set, so the decision fails
// rather than reading a nil bag. That is a real property of the attribute seam,
// and it is not this story's: a suite whose whole subject is "the instance keeps
// deciding" must not be built on a fixture that cannot decide at all.
//
// The `document` provider over the same connection is KEPT. It is never fetched
// here (the decisions are over `project`, which the local file serves inline), so
// it costs nothing and keeps the pushed wiring realistic — a connection, a
// provider over it, and a field-type declaration.
func staleWiringSet(now time.Time) model.WiringSet {
	set := sharedWiringSet(now)
	set.AttributeProviders = nil
	return set
}

// stalenessProbe is a booted instance with a poller, a shared recorder and a
// model it can actually decide over.
type stalenessProbe struct {
	*pollProbe
	ctx context.Context
}

// newStalenessProbe provisions a store from staleDecisionSeed, pushes the shared
// wiring an operator would have pushed, and boots a polling instance over both —
// the same route runServe takes.
//
// The interval is an hour so the ticker never fires on its own: every case here
// drives tick by hand, which is what makes "the alarm fired on THIS failure"
// deterministic instead of a race with a timer.
func newStalenessProbe(t *testing.T) *stalenessProbe {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "stale.db")
	seedPath := writeSeed(t, "stale-decisions.yaml", staleDecisionSeed)

	store, err := buildStore(context.Background(), dsn, seedPath)
	if err != nil {
		t.Fatalf("provisioning the store: %v", err)
	}
	if err := store.ReplaceWiring(context.Background(), staleWiringSet(time.Now().UTC())); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &stalenessProbe{pollProbe: newPollProbe(t, ctx, dsn, seedPath, "--wiring-poll", "1h"), ctx: ctx}
}

// decides runs the one decision this suite watches: may alice read the gold
// project, and may she read the bronze one. Both answers are asserted every time,
// because an instance whose registry went missing would deny BOTH and an
// assertion on the allow alone would pass for an instance that allowed
// everything.
func (p *stalenessProbe) decides(t *testing.T, when string) {
	t.Helper()
	for _, c := range []struct {
		object string
		allow  bool
	}{
		{"account:acme/project:atlas", true},
		{"account:acme/project:bronze", false},
	} {
		dec, err := p.stack.eng.Check(p.ctx, engine.Request{
			Account: "acme", Principal: "alice", Action: "read", Object: c.object,
		})
		if err != nil {
			t.Fatalf("%s: deciding %s failed: %v — the instance must keep deciding", when, c.object, err)
		}
		if dec.Allow != c.allow {
			t.Errorf("%s: Check(%s).Allow = %v, want %v — the instance must decide from the wiring it HAS",
				when, c.object, dec.Allow, c.allow)
		}
	}
}

// refreshFailsWith is every failure mode a refresh has, expressed the one way all
// of them reach the poller: as a coded error out of the wiring read.
//
// Three of the four named in the acceptance are contrived HERE rather than
// provoked from their origin, and that is deliberate rather than a shortcut. A
// running process discovers that read wiring is invalid, or that a connection
// name is unroutable, by trying to BUILD with it — which is the rebuild E4-S2
// holds, and which is also where a rebuild failure will arrive. Re-deriving the
// projection in this story to make the codes reachable one tick earlier would be
// the second-builder hazard wiring_boot.go's own doc warns against, over wiring
// nothing is going to adopt.
//
// What the poller owes these codes is therefore exactly what is asserted: it
// neither classifies nor buries them, it keeps last-good, and it goes on
// deciding. The same table drives all of them through ONE path, which is the
// "one reporter, not a per-failure-mode special case" requirement stated as a
// test rather than as a comment.
func refreshFailsWith() []struct {
	name string
	code aerr.Code
	err  error
} {
	return []struct {
		name string
		code aerr.Code
		err  error
	}{
		{
			name: "an unreachable store",
			code: aerr.APERTURE_STORAGE,
			err:  aerr.New(aerr.APERTURE_STORAGE, "dial tcp: connect: connection refused"),
		},
		{
			name: "wiring this build cannot read",
			code: aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE,
			err:  aerr.New(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, "the wiring tables are from an older build"),
		},
		{
			name: "a connection name this host cannot route",
			code: aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
			err: aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
				"the shared wiring declares a connection name this instance has no route for"),
		},
		{
			name: "something nothing bothered to code",
			// APERTURE_BOOT, and not the alarm's own code: readSharedWiring already
			// classifies a bare error on its way past, so nothing UNCODED can reach the
			// alarm through this path today. That is the pass-through guard working
			// twice over and is worth pinning — if this case ever starts reporting
			// APERTURE_WIRING_REFRESH_FAILED, something upstream stopped coding its
			// failures. The classifier's own uncoded branch is exercised directly, in
			// TestTheAlarmDoesNotRestampACodedFailure.
			code: aerr.APERTURE_BOOT,
			err:  errors.New("a bare error from somewhere nobody coded"),
		},
	}
}

// TestAFailedRefreshKeepsDecidingAndAlarmsWithItsOwnCode is the story's headline,
// and it walks every failure mode through the single reporter.
func TestAFailedRefreshKeepsDecidingAndAlarmsWithItsOwnCode(t *testing.T) {
	for _, tc := range refreshFailsWith() {
		t.Run(tc.name, func(t *testing.T) {
			probe := newStalenessProbe(t)
			baseline := probe.poll.digest
			probe.decides(t, "before the failure")

			probe.poll.store = wiringReadFails{Storage: probe.counting, err: tc.err}
			if probe.poll.tick(probe.ctx) {
				t.Fatal("a failed refresh reported a change")
			}

			// Last-good: nothing was swapped, the digest did not advance, and the
			// instance still answers the same way.
			if probe.poll.digest != baseline {
				t.Error("a failed refresh advanced the digest; the next successful read would " +
					"then miss the change this one could not confirm")
			}
			probe.decides(t, "after the failure")

			// Alarmed, and readable without the log.
			p := probe.health.Posture()
			if !p.Stale {
				t.Fatalf("a failed refresh did not raise the alarm: %+v", p)
			}
			if p.Code != string(tc.code) {
				t.Errorf("the alarm's code = %q, want %q — an operator handed the wrong code "+
					"loses that code's registry fixups, which are the remedy", p.Code, tc.code)
			}
			if p.Digest != baseline {
				t.Errorf("the posture reports digest %q, want the last-good %q the instance is still deciding from",
					p.Digest, baseline)
			}
			if p.Failures != 1 {
				t.Errorf("Failures = %d, want 1", p.Failures)
			}

			// And still on stderr, because a process whose facade nobody polls has to
			// say something.
			out := probe.out.String()
			if !strings.Contains(out, "keeps the wiring it has") {
				t.Errorf("the report did not say the instance kept its wiring: %q", out)
			}
			if !strings.Contains(out, string(tc.code)) {
				t.Errorf("the report did not name %s: %q", tc.code, out)
			}
		})
	}
}

// TestTheAlarmDoesNotRestampACodedFailure is CLAUDE.md's chain-depth rule applied
// to the one function in this story that wraps: a same-code re-stamp is invisible
// to CodeOf, so depth is what catches a Wrap that should have been a
// pass-through.
func TestTheAlarmDoesNotRestampACodedFailure(t *testing.T) {
	coded := aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "no route for \"main\"")
	if got := wiringRefreshAlarm(coded); got != coded {
		t.Errorf("wiringRefreshAlarm re-wrapped an already-coded failure: %v", got)
	}
	if depth := codedChainDepth(wiringRefreshAlarm(coded)); depth != 1 {
		t.Errorf("an already-coded failure has %d Aperture-coded errors in its chain, want exactly 1", depth)
	}

	bare := errors.New("nobody coded this")
	classified := wiringRefreshAlarm(bare)
	if code := aerr.CodeOf(classified); code != aerr.APERTURE_WIRING_REFRESH_FAILED {
		t.Errorf("an UNCODED failure got code %q, want APERTURE_WIRING_REFRESH_FAILED — "+
			"an alarm with no code is an operator told something is wrong and nothing about what", code)
	}
	if depth := codedChainDepth(classified); depth != 1 {
		t.Errorf("a classified failure has %d Aperture-coded errors in its chain, want exactly 1", depth)
	}
	if !errors.Is(classified, bare) {
		t.Error("classifying lost the underlying error")
	}
	if wiringRefreshAlarm(nil) != nil {
		t.Error("wiringRefreshAlarm(nil) invented a failure")
	}
}

// codedChainDepth counts the Aperture-coded errors in a chain. Exactly one is the
// contract: a second means something re-classified an error that already said
// something better, and CodeOf cannot see it when the two codes agree.
func codedChainDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}

// TestTheStoreGoesAwayAndComesBackAndTheAlarmClears is the acceptance criterion
// that names the whole arc: store failure -> continued correct decisions ->
// recovery, with the alarm CLEARED.
//
// The recovery half is asserted as hard as the alarm half, because a latching
// alarm passes every test that only checks that it fired — and an operator who
// has fixed the database and is still being paged stops reading the channel,
// which is silent staleness by a longer route.
func TestTheStoreGoesAwayAndComesBackAndTheAlarmClears(t *testing.T) {
	probe := newStalenessProbe(t)
	baseline := probe.poll.digest
	probe.decides(t, "healthy")
	if probe.health.Posture().Stale {
		t.Fatal("a healthy instance reported stale")
	}

	// The store goes away, and stays away across several ticks. The alarm's AGE has
	// to grow while the failure count rises: an operator escalating on "four hours"
	// needs a number that moves.
	probe.poll.store = wiringReadFails{
		Storage: probe.counting,
		err:     aerr.New(aerr.APERTURE_STORAGE, "dial tcp: connect: connection refused"),
	}
	for i := 0; i < 3; i++ {
		probe.poll.tick(probe.ctx)
		probe.now.advance(30 * time.Second)
		probe.decides(t, "while the store is away")
	}
	p := probe.health.Posture()
	if !p.Stale || p.Failures != 3 {
		t.Fatalf("after three failed refreshes: %+v, want stale with 3 failures", p)
	}
	if p.StaleFor != 90*time.Second {
		t.Errorf("StaleFor = %s, want 1m30s — the duration is the half an operator escalates on", p.StaleFor)
	}

	// Meanwhile somebody pushes a wiring change, which this instance cannot see.
	changed := staleWiringSet(time.Now().UTC())
	changed.FieldTypes[0].DeclaredType = "datetime"
	if err := probe.counting.ReplaceWiring(probe.ctx, changed); err != nil {
		t.Fatalf("pushing while the store is unreadable: %v", err)
	}

	// The store comes back. The refresh succeeds, the alarm clears completely, and
	// the change the failed reads could not confirm is STILL seen — which is what
	// the non-advancing digest bought.
	probe.poll.store = probe.counting
	if !probe.poll.tick(probe.ctx) {
		t.Error("the change was lost across the failed reads")
	}
	p = probe.health.Posture()
	switch {
	case p.Stale:
		t.Error("the alarm survived the recovery — a latching alarm trains an operator to ignore the channel")
	case p.StaleFor != 0:
		t.Errorf("StaleFor after recovery = %s, want 0", p.StaleFor)
	case p.Failures != 0:
		t.Errorf("Failures after recovery = %d, want 0", p.Failures)
	case p.Code != "":
		t.Errorf("Code after recovery = %q, want empty", p.Code)
	case p.LastRefresh.IsZero():
		t.Error("a successful refresh did not stamp LastRefresh")
	}
	// Nothing was adopted — that is E4-S2's job — so the posture must still name
	// the digest this instance DECIDES FROM and not the one the tables now hold.
	if p.Digest != baseline {
		t.Errorf("the posture reports digest %q after a change was merely NOTICED; want the "+
			"last-good %q, because claiming an adoption that has not happened is the same "+
			"lie as hiding a staleness", p.Digest, baseline)
	}
	probe.decides(t, "after recovery")
}

// TestASuccessfulRefreshWithNoChangeAlsoClearsTheAlarm pins the branch a
// change-driven design would get wrong. Most ticks of most deployments observe
// nothing, so an alarm that needed a CHANGE to clear would latch forever on every
// stable deployment — which is most of them.
func TestASuccessfulRefreshWithNoChangeAlsoClearsTheAlarm(t *testing.T) {
	probe := newStalenessProbe(t)

	probe.poll.store = wiringReadFails{
		Storage: probe.counting,
		err:     aerr.New(aerr.APERTURE_STORAGE, "connection refused"),
	}
	probe.poll.tick(probe.ctx)
	if !probe.health.Posture().Stale {
		t.Fatal("the alarm did not fire")
	}

	probe.poll.store = probe.counting
	if probe.poll.tick(probe.ctx) {
		t.Fatal("a tick against unchanged wiring reported a change")
	}
	if p := probe.health.Posture(); p.Stale {
		t.Errorf("a successful refresh that saw no change left the alarm standing: %+v — "+
			"the alarm's subject is 'could this instance find out', and a completed read "+
			"answers that even when the answer is 'nothing changed'", p)
	}
}

// TestAPollReportCarriesOnlyDigestsDurationsAndCodes is the disclosure rule for
// this surface, applied to both channels it writes to. A wiring report is
// operational narration on a shared process; an object type, an id or a key
// reaching it would make it a channel for model data, and a posture read is
// answered to a system-admin of ONE account.
func TestAPollReportCarriesOnlyDigestsDurationsAndCodes(t *testing.T) {
	probe := newStalenessProbe(t)
	probe.poll.store = wiringReadFails{
		Storage: probe.counting,
		err:     aerr.New(aerr.APERTURE_STORAGE, "dial tcp: connect: connection refused"),
	}
	probe.poll.tick(probe.ctx)

	p := probe.health.Posture()
	haystack := probe.out.String() + " " + p.Reason + " " + p.Digest + " " + p.Code
	// Nothing from the model, and nothing from the wiring's own statements: the
	// seed's object ids, its account, its principal, and the SQL the shared
	// providers run.
	for _, forbidden := range []string{
		"project:atlas", "project:bronze", "alice", "acme",
		"SELECT", "documents", "perm-project-read",
	} {
		if strings.Contains(haystack, forbidden) {
			t.Errorf("a staleness report or posture mentions %q: %q", forbidden, haystack)
		}
	}
}

// TestPollingOffIsNotStale is the default every deployment is in. Boot-only wiring
// is the wiring the instance was told to run: it never looks again, so it can
// never be stale in this sense, and reporting it as stale would put a standing
// alarm on every instance that never opted in.
func TestPollingOffIsNotStale(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "unpolled.db")
	pushWiring(t, dsn, staleWiringSet(time.Now().UTC()))
	routeSharedMain(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newPollProbe(t, ctx, dsn, "")
	if probe.poll != nil {
		t.Fatal("the unconfigured probe started a poller")
	}
	if p := probe.health.Posture(); p.Polling || p.Stale || p.Every != 0 {
		t.Errorf("a boot-only instance reported %+v, want not polling and not stale", p)
	}
}

// TestThePollerAndTheFacadeShareOneRecorder is the wiring assertion behind the
// whole surface, and it fails by passing if it is skipped: a facade holding a
// recorder of its OWN would answer every read with a permanently healthy posture
// no matter what the loop observed, every test of the recorder would still pass,
// and the result is silent staleness reintroduced one layer above the thing that
// closed it.
//
// So the probe is built the way runServe builds it — one recorder, handed to the
// facade and to the poller — and the assertion is that a failure the POLLER
// observed is visible through the FACADE.
func TestThePollerAndTheFacadeShareOneRecorder(t *testing.T) {
	probe := newStalenessProbe(t)
	if probe.poll.health != probe.health {
		t.Fatal("the poller and the probe hold different recorders")
	}

	svc := probe.stack.newService(service.WithWiringHealth(probe.health))
	probe.poll.store = wiringReadFails{
		Storage: probe.counting,
		err:     aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "no route for a connection this wiring names"),
	}
	probe.poll.tick(probe.ctx)

	// No gate is wired on a one-shot stack, so the read refuses rather than
	// answering — which is itself the contract (a system-tier read is never served
	// ungated). What this case needs is only that the two hold ONE recorder, which
	// the pointer identity above and the recorder's own state below establish.
	if _, err := svc.WiringPosture(probe.ctx, service.Actor{Principal: "alice", Account: "acme"}); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("an ungated facade answered the posture read: %v", err)
	}
	if p := probe.health.Posture(); !p.Stale || p.Code != string(aerr.APERTURE_WIRING_CONNECTION_UNROUTED) {
		t.Errorf("the failure the poller observed did not reach the shared recorder: %+v", p)
	}
}

// TestAnAdoptionThatFailsIsStaleAndNotHealthy is the case the interleaved merge of
// E4-S2 (the swap), E4-S3 (the frozen connection-name set) and E4-S4 (the alarm)
// left uncovered, and the defect it caught was live on the branch for one commit.
//
// tick clears any standing alarm as soon as the READ and the DIGEST succeed, which
// is right on its own terms: the alarm's subject is "could this instance find out
// whether the wiring changed", and a completed read answers that question. But the
// adoption comes AFTER that point, so a push this instance refuses used to leave
// the posture reporting HEALTHY while stderr said the opposite.
//
// That is the worst shape of the staleness this epic exists to close, because it is
// the one polling cannot discover. Every subsequent tick reads fine, clears fine,
// refuses the same push again, and reports a healthy instance running wiring its
// operator replaced. An operator watching the posture — which is the surface E4-S4
// built precisely so nobody has to tail a log — would see nothing at all.
//
// The frozen connection-name set is the cleanest way to provoke it: a refusal that
// happens BEFORE anything is rebuilt, with its own code, and one that can never
// clear by itself. It stands in for every failed adoption, which is why the
// assertions below are about the POSTURE and not about connections.
func TestAnAdoptionThatFailsIsStaleAndNotHealthy(t *testing.T) {
	probe := newStalenessProbe(t)
	baseline := probe.poll.digest
	probe.decides(t, "before the refused push")

	// A push that adds a connection name this instance has no route for. The read
	// and the digest both succeed, so refreshed() clears the alarm on the way past;
	// the adoption is what fails.
	set := staleWiringSet(time.Now().UTC())
	set.Connections = append(set.Connections,
		model.WiringConnection{Name: "replica", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err := probe.counting.ReplaceWiring(probe.ctx, set); err != nil {
		t.Fatalf("pushing the wiring: %v", err)
	}
	if probe.poll.tick(probe.ctx) {
		t.Fatal("a push this instance cannot adopt was reported as adopted")
	}

	// Last-good, and still deciding.
	if probe.poll.digest != baseline {
		t.Error("a refused adoption advanced the digest, so the next tick would not " +
			"re-detect the change and the refusal would be reported exactly once")
	}
	probe.decides(t, "after the refused push")

	// And the posture says so. This is the assertion that was failing.
	p := probe.health.Posture()
	if !p.Stale {
		t.Fatalf("an instance that REFUSED a push reports itself healthy: %+v\n"+
			"It knows the deployed wiring changed, it knows it did not adopt it, and the only "+
			"place that fact appears is stderr — which is what the posture exists to replace", p)
	}
	if p.Code != string(aerr.APERTURE_WIRING_RESTART_REQUIRED) {
		t.Errorf("the posture's code = %q, want %q: the operator needs the code whose fixups "+
			"say to restart, not a generic refresh failure", p.Code, aerr.APERTURE_WIRING_RESTART_REQUIRED)
	}
	if p.Digest != baseline {
		t.Errorf("the posture reports digest %q, want the last-good %q this instance is still "+
			"deciding from — claiming the pushed digest would claim an adoption that was refused",
			p.Digest, baseline)
	}

	// It does not clear by itself, and that is the point of THIS failure mode: a
	// store that went away comes back, but a name set this instance cannot route
	// stays unroutable until somebody restarts it. A second tick must not launder
	// the refusal into health.
	if probe.poll.tick(probe.ctx) {
		t.Fatal("the second tick adopted a push the first one refused")
	}
	if again := probe.health.Posture(); !again.Stale {
		t.Error("a second tick cleared the alarm and reported health, because the read " +
			"succeeded again — the refusal is re-detected every tick and must re-arm every time")
	}
}
