package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// decisionStack is the fully-wired decision graph every Aperture surface shares:
// storage -> object providers -> rules engine -> scope resolution -> engine ->
// service facade. It exists so `serve` and the one-shot commands (`check`,
// `enumerate`, `identifiers`, `explain`) CANNOT answer the same question
// differently.
//
// They used to. `serve` wired the rules engine and scope resolution; the one-shot
// commands called service.New(engine.New(store)) with neither, so a permission
// with a rule-backed scope strategy (`inclusive;rule=...`) had no rule evaluator:
// the resolver reported APERTURE_SCOPE_RULE_UNCONFIGURED, the facade folded that
// into a fail-closed deny, and the CLI returned a DIFFERENT VERDICT from the
// server for the same model. Missing rule diagnostics in `explain` were only the
// visible symptom. One builder, used by every command, is the fix.
type decisionStack struct {
	// eng is the wired PDP: scope resolution on, rules attached, plus whatever
	// per-command engine options the caller added.
	eng *engine.Engine
	// registry is the object-metadata registry built from the seed's `providers:`
	// and `objects:` sections. It is always non-nil (BuildRegistry returns an
	// empty registry for a seed declaring neither) and is handed to the facade so
	// `identifiers` can enumerate a type.
	registry *provider.Registry
	// attributes is the SUBJECT-attribute registry built from the seed's
	// `attribute_providers:` and `attributes:` sections — the bags a rule reads off
	// `principal` and off `account`. It is always non-nil (BuildAttributeRegistry returns an empty
	// registry for a seed declaring none) and is wired into the rules engine as
	// BOTH the principal resolver and the account resolver, so the CLI and `serve`
	// cannot disagree about what a principal or a tenant knows any more than they
	// can disagree about a rule.
	attributes *provider.AttributeRegistry
	// ruleSource is the storage-backed rule source the engine resolves rule
	// references through. `serve` also hands it to the facade (WithRuleSource) so
	// the node editor's what-if can preview an UNSAVED rule; the one-shot commands
	// have no editor and do not need it.
	ruleSource *service.StorageRuleSource
	// fetcher is the object-metadata fetcher rules read `object.*` through, or nil
	// when the seed declares no object source at all (nil means empty metadata,
	// which is exactly rules.NewEngine's own default).
	fetcher rules.MetadataFetcher
	// collisions are the object types declared in BOTH the seed's `providers:` and
	// `objects:` sections. The file-backed provider wins and the inline entries for
	// those types are discarded entirely — the documented default. Discarding data
	// silently would be hostile, and `seed` has no logging path of its own, so it
	// reports the fact and the caller surfaces it (reportCollisions).
	collisions []string
	// attributeCollisions are the attribute SLOTS declared in BOTH the seed's
	// `attribute_providers:` and `attributes:` sections. Unlike collisions above,
	// this is NOT a discard: the `attribute_providers:` entry becomes the slot's
	// SHARED layer, the inline block becomes its LOCAL layer, and a fetch reads
	// their merge with the shared layer winning every key both serve
	// (`provider.AttributeLayer`). Nothing is dropped.
	//
	// It is still reported, for a different reason than the object case. There the
	// warning says data was discarded; here it says which layer answers a
	// contested key — which is exactly what an operator debugging an unexpected
	// attribute value needs told, and which no verdict, trace or note says.
	attributeCollisions []string
	// wiringDigest is the content digest of the SHARED wiring set this stack was
	// built from — the value a background poll compares a later read against to
	// answer "would this instance be wired differently now?" (wiring_poll.go).
	//
	// It is recorded here, at the boot, rather than taken by the poller from its own
	// first read, and that is load-bearing: a push landing between the boot read and
	// the first tick would become a self-baselining loop's baseline, so the change
	// would never be reported and the instance would be stale for its whole lifetime
	// with nothing saying so.
	//
	// It is always set, including for an EMPTY wiring set, whose digest is an
	// ordinary value like any other. That is what makes the FIRST ever push to a
	// database detectable: an instance booted on empty tables holds the empty set's
	// digest, and the push changes it. A "" sentinel for "no wiring" would have made
	// the one transition that turns a file-wired fleet into a shared-wiring fleet
	// the single transition nothing noticed.
	wiringDigest string
	// declaredKeys is the DECLARED ATTRIBUTE KEY SET of every shared slot this
	// instance is wired with — the keys a rule may read off `principal` and
	// `account`. It is handed to the facade, which refuses a rule naming anything
	// else at validation (service.WithDeclaredAttributeKeys), and it reaches no
	// decision: an already-stored rule decides exactly as it did.
	//
	// A slot absent from the map declares nothing and is not enforced, so a
	// deployment that has declared nothing — every one that has not opted in — is
	// unaffected. See declaredAttributeKeySets for where the two sources are.
	declaredKeys map[provider.AttributeSlot]model.DeclaredKeys
	// conns are the database pools BuildRegistryWithConnections opened for the
	// seed's `connections:` block — one per named connection, shared by every
	// `kind: sql` provider entry referencing it. It is the only part of the stack
	// that holds an OS resource, and Close is what releases it. Always non-nil.
	conns *seed.Connections
}

// Close releases everything the stack holds open. Today that is the seed's
// database pools; a stack built from a seed with no `connections:` block closes
// nothing and the call is free, so every command defers it unconditionally
// rather than asking which kinds the seed happened to use.
//
// It is idempotent, so a `serve` that closes explicitly on shutdown may also
// defer it.
func (s decisionStack) Close() error {
	if s.conns == nil {
		return nil
	}
	return s.conns.Close()
}

// reportCollisions writes a warning naming every object type whose inline
// `objects:` entries were discarded because a `providers:` entry claimed the same
// type, and every attribute SLOT that is filled from both `attribute_providers:`
// and `attributes:`.
//
// The two are no longer the same fact and the warning no longer says they are.
// The object rule is a DISCARD: the `providers:` entry wins the type and the
// inline entries for it are dropped. The attribute rule is a LAYERING: the
// `attribute_providers:` entry is the slot's shared layer, the inline block is its
// local layer, and a fetch reads their merge with the shared layer winning every
// key both serve (`provider.AttributeLayer`). It is still worth a line, because
// which layer answers a contested key is exactly what an operator debugging an
// unexpected attribute value needs told.
//
// Nothing is written when there is no collision, so a normal boot stays silent.
// Only object TYPES and slot NAMES are named — never ids, never keys — so the
// warning cannot leak cross-account data or a directory's contents.
func (s decisionStack) reportCollisions(w io.Writer) {
	if w == nil {
		return
	}
	if len(s.collisions) > 0 {
		fmt.Fprintf(w, "warning: seed declares %d object type(s) in both providers: and objects: — "+
			"the providers: entry wins and the inline entries were discarded: %s\n",
			len(s.collisions), strings.Join(s.collisions, ", "))
	}
	if len(s.attributeCollisions) > 0 {
		fmt.Fprintf(w, "warning: seed declares %d attribute slot(s) in both attribute_providers: and attributes: — "+
			"the attribute_providers: entry is the shared layer and wins every key both serve; "+
			"the inline bags layer under it: %s\n",
			len(s.attributeCollisions), strings.Join(s.attributeCollisions, ", "))
	}
}

// buildDecisionStack wires the decision graph over an already-seeded store.
//
// seedPath is the same --seed value buildStore was given: the seed file is read a
// second time as a Document because several of its sections — `providers:`,
// `objects:` and `attributes:` — are runtime WIRING that Apply never writes to
// storage, so the file is their only source of truth.
//
// # Where the wiring comes from
//
// The file is no longer the only place it can come from. buildStore has already
// run Setup, so the store's five shared-wiring tables are readable, and this
// builder reads them (readSharedWiring) before it builds anything:
//
//   - wiring rows PRESENT -> the DATABASE is authoritative. The rows are projected
//     back into the four wiring sections of a Document (wiringDocument) and handed
//     to the same two builders the file path uses, so two instances cannot end up
//     with equivalent-but-different registries. The local document still supplies
//     its two DATA sections (`objects:` and `attributes:`) and the ROUTE for each
//     connection name the manifest declares — and it may ADD an object type or an
//     attribute slot the database never declared, which is what lets a Go host
//     with its own hand-written providers read a pushed wiring at all. A local
//     entry for a type or slot the database DOES declare fails the boot with
//     APERTURE_WIRING_LOCAL_COLLISION; see wiringDocument's file header.
//   - wiring rows EMPTY -> the local seed file's wiring is used exactly as it
//     always was. An empty set is an answer, not a failure, and it is the answer
//     every existing single-instance deployment gives: no flag, no configuration,
//     no behaviour change.
//
// # Wiring this instance cannot construct or route fails the boot HERE
//
// Nothing below this function degrades gracefully, because nothing below it can:
// an object type with no working provider yields absent metadata, which a rule
// reads as a missing path, and an attribute slot with no working provider yields a
// NIL BAG, which widens an exclusive grant instead of denying. So every one of the
// refusals this builder can meet takes the whole boot with it and the process exits
// non-zero — two from wiringDocument (a stored kind no second host can construct,
// naming the object type or the slot; a connection NAME this instance has no route
// for, naming the connection), and the rest from seed's own builders, which already
// name the entry they refused. What this function owes all of them is the
// pass-through guard below: bootError re-stamps nothing that already carries a
// code, because the code and its registry fixups ARE the remedy. See
// wiring_boot.go's "Refusing to start beats degrading".
//
// ctx is the boot's context, and it is here rather than on a package-level
// convenience because reading the wiring is a database read on the same store the
// rest of this function decides through: a cancelled boot must stop at it.
//
// Both sections feed ONE *provider.Registry, which in turn feeds BOTH the rules
// engine's metadata fetcher (so a rule can read object.category_id) AND the scope
// resolver's object lister (so implicit / exclusive scopes can enumerate a
// type's objects). When neither section is declared the fetcher and lister stay
// nil and behaviour is unchanged: rules see empty object metadata and enumeration
// of "all objects of a type" reports APERTURE_SCOPE_LISTER_UNCONFIGURED.
//
// Scope resolution falls back to literal pattern matching for grants whose
// permission declares no strategy, so plain pattern grants decide exactly as they
// did before.
//
// engOpts are per-command engine options appended after the shared ones — that is
// how `serve` adds --enforce-membership without forcing it on the one-shot
// commands.
//
// That split is a trap for anything that is NOT per-command. The enumeration
// bound is the worked example: wiring it as an engOpt the way
// --enforce-membership is wired would have given `serve` the configured ceiling
// and left `aperture enumerate` on 1000, two surfaces of one binary disagreeing
// about the same question with nothing anywhere reporting it. So cmd is taken
// here, and the flags that configure the PROCESS are read through
// sharedEngineOptions into the shared set below, where no command can fail to
// inherit them. Anything genuinely serve-shaped still arrives as an engOpt.
//
// cmd is the command whose flags were parsed. A command that declares none of
// the shared flags (or a bare &ucli.Command{} in a test) resolves every one of
// them to "unset", which is the library's own default and the behaviour this
// builder had before they existed.
func buildDecisionStack(ctx context.Context, cmd *ucli.Command, store model.Storage, seedPath string, engOpts ...engine.Option) (decisionStack, error) {
	// Resolved FIRST, before a seed is read or a connection pool is opened: a
	// malformed configured value is an operator typo, and it must fail the command
	// rather than fail it later holding resources this function would then have to
	// unwind.
	shared, err := sharedEngineOptions(cmd)
	if err != nil {
		return decisionStack{}, err
	}

	local, err := seedDocument(seedPath, classifyStore(cmd.String("store")))
	if err != nil {
		return decisionStack{}, err
	}
	// After Setup, before anything is built: the shared wiring decides which
	// document the two registry builders are handed. See the header comment.
	wiring, err := readSharedWiring(ctx, store)
	if err != nil {
		return decisionStack{}, err
	}
	// The digest of what this instance is ABOUT TO BE WIRED WITH, taken before the
	// set is projected into a document and therefore over exactly the rows that were
	// read. A background poll (--wiring-poll) compares a later read against it; with
	// polling off it is computed and never looked at, which costs one hash of a
	// snapshot already in memory. See decisionStack.wiringDigest for why the boot and
	// not the poller owns this value.
	digest, err := wiringDigest(wiring)
	if err != nil {
		return decisionStack{}, err
	}
	doc := local
	// buildOpts is empty on the file-only path, deliberately: a DB-wired boot
	// builds under seed.StrictProviderCollision() because its document was
	// ASSEMBLED from two sources that two people edit, and the silent type-level
	// discard that is an ordinary migration step within one file is a push on
	// another host switching off metadata checked into this one. See
	// wiringBuildOptions.
	var buildOpts []seed.BuildOption
	if !wiring.IsEmpty() {
		doc, err = wiringDocument(wiring, local)
		if err != nil {
			return decisionStack{}, err
		}
		buildOpts = wiringBuildOptions()
	}
	// The two-return form, always: the seed may declare `connections:`, whose
	// pools outlive the build and have to be closed by whoever owns the stack.
	// The one-return BuildRegistry refuses such a document precisely because it
	// cannot hand the pools back.
	reg, conns, err := doc.BuildRegistryWithConnections(seedBaseDir(seedPath), buildOpts...)
	if err != nil {
		// bootError, not a bare wrap, and for the reason spelled out on the
		// attribute build below: a provider declaration fails with
		// APERTURE_CONFIG_INVALID (naming the object type and what was wrong with
		// its statement set, its kind or its ttl) or with
		// APERTURE_SQL_PROVIDER_CONNECTION (naming the connection and the
		// environment variable it reads its DSN from), and re-stamping either
		// APERTURE_BOOT hands the operator "aperture failed to start" instead of
		// the remedy.
		//
		// It matters more now than it did when the wiring could only come from a
		// file: a DB-wired instance is refused here for wiring that lives in a
		// database somebody else pushed, so the code and its context are the only
		// thing pointing at which entry to go and fix.
		return decisionStack{}, bootError("cli: building object providers failed", err)
	}

	var fetcher rules.MetadataFetcher // nil => empty object metadata (unchanged default)
	// metaSource is the STRICT metadata source Enumerate's Fields predicate reads
	// through — the registry itself, not lenientFetcher. The leniency is right for
	// a rule (an unreadable object evaluates against empty metadata and the rule
	// denies) and wrong here: a filtered enumeration that quietly saw empty
	// metadata for every object would return nothing and read as "no access", so
	// the engine wants the registry's real error. Nil when the seed declares no
	// object source at all, which makes a filtered enumeration report
	// APERTURE_PROVIDER_UNREGISTERED rather than an empty result.
	var metaSource engine.MetadataFetcher
	scopeDeps := engine.ScopeDeps{}
	// The guard MUST test both wiring sections. Gating on `providers:` alone made
	// a seed that declared only `objects:` build a populated registry that nothing
	// ever read — inline metadata was invisible to rules and to enumeration.
	if hasObjectSources(doc) {
		fetcher = lenientFetcher{reg: reg}
		scopeDeps.Lister = reg
		metaSource = reg
	}

	// The seed's `attributes:` and `attribute_providers:` sections feed a DIFFERENT
	// registry: an attribute bag is keyed by a bare subject id and is never an
	// enumerable object set, so it deliberately cannot reach the scope resolver's
	// object lister.
	//
	// The pools are PASSED IN, not re-opened. `connections:` is the document's
	// single pool set, shared by every `kind: sql` entry of EITHER section — a
	// second set opened here would double every deployment's connections, and half
	// of them would be held by a registry this stack has no handle to close.
	attrs, err := doc.BuildAttributeRegistryWithConnections(seedBaseDir(seedPath), conns)
	if err != nil {
		// bootError, not a bare wrap: an attribute declaration fails with
		// APERTURE_ATTRIBUTE_SLOT_UNKNOWN (naming the three legal slots) or
		// APERTURE_METADATA_INVALID (naming the field and the cap it broke), and
		// re-stamping either APERTURE_BOOT would hand the operator "aperture
		// failed to start" instead of the remedy.
		_ = conns.Close()
		return decisionStack{}, bootError("cli: building attribute providers failed", err)
	}
	var ruleOpts []rules.Option
	// The gate MUST count every attribute source the document can declare, which
	// is why it is asked of the document rather than re-derived here — see
	// seed.Document.HasAttributeSources, and hasObjectSources below it for the bug
	// that taught us why a gate written over one section of two is a silent one.
	//
	// Wiring the resolver unconditionally would be harmless today (an unfilled
	// slot resolves to the floor bag), but the gate states the intent: with no
	// declared source, `principal` is exactly its floor — id and kind — and no
	// attribute machinery is consulted at all.
	if doc.HasAttributeSources() {
		// One registry, both resolver seams. The principal seam reads the user and
		// machine slots keyed on the principal's kind; the account seam reads the
		// account slot keyed on the ACTIVE account. They are separate options
		// because a rule's `principal` and `account` roots are separate contracts,
		// but wiring them from the same registry is what keeps the caches, the
		// value model and the leniency identical for both.
		ruleOpts = append(ruleOpts,
			rules.WithPrincipalResolver(attrs),
			rules.WithAccountResolver(attrs))
	}

	// The rules engine resolves references against the SAME store the node editor
	// saves through PutRule, so a saved rule takes effect on the next decision and
	// there is no second rule store to keep in sync.
	ruleSource := service.NewStorageRuleSource(store)
	scopeDeps.Rules = rules.NewEngine(ruleSource, fetcher, ruleOpts...)

	opts := make([]engine.Option, 0, len(shared)+len(engOpts)+3)
	opts = append(opts, engine.WithScopeResolution(nil, scopeDeps))
	if metaSource != nil {
		opts = append(opts, engine.WithMetadata(metaSource))
		// The SAME registry is the declared-reference source, so one object
		// source backs the lister, the Fields predicate and the dereference, and
		// an enumeration through `--via` reads the holder from the cache the
		// decision already warmed. Gated on the same condition for the same
		// reason: with no object source at all, an enumeration through a
		// reference must report APERTURE_PROVIDER_UNREGISTERED rather than an
		// empty result that reads as "no access".
		opts = append(opts, engine.WithReferences(reg))
	}
	// Shared before per-command, so a surface that genuinely needs to override a
	// shared option still can — and so the scopeDeps LITERAL above inherits the
	// configured bound: engine.New stamps it into the deps after every option has
	// run (engine.stampScopeBound), which is what keeps the member gather and the
	// result cap one number on this path.
	opts = append(opts, shared...)
	opts = append(opts, engOpts...)

	return decisionStack{
		eng:        engine.New(store, opts...),
		registry:   reg,
		attributes: attrs,
		ruleSource: ruleSource,
		fetcher:    fetcher,
		collisions: doc.ProviderCollisions(),

		attributeCollisions: doc.AttributeCollisions(),
		wiringDigest:        digest,
		// From the WIRING and the LOCAL document, not from doc: the DB-wired
		// projection does not carry a declared set. See declaredAttributeKeySets.
		declaredKeys: declaredAttributeKeySets(wiring, local),
		conns:        conns,
	}, nil
}

// newService composes the stack into a service facade. The provider registry is
// always wired (it is what backs ObjectIdentifiers); opts add the per-surface
// dependencies — `serve` passes storage, the admin gate, delegation,
// impersonation, audit and the editor's rule source, while a one-shot command
// passes nothing and gets the read-only decision facade.
//
// The ATTRIBUTE registry is wired here too, unconditionally and for the same
// reason every other attribute wiring lands in this one builder: a surface that
// assembled its own stack could answer differently from the rest. It is not a
// grant of access. service.ListAttributes is a system-tier read that refuses
// any actor without system-admin authority and refuses outright when no gate is
// wired — which is exactly the one-shot decision commands, so passing the
// registry to them changes nothing they can do.
//
// The DECLARED KEY SETS are wired here for the same reason, and the same way: they
// are a definition-time gate on what a rule may read, so `serve`'s editor and a
// one-shot command's validation have to apply the identical one. It is not a grant
// or a denial of anything — a deployment that declares nothing passes every rule it
// passed before — and it never reaches a decision.
func (s decisionStack) newService(opts ...service.Option) *service.Service {
	all := make([]service.Option, 0, len(opts)+3)
	all = append(all, service.WithProviders(s.registry), service.WithAttributes(s.attributes),
		service.WithDeclaredAttributeKeys(s.declaredKeys))
	all = append(all, opts...)
	return service.New(s.eng, all...)
}
