package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// E4-S3: the connection NAME SET is frozen for the life of a process, and a push
// that changes it is flagged rather than applied.
//
// These cases share wiring_swap_test.go's probe and its fixture, because the
// observable is the same one: a pushed field_types: row flips one verdict, so
// "adopted" and "held" are a decision apart and not a pointer comparison.
//
// # What the cases below have to distinguish
//
// Three outcomes look alike from the outside and are not the same thing:
//
//  1. the push was ADOPTED,
//  2. the push REACHED the rebuild and the rebuild refused it, and
//  3. the push was refused BEFORE anything was built, as a standing condition
//     about this process rather than a complaint about the document.
//
// All three leave the instance deciding, and (2) and (3) both leave the digest
// where it was. So every case here asserts more than "it was not adopted": the
// version POINTER (nothing was rebuilt at all), the CODE (only (3) is
// APERTURE_WIRING_RESTART_REQUIRED), and the added/removed split (which is what
// tells an operator whether to export a DSN or to drain a pool).
//
// The split is read off the coded error's context map, or off the line the poller
// printed. There is deliberately no second record of the condition on the holder:
// the operator-visible posture is service.WiringHealth, and a latch beside it was
// a second answer that only a successful swap cleared — so a push REVERTED to the
// boot's own wiring left the posture healthy and the latch still naming the added
// connection, for the life of the process. See wiring_swap.go's header, and
// TestARevertedConnectionChangeStopsBeingReported.
//
// # The two traps
//
// A REMOVED name is the half that passes silently under the obvious
// implementation. borrowBootPools is fail-closed on a name it has no pool for, so
// the ADD direction was already refused before this story; a name VANISHING from
// the manifest fails nothing at all — every provider that remains still resolves —
// so the push applied, and the process was left holding a pool for a connection
// the deployment had retired while its own `wiring diff` reported the retirement
// as landed. Confirmed by removing the check and watching
// TestAPushThatRemovesAConnectionNameIsFlaggedAndNotApplied adopt.
//
// The other trap is the BASELINE. Freezing on the pools this process opened
// (seed.Connections.Names) rather than on the boot MANIFEST's names looks
// equivalent and is not: the pool set also covers the whole of the local seed
// file's connections: block, which is a route table and not wiring, so every
// locally-routed name would read as one the push removed and every swap would be
// refused for the life of that deployment.
// TestALocallyRoutedConnectionIsNotReadAsOneThePushRemoved is that trap, and
// TestAPushThatLeavesTheConnectionNamesAloneIsStillAdopted is the other end of it:
// ReplaceWiring rewrites every row's stamps on every push, so a check written over
// anything but the NAMES would freeze the feature dead on the first re-push of
// identical wiring.

// swapSeedWithLocalRoute is swapSeed plus a connections: entry of this instance's
// OWN, under a name no shared manifest in these cases ever declares.
//
// That is a legitimate and ordinary deployment: connections: is the one section the
// additive rule does not apply to, because a local entry is a ROUTE and a
// locally-declared name is the route for a connection only this instance's own
// added providers can reach (connectionRoutes). It is here so the frozen baseline
// can be proven to be the manifest's names and not the pools.
const swapSeedWithLocalRoute = swapSeed + `
connections:
  aux:
    dsn_env: APERTURE_SWAP_AUX_DSN
`

// auxRouteEnv is the variable swapSeedWithLocalRoute's local entry reads. The DSN
// is never dialled — sql.Open is lazy and nothing here queries through aux.
const auxRouteEnv = "APERTURE_SWAP_AUX_DSN"

// mainOnlyWiring is the boot manifest for the cases that need a connection to
// REMOVE: one connection name and nothing that reads through it.
//
// Nothing references it on purpose. A providers: row over `document` would collide
// with the fixture's inline objects: entry under seed.StrictProviderCollision(),
// and that inline object is this file's whole lever — so the manifest declares the
// name alone, which is exactly what `aperture wiring push` writes for a deployment
// whose object providers a Go host supplies itself.
func mainOnlyWiring(now time.Time) model.WiringSet {
	return model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main", CreatedAt: now, UpdatedAt: now}},
	}
}

// withConnections is declaredReleasedWiring — the field-type push this file reads a
// swap through — carrying a chosen connection manifest.
//
// Pairing the two is what makes "held whole" observable: the field type is a change
// this instance CAN adopt on its own, so a push that carries both and changes
// nothing is a push whose adoptable half was held back by its unadoptable half.
func withConnections(now time.Time, names ...string) model.WiringSet {
	set := declaredReleasedWiring(now)
	for _, n := range names {
		set.Connections = append(set.Connections, model.WiringConnection{Name: n, CreatedAt: now, UpdatedAt: now})
	}
	return set
}

// push writes a wiring set and drives one tick, returning whether it was adopted.
func (p *swapProbe) push(t *testing.T, ctx context.Context, set model.WiringSet) bool {
	t.Helper()
	if err := p.store.ReplaceWiring(ctx, set); err != nil {
		t.Fatalf("pushing the wiring: %v", err)
	}
	return p.poll.tick(ctx)
}

// assertHeldWhole is the shared assertion for a push that changed the name set:
// nothing was installed, nothing the push carried took effect, the digest did not
// move, and the instance still decides.
//
// The version POINTER is the load-bearing half. "The verdict did not change" would
// also be true of a rebuild that ran and produced an equivalent stack, and the
// claim here is stronger than that: the push was refused before anything was
// built, so the very same version is still installed.
func (p *swapProbe) assertHeldWhole(t *testing.T, ctx context.Context, boot *wiringVersion, baseline string) {
	t.Helper()
	if p.live.current() != boot {
		t.Error("a name-set-changing push replaced the installed version. A push is adopted WHOLE or not at " +
			"all: the providers, field types and attribute providers beside the connection change are a set " +
			"somebody pushed together, and installing the parts that happen to fit is a wiring version that " +
			"was nobody's")
	}
	if verdict(t, ctx, p.live.current()) {
		t.Error("the field_types: row pushed alongside the connection change took effect. The rest of a " +
			"name-set-changing push is HELD, not applied")
	}
	if p.poll.digest != baseline {
		t.Error("the digest advanced past a push that was not adopted: the change would then be forgotten " +
			"and the instance stale for the rest of its life with nothing saying so")
	}
}

// assertRestartNames reads the added/removed split off the CODED ERROR a refused
// swap returns, which is the one machine-readable form of the condition: the
// context map carries the names that arrived, the names that went and the digest of
// the push that asked for them.
//
// Reading it from the error rather than from a latch on the holder is the point.
// One record cannot disagree with itself, and the record the operator actually
// reads — service.WiringHealth, which the poller writes from this very error — is
// cleared by any completed refresh, including the no-change tick a reverted push
// produces.
func assertRestartNames(t *testing.T, err error, added, removed []string) *aerr.CodedError {
	t.Helper()
	var coded *aerr.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("a refused push returned no coded error: %v", err)
	}
	gotAdded, _ := coded.Context["added"].([]string)
	gotRemoved, _ := coded.Context["removed"].([]string)
	if !sameNames(gotAdded, added) {
		t.Errorf("the refusal reports added = %v, want %v", gotAdded, added)
	}
	if !sameNames(gotRemoved, removed) {
		t.Errorf("the refusal reports removed = %v, want %v", gotRemoved, removed)
	}
	return coded
}

// sameNames compares two name lists, treating nil and empty as the same answer.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAPushThatAddsAConnectionNameIsFlaggedAndNotApplied is the story's first
// criterion. The route for the new name EXISTS — routeSharedMain exports the
// conventional variable — so the refusal cannot be mistaken for a routing problem:
// it is that a process resolves its routes once, at boot, and sql.Open being lazy
// means a pool conjured now would not fail here at all. It would fail at the first
// decision that needed the database, where an object provider yields no metadata
// and an attribute provider yields a NIL BAG that widens an exclusive grant.
func TestAPushThatAddsAConnectionNameIsFlaggedAndNotApplied(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	baseline := probe.poll.digest
	if verdict(t, ctx, boot) {
		t.Fatal("the booted instance already ALLOWS, so this case cannot tell a held push from an adopted one")
	}
	if strings.Contains(probe.out.String(), "RESTART THIS INSTANCE") {
		t.Fatal("a freshly booted instance already reports a restart condition")
	}

	if probe.push(t, ctx, withConnections(time.Now().UTC(), "main")) {
		t.Fatal("a push that ADDS a connection name was adopted. This process opened no pool for it and " +
			"cannot open one: a route is a per-instance fact resolved once, at boot")
	}

	probe.assertHeldWhole(t, ctx, boot, baseline)

	// The new name is not routed: the installed version reads through the pools the
	// boot opened, and "main" is not one of them.
	if _, ok := probe.live.current().stack.conns.Pool("main"); ok {
		t.Error("the added connection has a pool although the push was refused")
	}

	// The added/removed split reaches the operator on the line the poller printed:
	// which name arrived is what tells them to export a DSN rather than drain a pool.
	report := probe.out.String()
	for _, want := range []string{"CHANGED", "could not adopt", "RESTART THIS INSTANCE", `adds connection name "main"`} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal report does not mention %q. Report:\n%s", want, report)
		}
	}
}

// TestAPushThatRemovesAConnectionNameIsFlaggedAndNotApplied is the half that passed
// silently before this story, and the one "the existing pool is not torn down" is
// easy to satisfy by accident and hard to satisfy on purpose.
//
// Applying it needs no pool and breaks nothing: the rebuilt registry simply names
// no connection, every provider that remains resolves, and the process is left
// holding a pool for a connection the deployment retired — while `aperture wiring
// diff` reports the retirement as landed here. Draining that pool is a different
// problem from adopting wiring (it is open, it may have connections checked out,
// and its lifetime belongs to the boot's seed.Connections, which serve closes once
// on shutdown), so the push is refused rather than half-honoured.
func TestAPushThatRemovesAConnectionNameIsFlaggedAndNotApplied(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeed, mainOnlyWiring(time.Now().UTC()))

	boot := probe.live.current()
	baseline := probe.poll.digest
	pool, ok := boot.stack.conns.Pool("main")
	if !ok {
		t.Fatal("the boot opened no pool for the manifest's only connection, so this case cannot show one " +
			"surviving a refused removal")
	}

	// The push drops "main" and declares the field type in the same breath.
	if probe.push(t, ctx, declaredReleasedWiring(time.Now().UTC())) {
		t.Fatal("a push that REMOVES a connection name was adopted. The name set is frozen for the life of " +
			"a process in BOTH directions: draining a pool is not part of adopting wiring")
	}

	probe.assertHeldWhole(t, ctx, boot, baseline)

	// The pool is still open, and still the same one. A refusal that tore it down
	// would take the serving instance's database access with it.
	after, ok := probe.live.current().stack.conns.Pool("main")
	if !ok {
		t.Fatal("the pool for the removed connection is gone. The instance goes on deciding through it, and " +
			"its lifetime belongs to the Connections the BOOT opened")
	}
	if after != pool {
		t.Error("the pool for the removed connection was replaced")
	}

	report := probe.out.String()
	for _, want := range []string{"could not adopt", "RESTART THIS INSTANCE", `drops connection name "main"`} {
		if !strings.Contains(report, want) {
			t.Errorf("the refusal report does not mention %q. Report:\n%s", want, report)
		}
	}
}

// TestANameSetChangingPushIsHeldWhole is the story's third criterion, which asked
// for a decision between "apply the rest" and "hold the whole push", and for the
// answer to be consistent and written down.
//
// The answer is HELD WHOLE, and it holds because the check sits at the top of
// liveWiring.swap rather than inside the rebuild. That is what this case exists to
// keep: the property is true today by WHERE the check is, and a refactor that moved
// it into the rebuild — or that refused only the connections section and rebuilt
// the rest — would still pass every other case in this file.
//
// The push here touches the name set AND both provider-shaped sections: a new
// connection name, a field-type declaration this instance could adopt on its own,
// and an attribute provider over the connection the boot already routes. None of it
// lands.
func TestANameSetChangingPushIsHeldWhole(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	t.Setenv(connectionDSNEnvVar("replica"), unroutedDSN)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeed, mainOnlyWiring(time.Now().UTC()))

	boot := probe.live.current()
	baseline := probe.poll.digest
	bootReg, bootAttrs, bootEngine := boot.stack.registry, boot.stack.attributes, boot.stack.eng

	now := time.Now().UTC()
	set := withConnections(now, "main", "replica")
	set.AttributeProviders = []model.WiringAttributeProvider{{
		Subject:    "user",
		Kind:       "sql",
		Connection: "main",
		GetOne:     "SELECT department FROM users WHERE id = $1",
		CreatedAt:  now,
		UpdatedAt:  now,
	}}
	if probe.push(t, ctx, set) {
		t.Fatal("a push adding a connection name alongside a field type and an attribute provider was adopted")
	}

	probe.assertHeldWhole(t, ctx, boot, baseline)
	if want := `adds connection name "replica"`; !strings.Contains(probe.out.String(), want) {
		t.Errorf("the refusal report does not mention %q. Report:\n%s", want, probe.out.String())
	}

	// Nothing was rebuilt, which is the strong form of "held whole": not an
	// equivalent registry, the SAME registry.
	current := probe.live.current()
	if current.stack.registry != bootReg || current.stack.attributes != bootAttrs || current.stack.eng != bootEngine {
		t.Error("something was rebuilt for a push that was refused. The frozen-name-set check is answerable " +
			"without building anything, and its whole point is that the rest of the push is never evaluated")
	}
	// And the instance is still deciding, the criterion that has to survive every
	// refusal in this file.
	if verdict(t, ctx, current) {
		t.Error("the verdict changed although nothing was installed")
	}
}

// TestBothDirectionsOfAConnectionNameChangeAreReportedApart is the rename case: one
// push, one name gone and one name arrived. They are different operator situations
// — export a DSN for the new one, drain a pool for the old one — so a report that
// collapsed them into "the name set changed" would leave an operator guessing which
// half they are looking at.
//
// It calls swap directly rather than driving a tick, because the CODE and the chain
// depth are what is under test and the poller reports an error it does not return.
// aerr.Wrap re-stamps, so burying APERTURE_WIRING_RESTART_REQUIRED would cost the
// operator its fixups, and the first of them is the restart that IS the remedy.
func TestBothDirectionsOfAConnectionNameChangeAreReportedApart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	t.Setenv(connectionDSNEnvVar("replica"), unroutedDSN)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeed, mainOnlyWiring(time.Now().UTC()))

	err := probe.live.swap(ctx, withConnections(time.Now().UTC(), "replica"), "pushed-digest")
	mustRefuse(t, "a push that renames a connection", err,
		aerr.APERTURE_WIRING_RESTART_REQUIRED,
		`adds connection name "replica"`, `drops connection name "main"`, "RESTART THIS INSTANCE")

	coded := assertRestartNames(t, err, []string{"replica"}, []string{"main"})
	if got, _ := coded.Context["digest"].(string); got != "pushed-digest" {
		t.Errorf("the refusal names digest %q, want the digest of the push that requires the "+
			"restart: an operator reading a posture and an operator reading stderr must be looking at one push",
			got)
	}
}

// TestAPushThatLeavesTheConnectionNamesAloneIsStillAdopted is the positive control,
// and it is not a formality: ReplaceWiring is wholesale, so EVERY push rewrites
// every connection row with new stamps. A frozen-set check written over the rows,
// their order or their stamps rather than over their NAMES would refuse every push
// a DB-wired instance ever sees, and the hot-swap feature would be dead on a
// condition nobody configured.
func TestAPushThatLeavesTheConnectionNamesAloneIsStillAdopted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeed, mainOnlyWiring(time.Now().UTC().Add(-time.Hour)))

	if verdict(t, ctx, probe.live.current()) {
		t.Fatal("the booted instance already ALLOWS, so this case cannot tell an adopted push from a held one")
	}
	// The same one name, re-stamped, exactly as a re-run of the same pipeline writes.
	if !probe.push(t, ctx, withConnections(time.Now().UTC(), "main")) {
		t.Fatalf("a push that changed no connection NAME was refused. The poller reported:\n%s", probe.out.String())
	}
	if !verdict(t, ctx, probe.live.current()) {
		t.Error("the push was reported as adopted but the field_types: row it carried did not take effect")
	}
	if strings.Contains(probe.out.String(), "RESTART THIS INSTANCE") {
		t.Errorf("a push that changed no connection name was reported as needing a restart:\n%s", probe.out.String())
	}
}

// TestALocallyRoutedConnectionIsNotReadAsOneThePushRemoved pins the baseline. The
// frozen set is the boot MANIFEST's names; the pools this process opened are a
// superset of them, because connections: in the local seed file is a ROUTE TABLE
// and a locally-declared name is the route for a connection only this instance's
// own added providers reach.
//
// Freezing on the pool set looks equivalent and would read "aux" as a name every
// push removes — refusing every swap, forever, on every deployment that routes a
// connection from its own file.
func TestALocallyRoutedConnectionIsNotReadAsOneThePushRemoved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	t.Setenv(auxRouteEnv, unroutedDSN)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeedWithLocalRoute, mainOnlyWiring(time.Now().UTC().Add(-time.Hour)))

	// The local route really is a pool, so the two sets genuinely differ.
	if _, ok := probe.live.current().stack.conns.Pool("aux"); !ok {
		t.Fatal("the local connections: entry opened no pool, so this case cannot show the manifest and the " +
			"pool set differing")
	}
	if !probe.push(t, ctx, withConnections(time.Now().UTC(), "main")) {
		t.Fatalf("a push whose manifest matches the boot's was refused because this instance routes a "+
			"connection of its own. The frozen set is the MANIFEST's names, never the pools. The poller "+
			"reported:\n%s", probe.out.String())
	}
	if !verdict(t, ctx, probe.live.current()) {
		t.Error("the push was reported as adopted but did not take effect")
	}
}

// TestTheFrozenNameSetIsCheckedBeforeAnythingIsRebuilt asserts the ORDER directly,
// with a rebuild that records whether it ran. Every other case here infers it from
// pointer identity; this one makes it unmistakable, because "restart required" is a
// standing fact about this process where "the rebuild failed" is a complaint about
// a document, and collapsing them costs an operator the difference between
// correcting a push and restarting a host.
//
// The second half is the clear: a push that restores the name set is adopted and
// the latched condition goes away, because an instance that went on reporting
// "restart required" after adopting a push would be reporting a fact about wiring
// it no longer runs.
func TestTheFrozenNameSetIsCheckedBeforeAnythingIsRebuilt(t *testing.T) {
	ctx := context.Background()
	var rebuilds int
	live := newLiveWiring(
		&wiringVersion{stack: decisionStack{wiringConnections: []string{"main"}}, digest: "boot"},
		func(_ context.Context, _ model.WiringSet, digest string) (*wiringVersion, error) {
			rebuilds++
			return &wiringVersion{stack: decisionStack{wiringConnections: []string{"main"}}, digest: digest}, nil
		})

	now := time.Now().UTC()
	err := live.swap(ctx, withConnections(now, "main", "replica"), "adds-replica")
	mustRefuse(t, "a push that adds a connection name", err,
		aerr.APERTURE_WIRING_RESTART_REQUIRED, `"replica"`)
	if rebuilds != 0 {
		t.Errorf("the rebuild ran %d time(s) for a push refused on the frozen name set. The check needs no "+
			"registry, no pool and no seed file, and running the rebuild first turns a standing condition "+
			"about this process into a report about the document", rebuilds)
	}
	if live.current().digest != "boot" {
		t.Error("the refused push still moved the installed version")
	}
	assertRestartNames(t, err, []string{"replica"}, nil)

	// The operator's remedy: a push that restores the name set. It is adopted.
	if err := live.swap(ctx, withConnections(now, "main"), "restores-main"); err != nil {
		t.Fatalf("a push that restored the frozen name set was refused: %v", err)
	}
	if rebuilds != 1 {
		t.Errorf("the rebuild ran %d time(s) for one adoptable push, want 1", rebuilds)
	}
	if live.current().digest != "restores-main" {
		t.Error("the adoptable push was not installed")
	}
	// What CLEARS the condition an operator reads is not asserted here, because this
	// file's holder keeps no record of it: service.WiringHealth does, the poller
	// writes it from the error above, and any completed refresh clears it — including
	// the no-change tick a reverted push produces
	// (TestARevertedConnectionChangeStopsBeingReported).
}
