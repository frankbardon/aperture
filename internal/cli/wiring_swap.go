package cli

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
// # The connection name set is frozen, and a push that changes it is HELD WHOLE
//
// One thing a swap will not do is renegotiate which connections this process can
// reach. The shared manifest carries connection NAMES and nothing else; a route —
// which server, which credential, how big a pool, how long a statement may take —
// is a per-instance fact this process resolves ONCE, at boot (connectionRoutes).
// So the name set is a boot-time contract between the manifest and the routes this
// instance can supply locally, and it is not a runtime one: a process cannot
// conjure a route for a name that appeared while it was running, and draining a
// pool for a name that vanished is a different problem from adopting wiring.
//
// liveWiring.swap therefore compares the pushed manifest's name set against the
// one the boot resolved routes for, BEFORE it builds anything, and refuses the
// whole push when they differ in either direction
// (liveWiring.refuseFrozenConnectionNames). Held WHOLE is the deliberate half: the
// providers, field types and attribute providers that arrived in the same push are
// a coherent set somebody pushed together, and applying the parts that happen to
// fit would install a wiring version that was nobody's — the same defect a
// field-by-field swap would be, one push higher up. So nothing is installed, the
// digest does not advance, and the instance goes on deciding through the wiring it
// has until it is restarted. That is stated here, pinned by
// TestANameSetChangingPushIsHeldWhole, and it is why the check sits at the top of
// swap rather than inside the rebuild.
//
// The seams the rest of the epic attaches here:
//
//   - E4-S4 (last-good on failure, and the alarm) owns what wiring_poll.go's tick
//     does with the error this returns. Last-good is already the behaviour: a
//     failed rebuild installs nothing, the digest does not advance, and the
//     instance keeps deciding through the version it has. The frozen-name-set
//     condition is latched separately, on the holder, for whatever
//     operator-visible posture that story lands: liveWiring.restartRequired is the
//     one seam it needs, and it is deliberately the narrowest thing that can be —
//     no writer, no clock, no second alarm mechanism. "A restart is required" is
//     mutable runtime state and therefore not a service.Capabilities boolean; that
//     tension is E4-S4's to resolve, and nothing here pre-empts it.

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

	// frozen is the connection NAME SET this process resolved routes for at boot,
	// sorted, and it never changes: it is the process's half of a contract only a
	// restart can renegotiate. See the file header, and decisionStack.wiringConnections
	// for why it is the manifest's names and not the pool set.
	//
	// Read without the mutex because it is written once, before the holder is
	// published, and never again.
	frozen []string

	// restart latches the frozen-name-set condition for a reader that is not the
	// poll goroutine: nil means this instance has been asked to adopt nothing it
	// cannot, and non-nil names what the push changed.
	//
	// It is an atomic pointer to an immutable record for the same reason cur is: the
	// writer is the poll goroutine and the reader is whatever surface reports the
	// instance's posture, and a posture read must never wait on a rebuild. It is the
	// ONE seam E4-S4 needs from this file — see restartRequired.
	restart atomic.Pointer[wiringRestart]

	// swapping serialises writers. It protects the REBUILD as well as the store,
	// so a second refresh cannot start while the first is half-way through reading
	// the seed file and constructing registries.
	swapping sync.Mutex
}

// wiringRestart is the frozen-connection-name condition in the form something other
// than a log line can read: which names the deployed wiring added, which it
// dropped, and the digest of the push that asked for them.
//
// It carries NAMES and a digest and nothing else. A connection name is
// operator-supplied configuration and is safe to report; a DSN, a credential or a
// pool size is not, and none of them is in the shared tables to begin with.
//
// It is immutable once stored, and it is deliberately minimal. There is no
// timestamp, no counter and no severity on it: how long an instance has been
// superseded, and how that is surfaced to an operator, is ONE staleness question
// that belongs in one place (E4-S4), and a second answer to it invented here would
// be a second alarm mechanism.
type wiringRestart struct {
	// digest is the digest of the pushed wiring that requires the restart — the same
	// value wiring_poll.go compares and reports, so an operator reading a posture and
	// an operator reading stderr are looking at one push.
	digest string
	// added are names the pushed manifest declares that the boot's manifest did not,
	// sorted; removed are names the boot's declared that the push no longer does,
	// sorted. At least one of the two is non-empty.
	//
	// An added name is normally one this process opened no pool for, but not always:
	// a name this instance's own seed file already routes could arrive in the
	// manifest, and it is a restart all the same. The frozen set is the MANIFEST's,
	// so the answer is the same on every instance in the fleet — where "adopted here,
	// restart required on the peer" would leave two instances running two wiring
	// versions with only one of them saying so.
	added   []string
	removed []string
}

// newLiveWiring holds boot as the version every request is answered through until
// a swap replaces it.
//
// rebuild may be nil, which makes the holder a fixed one: every swap is refused
// rather than silently doing nothing, because a process that accepted a change it
// cannot apply and said so nowhere is the silently-stale instance this epic exists
// to close.
// The frozen connection name set is taken from the BOOT VERSION rather than from a
// caller, so no surface can construct a holder that is frozen on a name set its own
// stack was not wired from. It is the same argument decisionStack.wiringDigest
// makes: the boot read owns the value, and a second derivation of it is a second
// answer that can disagree.
func newLiveWiring(boot *wiringVersion, rebuild wiringRebuild) *liveWiring {
	l := &liveWiring{rebuild: rebuild, frozen: boot.stack.wiringConnections}
	l.cur.Store(boot)
	return l
}

// restartRequired reports the frozen-connection-name condition, or nil when this
// instance has not been asked to adopt a name set it cannot.
//
// It is the whole of this file's operator-visible surface, and it is a READ: no
// writer, no clock, no formatting and no side effect, so a posture reader cannot
// perturb what it is reporting and cannot block on a rebuild. The returned record
// is immutable and safe to hold.
//
// It is nil-safe in the only sense that matters here — the record, not the
// receiver — because "nothing to report" is the state every instance is in for its
// whole life unless somebody pushes a connection change.
func (l *liveWiring) restartRequired() *wiringRestart {
	return l.restart.Load()
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
// The FROZEN CONNECTION NAME SET is checked first, before anything is built and
// before the holder's ability to rebuild at all is consulted. Comparing the pushed
// manifest's names against the ones this process resolved routes for at boot needs
// no registry, no pool and no seed file, and its outcome is not the same fact as a
// rebuild failure: "this instance must be restarted to adopt this push" is a
// STANDING condition about the process, where a failed rebuild is a statement about
// the push. Both leave the instance deciding through the wiring it has; only one of
// them is fixed by correcting the pushed document. See
// refuseFrozenConnectionNames.
//
// A successful swap CLEARS the latched condition, because a push that restores the
// name set is the operator's own remedy and an instance that went on reporting
// "restart required" after adopting one would be reporting a fact about a push it
// no longer runs. A failed REBUILD deliberately leaves the latch alone: whether a
// rebuild failure is itself an operator-visible posture is E4-S4's question, and
// answering it here would put two mechanisms on one signal.
func (l *liveWiring) swap(ctx context.Context, set model.WiringSet, digest string) error {
	l.swapping.Lock()
	defer l.swapping.Unlock()

	if err := l.refuseFrozenConnectionNames(set, digest); err != nil {
		return err
	}
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
	// Cleared AFTER the install, so a reader that saw the condition and then sees it
	// gone is looking at an instance that really is running the push.
	l.restart.Store(nil)
	return nil
}

// refuseFrozenConnectionNames refuses a push whose connection NAME SET differs from
// the one this process resolved routes for at boot, in either direction, and latches
// the condition for a reader that is not watching stderr.
//
// # Why both directions, and why they are reported apart
//
// An ADDED name is normally one this instance has no pool for and cannot make one
// for: sql.Open is lazy, so the failure would not even be at the push — it would be
// the first decision that needed the database, and an object provider that cannot
// reach its database yields no metadata while an attribute provider that cannot
// yields a NIL BAG, which widens an exclusive grant instead of denying it. That is
// the same fact a boot refuses with APERTURE_WIRING_CONNECTION_UNROUTED, from the
// running process's side, and borrowBootPools is still the backstop for it.
//
// It is refused even in the case where this instance HAPPENS to route the new name
// already, out of its own seed file's connections: block. The frozen set is the
// manifest's, not the pool set, so every instance in the fleet gives the same answer
// to the same push; a rule that adopted where the local file helped and refused
// where it did not would leave two instances running two wiring versions, with only
// one of them reporting anything.
//
// A REMOVED name is the quieter one, and until this check existed it was applied
// SILENTLY: the rebuild simply built a registry that named no connection, every
// remaining provider still resolved, and the process was left holding a pool for a
// connection the deployment had retired — with the operator's own wiring diff saying
// the retirement had landed. Draining that pool is a different problem from adopting
// wiring (it is open, it may have checked-out connections, and its lifetime belongs
// to the boot's seed.Connections, which serve closes once on shutdown), so this
// refuses the push rather than pretending to. The pool is NOT torn down, and the
// instance goes on deciding through it.
//
// The two are reported apart because they are different operator situations —
// "export a DSN for the new name, then restart" versus "this instance is still
// holding a pool for a name you retired; restart it when you are ready" — even
// though the coded remedy is the same restart.
//
// # Why this code
//
// APERTURE_WIRING_CONNECTION_UNROUTED, reused verbatim from the boot half
// (refuseUnroutedConnections), because the condition is the same disagreement
// between a shared manifest's name set and the routes ONE instance can supply. It is
// the code the story's own notes ask for where the meaning matches, and the fixups
// on it — export the conventional variable, declare a local connections: entry,
// supply seed.WithConnectionOpener, read the manifest with `aperture wiring show` —
// are exactly the remedies for the added half. The message carries the rest, which is
// the restart, and the fact that the whole push was held.
//
// Constructed and never wrapped, so there is exactly one Aperture-coded error in the
// chain: a pass-through guard would be pointless here because nothing below has
// coded anything yet, and a wrap of a coded error would bury the fixups that ARE the
// remedy.
//
// Only connection NAMES and a digest reach the message and the context map. A name
// is operator-supplied configuration; a DSN is not, and is not in the shared tables
// to be leaked in the first place.
func (l *liveWiring) refuseFrozenConnectionNames(set model.WiringSet, digest string) error {
	added, removed := diffConnectionNames(l.frozen, wiringConnectionNames(set))
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	l.restart.Store(&wiringRestart{digest: digest, added: added, removed: removed})

	var changes []string
	if len(added) > 0 {
		changes = append(changes, fmt.Sprintf("it adds connection %s %s",
			plural("name", "names", len(added)), strings.Join(quoteEach(added), ", ")))
	}
	if len(removed) > 0 {
		changes = append(changes, fmt.Sprintf("it drops connection %s %s",
			plural("name", "names", len(removed)), strings.Join(quoteEach(removed), ", ")))
	}
	return aerr.WithContext(aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		fmt.Sprintf("cli: the deployed wiring changes this instance's connection NAME SET — %s — and that set is FIXED for the "+
			"life of a process: the shared tables carry a connection's name and nothing else, because which server, which "+
			"credential and how big a pool are per-instance facts this instance resolves once, at boot. It can neither open a "+
			"pool for a name that appeared while it was running nor drain one for a name that vanished. RESTART THIS INSTANCE "+
			"to adopt the push. Nothing in it was applied — not the connections, and not the providers, field types or "+
			"attribute providers pushed beside them, because a push is adopted whole or not at all — so this instance keeps the "+
			"wiring it has and goes on deciding meanwhile", strings.Join(changes, ", and ")),
		map[string]any{"added": added, "removed": removed, "digest": digest})
}

// wiringConnectionNames is the connection NAME SET of a wiring snapshot, sorted.
//
// Sorted so that two reads of the same manifest compare equal however a backend
// ordered its rows, which is the same reason wiringDigest sorts before it hashes. The
// names are the whole of the connections section — model.WiringConnection carries a
// name and its stamps and nothing else — so this is not a projection of the section,
// it IS the section, which is why a stamp-only re-push (every push rewrites every
// row) is correctly read as no change at all.
//
// Returns nil for an empty section, which compares equal to another empty one.
func wiringConnectionNames(set model.WiringSet) []string {
	if len(set.Connections) == 0 {
		return nil
	}
	out := make([]string, 0, len(set.Connections))
	for _, c := range set.Connections {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// diffConnectionNames reports what next adds to was and what it drops from it, both
// sorted, given two sorted inputs.
//
// It is a set difference and not a length or an order comparison, deliberately: an
// operator who renames one connection has both an add and a drop in one push and
// needs told both names, and a push that only re-stamps its rows must come back empty
// from both.
func diffConnectionNames(was, next []string) (added, removed []string) {
	have := make(map[string]struct{}, len(was))
	for _, n := range was {
		have[n] = struct{}{}
	}
	want := make(map[string]struct{}, len(next))
	for _, n := range next {
		want[n] = struct{}{}
		if _, ok := have[n]; !ok {
			added = append(added, n)
		}
	}
	for _, n := range was {
		if _, ok := want[n]; !ok {
			removed = append(removed, n)
		}
	}
	return added, removed
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
// A name the boot opened no pool for is REFUSED, with the same code the boot uses
// for a name it has no route for (APERTURE_WIRING_CONNECTION_UNROUTED), because it
// is the same fact from the running process's side — this instance cannot reach a
// database the pushed wiring names.
//
// It is now a BACKSTOP rather than the mechanism. refuseFrozenConnectionNames
// catches the whole condition before a rebuild starts, and catches the REMOVED
// direction this function structurally cannot see, so nothing that reaches here
// should ever fail. It is kept, unchanged, because it is fail-closed at the exact
// point a pool is handed out: any future path that rebuilt without going through
// swap — an embedding host's own refresh, a test — must not be able to read through
// a connection this process never opened.
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
