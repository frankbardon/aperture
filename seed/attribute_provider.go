package seed

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/frankbardon/aperture/csvprovider"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/sqlprovider"
)

// The attribute_providers: block.
//
// It is the attribute seam's counterpart of providers:, and it is a SEPARATE
// TOP-LEVEL KEY rather than a variant of providers: — a discriminated entry in
// one list, or a subject: field on seed.Provider. That is a deliberate refusal,
// and the reason is the statements themselves.
//
// The two "get all" contracts genuinely differ:
//
//	providers:            get_all: SELECT 'brand:' || b.id AS id, b.tier FROM brands b
//	attribute_providers:  get_all: SELECT u.id AS id, u.department FROM users u
//
// An OBJECT provider's get_all selects each row's FULL IDENTITY, because an
// object id is a segmented path a scope can contain and pattern-match, and
// Aperture supplies no template for composing one. An ATTRIBUTE provider's
// selects a BARE ID, because an attribute key is an opaque handle from the
// host's directory with no segment structure at all (see provider.AttributeRecord).
//
// Sharing one struct would therefore mean doc comments that contradict
// themselves depending on which key the entry was filed under — and, far worse,
// it would make copying an object provider's get_all into an attribute entry a
// silent fault. Ids of the form "user:alice" would enumerate happily, cache
// happily, and match NO principal id ever presented to a Fetch. Nothing would
// error; the slot would simply never answer. A failure with no error is exactly
// the kind this repository writes down and then designs out.
//
// get_one differs in the same direction: an object provider binds the identity's
// TERMINAL SEGMENT VALUE ("brand:42" and "account:acme/brand:42" both bind
// "42"), while an attribute provider binds the BARE SUBJECT ID VERBATIM, because
// there is nothing to strip.
//
// Like providers:, objects:, field_types:, connections: and attributes:, this is
// runtime WIRING and not model state: Apply writes no row for it, and because
// Export reads the model back OUT of storage, an export reproduces none of it.
// See seed/provider.go:17-20, which states the rule for the section this one
// mirrors.

const (
	// attributeKindCSV is the file-backed attribute source: one CSV whose id
	// column holds BARE subject ids.
	attributeKindCSV = "csv"
	// attributeKindSQL is the database-backed attribute source: two statements
	// run against a named entry of the document's shared connections: pool.
	attributeKindSQL = "sql"
)

// attributeKinds is the closed set of implementations an attribute_providers:
// entry may select, in the order an error names them. It is derived from nothing
// — it is the list — so a kind that is legal here and unnamed in the refusal is
// impossible.
func attributeKinds() []string { return []string{attributeKindCSV, attributeKindSQL} }

// AttributeProvider declares an EXTERNAL source for one attribute SLOT, so a
// host can back `principal.department` and `account.plan` with its real user
// table or a CSV export instead of listing bags inline:
//
//	connections:
//	  main:
//	    dsn_env: APP_DATABASE_URL
//
//	attribute_providers:
//	  - subject: user
//	    kind: sql
//	    connection: main
//	    get_one: SELECT department, clearance FROM users WHERE id = $1
//	    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
//	  - subject: machine
//	    kind: csv
//	    path: machines.csv
//	  - subject: account
//	    kind: sql
//	    connection: main
//	    get_one: SELECT plan, region FROM accounts WHERE id = $1
//
// It is the counterpart of Provider (providers:), and the file doc above records
// why the two are separate structs under separate keys rather than one list with
// a discriminator. The type is named AttributeProvider because the KEY is
// attribute_providers:; provider.AttributeProvider — the interface an entry is
// resolved INTO — is a different package's name for a different thing, and the
// two never appear in one signature.
//
// Every entry is runtime WIRING: Apply writes nothing for it and an export
// reproduces none of it.
type AttributeProvider struct {
	// Subject names the attribute SLOT this entry fills: "user" or "machine" for
	// a principal, "account" for the tenant a decision is made in. The set is
	// closed — it is the parties a decision has — and an unknown value is
	// APERTURE_ATTRIBUTE_SLOT_UNKNOWN naming the three legal ones. Each slot may
	// be declared at most once.
	//
	// It is spelled subject: rather than kind: for the reason Attribute.Subject
	// gives: "account" is a slot but not a principal KIND, and kind: is already
	// this entry's implementation selector.
	Subject string `yaml:"subject" json:"subject"`
	// Kind selects the implementation: "csv" (a file, resolved from Path) or
	// "sql" (statements run against a named Connection). Required.
	Kind string `yaml:"kind" json:"kind"`
	// Path is the data source for file-backed kinds (csv), resolved relative to
	// the seed file's directory when it is not absolute. Its id column holds BARE
	// subject ids — "alice", not "user:alice" — because an attribute key is an
	// opaque handle, not an identity. Required for kind: csv.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Connection names the entry in the document's connections: block this
	// provider reads through. Required for kind: sql, and a name with no matching
	// connections: entry is a HARD ERROR AT BUILD — the pool is shared, so a typo
	// here would otherwise open a second pool to the same server, or none at all.
	//
	// The pool is the SAME one the document's object providers read through: one
	// connections: entry is one pool, however many providers of either kind name
	// it.
	Connection string `yaml:"connection,omitempty" json:"connection,omitempty"`
	// DSNLiteral is the FORBIDDEN dsn: key, decoded here for the same reason
	// Connection.DSNLiteral is: so a document that spells credentials inline is
	// rejected BY NAME at Parse rather than silently ignored. Credentials belong
	// to a connections: entry's dsn_env:, never to a provider entry, and never as
	// a literal. It is never read as a DSN.
	DSNLiteral string `yaml:"dsn,omitempty" json:"dsn,omitempty"`
	// GetOne is the "get one" statement for kind: sql, taking exactly one
	// placeholder to which the BARE SUBJECT ID is bound VERBATIM:
	//
	//	get_one: SELECT department, clearance FROM users WHERE id = $1
	//
	// This is the first of the two contract differences from a providers: entry,
	// which binds the identity's terminal segment value instead. A subject key is
	// already bare, so there is nothing to strip and nothing is stripped.
	//
	// Every column it returns becomes an attribute field keyed by the column
	// name. Required for kind: sql.
	GetOne string `yaml:"get_one,omitempty" json:"get_one,omitempty"`
	// GetAll is the "get all" statement for kind: sql, taking NO parameters and
	// selecting each row's BARE id as the IDColumn:
	//
	//	get_all: SELECT u.id AS id, u.department FROM users u
	//
	// NOT 'user:' || u.id AS id. That is the second contract difference from a
	// providers: entry, and it is the one with no error attached: an identity-
	// shaped key enumerates and caches perfectly happily and then matches no
	// principal id a Fetch ever presents, so the slot silently never answers.
	//
	// # get_all is OPTIONAL here, where it is REQUIRED for an object provider
	//
	// A providers: entry must declare both statements, because an object provider
	// that could be fetched from but not enumerated answers List with an error,
	// and an errored enumeration reads as "no access" one layer up — a denial
	// caused by a wiring gap.
	//
	// That reason does not apply here. Attribute enumeration NEVER participates
	// in scope resolution: an *provider.AttributeRegistry structurally cannot be
	// a scope.ObjectLister, and TestAttributeRegistryIsNotAScopeLister asserts
	// the negative against the real interface. Enumeration is a system-tier admin
	// read and nothing else. So omitting get_all yields a FETCH-ONLY slot: every
	// decision path works unchanged, and only List/Query refuse, with a coded
	// error from the provider that cannot enumerate.
	//
	// That is a feature, not a tolerated gap. It lets a host expose the
	// attributes of the principal currently being decided about WITHOUT exposing
	// its entire user table to an admin enumeration.
	GetAll string `yaml:"get_all,omitempty" json:"get_all,omitempty"`
	// IDColumn names the get_all result column holding each row's BARE subject
	// id. Empty means sqlprovider.DefaultIDColumn ("id"). The column is removed
	// from the row before the rest becomes the attribute bag: it is the key, not
	// a field.
	IDColumn string `yaml:"id_column,omitempty" json:"id_column,omitempty"`
	// TTL is this slot's cache freshness window as a Go duration ("30s", "5m").
	// Empty adopts the registry default; "0" never expires — set that for a
	// static file you reload explicitly.
	//
	// A slot's cache is its own: the three slots have genuinely different change
	// rates and cardinalities, and one number covering all of them would tune for
	// whichever was declared last.
	TTL string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// MaxSize caps cached bags for this slot; 0 uses the registry default.
	MaxSize int `yaml:"max_size,omitempty" json:"max_size,omitempty"`
}

// attributeSource is one RESOLVED attribute_providers: entry: the declaration
// with its slot parsed, its path made absolute, its connection looked up to a
// live pool and a statement budget, and its cache options built. It is what an
// attributeSourceOpener is handed, so an opener never re-reads the document and
// never re-derives a rule this file already applied.
type attributeSource struct {
	// Slot is the parsed subject: — one of the three, never anything else.
	Slot provider.AttributeSlot
	// Kind is the declared implementation, one of attributeKinds().
	Kind string
	// Path is the absolute file path for kind: csv (baseDir already applied).
	Path string
	// Connection is the connections: name for kind: sql, kept for diagnostics.
	Connection string
	// Pool is the live shared handle for kind: sql; nil for every other kind.
	Pool Pool
	// QueryTimeout is the CONNECTION's statement budget, inherited unchanged: it
	// is a property of the database being read, not of the slot reading it.
	QueryTimeout time.Duration
	// GetOne, GetAll and IDColumn are the declared statements and id column, with
	// GetAll empty for a fetch-only slot.
	GetOne, GetAll, IDColumn string
	// cacheOpts are the per-slot cache options ttl: and max_size: imply, already
	// parsed. Empty means "inherit the registry defaults".
	cacheOpts []provider.CacheOption
}

// attributeSourceOpener turns one resolved entry into the live
// provider.AttributeProvider that serves its slot.
//
// It is the seam the loader adapters plug into: the kind: csv arm is an adapter
// over csvprovider, and the kind: sql arm is E3-S3's over sqlprovider.
// Everything ABOVE the seam — the block, its validation, the connection lookup,
// the slot precedence, the cache options — is settled here and does not move
// when a loader lands.
//
// It is unexported on purpose. WithConnectionOpener is exported because a host
// legitimately owns its database handles; a host that owns its attribute SOURCE
// implements provider.AttributeProvider and registers it directly against a
// provider.AttributeRegistry, with no seed document involved at all.
type attributeSourceOpener func(src attributeSource) (provider.AttributeProvider, error)

// openAttributeSource is the default opener: it dispatches on the declared kind.
// Validation has already run, so the arms may assume every field they need is
// present and resolved.
func openAttributeSource(src attributeSource) (provider.AttributeProvider, error) {
	switch src.Kind {
	case attributeKindCSV:
		return csvAttributeSource(src)
	case attributeKindSQL:
		return sqlAttributeSource(src)
	default:
		// Unreachable: resolveAttributeSource refuses an unknown kind before an
		// opener is ever called. Stated rather than panicked, because a future
		// kind added to attributeKinds() and not to this switch should fail
		// loudly at boot rather than take the process down.
		return nil, unwiredAttributeKind(src)
	}
}

// csvAttributeSource builds the file-backed attribute provider for a kind: csv
// entry, from the absolute path resolveAttributeSource already made.
//
// The file is read HERE, not on the first decision that needs it, so a malformed
// one is a coded error at build naming the file and the row. An attribute bag is
// read by every rule against every object in a decision, so an unreadable file
// is not one object type failing to answer — it is every decision for the slot —
// and boot is where the operator is present to fix it.
//
// The error passes through verbatim. csvprovider's rejections are already coded
// and already say which file, which line, and which column is wrong;
// re-stamping to add the subject would trade APERTURE_METADATA_INVALID's fixups
// (or APERTURE_ATTRIBUTE_PROVIDER_INVALID's) for a generic code, or, at the same
// code, bury the real one a layer deeper. The slot is one grep away in the seed
// file; the offending cell is not.
//
// Nothing here re-derives what the block settled: id_column: is not consulted,
// because it names a result column of a kind: sql get_all — a CSV's id column is
// spelled "id", the same spelling an object CSV uses.
func csvAttributeSource(src attributeSource) (provider.AttributeProvider, error) {
	impl, err := csvprovider.NewAttributes(src.Path)
	if err != nil {
		if aerr.CodeOf(err) != "" {
			return nil, err
		}
		return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
			"seed: csv attribute provider for subject %q cannot be loaded", src.Slot)
	}
	return impl, nil
}

// sqlAttributeSource builds the database-backed attribute provider for a kind:
// sql entry, reading through the shared pool src.Pool already holds.
//
// The YAML path and the Go path (sqlprovider.NewAttributes(querier, cfg)) meet
// exactly here, and neither is a special case of the other: resolveAttributeSource
// has already turned connection: into a live pool and a statement budget, and
// this arm then calls the SAME constructor a host calls, with the same
// AttributeConfig a host fills in by hand. So every rule the constructor enforces
// — the slot, the required get_one, the id column, the timeout — is enforced
// identically whichever way the slot was declared, and the two contract
// differences from an object provider (a bare id bound verbatim, a bare id
// selected) live in ONE implementation rather than in a YAML-only copy of it.
//
// Nothing is dialled here. sql.Open is lazy, and pinging would make building a
// registry a network operation; a wrong host or password surfaces on the first
// decision touching the slot, as APERTURE_SQL_PROVIDER_QUERY. What IS eager is
// everything Aperture can decide on its own, and all of it has already happened
// by the time this arm runs.
//
// The QUERY TIMEOUT is the CONNECTION's, carried on the resolved source rather
// than re-read from the entry: it is a property of the database being read, and
// an object type and an attribute slot over one pool that disagreed about how
// long the server may take would be two opinions about the same server.
//
// The error passes through when it is already coded — sqlprovider's refusals
// carry APERTURE_ATTRIBUTE_SLOT_UNKNOWN or APERTURE_CONFIG_INVALID and their
// registry fixups with them, and Wrap RE-STAMPS, so wrapping would trade a
// specific remedy for a generic one.
func sqlAttributeSource(src attributeSource) (provider.AttributeProvider, error) {
	impl, err := sqlprovider.NewAttributes(src.Pool, sqlprovider.AttributeConfig{
		Slot:       src.Slot,
		FetchQuery: src.GetOne,
		// Empty when the entry declared no get_all:, which is legal here and
		// yields a fetch-only slot — see AttributeProvider.GetAll.
		ListQuery: src.GetAll,
		IDColumn:  src.IDColumn,
		Timeout:   src.QueryTimeout,
	})
	if err != nil {
		if aerr.CodeOf(err) != "" {
			return nil, err
		}
		return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
			"seed: sql attribute provider for subject %q cannot be built", src.Slot)
	}
	return impl, nil
}

// unwiredAttributeKind is the refusal for a declared kind whose loader this
// build does not carry. It names the slot and the kind, never the statements or
// the path.
func unwiredAttributeKind(src attributeSource) error {
	return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
		fmt.Sprintf("seed: attribute provider for subject %q declares kind %q, which this build carries no loader for", src.Slot, src.Kind),
		map[string]any{"subject": src.Slot.String(), "kind": src.Kind, "kinds": attributeKinds()})
}

// BuildAttributeRegistryWithConnections is BuildAttributeRegistry given the
// document's ALREADY-OPEN database pools, and it is the form a host with a
// kind: sql attribute provider calls:
//
//	reg, conns, err := doc.BuildRegistryWithConnections(dir)
//	if err != nil { return err }
//	defer conns.Close()
//	attrs, err := doc.BuildAttributeRegistryWithConnections(dir, conns)
//
// It TAKES a *Connections rather than opening one, which is the whole point:
// connections: is the document's single pool set, and the object registry and
// the attribute registry read through the SAME pools. A second entry point that
// opened its own would mean two pools per named connection in every deployment
// that used both sections — precisely the duplication the connections: block
// exists to prevent, arrived at by wiring rather than by a typo.
//
// conns may be nil, and that is what BuildAttributeRegistry passes: a document
// whose attribute providers are all kind: csv needs no pool at all. A kind: sql
// entry then fails with APERTURE_SQL_PROVIDER_CONNECTION naming this method,
// rather than lazily on the first decision that needed the database.
//
// The registry it returns combines BOTH attribute sources, exactly as
// BuildRegistry combines providers: and objects: — see BuildAttributeRegistry
// for the precedence rule and the failure modes.
func (d *Document) BuildAttributeRegistryWithConnections(baseDir string, conns *Connections) (*provider.AttributeRegistry, error) {
	return d.buildAttributeRegistry(baseDir, conns, openAttributeSource)
}

// buildAttributeRegistry is the one implementation behind both public entry
// points, with the source opener injected so the wiring above the loader seam is
// provable without a CSV file or a database — the same reasoning that makes
// WithConnectionOpener exist.
func (d *Document) buildAttributeRegistry(baseDir string, conns *Connections, open attributeSourceOpener) (*provider.AttributeRegistry, error) {
	// attribute_providers: is resolved FIRST, before any inline entry is grouped,
	// so a document that declares both fails on the external declaration it
	// cannot satisfy rather than on an inline entry whose own validity says nothing
	// about it. That ordering mirrors BuildRegistryWithConnections registering
	// providers: before objects:.
	//
	// The precedence between the two sections is no longer this ordering, though:
	// it is the LAYER each is registered in, which is a property of the registry
	// rather than of a loop (see the registration calls below).
	sources, err := d.attributeSources(baseDir, conns)
	if err != nil {
		return nil, err
	}
	groups, err := d.groupAttributes()
	if err != nil {
		return nil, err
	}
	reg := provider.NewAttributeRegistry()
	// Slots are filled in provider.AttributeSlots() order, not file order, so a
	// document with two bad slots always fails on the same one and a build is
	// reproducible.
	//
	// A slot declared in BOTH sections gets BOTH, in the two layers the registry
	// keeps: the attribute_providers: entry is the SHARED layer and the inline
	// attributes: block is the LOCAL one, so the external source wins every key
	// both serve and the inline bags contribute the keys it does not. That is a
	// reversal of the rule this file used to state — the inline bags were discarded
	// entirely — and the argument for it is provider.AttributeLayer's: a shared
	// directory a deployment administers and a block in one instance's file are not
	// two candidates for one slot, they are two layers of it, and refusing the
	// second meant an instance could not add a field the directory does not carry
	// without abandoning the directory.
	//
	// Which section is which layer is not a choice either. attribute_providers:
	// names a source every instance of the deployment reads (a database row
	// projected back into this section, or a directory), and attributes: is data
	// written into one instance's file; if the file could override a key the
	// directory serves, one machine would silently answer a deployment-wide rule
	// differently.
	for _, slot := range provider.AttributeSlots() {
		if src, ok := sources[slot]; ok {
			impl, err := open(src)
			if err != nil {
				return nil, err
			}
			if impl == nil {
				return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
					fmt.Sprintf("seed: the attribute source opener returned no provider for subject %q", slot),
					map[string]any{"subject": slot.String(), "kind": src.Kind})
			}
			if err := reg.Register(slot, impl, src.cacheOpts...); err != nil {
				return nil, err
			}
		}
		records, ok := groups[slot]
		if !ok {
			continue
		}
		// NewStaticAttributes is the backstop for everything about a KEY that the
		// inline loader deliberately does not restate: the empty key, the account
		// wildcard, and a duplicate within the record set. Its errors pass through
		// verbatim, keeping their codes and their registry fixups.
		impl, err := provider.NewStaticAttributes(records)
		if err != nil {
			return nil, err
		}
		// TTL 0: inline data is fixed for the life of the process, so a freshness
		// window would only buy re-reads of a value that cannot have changed. It is
		// the LOCAL layer's window and nothing else's — the shared layer keeps the
		// ttl: its own entry declared, which is why the two caches are separate.
		if err := reg.RegisterLocal(slot, impl, provider.WithTTL(0)); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// attributeSources validates every attribute_providers: entry and resolves it,
// keyed by slot. Entries are walked in DECLARATION order so the first offending
// line in the file is the one reported.
//
// Everything Aperture can check without reading a file or dialling a database is
// checked here, eagerly, because a source that only fails under a decision fails
// as a denial: the subject must name a slot, the slot must be claimed at most
// once, the kind must be one this build knows, a csv entry must carry a path, a
// sql entry must carry a connection: naming a DECLARED connection and a get_one,
// and a ttl: must parse.
func (d *Document) attributeSources(baseDir string, conns *Connections) (map[provider.AttributeSlot]attributeSource, error) {
	if len(d.AttributeProviders) == 0 {
		return nil, nil
	}
	out := make(map[provider.AttributeSlot]attributeSource, len(d.AttributeProviders))
	for _, ap := range d.AttributeProviders {
		src, err := resolveAttributeSource(ap, baseDir, conns)
		if err != nil {
			return nil, err
		}
		if _, dup := out[src.Slot]; dup {
			// A slot is claimed by at most one source. A duplicate is refused
			// rather than resolved by precedence because the two declarations
			// would be two different directories for one party, and "last writer
			// wins" over that is how one deployment's user table quietly shadows
			// another's — a failure that surfaces as attributes that are merely
			// wrong rather than absent.
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				fmt.Sprintf("seed: attribute provider subject %q is declared more than once", src.Slot),
				map[string]any{"subject": src.Slot.String()})
		}
		out[src.Slot] = src
	}
	return out, nil
}

// resolveAttributeSource validates ONE entry and resolves it into the source an
// opener can build from.
func resolveAttributeSource(ap AttributeProvider, baseDir string, conns *Connections) (attributeSource, error) {
	if strings.TrimSpace(ap.Subject) == "" {
		return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: attribute provider is missing subject",
			map[string]any{"kind": ap.Kind, "subjects": slotNames()})
	}
	// ParseAttributeSlot is the one crossing point between a bare string and a
	// slot this registry serves, and it stays the authority on the closed set:
	// its CODE is what this refusal reports, so the entry keeps
	// APERTURE_ATTRIBUTE_SLOT_UNKNOWN's fixups rather than
	// APERTURE_CONFIG_INVALID's generic ones. Only the MESSAGE is the seed's,
	// because the offending subject and the section it was written in are the one
	// thing the provider package cannot know.
	slot, err := provider.ParseAttributeSlot(ap.Subject)
	if err != nil {
		return attributeSource{}, aerr.WithContext(aerr.CodeOf(err),
			fmt.Sprintf("seed: attribute provider declares an unknown subject %q; the subjects are %s",
				ap.Subject, strings.Join(slotNames(), ", ")),
			map[string]any{"subject": ap.Subject, "subjects": slotNames()})
	}

	src := attributeSource{
		Slot:     slot,
		Kind:     strings.TrimSpace(ap.Kind),
		GetOne:   ap.GetOne,
		GetAll:   ap.GetAll,
		IDColumn: ap.IDColumn,
	}
	if ap.TTL != "" {
		ttl, err := time.ParseDuration(ap.TTL)
		if err != nil {
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: attribute provider has an invalid ttl",
				map[string]any{"subject": slot.String(), "ttl": ap.TTL})
		}
		src.cacheOpts = append(src.cacheOpts, provider.WithTTL(ttl))
	}
	if ap.MaxSize != 0 {
		src.cacheOpts = append(src.cacheOpts, provider.WithMaxSize(ap.MaxSize))
	}

	switch src.Kind {
	case attributeKindCSV:
		if strings.TrimSpace(ap.Path) == "" {
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: csv attribute provider is missing path",
				map[string]any{"subject": slot.String()})
		}
		path := ap.Path
		if !filepath.IsAbs(path) && baseDir != "" {
			path = filepath.Join(baseDir, path)
		}
		src.Path = path
	case attributeKindSQL:
		// get_one is required and get_all is not — see AttributeProvider.GetAll
		// for why the asymmetry with providers: is deliberate rather than an
		// oversight. Without get_one there is no fetch at all, and a slot that
		// cannot answer the decision path is not a source.
		if strings.TrimSpace(ap.GetOne) == "" {
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: sql attribute provider is missing get_one; it is the statement the decision path fetches one subject's bag through, and it binds the BARE subject id to a single placeholder",
				map[string]any{"subject": slot.String()})
		}
		name := strings.TrimSpace(ap.Connection)
		if name == "" {
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: sql attribute provider is missing connection; name an entry of the document's connections: block",
				map[string]any{"subject": slot.String()})
		}
		src.Connection = name
		if conns == nil {
			// The document's pools were never handed to this build. Refused here,
			// at build, rather than answered with a lazily-opened private pool:
			// connections: is one shared pool set, and a second one opened behind
			// the caller's back would outlive every handle it could be closed by.
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: sql attribute provider for subject %q references connection %q, but this build was given no open connections; call BuildAttributeRegistryWithConnections with the *Connections that BuildRegistryWithConnections returned, so both registries read through ONE pool per name", slot, name),
				map[string]any{"subject": slot.String(), "connection": name})
		}
		pool, ok := conns.get(name)
		if !ok {
			// A hard error, and deliberately not a lazily-opened pool: the pool is
			// SHARED, so a typo'd name would otherwise open a second pool to the
			// same server (double the connections, half of them invisible) or none
			// at all (a slot that answers nothing).
			return attributeSource{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: sql attribute provider for subject %q references connection %q, which the connections: block does not declare", slot, name),
				map[string]any{
					"subject":    slot.String(),
					"connection": name,
					"declared":   conns.Names(),
				})
		}
		src.Pool = pool
		// The statement budget belongs to the CONNECTION, not to the entry: it is
		// a property of the database being read, and an object type and an
		// attribute slot over one pool that disagreed about how long the server
		// may take would be two opinions about the same server.
		src.QueryTimeout = conns.queryTimeout(name)
	case "":
		return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("seed: attribute provider for subject %q is missing kind; declare one of %s",
				slot, strings.Join(attributeKinds(), ", ")),
			map[string]any{"subject": slot.String(), "kinds": attributeKinds()})
	default:
		return attributeSource{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("seed: attribute provider for subject %q declares an unknown kind %q; the kinds are %s",
				slot, src.Kind, strings.Join(attributeKinds(), ", ")),
			map[string]any{"subject": slot.String(), "kind": src.Kind, "kinds": attributeKinds()})
	}
	return src, nil
}

// AttributeSourceInline is the source label for a slot filled from the
// attributes: block rather than from an attribute_providers: entry. It is not a
// kind — inline is not selectable with kind: — which is exactly why it needs a
// name of its own: an operator reading "inline" must not go looking for a loader
// that does not exist.
const AttributeSourceInline = "inline"

// AttributeSlotSources reports, per slot, WHERE this document says that slot's
// bags come from: the declared kind: of its attribute_providers: entry ("csv",
// "sql"), or AttributeSourceInline when only the attributes: block fills it. A
// slot the document does not fill at all is absent from the map.
//
// It exists so a surface that DISPLAYS the wiring — `aperture attributes slots`
// — does not have to re-derive the precedence rule. The rule is one rule and it
// is defined in this file: an attribute_providers: entry is the SHARED layer and
// WINS every key both sections serve, with the inline bags layered under it (see
// AttributeCollisions). A CLI that walked the two sections itself would be a
// second implementation of it, and the two would eventually disagree about which
// source an operator is actually running — the one question the listing exists to
// answer.
//
// A slot filled by both therefore reports the external kind:, because that is the
// source a contested key is answered from. That it is the WINNER rather than the
// only source is what AttributeCollisions reports, and a listing that needs to
// name both asks provider.AttributeRegistry.Layers for the shape it actually
// built.
//
// It reports WIRING, not contents: slot names and kinds, never a key and never a
// bag. What a slot's cache is actually configured with is a property of the
// built registry, not of the document, and is read from
// provider.AttributeRegistry.CacheConfigFor.
//
// A malformed subject: is skipped rather than reported, exactly as
// AttributeCollisions skips it: an unknown subject fails the BUILD, and a report
// asked of a document that cannot build should still answer for the slots that
// can.
func (d *Document) AttributeSlotSources() map[string]string {
	if d == nil {
		return nil
	}
	out := make(map[string]string, len(provider.AttributeSlots()))
	// Inline first, external second: the external entry OVERWRITES, which is the
	// precedence rule spelled as an assignment order rather than as a condition.
	for _, a := range d.Attributes {
		slot, err := provider.ParseAttributeSlot(strings.TrimSpace(a.Subject))
		if err != nil {
			continue
		}
		out[slot.String()] = AttributeSourceInline
	}
	for _, ap := range d.AttributeProviders {
		slot, err := provider.ParseAttributeSlot(strings.TrimSpace(ap.Subject))
		if err != nil {
			continue
		}
		out[slot.String()] = strings.TrimSpace(ap.Kind)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AttributeCollisions reports the SLOTS declared in BOTH the
// attribute_providers: and attributes: sections, in provider.AttributeSlots()
// order.
//
// It is Document.ProviderCollisions for the attribute seam, and the two no longer
// report the same rule. The OBJECT rule is still a discard: a providers: entry
// wins a type and the inline objects: entries for it are dropped. The ATTRIBUTE
// rule is a LAYERING: the external attribute_providers: entry is the slot's shared
// layer, the inline attributes: block is its local layer, and a fetch reads their
// merge with the shared layer winning every key both serve (provider.AttributeLayer
// is the full account).
//
// So an inline id the external source lacks IS resolvable — that is the point of
// the local layer — while an inline value for a key the external source does serve
// is not read, ever, on any instance.
//
// Field-level merging with a CONFIGURABLE or order-dependent winner is what remains
// impossible to debug, and it is still refused: a rule reading a department one
// machine's file silently overrode is a support ticket nobody can reproduce. What
// makes the layering safe is that the winner is fixed and is the deployment-wide
// source, so a contested key reads the same on every instance.
//
// The layering is not silent either. seed has no logging path of its own, so it
// reports the fact here and the caller surfaces it — internal/cli prints a
// warning naming the slots. Only SLOT NAMES are reported, never keys, so the
// warning cannot leak a directory's contents.
func (d *Document) AttributeCollisions() []string {
	if d == nil || len(d.Attributes) == 0 || len(d.AttributeProviders) == 0 {
		return nil
	}
	filed := make(map[provider.AttributeSlot]bool, len(d.AttributeProviders))
	for _, ap := range d.AttributeProviders {
		slot, err := provider.ParseAttributeSlot(strings.TrimSpace(ap.Subject))
		if err != nil {
			// An unknown subject fails the BUILD; here it is simply not a
			// collision, so a report asked of a malformed document still answers.
			continue
		}
		filed[slot] = true
	}
	inline := make(map[provider.AttributeSlot]bool, len(d.Attributes))
	for _, a := range d.Attributes {
		slot, err := provider.ParseAttributeSlot(strings.TrimSpace(a.Subject))
		if err != nil {
			continue
		}
		inline[slot] = true
	}
	var collided []string
	for _, slot := range provider.AttributeSlots() {
		if filed[slot] && inline[slot] {
			collided = append(collided, slot.String())
		}
	}
	return collided
}
