package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// E4-S2: providers, field types and attribute providers rebuild live, and a
// decision sees ONE version.
//
// # The lever these cases pull, and why it needs no database
//
// The only provider kind shared wiring may carry is `kind: sql`, so a case that
// proved a swap by re-pointing a statement would need a live Postgres and would
// therefore not run in CI. A `field_types:` row needs no connection at all and it
// is every bit as load-bearing: it CANONICALISES the metadata an inline objects:
// entry carries, so declaring `document.released: datetime` turns
// "2026-03-04T01:02:03.456Z" into "2026-03-04T01:02:03Z" (see
// seed.applyFieldTypes). A rule comparing that field against the canonical text
// therefore DENIES before the declaration and ALLOWS after it.
//
// That flip is the whole observable these cases rest on: one shared wiring row,
// one verdict, no database, and a decision-level assertion rather than a
// structural one. A test that only compared registry pointers would pass against a
// swap that installed a rebuilt registry nothing read.
//
// # The property that needs the most care
//
// "A decision sees one coherent wiring version" cannot be proven by holding the
// engine and swapping around it — that assertion is true of any implementation,
// because a held engine is a held engine. It is proven by resolving the version
// the way a request does (liveWiring.current), swapping, and then finishing the
// decision: the pinned version must still answer the pre-swap verdict while the
// holder hands the post-swap one to everybody else.
//
// The naive implementation that assertion exists to catch is the obvious one:
// mutate the installed version in place (cur.stack = next.stack) instead of
// storing a new one. Under it the pinned pointer IS the new version and the
// pre-swap verdict is gone, so TestADecisionPinsOneWiringVersionForItsWholeDuration
// fails. Confirmed by making exactly that edit.

// swapSeed is the model every case here decides against: one rule-backed
// permission over one inline object, plus one inline attribute bag.
//
// The rule compares object.released against the CANONICAL datetime text, and the
// object's own metadata is written in a NON-canonical form that names the same
// instant. So the verdict is a direct readout of whether this instance's wiring
// declares the field:
//
//	no field_types: row -> "2026-03-04T01:02:03.456Z" != "2026-03-04T01:02:03Z" -> DENY
//	document.released: datetime -> canonicalised -> equal -> ALLOW
//
// The attribute bag is a %s so a case can rewrite the file between a boot and a
// refresh, which is how the attribute-cache criterion is asserted: the rebuilt
// slot must serve the NEW bag, not the one the old configuration cached.
const swapSeed = `
accounts:
  - {id: acme, name: Acme Corp}
memberships:
  - {principal: alice, account: acme}
object_types:
  - name: document
    description: A protected document.
    actions: [read]
permissions:
  - id: perm-doc-read
    object_type: document
    action: read
    scope_strategy: "inclusive;rule=released-on-time"
    description: Read a document the released-on-time rule selects.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May read released documents., permissions: [perm-doc-read]}
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-read
    object: "account:acme/**"
    effect: allow
rules:
  - name: released-on-time
    description: Selects the document released at exactly the canonical instant.
    ast:
      type: compare
      op: eq
      left: {type: var, name: object.released}
      right: {type: literal, value: "2026-03-04T01:02:03Z"}
objects:
  - id: "account:acme/document:42"
    metadata: {released: "2026-03-04T01:02:03.456Z"}
attributes:
  - subject: user
    id: alice
    metadata:
      department: %s
`

// declaredReleasedWiring is the pushed wiring the flip turns on: one field_types
// row and NOTHING else.
//
// No connections: and no providers: on purpose. A connection NAME SET is frozen for
// the life of a process (borrowBootPools), so a push that introduced one could not
// be adopted at all and every case here would be asserting the refusal instead of
// the swap. This is the smallest push that changes what a decision reads.
func declaredReleasedWiring(now time.Time) model.WiringSet {
	return model.WiringSet{
		FieldTypes: []model.WiringFieldType{{
			ObjectType:   "document",
			Field:        "released",
			DeclaredType: "datetime",
			CreatedAt:    now,
			UpdatedAt:    now,
		}},
	}
}

// swapProbe is one serving instance: the store, the boot stack, the version holder
// the poller adopts through, and the poller itself.
type swapProbe struct {
	store    model.Storage
	seedPath string
	live     *liveWiring
	poll     *wiringPoll
	out      *strings.Builder
}

// newSwapProbe boots an instance on department and drives the whole real path:
// buildStore, buildDecisionStack, newLiveWiring over buildWiredStack, and the
// poller started from the boot's own digest.
//
// The boot sees EMPTY shared wiring, which is the state every deployment that has
// never run `aperture wiring push` is in.
func newSwapProbe(t *testing.T, ctx context.Context, department string) *swapProbe {
	t.Helper()
	return newSwapProbeWiredWith(t, ctx, department, swapSeed, model.WiringSet{})
}

// newSwapProbeWiredWith is newSwapProbe over a store that ALREADY carries shared
// wiring, and over a chosen seed fixture.
//
// The frozen-connection-name cases need both: a boot whose manifest declares a
// connection (so a later push can REMOVE it) and, for the locally-routed case, a
// seed file with a connections: block of its own. Everything else about the path is
// identical, because the property under test is what a swap does and not how the
// boot was assembled.
//
// boot is written with ReplaceWiring after buildStore has run Setup and before the
// stack is built, which is the real order: a DB-wired instance reads rows somebody
// else pushed before it was started.
func newSwapProbeWiredWith(t *testing.T, ctx context.Context, department, fixture string, boot model.WiringSet) *swapProbe {
	t.Helper()

	dir := t.TempDir()
	seedPath := filepath.Join(dir, "swap.yaml")
	writeSeedFixture(t, seedPath, fixture, department)
	dsn := "file:" + filepath.Join(dir, "swap.db")

	store, err := buildStore(ctx, dsn, seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if !boot.IsEmpty() {
		if err := store.ReplaceWiring(ctx, boot); err != nil {
			t.Fatalf("seeding the boot wiring: %v", err)
		}
	}

	probe := &swapProbe{store: store, seedPath: seedPath, out: &strings.Builder{}}
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: append(storeFlags(), wiringPollFlag()),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			stack, err := buildDecisionStack(ctx, cmd, store, seedPath)
			if err != nil {
				return err
			}
			t.Cleanup(func() { _ = stack.Close() })
			probe.live = newLiveWiring(
				&wiringVersion{
					stack: stack,
					svc:   stack.newService(),
					// The handler is what liveWiring.ServeHTTP resolves once per request;
					// it answers with the digest of the version serving it, which is how
					// the in-flight case below observes which version a request got.
					handler: digestHandler(stack.wiringDigest),
					digest:  stack.wiringDigest,
				},
				func(_ context.Context, set model.WiringSet, digest string) (*wiringVersion, error) {
					next, err := buildWiredStack(dsn, store, seedPath, set, borrowBootPools(stack.conns), nil, nil)
					if err != nil {
						return nil, err
					}
					return &wiringVersion{
						stack:   next,
						svc:     next.newService(),
						handler: digestHandler(digest),
						digest:  digest,
					}, nil
				})
			probe.poll = startWiringPoll(ctx, store, time.Hour, stack.wiringDigest, probe.live.swap, probe.out)
			return nil
		},
	}
	if err := cmd.Run(ctx, []string{"probe", "--store", dsn, "--seed", seedPath, "--wiring-poll", "1h"}); err != nil {
		t.Fatalf("booting the probe: %v", err)
	}
	if probe.poll == nil {
		t.Fatal("--wiring-poll 1h started no poller")
	}
	return probe
}

// writeSwapSeed writes the default fixture with department substituted in.
func writeSwapSeed(t *testing.T, path, department string) {
	t.Helper()
	writeSeedFixture(t, path, swapSeed, department)
}

// writeSeedFixture writes one of this file's fixtures with department substituted in.
func writeSeedFixture(t *testing.T, path, fixture, department string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(fixture, department)), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}

// digestHandler is a handler that names the version it belongs to. It stands in
// for server.New(svc), which no case here needs: what is being asserted about
// ServeHTTP is which VERSION answered a request, not what the Twirp surface does
// with it.
func digestHandler(digest string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(digest))
	})
}

// releasedQuery is the decision every case reads the wiring through.
func releasedQuery() service.Query {
	return service.Query{Account: "acme", Principal: "alice", Action: "read", Object: "account:acme/document:42"}
}

// verdict answers releasedQuery through one version and fails the test on an
// input-validation error, which is never what these cases are about.
func verdict(t *testing.T, ctx context.Context, v *wiringVersion) bool {
	t.Helper()
	res, err := v.svc.Check(ctx, releasedQuery())
	if err != nil {
		t.Fatalf("Check through version %s: %v", shortDigest(v.digest), err)
	}
	return res.Allow
}

// pushFieldType writes the wiring that declares document.released and drives one
// tick, asserting the change was adopted.
func (p *swapProbe) pushFieldType(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := p.store.ReplaceWiring(ctx, declaredReleasedWiring(time.Now().UTC())); err != nil {
		t.Fatalf("pushing the field type: %v", err)
	}
	if !p.poll.tick(ctx) {
		t.Fatalf("the push was not adopted; the poller reported:\n%s", p.out.String())
	}
}

// TestAWiringPushIsReflectedInADecisionWithoutARestart is the story's first
// criterion, asserted as a VERDICT rather than as a rebuilt pointer: the same
// decision, the same process, two answers either side of one `wiring push`.
func TestAWiringPushIsReflectedInADecisionWithoutARestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	if verdict(t, ctx, probe.live.current()) {
		t.Fatal("the booted instance ALLOWED before document.released was declared: the fixture's " +
			"non-canonical metadata should not equal the canonical text the rule compares against, " +
			"so this case can no longer tell a swap from no swap")
	}

	probe.pushFieldType(t, ctx)

	if !verdict(t, ctx, probe.live.current()) {
		t.Error("a pushed field_types: row did not change what this instance decides. Either nothing was " +
			"rebuilt, or what was rebuilt is not what the decision reads")
	}
}

// TestASwapRebuildsTheProviderRegistryTheFieldTypesAndTheAttributeProviders is the
// second criterion. All three move, and the BOOT version is left exactly as it was
// — which is the half that makes one-version-per-decision possible at all.
func TestASwapRebuildsTheProviderRegistryTheFieldTypesAndTheAttributeProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	bootReg, bootAttrs, bootEngine := boot.stack.registry, boot.stack.attributes, boot.stack.eng

	probe.pushFieldType(t, ctx)
	next := probe.live.current()

	if next == boot {
		t.Fatal("the holder still returns the boot version: nothing was installed")
	}
	if next.stack.registry == bootReg {
		t.Error("the object provider registry was not rebuilt")
	}
	if next.stack.attributes == bootAttrs {
		t.Error("the attribute registry was not rebuilt. A reused registry keeps the slot caches the OLD " +
			"configuration filled, and a slot's ttl: is the window a revoked clearance keeps authorizing for")
	}
	if next.stack.eng == bootEngine {
		t.Error("the decision engine was not rebuilt, so it still reads through the superseded registries")
	}
	// The field types are not a field of the stack — they are FOLDED INTO the
	// registry — so the only honest assertion about them is behavioural, and it is
	// the verdict flip. Asserted here too so this case fails if the rebuild replaced
	// three pointers with three equivalent ones.
	if !verdict(t, ctx, next) {
		t.Error("the rebuilt registry does not carry the pushed field_types: declaration")
	}

	// And the boot version is untouched: a swap installs, it does not mutate.
	if boot.stack.registry != bootReg || boot.stack.attributes != bootAttrs || boot.stack.eng != bootEngine {
		t.Error("the swap mutated the version the boot built. A decision holding it would then see half " +
			"of a push, which is the one thing this machinery exists to prevent")
	}
}

// TestADecisionPinsOneWiringVersionForItsWholeDuration is the criterion with the
// most room for a plausible-looking wrong implementation, so it is asserted the way
// a request actually works: resolve the version once, let a swap land, then finish
// the decision.
//
// It FAILS on the naive implementation — mutating the installed version in place
// rather than storing a new one — because the pinned pointer would then be the new
// version and the pre-swap verdict would be unobtainable. That was confirmed by
// making the edit.
func TestADecisionPinsOneWiringVersionForItsWholeDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	// The pin: one resolution, at entry, exactly as liveWiring.ServeHTTP makes it.
	pinned := probe.live.current()
	if verdict(t, ctx, pinned) {
		t.Fatal("the pre-swap verdict is already ALLOW, so this case cannot distinguish the two versions")
	}

	probe.pushFieldType(t, ctx)

	// The decision that was already under way finishes on the wiring it started on.
	if verdict(t, ctx, pinned) {
		t.Error("a swap changed what a decision already in flight sees. A decision must see ONE coherent " +
			"wiring version for its whole duration: half of a push is a verdict that answers no question " +
			"anybody asked, and nothing anywhere reports it")
	}
	// While everybody who resolves after it sees the new one.
	if !verdict(t, ctx, probe.live.current()) {
		t.Error("a decision starting after the swap still sees the superseded wiring")
	}
	if probe.live.current() == pinned {
		t.Error("the holder returns the same version pointer after a swap: the version was mutated in " +
			"place rather than replaced")
	}
}

// TestARequestIsAnsweredByTheVersionResolvedAtItsEntry is the same property at the
// seam `serve` actually mounts. The listener, the authenticator and the http.Server
// are built once and survive a swap; only what is beneath them changes.
func TestARequestIsAnsweredByTheVersionResolvedAtItsEntry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	bootDigest := probe.live.current().digest
	if got := getBody(t, probe.live); got != bootDigest {
		t.Fatalf("the booted handler answered %q, want the boot digest %q", shortDigest(got), shortDigest(bootDigest))
	}

	probe.pushFieldType(t, ctx)
	swapped := probe.live.current().digest
	if swapped == bootDigest {
		t.Fatal("the adopted version carries the boot digest, so this case proves nothing")
	}
	if got := getBody(t, probe.live); got != swapped {
		t.Errorf("a request after the swap was answered by version %q, want the adopted %q: the handler "+
			"is resolved per request, not once at mount", shortDigest(got), shortDigest(swapped))
	}
}

// getBody drives one request through the dispatcher and returns the body.
func getBody(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

// TestARebuiltSlotDoesNotServeTheOldConfigurationsCache is the security criterion.
// A slot's ttl: is the window a REVOKED clearance keeps authorizing for, so a
// rebuilt slot that answered out of the superseded configuration's cache would keep
// authorizing against wiring the operator has already replaced — access-widening,
// with nothing in any verdict, trace or note to say so.
//
// The bag is warmed on the boot version FIRST, so the assertion is about a cache
// that is genuinely populated rather than one that happened to be empty.
func TestARebuiltSlotDoesNotServeTheOldConfigurationsCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	if got := department(t, ctx, boot); got != "eng" {
		t.Fatalf("the booted instance reads alice's department as %q, want \"eng\"", got)
	}
	// Read twice: whatever caching the slot does, it has now done it.
	if got := department(t, ctx, boot); got != "eng" {
		t.Fatalf("the second read of alice's department gave %q, want \"eng\"", got)
	}

	// The wiring this instance is told to adopt changes, and so does the bag behind
	// the slot. The refusal this case guards against is serving "eng" out of the old
	// registry's cache for the whole of its ttl.
	writeSwapSeed(t, probe.seedPath, "legal")
	probe.pushFieldType(t, ctx)

	if got := department(t, ctx, probe.live.current()); got != "legal" {
		t.Errorf("the rebuilt user slot reads alice's department as %q, want \"legal\": the swap served an "+
			"entry fetched under the old configuration's ttl, which is the window a revoked clearance "+
			"keeps authorizing for", got)
	}
	// The superseded version keeps its own answer, for the same reason a decision in
	// flight keeps its own wiring. That is not staleness; it is the pin.
	if got := department(t, ctx, boot); got != "eng" {
		t.Errorf("the superseded version's bag changed to %q under a decision that may still be reading "+
			"it, want \"eng\"", got)
	}
}

// department reads alice's user-slot bag through one version's attribute registry.
func department(t *testing.T, ctx context.Context, v *wiringVersion) string {
	t.Helper()
	bag, err := v.stack.attributes.Attributes(ctx, principalKindUser, "alice")
	if err != nil {
		t.Fatalf("reading alice's attributes through version %s: %v", shortDigest(v.digest), err)
	}
	s, _ := bag["department"].(string)
	return s
}

// TestASwapCannotBeSeenHalfDone drives decisions and swaps at the same time, which
// is what `go test -race` is pointed at, and asserts the property a race detector
// cannot: that every decision's answer belongs to ONE version.
//
// Each reader resolves a version the way a request does and then asks TWO
// independent questions of it — the verdict (which the field_types: row decides) and
// alice's department (which the seed file decides). Both are rewritten together, so
// there are exactly two legal pairs. A third pair is a torn read: one decision that
// saw half of one version and half of another.
func TestASwapCannotBeSeenHalfDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	bad := make(chan string, readers)

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := probe.live.current()
				res, err := v.svc.Check(ctx, releasedQuery())
				if err != nil {
					bad <- fmt.Sprintf("Check failed mid-swap: %v", err)
					return
				}
				bag, err := v.stack.attributes.Attributes(ctx, principalKindUser, "alice")
				if err != nil {
					bad <- fmt.Sprintf("attribute read failed mid-swap: %v", err)
					return
				}
				dept, _ := bag["department"].(string)
				// The two legal pairs: booted wiring (no declaration, "eng") and adopted
				// wiring (declared, "legal"). Anything else is half of a swap.
				switch {
				case !res.Allow && dept == "eng":
				case res.Allow && dept == "legal":
				default:
					bad <- fmt.Sprintf("a decision saw half of a swap: allow=%v department=%q", res.Allow, dept)
					return
				}
			}
		}()
	}

	// One swap, landing while every reader is mid-flight. The seed file is rewritten
	// first so the rebuild reads the new bag together with the new wiring.
	writeSwapSeed(t, probe.seedPath, "legal")
	probe.pushFieldType(t, ctx)

	close(stop)
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
	if !verdict(t, ctx, probe.live.current()) {
		t.Error("the swap did not take effect, so the readers above were never given anything to tear")
	}
}

// TestAFailedRebuildInstallsNothing is the last criterion, and it is the one that
// keeps an access engine answering: a refused rebuild leaves the instance exactly as
// it was rather than half-wired.
//
// A kind: csv provider row is the refusal used, and it has to be something OTHER
// than a connection name now that E4-S3 refuses a name-set change before the rebuild
// ever starts: this case needs a push that genuinely reaches the builder and fails
// there. kind: csv is that push — it is storable (model.ValidateWiringProvider records
// a kind verbatim) and unbuildable as shared wiring, because its only data source is a
// filesystem path and the shared tables have no column for one. It touches no
// connection at all, so the frozen name set is unchanged and the rebuild is what
// refuses it.
func TestAFailedRebuildInstallsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	baseline := probe.poll.digest

	now := time.Now().UTC()
	if err := probe.store.ReplaceWiring(ctx, model.WiringSet{
		Providers: []model.WiringProvider{{
			ObjectType: "document",
			Kind:       "csv",
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
	}); err != nil {
		t.Fatalf("pushing the set the rebuild cannot construct: %v", err)
	}
	if probe.poll.tick(ctx) {
		t.Fatal("a set carrying a kind: csv provider row was ADOPTED")
	}

	if probe.live.current() != boot {
		t.Error("a failed rebuild replaced the installed version. The instance must swap WHOLLY or keep " +
			"what it had; there is no state in which some of a push is installed")
	}
	if verdict(t, ctx, probe.live.current()) {
		t.Error("the verdict changed although nothing was installed")
	}
	if probe.poll.digest != baseline {
		t.Error("the digest advanced past a rebuild that failed: the change would then be forgotten and " +
			"the instance stale for the rest of its life with nothing saying so")
	}
	report := probe.out.String()
	for _, want := range []string{"CHANGED", "could not adopt", "keeps the wiring it has", "document"} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal report does not mention %q. Report:\n%s", want, report)
		}
	}
}

// TestAHolderWithNoRebuildRefusesASwapRatherThanIgnoringIt is the nil case. A
// process that accepted a change it cannot apply and said so nowhere is the
// silently-stale instance this epic exists to close, so the refusal is explicit.
func TestAHolderWithNoRebuildRefusesASwapRatherThanIgnoringIt(t *testing.T) {
	live := newLiveWiring(&wiringVersion{digest: "boot"}, nil)
	err := live.swap(context.Background(), model.WiringSet{}, "next")
	if err == nil {
		t.Fatal("a holder with no rebuild accepted a swap")
	}
	if live.current().digest != "boot" {
		t.Error("the refused swap still moved the installed version")
	}
}

// TestABorrowedPoolIsNeverClosedByTheRebuild is the resource half, and the bug it
// pins is a quiet one: seed.Connections.Close closes every pool it holds, and a
// rebuild is handed a second Connections over the SAME handles. Without the
// no-op Close, a superseded version's shutdown — or a FAILED rebuild's own cleanup,
// which closes every pool it opened on the way out — would take the serving
// instance's database access with it, and every SQL-backed decision after the first
// push would fail with APERTURE_SQL_PROVIDER_QUERY.
func TestABorrowedPoolIsNeverClosedByTheRebuild(t *testing.T) {
	var closed int
	p := &countingPool{onClose: func() { closed++ }}
	if err := (borrowedPool{Pool: p}).Close(); err != nil {
		t.Fatalf("closing a borrowed pool: %v", err)
	}
	if closed != 0 {
		t.Errorf("closing the borrowed wrapper closed the underlying pool %d time(s); the pool's lifetime "+
			"belongs to the Connections the BOOT opened", closed)
	}
}

// countingPool is a seed.Pool that counts its own Close. It is never queried.
type countingPool struct {
	onClose func()
}

func (c *countingPool) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("countingPool is never queried")
}

func (c *countingPool) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("countingPool is never queried")
}

func (c *countingPool) Close() error {
	c.onClose()
	return nil
}

// principalKindUser is the principal KIND the user attribute slot answers for. It
// is spelled out rather than taken from a constant because AttributeRegistry
// dispatches on the kind of the principal asking, not on a slot name, and the two
// happening to be the same word is not a thing to depend on.
const principalKindUser = "user"

// TestAPollerStartedWithNoSwapperRefusesLoudlyRatherThanPanicking is the fallback
// guard. A nil swapper called from the poll goroutine would panic, and a panic there
// takes the process down — which for an embedded access engine means the host stops
// deciding, the one outcome this epic rules out. So the omission becomes a refusal on
// every tick: the instance keeps its wiring, the digest does not advance, and the
// condition is reported rather than fatal.
func TestAPollerStartedWithNoSwapperRefusesLoudlyRatherThanPanicking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	// A second poller over the same store, deliberately given no swapper.
	out := &strings.Builder{}
	poll := startWiringPoll(ctx, probe.store, time.Hour, probe.live.current().digest, nil, out)
	if poll == nil {
		t.Fatal("a 1h interval started no poller")
	}
	t.Cleanup(func() { _ = poll.Close() })
	baseline := poll.digest

	if err := probe.store.ReplaceWiring(ctx, declaredReleasedWiring(time.Now().UTC())); err != nil {
		t.Fatalf("pushing the field type: %v", err)
	}
	if poll.tick(ctx) {
		t.Fatal("a poller with no swapper reported a change as ADOPTED")
	}
	if poll.digest != baseline {
		t.Error("the digest advanced although nothing could be adopted")
	}
	if !strings.Contains(out.String(), "could not adopt") {
		t.Errorf("the refusal was not reported. Report:\n%s", out.String())
	}
}
