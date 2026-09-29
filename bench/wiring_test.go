package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// WHAT THE SHARED-WIRING HOT SWAP COSTS A DECISION.
//
// E4 gave a long-lived `aperture serve` two new pieces of machinery: an opt-in
// background re-read of the shared wiring tables (--wiring-poll), and a swap that
// rebuilds every registry a decision reads and installs the result as one
// immutable version. Both sit OUTSIDE the decision path by construction — a
// request resolves its version with one atomic pointer load at entry and the
// rebuild happens on the poll goroutine — so what this file exists to hold is the
// consequence: neither costs a decision anything that matters at any setting an
// operator would choose, and both stay inside the FR-31 budget even when driven far
// past one. "Off the decision path by construction" is an argument; these two cases
// are the measurement, and they report what they measured rather than the
// conclusion they were built to support.
//
// A claim about performance that nobody measured is a claim, so there are two
// cases here and they answer two different questions:
//
//   - TestCheckNFRWiringPoll — does a live poll loop cost a decision anything?
//     Same fixture, same query and the same sample budget as TestCheckNFR, with a
//     background loop doing exactly what a tick does, at an interval chosen to
//     BOUND every deployable one from above.
//   - TestCheckNFRAfterAWiringSwap — a swap installs a version whose caches are
//     EMPTY, so there is a cold period after every push. How long, and does the
//     instance come back to the NFR afterwards?
//
// Both are named TestCheckNFR* so the one documented invocation covers them:
//
//	APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
//
// -run takes an unanchored regexp and that command is the only one anybody runs,
// so a case named anything else would be a gate case that is never run.

// wiringPollBenchInterval is how often the load loop in TestCheckNFRWiringPoll
// does a tick's work. It is an UPPER BOUND ON A DEPLOYMENT, not a deployment.
//
// The honest difficulty with benchmarking a poll interval is that no deployable
// value ticks inside a benchmark at all. The default is 30s (the window a fleet is
// allowed to disagree with itself — see docs/src/cli/serve.md, "Choosing an
// interval"), and a gate case measures for seconds, so a case that polled at the
// default would measure ZERO ticks and assert nothing. That is the fail-by-passing
// shape: a green case proving only that a ticker did not fire.
//
// So the case measures the pathological end instead and reads the result as a
// BOUND. 1ms is 30 000 ticks for every one the default makes, and whatever a tick
// costs at that rate, a deployable interval pays a thirty-thousandth of it. The
// measured answer is that the cost at 1ms is already small — the p99 inside the
// run-to-run spread of the loop-free arm, throughput a few percent down, both arms
// clearing both thresholds — which is what makes the conclusion an operator acts on
// (choose the interval on staleness grounds, never on decision latency) a
// consequence of the measurement rather than an argument about it. The figures, both
// runs of them, are in docs/benchmarks.md under "The hot-swap cases"; this constant
// is not the place to keep a number that moves.
//
// It is deliberately not smaller. Below roughly this the loop stops being a model
// of a poller and becomes a second CPU-bound goroutine, and what it would then
// measure is scheduler contention on the machine running the gate. It is also not
// LARGER: at 10ms the tick count inside a measurement falls to a couple of thousand
// and the arm stops being a bound worth quoting.
const wiringPollBenchInterval = time.Millisecond

// coldWindow is how many decisions TestCheckNFRAfterAWiringSwap times one by one
// on a freshly installed wiring version, to see the cold period out.
//
// It is generous on purpose: the quantity being reported is "how long does a push
// cost", and a window that ended inside the cold period would report a floor
// rather than a cost. A hundred decisions is already well past convergence on
// every fixture here (the caches a version starts with are filled by the first
// decision that needs each entry), and timing a hundred Checks is free next to the
// sample budgets around it.
const coldWindow = 100

// wiringLoad is a background loop that does exactly what one wiringPoll tick does:
// one GetWiring against the store the decisions are running against, then a
// content digest of the result.
//
// # Why this models the tick rather than driving it
//
// The real loop is internal/cli's wiringPoll, and it is unexported in a package
// this one must not import to run a benchmark. What it does per tick, though, is
// two operations wide and both are public: readSharedWiring is one
// model.Storage.GetWiring, and wiringDigest is a canonical-JSON SHA-256 over the
// returned set. Those are what this reproduces, against the same store, at a fixed
// interval, with no change ever deployed so the swap never fires — which is the
// state a poller is in on every tick of every deployment whose wiring is stable,
// i.e. almost all of them.
//
// The drift this accepts is named so it can be checked: if a tick ever stops being
// one read plus one digest — a second query, a per-tick rebuild, a probe — this
// load model stops bounding it and the comment above is what says so. Nothing here
// is a second implementation of anything that decides; it is a load generator.
//
// # Why it counts its ticks
//
// A loop that silently stopped — a cancelled context, a store that started
// failing, an interval that never elapsed — would leave the case measuring an
// undisturbed decision path and reporting it as the polling-on arm. stop() asserts
// the loop ticked, so the arm cannot pass by measuring nothing.
type wiringLoad struct {
	ticks  atomic.Int64
	failed atomic.Pointer[error]
	cancel context.CancelFunc
	done   chan struct{}
}

// startWiringLoad starts the load loop against store and returns it. It never
// fails the test from its own goroutine — a failure is recorded and reported by
// stop, on the test's goroutine, which is where testing.T may be used.
func startWiringLoad(store model.Storage, every time.Duration) *wiringLoad {
	ctx, cancel := context.WithCancel(context.Background())
	l := &wiringLoad{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := wiringLoadTick(ctx, store); err != nil {
					if ctx.Err() != nil {
						return
					}
					l.failed.Store(&err)
					return
				}
				l.ticks.Add(1)
			}
		}
	}()
	return l
}

// stop ends the loop, waits for it, and asserts it did the work the arm claims it
// did: it ticked, and nothing it did failed.
func (l *wiringLoad) stop(t *testing.T, label string) int64 {
	t.Helper()
	l.cancel()
	<-l.done
	if errp := l.failed.Load(); errp != nil {
		t.Fatalf("%s: the wiring poll load loop failed: %v", label, *errp)
	}
	n := l.ticks.Load()
	if n == 0 {
		// Not a warning. A zero-tick loop makes this arm identical to the
		// polling-off arm while claiming to be the polling-on one.
		t.Fatalf("%s: the wiring poll load loop never ticked, so this arm measured an "+
			"undisturbed decision path and called it the polling-on one", label)
	}
	return n
}

// wiringLoadTick is one tick's work: the read and the digest, in that order.
//
// The stamps are zeroed before the digest for the reason the production digest
// does it — a push rewrites every row, so a stamp-sensitive digest would report a
// change for an identical re-push — and the set is sorted so the digest is a
// property of the wiring and not of the read. The production helper copies the set
// first because its caller goes on to use it; this one owns what GetWiring handed
// it, and the copy is four slice memcpys either way.
func wiringLoadTick(ctx context.Context, store model.Storage) (string, error) {
	set, err := store.GetWiring(ctx)
	if err != nil {
		return "", err
	}
	for i := range set.Connections {
		set.Connections[i].CreatedAt, set.Connections[i].UpdatedAt = time.Time{}, time.Time{}
	}
	for i := range set.Providers {
		set.Providers[i].CreatedAt, set.Providers[i].UpdatedAt = time.Time{}, time.Time{}
	}
	for i := range set.FieldTypes {
		set.FieldTypes[i].CreatedAt, set.FieldTypes[i].UpdatedAt = time.Time{}, time.Time{}
	}
	for i := range set.AttributeProviders {
		set.AttributeProviders[i].CreatedAt, set.AttributeProviders[i].UpdatedAt = time.Time{}, time.Time{}
	}
	set.Sort()
	raw, err := json.Marshal(set)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// deployWiringFixture writes a shared wiring set into the bench store, so the
// read and the digest a tick performs have something to read and digest.
//
// An empty set would be the cheapest possible tick and therefore the least
// informative one: GetWiring would return four empty slices and the digest would
// hash a constant. The size here is meant to be a plausibly large DEPLOYMENT
// rather than a stress test — a couple of dozen object types with a field-type
// declaration or two each, and all three attribute slots wired — because the
// question is what a tick costs a real fleet and not how big a JSON document
// SHA-256 can chew.
//
// It is wiring in the shape the five tables hold, not wiring this process is
// wired FROM: the bench's providers are the hand-built benchProvider, and nothing
// here is projected into a registry. That is exactly the relationship a poller has
// with the rows on a tick that finds no change.
//
// The object types are written first because a wiring provider row is a CHILD of
// apt_object_types — every backend refuses one whose object type the model has no
// row for, which is the same refusal `aperture wiring push` reports as
// APERTURE_WIRING_OBJECT_TYPE_UNKNOWN. A fixture that skipped them would be
// measuring a set no store would hold.
func deployWiringFixture(tb testing.TB, store *memory.Store) model.WiringSet {
	tb.Helper()
	ctx := context.Background()
	const providers = 24
	set := model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
	}
	for i := 0; i < providers; i++ {
		objectType := fmt.Sprintf("doctype%02d", i)
		if err := store.PutObjectType(ctx, model.ObjectType{
			Name:    objectType,
			Actions: []string{"read"},
		}); err != nil {
			tb.Fatalf("deploy wiring fixture: object type %q: %v", objectType, err)
		}
		set.Providers = append(set.Providers, model.WiringProvider{
			ObjectType: objectType,
			Kind:       "sql",
			Connection: "main",
			GetOne:     fmt.Sprintf("SELECT id, owner, level FROM %s WHERE id = $1", objectType),
			GetAll:     fmt.Sprintf("SELECT id, owner, level FROM %s", objectType),
			IDColumn:   "id",
			TTL:        "30s",
			MaxSize:    4096,
			References: []model.WiringReference{{Field: "project", TargetType: "project"}},
		})
		set.FieldTypes = append(set.FieldTypes,
			model.WiringFieldType{ObjectType: objectType, Field: "owner", DeclaredType: "string"},
			model.WiringFieldType{ObjectType: objectType, Field: "level", DeclaredType: "int"},
		)
	}
	for _, subject := range []string{"user", "group", "account"} {
		set.AttributeProviders = append(set.AttributeProviders, model.WiringAttributeProvider{
			Subject:    subject,
			Kind:       "sql",
			Connection: "main",
			GetOne:     "SELECT id, clearance, region FROM " + subject + "_attrs WHERE id = $1",
			GetAll:     "SELECT id FROM " + subject + "_attrs",
			IDColumn:   "id",
			TTL:        "30s",
			MaxSize:    2048,
			DeclaredKeys: model.DeclaredKeys{
				Declared: true,
				Keys:     []string{"clearance", "region"},
			},
		})
	}
	if err := store.ReplaceWiring(ctx, set); err != nil {
		tb.Fatalf("deploy wiring fixture: %v", err)
	}
	return set
}

// TestCheckNFRWiringPoll is the POLLING-ON arm of the hard NFR gate: the same
// p99 < 1ms / >= 10k checks/sec assertion as TestCheckNFR, over the same fixture
// and the same query, with a live wiring poll loop running underneath it.
//
// It exists because "the swap machinery is off the decision path" is a claim about
// construction, and the background loop is the one part of E4 that competes with a
// decision for a core whether the design is right or not. The arms are deliberately
// comparable: same fixture, same query, same audit axis, same sample budget
// (fullSamples), so the two sets of numbers differ in the loop and nothing else and
// the comparison is worth writing down.
//
// The interval is wiringPollBenchInterval and bounds every deployable one from
// above — see that constant. The tick count is logged and asserted non-zero, so a
// green run is a run in which the loop really did tick inside the measurement.
//
// Gated identically to the rest of the suite: skipped under -short and unless
// APERTURE_BENCH_ASSERT=1.
func TestCheckNFRWiringPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	for _, withAudit := range []bool{false, true} {
		name := "audit-off"
		if withAudit {
			name = "audit-on"
		}
		t.Run(name, func(t *testing.T) {
			m := buildModel(t)
			deployWiringFixture(t, m.store)
			svc, closeFn := newService(t, m, withAudit)
			defer closeFn()

			label := name + "/wiring-poll-" + wiringPollBenchInterval.String()
			load := startWiringLoad(m.store, wiringPollBenchInterval)
			assertCheckNFR(t, svc, toQuery(m.req), label, true, fullSamples)
			ticks := load.stop(t, label)
			t.Logf("%s: the loop completed %d ticks (one GetWiring + one digest each) inside the "+
				"measurement, at %v — %.0fx the 30s default", label, ticks, wiringPollBenchInterval,
				float64(30*time.Second)/float64(wiringPollBenchInterval))
		})
	}
}

// TestCheckNFRAfterAWiringSwap is the COLD-CACHE arm: what an adopted push costs
// the decisions that follow it, and whether the instance returns to the NFR.
//
// # The thing being measured
//
// A swap installs a whole new wiring version — a fresh object registry, a fresh
// rules engine over it, a fresh decision engine — and every cache in it starts
// EMPTY. The superseded version's warmed per-type metadata cache and parsed-pattern
// cache go with it. That is by design, and the design is a security one: a rebuilt
// slot must never answer from an entry fetched under the old configuration's ttl:,
// which is the window a revoked clearance would otherwise keep authorizing for.
// The price is a brief cold period after each push, which E4-S2 recorded as a known
// limitation at the cadence of a human pushing wiring. This case is what turns
// "brief" into a number.
//
// # What it asserts, and what it only reports
//
// It ASSERTS that the version installed by a swap clears the same p99 and
// throughput targets the booted one does. That is the property that matters and
// the one a regression would break: a swap that installed a permanently colder
// stack — a registry that never caches, a rules engine that recompiles per
// decision — would show up here and nowhere else in the suite, because every other
// case measures a stack that booted.
//
// It only REPORTS the cold figures themselves. A wall-clock assertion on a single
// uncached decision is a measurement of the machine: one Check that has to compile
// a rule and fetch metadata is exactly the sample a descheduled goroutine lands in,
// and best-of-rounds cannot help a population of one. The numbers are logged so a
// human reads them, and the sentence they support in
// docs/src/operations/wiring-refresh.md is about a cost in decisions rather than a
// threshold in milliseconds.
//
// The rebuild uses buildRuleLayer against the SAME store, which is what a swap
// does: the model is untouched, the registries are new. The rule-backed fixture is
// used rather than the literal one because it exercises all three caches a version
// carries — parsed patterns, compiled rules, per-type metadata — where a literal
// scope check would only warm the first.
func TestCheckNFRAfterAWiringSwap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	ctx := context.Background()
	m := buildModel(t)
	q := ruleQuery(t, m, ruleScalar)

	// The booted version, warm: the control every cold number below is read
	// against.
	//
	// The control is the MEDIAN and not the p99, for a reason the first draft of
	// this case got wrong and reported nonsense over: the cold period is a handful
	// of decisions, and a handful of samples read against the TAIL of a thousand
	// comes out "faster than steady state" — the tail is where a descheduled
	// goroutine lands, not where a typical decision does. What a one-off cost has to
	// be compared with is what a decision usually costs. The p99 still appears
	// below, as the gate, which is where an absolute threshold belongs.
	booted, closeBooted := newRuleService(t, m, false)
	defer closeBooted()
	warm(t, booted, q, true)
	steady := measureQuantile(ctx, booted, q, coldWindow*10, 0.5)
	t.Logf("after-swap: the BOOTED version's warm median Check over %d samples is %v", coldWindow*10, steady)

	// The swap. Same store, same model, new registries — which is the whole of
	// what a version is.
	swapped := swappedRuleService(t, m)

	cold := make([]time.Duration, coldWindow)
	for i := range cold {
		t0 := time.Now()
		if _, err := swapped.Check(ctx, q); err != nil {
			t.Fatalf("after-swap: Check %d: %v", i, err)
		}
		cold[i] = time.Since(t0)
	}

	// Two numbers, because they answer the two questions an operator has: how long
	// is the cold period, and what did the whole push cost.
	//
	// converged is the first decision from which none of the following ones is
	// slower than twice the booted median — "twice" being a threshold for a LOG
	// line, not an assertion, chosen to sit clear of ordinary jitter at these
	// latencies. excess is the total time the cold window spent above that median,
	// which is the one-off cost of the push expressed as work rather than as a rate.
	converged := coldWindow
	for i := range cold {
		settled := true
		for j := i; j < coldWindow; j++ {
			if cold[j] > 2*steady {
				settled = false
				break
			}
		}
		if settled {
			converged = i
			break
		}
	}
	var excess time.Duration
	for _, d := range cold {
		if d > steady {
			excess += d - steady
		}
	}
	t.Logf("after-swap: the first decision on the new version took %v (%.1fx the booted median); the window "+
		"settled within twice that median after %d decisions; the whole cold period cost %v above steady state "+
		"across %d decisions", cold[0], float64(cold[0])/float64(steady), converged, excess, coldWindow)

	// And the assertion: a swapped version is a normal version. It has been warmed
	// by the cold window above, so this measures the same steady state every other
	// case measures — which is the point.
	assertCheckNFR(t, swapped, q, "after-swap", true, variantSamples)
}

// swappedRuleService builds the rule-backed decision stack a second time over the
// same store, which is what a wiring swap installs: a new object registry, a new
// rules engine over it, a new decision engine, and a facade over that. Every cache
// in the result is empty.
//
// It goes through buildRuleLayer and mirrors newRuleService rather than reaching
// into the booted stack, for the reason serveFacadeOptions exists in
// internal/cli/serve.go: a version composed differently from the one the boot
// composed is two engines in one process, and a bench that measured a
// differently-composed stack would be measuring something no push installs.
//
// Audit is off. It is a process-lifetime dependency that a swap passes through
// untouched — the recorder outlives every version — so re-wiring one here would
// measure a second audit writer rather than a second wiring version.
func swappedRuleService(tb testing.TB, m benchModel) *service.Service {
	tb.Helper()
	must := func(err error) {
		if err != nil {
			tb.Fatalf("rebuild the wiring version: %v", err)
		}
	}
	reg, ruleEngine, _ := buildRuleLayer(tb, context.Background(), m.store, must)
	eng := engine.New(m.store, engine.WithScopeResolution(
		scope.DefaultRegistry(),
		engine.ScopeDeps{Lister: reg, Rules: ruleEngine},
	))
	return service.New(eng)
}
