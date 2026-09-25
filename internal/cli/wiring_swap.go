package cli

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
)

// THE SWAP: how a running instance starts deciding through wiring somebody else
// pushed, without a restart and without any decision ever seeing half of it.
//
// wiring_poll.go detects the change. This file adopts it. The two are deliberately
// separate: detection is a digest comparison that every deployment can afford to
// have switched off, and adoption is a rebuild of everything a decision reads.
//
// # A version, not a set of fields
//
// The unit that is installed is a whole wiringVersion: one *provider.Registry, one
// *provider.AttributeRegistry, the field types folded into both, the rules engine
// over them, the decision engine, the service facade and the HTTP handler. It is
// built entirely, and only then stored into an atomic pointer.
//
// The obvious cheaper implementation — keep the stack and replace its registry
// fields, or re-register providers into the registry the engine already holds — is
// the one bug this design exists to make unrepresentable. A decision reads object
// metadata, then lists a type's objects, then reads a principal bag; a mutation
// landing between any two of those reads gives one verdict a provider set that
// never existed as anybody's wiring. Nothing errors, nothing is logged, and the
// verdict is simply not the answer to any question that was asked. So a version is
// IMMUTABLE once built, and a swap moves one pointer.
//
// # One version per decision, and how that is achieved
//
// A surface resolves the version ONCE at entry (liveWiring.current) and answers the
// whole request through it. Because the version is immutable and the graph beneath
// it is reachable only from it, every read that decision makes — metadata, lister,
// references, principal and account bags, declared keys, rule evaluator — comes
// from the same version by construction. A swap landing mid-decision changes the
// pointer, not the graph the decision is holding.
//
// This is the same guarantee rules.WithDecisionAttributes gives a decision about
// its principal bag, and it is reached the other way round on purpose. That memo
// has to be a CONTEXT SCOPE because the bag is resolved deep inside expr
// evaluation, where there is no receiver to carry it. Here the wiring IS the
// receiver: the engine a decision runs on is the version's engine, so there is no
// scope to open, no context value to forget to thread, and no fallback path that
// silently answers from "whatever is current" — which is exactly the shape a
// context value would have needed.
//
// # What a swap costs, and what it deliberately does not
//
// A swap never blocks a decision. Readers pay one atomic load; the rebuild happens
// on the poll goroutine, and the mutex below is held by WRITERS only, so there is
// no lock anywhere on the decision path (which is what keeps
// APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/ answerable at all).
//
// It does cost the caches the superseded version had warmed: the new engine's
// pattern cache, the new registry's per-type metadata cache and the new attribute
// registry's per-slot caches all start empty. That is a blip at the cadence of a
// human pushing wiring, and for the attribute caches it is not a cost at all but
// the REQUIREMENT — see "The attribute caches" below.
//
// # The attribute caches
//
// A slot's `ttl:` is the window a REVOKED clearance keeps authorizing for. A
// rebuilt slot that served entries fetched under the old configuration's TTL would
// therefore keep authorizing against wiring the operator has already replaced, with
// nothing in any verdict, trace or note to say so — access-widening, not a
// performance wrinkle.
//
// Nothing here invalidates anything, because there is nothing to invalidate: a
// version's *provider.AttributeRegistry is FRESHLY CONSTRUCTED, and its per-slot
// caches are created with it. That is strictly stronger than calling
// AttributeRegistry.InvalidateAll over a reused registry — invalidation is a
// mechanism that can be forgotten for one slot, and construction cannot be — and it
// is why no second mechanism is introduced here. The registry's own
// Invalidate / InvalidateSlot / InvalidateAll remain the way an OPERATOR drops an
// entry (`aperture attributes invalidate`); they are not how a swap works.
// TestARebuiltSlotDoesNotServeTheOldConfigurationsCache is what holds it.
//
// The superseded version's caches stay valid for the decisions still holding it,
// which is the same statement as "one version per decision" and is bounded by one
// request.
//
// # Where the pools come from
//
// A rebuild does NOT dial a second set of database pools. It reads through the ones
// this process opened at boot (borrowBootPools), because opening a set per push
// would double a deployment's connections on every push and leave half of them held
// by a registry nobody has a handle to close. A superseded version is therefore
// never Closed — its Connections are borrowed and its registries hold no OS
// resource — and the BOOT version's pools are closed once, by serve's defer, when
// the process stops.
//
// The seams the rest of the epic attaches here:
//
//   - E4-S3 (the frozen connection-name set) belongs in liveWiring.swap, BEFORE
//     the rebuild, and is marked there. Today a name the boot opened no pool for is
//     refused by borrowBootPools — fail-closed and last-good, which is the right
//     outcome — but as a rebuild failure rather than as the specific, surfaced
//     "restart required" condition that story owes an operator.
//   - E4-S4 (last-good on failure, and the alarm) owns what wiring_poll.go's tick
//     does with the error this returns. Last-good is already the behaviour: a
//     failed rebuild installs nothing, the digest does not advance, and the
//     instance keeps deciding through the version it has.

// wiringVersion is ONE coherent wiring version this process can answer a request
// through: the decision stack, the facade composed over it, the HTTP handler that
// serves that facade, and the digest of the shared wiring it was all built from.
//
// It is immutable after newLiveWiring or liveWiring.swap has built it. Nothing
// mutates a field of a version that has been stored, and nothing should be added
// here that would need to: a mutable field on a version is a torn read wearing a
// different hat.
//
// svc is the facade handler answers through, and it is held here as well as inside
// the handler because a version is a thing a NON-HTTP consumer resolves too — a
// test, and whatever E4-S4 surfaces the staleness through. handler is nil for a
// holder no HTTP surface is attached to; only ServeHTTP reads it, and only `serve`
// installs one.
type wiringVersion struct {
	stack   decisionStack
	svc     *service.Service
	handler http.Handler
	digest  string
}

// wiringRebuild builds the next version from a wiring set that has just been read,
// and from the digest that was computed over exactly that read.
//
// It is supplied by the surface that owns the process rather than written here,
// because only that surface knows what has to be re-composed AROUND a rebuilt
// stack: `serve` re-wires the authority gate, delegation, impersonation and the
// editor's rule source over the new engine, and an embedding host would wire its
// own. What every implementation owes is the contract this file rests on — build
// everything, install nothing, and return an error rather than a half-built
// version.
type wiringRebuild func(ctx context.Context, set model.WiringSet, digest string) (*wiringVersion, error)

// liveWiring is the one mutable cell in the design: an atomic pointer to the
// version this process is currently answering new requests through.
//
// Readers (every decision) load it. Writers (the poll goroutine) hold the mutex
// for the whole rebuild-then-install, so two refreshes cannot interleave and a
// failed one cannot leave the cell holding something that was never fully built.
// The mutex is deliberately NOT taken by current(): a reader that waited on a
// rebuild would be a decision blocking on a wiring read, which is the one thing
// this machinery is not allowed to cost.
type liveWiring struct {
	// cur is never nil after newLiveWiring: a holder with no version is a process
	// with no wiring, and there is no point in the lifetime of either where that is
	// a legal state.
	cur     atomic.Pointer[wiringVersion]
	rebuild wiringRebuild

	// swapping serialises writers. It protects the REBUILD as well as the store,
	// so a second refresh cannot start while the first is half-way through reading
	// the seed file and constructing registries.
	swapping sync.Mutex
}

// newLiveWiring holds boot as the version every request is answered through until
// a swap replaces it.
//
// rebuild may be nil, which makes the holder a fixed one: every swap is refused
// rather than silently doing nothing, because a process that accepted a change it
// cannot apply and said so nowhere is the silently-stale instance this epic exists
// to close.
func newLiveWiring(boot *wiringVersion, rebuild wiringRebuild) *liveWiring {
	l := &liveWiring{rebuild: rebuild}
	l.cur.Store(boot)
	return l
}

// current is THE PIN: the one resolution of "which wiring answers this request",
// made once at entry and used for the whole of it.
//
// Calling it twice in one decision is the defect, not the API. There is nothing
// here to enforce that — the version is an ordinary pointer — which is why the
// surfaces resolve it exactly once, at the outermost boundary they own, and hand
// the version down. ServeHTTP is that boundary for `serve`.
func (l *liveWiring) current() *wiringVersion {
	return l.cur.Load()
}

// ServeHTTP resolves the version once and answers the whole request through it. It
// is the stable handler `serve` mounts, so the http.Server, the listener and the
// authentication middleware are all built once and survive every swap — only what
// is BENEATH them changes.
//
// A request that is in flight when a swap lands finishes on the handler it started
// on, and therefore on the registries, the engine and the facade it started on.
// That is the whole of "a decision sees one coherent wiring version": there is no
// point after this line at which the request can observe a different one.
func (l *liveWiring) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.current().handler.ServeHTTP(w, r)
}

// swap rebuilds from set and installs the result, or installs nothing and returns
// why.
//
// The ORDER is the contract: everything is built first, and the pointer moves last
// and once. A rebuild that fails — an unconstructable provider kind, a connection
// name this process opened no pool for, a seed file that has since been made
// invalid — leaves the holder exactly as it was, so the instance keeps deciding
// through the version it already has. There is no state in which some of a push is
// installed and some is not.
//
// digest is carried through rather than recomputed so that the value the poller
// COMPARED, the set the version was BUILT FROM and the digest the version RECORDS
// are one read. Recomputing here would let a push landing between the two reads be
// adopted under the earlier read's digest, which is a stale instance that believes
// it is current.
//
// E4-S3's frozen connection-name check goes at the top of this function, before the
// rebuild: comparing set.Connections against the manifest this process resolved
// routes for at boot is a decision that can be made without building anything, and
// its outcome ("restart required") is not the same fact as "the rebuild failed".
func (l *liveWiring) swap(ctx context.Context, set model.WiringSet, digest string) error {
	l.swapping.Lock()
	defer l.swapping.Unlock()

	if l.rebuild == nil {
		return aerr.New(aerr.APERTURE_BOOT,
			"cli: this process has no way to rebuild its wiring, so a deployed change cannot be adopted without a restart")
	}
	next, err := l.rebuild(ctx, set, digest)
	if err != nil {
		// No guard and no wrap: the rebuild's errors are the boot's errors, already
		// carrying the code and the fixups that name the entry to go and fix
		// (buildWiredStack routes every one of them through bootError). Re-stamping
		// here would replace the remedy with "aperture failed to start" on a process
		// that did not fail to start.
		return err
	}
	if next == nil {
		// Unreachable for the rebuilds in this repository, and refused rather than
		// stored because a nil version installed into the cell would panic the next
		// request instead of the refresh that produced it.
		return aerr.New(aerr.APERTURE_BOOT,
			"cli: rebuilding the wiring produced no version, so this instance keeps the wiring it has")
	}
	l.cur.Store(next)
	return nil
}

// borrowBootPools is the ConnectionOpener a REBUILD resolves `connections:`
// through: it hands back the pool this process opened at boot for each name,
// wrapped so the rebuilt registry cannot close it.
//
// Reusing them is not an optimisation. sql.Open is lazy, so a second set would
// cost nothing at the moment of the push and then quietly double the deployment's
// connection ceiling against its database — once per push, forever, with the
// superseded sets held by registries no caller has a handle to Close. One pool per
// declared name, for the life of the process, is the same promise
// seed.Connections makes a boot.
//
// A name the boot opened no pool for is REFUSED, which is what freezes the
// connection name set in practice: the rebuild fails, nothing is installed, and the
// instance keeps deciding. It is deliberately the same code the boot uses for a
// name it has no route for (APERTURE_WIRING_CONNECTION_UNROUTED), because it is the
// same fact from the running process's side — this instance cannot reach a database
// the pushed wiring names. E4-S3 owns turning it into the surfaced "restart
// required" condition an operator can see without reading stderr; until then the
// behaviour is correct and the reporting is a log line.
//
// The message names the CONNECTION and never a DSN, and the context map carries
// names only — the rule Connections.Names and refuseUnroutedConnections both obey.
func borrowBootPools(conns *seed.Connections) seed.ConnectionOpener {
	return func(name string, _ seed.ConnectionSettings) (seed.Pool, error) {
		pool, ok := conns.Pool(name)
		if !ok {
			return nil, aerr.WithContext(aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
				fmt.Sprintf("cli: the deployed wiring declares connection %q, which this instance opened no pool for when it started: "+
					"the connection NAME SET is fixed for the life of a process, because a route is a per-instance fact this instance "+
					"resolves once, at boot. This instance keeps the wiring it has and goes on deciding; restart it to pick the new "+
					"connection up", name),
				map[string]any{"connection": name})
		}
		return borrowedPool{Pool: pool}, nil
	}
}

// borrowedPool is a pool a rebuilt registry may READ through and must not close.
//
// seed.Connections.Close is documented to close every pool it holds exactly once,
// and a rebuild is handed a second Connections holding the same handles — so
// without this wrapper a superseded version's Close (or a failed rebuild's own
// cleanup, which closes every pool it opened on the way out) would take the
// serving instance's database access with it. The failure would look like
// APERTURE_SQL_PROVIDER_QUERY on every SQL-backed decision after the first push.
type borrowedPool struct {
	seed.Pool
}

// Close is a no-op. The pool's lifetime belongs to the Connections the BOOT opened,
// which serve closes once, on shutdown.
func (borrowedPool) Close() error { return nil }
