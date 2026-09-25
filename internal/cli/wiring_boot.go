package cli

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
)

// The model -> seed projection behind BOOTING FROM SHARED WIRING: the read half
// of what internal/cli/wiring_project.go writes.
//
// # Why this is a projection back onto a Document and not a second builder
//
// A second instance has to end up with the same *provider.Registry, the same
// field types and the same *provider.AttributeRegistry a seed file would have
// produced — not an equivalent one. So the wiring rows are projected back into
// the four wiring sections of a seed.Document and handed to the SAME
// BuildRegistryWithConnections / BuildAttributeRegistryWithConnections the file
// path uses.
//
// Writing a parallel builder over model.Wiring* would have meant a second copy of
// every rule those two already own — that get_one and get_all are both required,
// that a kind is one of a closed set, that a ttl parses as a Go duration, that a
// reference target must be a type the registry serves, that "date"/"datetime" is
// the whole field-type vocabulary, that a slot is one of exactly three. Each of
// those would then be two rules that can disagree, and the one that disagrees
// silently is the one in the layer nobody reads: a divergence here does not error,
// it authorizes differently on two instances that were supposed to be identical.
//
// It also answers, without a line of code, the question `aperture wiring push`
// deliberately left open: push does NOT check statement-set completeness (see
// wiring_project.go), because the statement contract belongs to the registry
// builder. A pushed entry with an empty get_one is refused HERE, loudly, by
// sqlprovider.New's own APERTURE_CONFIG_INVALID, on the boot that reads it.
//
// # What the projection carries, and what it cannot
//
// Carried: providers:, field_types:, connections: (names only) and
// attribute_providers:. Not carried, because the tables have no column for them
// and never will: a path (kind: csv is refused at push), a DSN, a dsn_env
// variable NAME, and the pool tuning. Those are the ROUTE, and the route is a
// per-instance fact — see connectionRoutes.
//
// objects: and attributes: are the local document's own, untouched: both carry
// DATA rather than a pointer to data, and they belong to the instance whose seed
// file lists them.
//
// # The database is authoritative; the local file may only ADD
//
// The local file's own four WIRING sections are not discarded. They are layered
// on top of the projection ADDITIVELY: a local entry for an object type or an
// attribute slot the shared wiring never declared is carried through and built
// exactly as it would be on an unpushed instance, and a local entry for one the
// shared wiring DOES declare fails the boot with
// APERTURE_WIRING_LOCAL_COLLISION naming it.
//
// Additive is not a convenience. It is the only shape that composes with a Go
// host: Arc's `wave` and `metric` object providers are hand-written Go registered
// onto the same *provider.Registry, so "the database is the only source" would
// mean Arc could never read a pushed wiring at all. A local kind: csv provider is
// the same case in YAML — a path is machine-local, which is why it cannot BE
// shared wiring (APERTURE_WIRING_KIND_UNSHAREABLE) and therefore must stay
// addable.
//
// A collision is refused in BOTH directions rather than resolved by precedence,
// and that is the load-bearing half. Either resolution is silent and either one
// changes what a decision reads: the database winning discards wiring somebody
// checked into this instance's file, and the file winning means one instance in a
// fleet answers from a source its peers cannot see. Neither surfaces as an error
// on any later decision — it surfaces as a different verdict, on an instance
// that looks identically configured.
//
// # Refusing to start beats degrading, and the reason is not tidiness
//
// Three things can be in the shared wiring that THIS instance cannot turn into a
// working registry, and all three fail the boot rather than being skipped:
//
//   - A kind no second host can construct from a row — today kind: csv, whose only
//     data source is a filesystem path the tables have no column for. Refused by
//     layerProviders / layerAttributeProviders through checkWiringKind, the same
//     function the push calls, naming the object type or the slot.
//   - An incomplete statement set, an unparseable ttl, a reference to a type the
//     registry does not serve, a field type outside the two-word vocabulary. NOT
//     refused here: those belong to seed's own builders, they already name the
//     entry, and restating them would be the rule-in-two-places hazard this file's
//     whole design avoids. What this layer owes them is only that buildDecisionStack
//     does not BURY them — see bootError's pass-through guard.
//   - A connection name this instance has no route for. Refused by
//     connectionRoutes / refuseUnroutedConnections, naming the connection.
//
// Degrading instead would be silent in the one direction that matters. An object
// type with no working provider does not error on a decision: a rule reading
// object.tier reads a missing path. An attribute slot with no working provider is
// worse — the leniency contract collapses a provider failure to a NIL BAG, and a
// rule that excluded on an attribute it can no longer see stops excluding, so the
// grant WIDENS (rules/attribute_leniency_test.go,
// TestAMissingBagWidensAnExclusiveGrant). Neither shows up in a verdict, a trace or
// a note. A per-decision refusal is not the alternative either: it would let a
// misconfigured instance serve traffic for the types it happened to have, which is
// a fleet that answers the same question two ways and no operator looking.
//
// # Where the collision check lives, and why part of it is not here
//
// The rule is about the REGISTRY, not about which syntax declared the entry, so
// the refusal has to exist at the registry as well as in this projection:
//
//   - A file-declared entry passes through this function, and the three layer*
//     helpers below refuse it here, naming the two SECTIONS — which is the thing
//     an operator can act on.
//   - A GO-registered entry never reaches this function at all. A host calls
//     provider.Registry.Register (Arc's RegisterProviders, from its own
//     apertureScopeDeps) on the registry a decision stack already built, long
//     after any document has been read. Its collision is refused by Register's
//     own duplicate check — APERTURE_PROVIDER_INVALID naming the object type, and
//     APERTURE_ATTRIBUTE_PROVIDER_INVALID naming the slot on the attribute
//     registry — which is structural: a *provider.Registry holds at most one
//     provider per type and has never accepted a second.
//
// So there is one arbiter and two messages, not two rules. This layer's job is to
// say WHICH SOURCE before the registry says merely "twice", because "object type
// already has a registered provider" is the truth and not the remedy when the
// other declaration is in a database on another host. Do not add a second
// registry-level check here: a projection cannot see a Register call that has not
// happened yet, and one that tried would be a rule that disagrees with the
// registry the moment a host registers in a different order.
//
// The inline-data sections are refused on the same axis, one by each mechanism it
// already has:
//
//   - objects: against a shared providers: entry — seed.StrictProviderCollision(),
//     which is exactly that posture and is passed on this path only (see
//     wiringBuildOptions). On the file-only path the overlap stays the documented
//     silent discard, because adding a providers: row while inline entries are
//     still in the file is an ordinary migration step.
//   - attributes: against a shared attribute_providers: entry — refused by
//     layerAttributeProviders, because the attribute builder takes no BuildOption
//     and the posture has nowhere else to be stated. The hazard there is the worse
//     of the two: a discarded inline bag is a MISSING bag, and a missing bag
//     WIDENS an exclusive grant with nothing in the verdict saying so.

// connectionDSNEnvPrefix and connectionDSNEnvSuffix bracket the environment
// variable a DB-declared connection name is read through when the local seed
// document declares no route for it — "main" becomes
// APERTURE_CONNECTION_MAIN_DSN.
//
// This is the CLI's route of last resort, and it is a route rather than a new
// mechanism: it resolves to a seed.Connection whose dsn_env: names this variable,
// so the DSN is still read by seed.resolveConnection out of the environment, the
// pool is still opened by the ordinary ConnectionOpener, and an unset variable is
// still the same coded refusal a seed file's own dsn_env: would raise. A Go host
// supplies its route through seed.WithConnectionOpener instead and never reaches
// this; `aperture serve --store <dsn>` with no --seed has no Go and no file, and
// its environment is the only thing left that is per-instance.
//
// The shared wiring names none of this. It carries the connection NAME, and the
// name is all it carries.
const (
	connectionDSNEnvPrefix = "APERTURE_CONNECTION_"
	connectionDSNEnvSuffix = "_DSN"
)

// sharedProvidersSection and sharedAttributeProvidersSection are the section
// names a BOOT refusal spells, where a PUSH refusal spells "providers" and
// "attribute_providers".
//
// The word "shared" is the whole difference and it is load-bearing. checkWiringKind
// is one function reached from both ends of the wiring — it is called at the push
// (wiring_project.go) and again here, on the boot that reads the rows back — and
// its message says which SECTION the offending entry is in. At a push that section
// is a key in the document the operator is holding; at a boot it is a row in a
// database somebody else pushed to, possibly from another host, and an operator
// sent to look in "providers:" would grep a local seed file that never mentioned
// the type. Naming the section differently is how one message serves both without
// sending half its readers to the wrong file.
const (
	sharedProvidersSection          = "shared providers:"
	sharedAttributeProvidersSection = "shared attribute_providers:"
)

// wiringDocument projects a non-empty shared wiring set into the four wiring
// sections of a seed.Document, layers this instance's own LOCAL wiring on top of
// it additively, and carries local's two DATA sections (objects: and attributes:)
// through unchanged.
//
// local is this instance's own seed document — possibly empty or nil — and is
// read for four things: the data sections, the ROUTE for each connection name the
// shared manifest declares, its own connections: block, and the object types and
// attribute slots it ADDS. The database entries are authoritative: a local entry
// for a type or slot the shared wiring already declares fails the boot with
// APERTURE_WIRING_LOCAL_COLLISION rather than either side quietly winning. See
// the file header for why both resolutions are worse than a refusal, and for
// where the same rule is enforced for a host that registers in Go.
//
// The sections are layered in a fixed order — connections, providers, field
// types, attribute providers — so a document with collisions on two axes always
// fails on the same one and a boot is reproducible.
func wiringDocument(set model.WiringSet, local *seed.Document) (*seed.Document, error) {
	doc := &seed.Document{}
	if local != nil {
		doc.Objects = local.Objects
		doc.Attributes = local.Attributes
	}

	conns, err := connectionRoutes(set.Connections, local)
	if err != nil {
		return nil, err
	}
	doc.Connections = conns

	if doc.Providers, err = layerProviders(set.Providers, local); err != nil {
		return nil, err
	}
	if doc.FieldTypes, err = layerFieldTypes(set.FieldTypes, local); err != nil {
		return nil, err
	}
	if doc.AttributeProviders, err = layerAttributeProviders(set.AttributeProviders, local); err != nil {
		return nil, err
	}
	return doc, nil
}

// wiringBuildOptions are the seed.BuildOptions the object registry is built under
// on a DB-wired boot, and on that boot only.
//
// One option, and it is the posture rather than a new mechanism:
// seed.StrictProviderCollision() turns an object type declared in BOTH the
// document's providers: and objects: sections from a silent type-level discard
// into an APERTURE_CONFIG_INVALID naming the type.
//
// On the file-only path that discard stays the default, deliberately: pointing a
// type at a CSV while its inline entries are still in the file is an ordinary
// migration step in ONE document a single author owns, and a seed that booted
// yesterday must not stop booting because a providers: row was added. Here the
// document is one Aperture ASSEMBLED from two sources that two different people
// edit, on two different machines, and the overlap is cross-source by
// construction: the providers: section is the database's and the objects: section
// is this instance's file. A discard there is a push on another host silently
// switching off metadata checked into this instance's seed — which is exactly the
// additive rule's whole subject, so the strict posture is what states it.
//
// It applies to the WHOLE assembled document, which means a local file that
// declares both a providers: entry and inline objects: for one type is also
// refused once wiring rows exist. That is one rule applied uniformly to one
// document rather than a second rule with an exception in it, and the refusal
// names the type either way.
func wiringBuildOptions() []seed.BuildOption {
	return []seed.BuildOption{seed.StrictProviderCollision()}
}

// layerProviders projects the shared providers: entries and appends the local
// document's own, refusing any object type both declare.
//
// The shared entries come first so the section reads database-then-local, but the
// order carries no precedence: a collision is refused, so there is never a second
// entry for one type to be resolved against. It matters only for
// seed.declareReferences, which runs in its own pass after every type is
// registered and therefore lets a local provider's references: name a shared type
// and the other way round.
//
// A local entry is carried VERBATIM, path: included. That is the point: kind: csv
// cannot be shared wiring at all (a filesystem path is machine-local), so a local
// file is the only place a csv provider can ever be declared, and dropping it
// from a DB-wired boot would make pushing any wiring at all a silent loss of
// every csv-backed type.
//
// Which is exactly why a SHARED entry's kind is checked here and a local one's is
// not. The two sections are held to different vocabularies on purpose: the local
// file may say anything seed's builder implements, and the shared rows may only say
// what a second host can construct from a row (checkWiringKind — today, kind: sql).
// The refusal is the same function the push calls, so there is one vocabulary in
// one place with two call sites, and it names the OBJECT TYPE, which is the thing
// an operator can go and look up.
func layerProviders(shared []model.WiringProvider, local *seed.Document) ([]seed.Provider, error) {
	out := make([]seed.Provider, 0, len(shared))
	declared := make(map[string]struct{}, len(shared))
	for _, p := range shared {
		if err := checkWiringKind(p.Kind, sharedProvidersSection,
			map[string]any{"object_type": p.ObjectType}); err != nil {
			return nil, err
		}
		out = append(out, wiringSeedProvider(p))
		declared[p.ObjectType] = struct{}{}
	}
	if local != nil {
		var collided []string
		for _, p := range local.Providers {
			if _, dup := declared[p.ObjectType]; dup {
				collided = append(collided, p.ObjectType)
				continue
			}
			out = append(out, p)
		}
		if len(collided) > 0 {
			return nil, localCollision("object type", "providers:", "providers:", collided)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// layerFieldTypes regroups the shared field-type rows and appends the local
// document's own entries, refusing any object type both declare.
//
// A field_types: entry is keyed by object type and applies to the objects:
// section only, so the shared rows and the local inline objects: they type are the
// INTENDED composition and not a collision — a pushed declaration that says
// project.started_on is a date types the entries this instance's file lists for
// project. What cannot happen is two declarations for one type: seed's own
// fieldTypeIndex refuses a type declared twice, so an un-layered append would
// fail the build with "object_type declared twice" and leave the operator to work
// out that the other one is in a database.
func layerFieldTypes(shared []model.WiringFieldType, local *seed.Document) ([]seed.FieldType, error) {
	out := wiringSeedFieldTypes(shared)
	if local == nil || len(local.FieldTypes) == 0 {
		return out, nil
	}
	declared := make(map[string]struct{}, len(out))
	for _, ft := range out {
		declared[ft.ObjectType] = struct{}{}
	}
	var collided []string
	for _, ft := range local.FieldTypes {
		if _, dup := declared[ft.ObjectType]; dup {
			collided = append(collided, ft.ObjectType)
			continue
		}
		out = append(out, ft)
	}
	if len(collided) > 0 {
		return nil, localCollision("object type", "field_types:", "field_types:", collided)
	}
	return out, nil
}

// layerAttributeProviders projects the shared attribute_providers: entries and
// appends the local document's own, refusing any slot both declare — and refusing
// a local INLINE attributes: entry for a slot the shared wiring declares, which is
// the same collision arriving through the data section.
//
// The inline half has to be refused here rather than by a build option, because
// BuildAttributeRegistryWithConnections takes none: the attribute seam's
// external-wins-entirely precedence has no strict posture to turn on, so this is
// the only place the additive rule can be stated for it.
//
// It is also the worse of the two hazards, and the reason the check is not
// "tidiness". The external source wins a slot ENTIRELY — no per-subject merge, no
// fallback — so a push on another host silently replaces this instance's inline
// bags with a directory that has never heard of its subjects. Every read of that
// slot then returns an empty bag, and an empty bag does not deny: it WIDENS an
// exclusive grant, because a rule that excluded on an attribute no longer sees the
// attribute. Nothing in the resulting verdict says a bag went missing.
func layerAttributeProviders(shared []model.WiringAttributeProvider, local *seed.Document) ([]seed.AttributeProvider, error) {
	out := make([]seed.AttributeProvider, 0, len(shared))
	declared := make(map[string]struct{}, len(shared))
	for _, ap := range shared {
		// The kind is checked for the same reason it is on the provider side, and
		// the stakes are the worse of the two: a shared kind: csv row projects to a
		// pathless entry, and a slot whose provider cannot be constructed is a slot
		// that answers with a nil bag — which widens an exclusive grant instead of
		// denying. Refused before a registry exists, naming the SLOT.
		if err := checkWiringKind(ap.Kind, sharedAttributeProvidersSection,
			map[string]any{"subject": strings.TrimSpace(ap.Subject)}); err != nil {
			return nil, err
		}
		out = append(out, wiringSeedAttributeProvider(ap))
		declared[strings.TrimSpace(ap.Subject)] = struct{}{}
	}
	if local != nil {
		var collided []string
		for _, ap := range local.AttributeProviders {
			if _, dup := declared[strings.TrimSpace(ap.Subject)]; dup {
				collided = append(collided, strings.TrimSpace(ap.Subject))
				continue
			}
			out = append(out, ap)
		}
		if len(collided) > 0 {
			return nil, localCollision("attribute slot", "attribute_providers:", "attribute_providers:", collided)
		}
		// The inline section is NOT dropped when it does not collide: a slot the
		// shared wiring never declared is served from this instance's own bags,
		// exactly as it is on an unpushed instance.
		var inline []string
		for _, a := range local.Attributes {
			if _, dup := declared[strings.TrimSpace(a.Subject)]; dup {
				inline = append(inline, strings.TrimSpace(a.Subject))
			}
		}
		if len(inline) > 0 {
			return nil, localCollision("attribute slot", "attributes:", "attribute_providers:", inline)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// localCollision is the boot refusal for names a LOCAL wiring section declares
// that the shared wiring already declares.
//
// noun names what collided ("object type", "attribute slot"); localSection and
// sharedSection name the two YAML keys, so the message says which two places to
// go and read rather than only what is wrong. Only TYPE and SLOT names ride in
// it — never an object id, never an attribute key — for the same reason
// decisionStack.reportCollisions names only those: an object id can embed an
// account, and an error message is the wrong place for one.
//
// Names are sorted and de-duplicated so an operator fixing a document sees every
// colliding entry on that axis in one pass, in the same order every time.
func localCollision(noun, localSection, sharedSection string, names []string) error {
	sort.Strings(names)
	names = slices.Compact(names)
	return aerr.WithContext(aerr.APERTURE_WIRING_LOCAL_COLLISION,
		fmt.Sprintf("cli: this instance's seed file declares %s %s in %s, and the shared wiring in its database already declares %s in %s; with wiring rows present the database is AUTHORITATIVE and the local file may only ADD %ss the database never declared, so the collision is refused rather than resolved — delete the local declaration, or push a wiring document that omits the shared one if the local wiring is what this fleet should use",
			noun, strings.Join(quoteEach(names), ", "), localSection,
			plural("it", "them", len(names)), sharedSection, noun),
		map[string]any{
			"collisions":     names,
			"local_section":  localSection,
			"shared_section": sharedSection,
		})
}

// plural picks between two words by count, so a refusal naming one entry does not
// read as a formatting bug.
func plural(one, many string, n int) string {
	if n == 1 {
		return one
	}
	return many
}

// wiringSeedProvider converts one stored provider entry into its seed form.
//
// Path is left empty deliberately, and there is nothing to leave out: kind: csv
// cannot be SHARED wiring (a filesystem path is machine-local), so the shared
// tables have no path column and a stored entry can never want one.
//
// A row that nevertheless spells kind: csv — hand-written, or written by a build
// that predates the push check — never reaches this function: layerProviders
// refuses it first, with checkWiringKind's APERTURE_WIRING_KIND_UNSHAREABLE naming
// the object type. Letting it through would have reached seed's own
// buildObjectProvider, which refuses a pathless csv entry for the missing path —
// the right refusal for the wrong reason, and a remedy an operator cannot carry
// out, because there is no path column to put a path in.
func wiringSeedProvider(p model.WiringProvider) seed.Provider {
	var refs map[string]string
	if len(p.References) > 0 {
		refs = make(map[string]string, len(p.References))
		for _, r := range p.References {
			refs[r.Field] = r.TargetType
		}
	}
	return seed.Provider{
		ObjectType: p.ObjectType,
		Kind:       p.Kind,
		Connection: p.Connection,
		GetOne:     p.GetOne,
		GetAll:     p.GetAll,
		IDColumn:   p.IDColumn,
		TTL:        p.TTL,
		MaxSize:    p.MaxSize,
		References: refs,
	}
}

// wiringSeedFieldTypes regroups the flat field-type rows into one seed entry per
// object type.
//
// The flattening is the storage shape (one row per object type and field, so the
// table has a primary key); the seed shape is one entry per object type carrying
// a field map, and seed's fieldTypeIndex refuses an object type declared twice.
// So the rows are GROUPED rather than mapped one-for-one, and the entries come
// out in the sorted order GetWiring returned the rows in.
func wiringSeedFieldTypes(rows []model.WiringFieldType) []seed.FieldType {
	if len(rows) == 0 {
		return nil
	}
	out := make([]seed.FieldType, 0, len(rows))
	at := make(map[string]int, len(rows))
	for _, ft := range rows {
		i, ok := at[ft.ObjectType]
		if !ok {
			out = append(out, seed.FieldType{ObjectType: ft.ObjectType, Fields: map[string]string{}})
			i = len(out) - 1
			at[ft.ObjectType] = i
		}
		out[i].Fields[ft.Field] = ft.DeclaredType
	}
	return out
}

// wiringSeedAttributeProvider converts one stored attribute-provider entry into
// its seed form.
//
// DeclaredKeys has no seed key yet, so nothing is projected for it and the
// distinction it exists to carry — "not declared" versus "declared empty" — is
// not collapsed on the way through: it is simply not on this path. Enforcing a
// declared set is a separate story, and it reads the model rows, not this
// Document.
func wiringSeedAttributeProvider(ap model.WiringAttributeProvider) seed.AttributeProvider {
	return seed.AttributeProvider{
		Subject:    ap.Subject,
		Kind:       ap.Kind,
		Connection: ap.Connection,
		GetOne:     ap.GetOne,
		GetAll:     ap.GetAll,
		IDColumn:   ap.IDColumn,
		TTL:        ap.TTL,
		MaxSize:    ap.MaxSize,
	}
}

// connectionRoutes resolves every name in the shared manifest to this instance's
// own route for it.
//
// The shared wiring holds a manifest of NAMES. A route — which server, which
// credential, how big a pool, how long a statement may take — is a per-instance
// fact: two instances may reach one logical database through different hosts, and
// the database has no column for any of it. So each name is resolved locally, in
// one of three ways, and never from the row:
//
//  1. A Go host passes seed.WithConnectionOpener and answers for every name
//     itself. That is the documented seam and the only one a host needs; it sees
//     the name and builds its own pool. Arc does exactly this today, with a
//     dsn_env: it has deliberately set to a placeholder.
//  2. This instance's own seed file declares a connections: entry under the same
//     name. The entry is used verbatim — dsn_env:, pool sizes, query_timeout —
//     because a route is precisely what a local connections: entry is. Its four
//     wiring sections are not read for a DB-wired boot; this one key is a route
//     table, not wiring.
//  3. Neither: the name resolves to the conventional environment variable
//     (connectionDSNEnvVar), which is what `aperture serve --store <dsn>` with no
//     --seed has left.
//
// A name none of the three answers for is refused HERE, before a pool is opened
// and long before a decision is made, with APERTURE_WIRING_CONNECTION_UNROUTED
// naming the connection and the variable the conventional route wanted
// (refuseUnroutedConnections, which also explains why leaving it to
// seed.resolveConnection's APERTURE_SQL_PROVIDER_CONNECTION hands the operator a
// remedy pointing at a file that never mentioned the name).
//
// # Why the WHOLE local block is carried, not only the shared names it routes
//
// connections: is the one section the additive rule does not apply to, in either
// half. A local entry under a shared NAME is that name's route (case 2 above) and
// not a competing declaration, so it is not a collision; and a local entry under a
// name the manifest never mentions is the route for a connection only this
// instance's own ADDED kind: sql providers can reach, so dropping it would make
// every locally-added SQL provider fail the build for an undeclared connection.
// So the map starts as the local block in full and the manifest fills in the rest.
//
// Carrying entries nothing references costs one lazy pool and one dsn_env: lookup
// each, which is precisely what the same file costs on the pure-file path —
// seed.openConnections resolves every DECLARED connection whether a provider uses
// it or not. A DB-wired boot therefore adds no failure mode a file-only boot of the
// same document did not already have.
func connectionRoutes(manifest []model.WiringConnection, local *seed.Document) (map[string]seed.Connection, error) {
	out := make(map[string]seed.Connection, len(manifest))
	// Local entries are copied VERBATIM, DSNLiteral included. A literal dsn: is
	// refused by seed.Parse before a document is usable for anything, so a parsed
	// file cannot carry one, and re-deriving that refusal here would be the same
	// rule in two places — the one thing wiring_project.go's own doc comment warns
	// against.
	if local != nil {
		for name, c := range local.Connections {
			out[name] = c
		}
	}
	// Only the CONVENTIONAL names are checked for a collision. A local document
	// may legitimately point two connections at one dsn_env: — two logical names
	// over one server is two pools, which the seed path has always allowed — but
	// two DB-declared names that DERIVE the same variable is an ambiguity this
	// convention created, and resolving it either way would route one of them
	// somewhere nobody asked for.
	derived := make(map[string][]string, len(manifest))
	for _, c := range manifest {
		if _, routed := out[c.Name]; routed {
			continue
		}
		env := connectionDSNEnvVar(c.Name)
		derived[env] = append(derived[env], c.Name)
		out[c.Name] = seed.Connection{DSNEnv: env}
	}
	for _, env := range sortedMapKeys(derived) {
		names := derived[env]
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("cli: shared wiring declares connections %s, which all read their DSN from the same environment variable %s because the names differ only in characters the variable spelling cannot keep; declare a connections: entry for each of them in this instance's seed file, naming a distinct dsn_env:, or rename them in the shared wiring",
				strings.Join(quoteEach(names), ", "), env),
			map[string]any{"connections": names, "dsn_env": env})
	}
	// The ambiguity above is checked FIRST, deliberately: an ambiguous manifest is
	// a deployment-wide mistake and every instance in the fleet must report the
	// same one, where an absent route is a per-instance fact that is routinely
	// true here and false on the peer.
	if err := refuseUnroutedConnections(derived); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// refuseUnroutedConnections fails the boot for every DB-declared connection name
// that fell through to the conventional environment variable and found it unset or
// empty — that is, a name this instance has no route for at all.
//
// derived maps each conventional variable to the names that resolved to it, so only
// names the local document did NOT route are in it. A locally-routed name is that
// name's route and is never examined here; a Go host does not reach this function
// at all, because wiringDocument is unexported in an internal package and a host
// embedding the library builds its own document and answers for every name through
// seed.WithConnectionOpener.
//
// # Why this is not the same check twice
//
// seed.resolveConnection already refuses an unset dsn_env, and it runs on this
// path: openConnections resolves EVERY declared connection before it opens any
// pool, ahead of the ConnectionOpener, so nothing here relaxes a requirement and
// nothing here adds one. What it adds is the REMEDY, and the remedy is the whole
// reason it exists. seed's refusal reads "connection "main" reads its DSN from
// APERTURE_CONNECTION_MAIN_DSN, which is unset" and its fixups send an operator to
// a document's connections: block — and a DB-declared name is not in this
// instance's document, so the operator greps a seed file that has never mentioned
// main. This refusal says the name came out of the SHARED WIRING and lists the
// three routes, one of which is "export this variable" and needs no file at all.
//
// The duplication cannot drift in the dangerous direction, which is the reason it
// is acceptable at all. Both checks are refusals of the same absence: if seed's
// were ever relaxed this one still fails the boot, and if this one were deleted
// seed's still does. There is no combination in which an unrouted name boots.
//
// Every unrouted name is named in one refusal, sorted, so an operator exports one
// batch of variables rather than rediscovering the next missing one on each
// restart.
func refuseUnroutedConnections(derived map[string][]string) error {
	type unrouted struct {
		name string
		env  string
	}
	var missing []unrouted
	for _, env := range sortedMapKeys(derived) {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			continue
		}
		names := derived[env]
		sort.Strings(names)
		for _, name := range names {
			missing = append(missing, unrouted{name: name, env: env})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	pairs := make([]string, 0, len(missing))
	names := make([]string, 0, len(missing))
	envs := make([]string, 0, len(missing))
	for _, m := range missing {
		pairs = append(pairs, fmt.Sprintf("%q (%s)", m.name, m.env))
		names = append(names, m.name)
		envs = append(envs, m.env)
	}
	return aerr.WithContext(aerr.APERTURE_WIRING_CONNECTION_UNROUTED,
		fmt.Sprintf("cli: the shared wiring in this instance's database declares connection %s %s, and this instance has no route for %s: the shared tables carry a connection's NAME and nothing else, because which server, which credential and how big a pool are per-instance facts. Export the variable named beside each one with this instance's DSN, or declare a connections: entry under the same name in this instance's --seed file naming a dsn_env: of your choosing (a Go host supplies seed.WithConnectionOpener instead). It is refused at BOOT rather than at the first decision that needed the database, because an object provider that cannot reach its database yields no metadata and an attribute provider that cannot yields a nil bag — and a missing bag WIDENS an exclusive grant instead of denying it",
			plural("name", "names", len(pairs)), strings.Join(pairs, ", "),
			plural("it", "them", len(pairs))),
		map[string]any{"connections": names, "dsn_env": envs})
}

// connectionDSNEnvVar is the environment variable a DB-declared connection name
// reads its DSN from when nothing local declares a route for it:
// "main" -> APERTURE_CONNECTION_MAIN_DSN.
//
// Every character that is not an ASCII letter or digit becomes an underscore,
// because a name is an arbitrary string and an environment variable is not. That
// is lossy on purpose — a legible variable an operator can export by hand is
// worth more than a reversible encoding nobody would type — and the collision it
// can create is refused by connectionRoutes rather than resolved.
func connectionDSNEnvVar(name string) string {
	var b strings.Builder
	b.Grow(len(connectionDSNEnvPrefix) + len(name) + len(connectionDSNEnvSuffix))
	b.WriteString(connectionDSNEnvPrefix)
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString(connectionDSNEnvSuffix)
	return b.String()
}

// sortedMapKeys returns a map's keys in a stable order, so a refusal built by
// walking one is reproducible.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
