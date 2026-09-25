package seed

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/frankbardon/aperture/csvprovider"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// Provider declares an object-metadata provider for one object-type, so a host
// can link object instances to real data through YAML instead of Go wiring.
// BuildRegistry turns the document's providers into a live *provider.Registry.
//
// This section is runtime WIRING, not model state: Apply never writes it to
// storage (a provider produces no model rows), and because the model is exported
// by reading storage back, an export does not reproduce it.
//
// It is one of the four SHARED wiring sections, and the seed file is therefore
// not its only home. `aperture wiring push` writes every entry here to
// apt_wiring_providers (and its References map to apt_wiring_provider_references),
// `aperture wiring pull` reads them back as this same section, and an instance
// booting against a store that holds rows builds its registry from them. Where the
// store holds rows the DATABASE is authoritative and a local document may only ADD
// a type it never declared; where the store holds none, this section is the whole
// answer, exactly as it always was.
//
// Two things about an entry are never shared, because the tables have no column
// for them and never will: Path — a filesystem path is machine-local, which is why
// `kind: csv` is refused at the push and stays perfectly legal here — and anything
// about the Connection beyond its NAME. See internal/cli/wiring_project.go for
// every rule a push refuses on, and skills/shared-wiring.md for the contract.
type Provider struct {
	// ObjectType is the type whose instances this provider serves (e.g. "brand").
	// An object's identity terminal-segment type must equal it, and each type may
	// be declared at most once.
	ObjectType string `yaml:"object_type" json:"object_type"`
	// Kind selects the provider implementation: "csv" (a file, resolved from
	// Path) or "sql" (two statements run against a named Connection).
	Kind string `yaml:"kind" json:"kind"`
	// Path is the data source for file-backed kinds (csv): the CSV file, resolved
	// relative to the seed file's directory when it is not absolute.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Connection names the entry in the document's connections: block this
	// provider reads through. Required for kind: sql, and a name with no
	// matching connections: entry is a hard error at build — the pool is shared,
	// so a typo here would otherwise open a second pool to the same server, or
	// none at all.
	Connection string `yaml:"connection,omitempty" json:"connection,omitempty"`
	// GetOne is the "get one" statement for kind: sql, taking exactly one
	// placeholder to which the identity's TERMINAL SEGMENT VALUE is bound
	// ("brand:42" and "account:acme/brand:42" both bind "42"):
	//
	//	get_one: SELECT tier, seats FROM brands WHERE id = $1
	//
	// Every column it returns becomes a metadata field keyed by the column name.
	// It becomes sqlprovider.Config.FetchQuery. Required for kind: sql.
	GetOne string `yaml:"get_one,omitempty" json:"get_one,omitempty"`
	// GetAll is the "get all" statement for kind: sql, taking NO parameters and
	// selecting each row's FULL identity as the IDColumn:
	//
	//	get_all: SELECT 'brand:' || b.id AS id, b.tier, b.seats FROM brands b
	//
	// It becomes sqlprovider.Config.ListQuery, and it is required alongside
	// get_one rather than optional: a provider that could be fetched from but not
	// enumerated would answer List with an error, and an errored enumeration
	// reads as "no access" one layer up.
	GetAll string `yaml:"get_all,omitempty" json:"get_all,omitempty"`
	// IDColumn names the get_all result column holding each row's identity.
	// Empty means sqlprovider.DefaultIDColumn ("id"). The column is removed from
	// the row before the rest becomes metadata: it is the identity, not a field.
	IDColumn string `yaml:"id_column,omitempty" json:"id_column,omitempty"`
	// TTL is the cache freshness window as a Go duration ("30s", "5m"). "0" (or an
	// empty string, which adopts the registry default of 30s) — set "0" for a
	// static file you reload explicitly so cached metadata never expires.
	TTL string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
	// MaxSize caps cached entries for this type; 0 uses the registry default.
	MaxSize int `yaml:"max_size,omitempty" json:"max_size,omitempty"`
	// References declares that a metadata field this provider serves holds
	// identities of another object-type — an application-level foreign key, with
	// no database constraint behind it. It maps FIELD NAME to TARGET OBJECT-TYPE:
	//
	//	- object_type: dataset
	//	  kind: sql
	//	  connection: main
	//	  get_one: SELECT to_jsonb(d.brand_ids) AS current_brands FROM datasets d WHERE d.id = $1
	//	  get_all: SELECT 'dataset:' || d.id AS id, to_jsonb(d.brand_ids) AS current_brands FROM datasets d
	//	  references:
	//	    current_brands: brand
	//
	// The field's VALUE is full canonical identities ("brand:1", or
	// "account:acme/brand:1"), composed by the developer where the data is
	// loaded — a scalar for a single reference, a list for many. That is what
	// lets an enumeration filter on one through the ordinary Filter.Fields
	// contract with no new matching code.
	//
	// It is declared on the HOLDING side only: the type whose provider actually
	// returns the field. There is no inbound spelling on brand, because brand has
	// no column listing its datasets — and a second referencing field
	// (archived_brands) would make an unnamed reverse edge ambiguous anyway.
	//
	// The block is a CLOSED SET OF ONE DESCRIPTOR KIND: a field name maps to a
	// target object-type and to nothing else. In particular, do NOT add a type:
	// descriptor here — the loader is the single typing mechanism (a CSV column
	// suffix, a cast in the developer's SQL), and a second place to declare a
	// type is a second place for the two declarations to disagree.
	//
	// The target must be a type this document's registry serves; an unknown one
	// is APERTURE_PROVIDER_REFERENCE_INVALID at build, naming the field and the
	// target. A field name that matches nothing is NOT an error: metadata fields
	// are discovered at fetch, not declared. Like the rest of this struct it is
	// runtime wiring: Apply never writes it and an export never reproduces it, and
	// like the rest of this struct it IS shared — a push flattens the map into
	// apt_wiring_provider_references and a pull reads it back under this key.
	References map[string]string `yaml:"references,omitempty" json:"references,omitempty"`
}

// ParseFile reads path and parses it into a Document, inferring the format from
// the file extension (.json → JSON, otherwise YAML). It is Parse plus file IO,
// without applying anything to storage; callers that also need the model loaded
// into a store use LoadFile.
func ParseFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_CONFIG_INVALID, "seed: reading file failed", err)
	}
	return Parse(data, formatFor(path))
}

// BuildOption tunes how BuildRegistry resolves the document's two wiring
// sections. It is Go-level wiring on purpose: the seed FILE stays a plain
// declaration of what exists, and the decision to make an ambiguous one fatal is
// taken by the host that builds the registry, where a reviewer sees it.
type BuildOption func(*buildConfig)

// buildConfig is the resolved set of BuildOptions.
type buildConfig struct {
	// strictProviderCollision refuses to build a document whose two wiring
	// sections claim the same object type, instead of applying the default
	// type-level precedence rule. See StrictProviderCollision.
	strictProviderCollision bool
	// openConnection replaces the default pgx pool opener. Nil means openPool.
	// See WithConnectionOpener.
	openConnection ConnectionOpener
}

// WithConnectionOpener replaces how BuildRegistryWithConnections turns a
// resolved connections: entry into a live pool. The default opens a real
// database/sql pool over the pgx driver.
//
// It is the seam that keeps the wiring testable: a whole registry can be built
// from a YAML fixture, statements and all, with no database present — which is
// also how CI runs, since the pipeline has no service containers. A host with
// its own instrumented, retrying, or read-replica handle uses the same seam.
//
// The opener is called once per DECLARED connection, never once per provider
// entry: sharing the pool is the point.
func WithConnectionOpener(open ConnectionOpener) BuildOption {
	return func(c *buildConfig) { c.openConnection = open }
}

// StrictProviderCollision refuses to build a document that declares an object
// type in BOTH the providers: and objects: sections, failing with
// APERTURE_CONFIG_INVALID naming the colliding type(s), instead of applying the
// default precedence rule.
//
// The rule it replaces is the DEFAULT, and it is TOTAL and TYPE-LEVEL: the
// file-backed providers: entry wins, and EVERY inline objects: entry for that
// type is discarded. There is no object-level merge, no field-level merge, and
// no fallback — an inline id the file happens to lack is simply not resolvable,
// exactly as if the entry had never been written. Field-level merging is the
// most useful-sounding behaviour and the most impossible to debug: a rule
// reading a field the CSV silently did not override is a support ticket nobody
// can reproduce. Predictability wins.
//
// The discard is the default because adding a CSV for a type that also has
// inline entries is a normal migration step, not a fault: a seed that booted
// yesterday must not refuse to boot today because someone added a providers:
// row. This option is for the host that would rather read the overlap as an
// authoring mistake — a checked-in seed nobody is mid-migration on — and wants
// the build to stop instead.
//
// The default is not silent either: Document.ProviderCollisions reports exactly
// which types the build discards, so a host with a logger can say so at load.
//
// Every inline entry is validated either way — a malformed declaration fails the
// load whether or not its type is ultimately discarded.
func StrictProviderCollision() BuildOption {
	return func(c *buildConfig) { c.strictProviderCollision = true }
}

// BuildRegistry constructs a live *provider.Registry from the document's three
// wiring sections: providers (a declared implementation per object-type),
// objects (metadata declared inline, served by an in-memory provider.Static per
// object-type), and field_types (the declared type of selected inline metadata
// fields, currently dates). Relative file paths are resolved against baseDir
// (typically the seed file's directory; pass "" to resolve against the process
// CWD). It always returns a usable registry — empty when no section is declared
// — so a caller can wire it unconditionally.
//
// A malformed providers entry (missing object_type, unknown kind, missing path,
// unparseable ttl, or a duplicate object_type), a malformed objects entry
// (missing id, a duplicate id, metadata that is not a mapping, or a value the
// shared value model rejects), and a malformed field_types entry (missing
// object_type, a duplicated object_type, an empty field name, or an unknown
// declared type) are all APERTURE_CONFIG_INVALID; a malformed id passes through
// as APERTURE_IDENTITY_INVALID, and a type claimed twice by two providers
// entries is APERTURE_PROVIDER_INVALID.
//
// A type claimed by BOTH the providers: and objects: sections is not an error:
// the providers: entry wins and every inline entry for that type is discarded
// entirely. The discarded types are reported by Document.ProviderCollisions, and
// StrictProviderCollision turns the collision back into an
// APERTURE_CONFIG_INVALID for hosts that want one.
//
// field_types: applies to the objects: section ONLY. A providers: entry carries
// its own typing (a CSV header's :date / :datetime column suffix), so a declared
// field type is never silently imposed on rows a provider loaded — one type
// declaration in two places could disagree with itself, which is the failure
// this document's derive-the-object-type-from-the-identity rule already avoids
// elsewhere. Inline entries are still fully validated against the declaration
// when a providers: entry wins their type; the canonicalised values are then
// discarded along with the rest of those entries.
//
// The field_types: section is validated FIRST, before anything is registered, so
// a typo'd declaration fails the build even in a document that declares no
// objects at all.
//
// A providers: entry's references: block is applied LAST, once every type is
// registered, so a reference may name a target declared later in the file. A
// reference whose target no provider serves is
// APERTURE_PROVIDER_REFERENCE_INVALID naming the field and the target; a
// reference on a field name no object happens to carry is not an error at all,
// because metadata fields are discovered at fetch rather than declared.
//
// No section touches storage: Apply writes no row for any of them, and an export
// reproduces none.
// A document declaring connections: owns database pools, which have a lifetime
// this signature cannot hand back — so it refuses one, naming
// BuildRegistryWithConnections, rather than opening pools nothing can close.
// That is the whole difference between the two forms.
func (d *Document) BuildRegistry(baseDir string, opts ...BuildOption) (*provider.Registry, error) {
	if len(d.Connections) > 0 {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			"seed: this document declares connections:, whose pools have a lifetime BuildRegistry cannot return; call BuildRegistryWithConnections and close the Connections it gives you at shutdown",
			map[string]any{"connections": len(d.Connections)})
	}
	reg, conns, err := d.BuildRegistryWithConnections(baseDir, opts...)
	if err != nil {
		return nil, err
	}
	// Unreachable in practice — no connections: means no pools — but closing an
	// empty set costs nothing and keeps the guarantee local.
	_ = conns.Close()
	return reg, nil
}

// BuildRegistryWithConnections is BuildRegistry plus the resource half: it also
// opens ONE database pool per entry in the document's connections: block and
// returns them as a *Connections the caller owns and must Close at shutdown.
//
//	reg, conns, err := doc.BuildRegistryWithConnections(dir)
//	if err != nil { return err }
//	defer conns.Close()
//
// Every kind: sql provider entry reads through the pool its connection: names,
// so three entries over one database are three providers and ONE pool. The
// returned set is always non-nil on success and is empty — Close a no-op — for a
// document declaring no connections, so a caller can defer unconditionally and
// need not care which kinds the seed happened to use.
//
// Everything Aperture can check without dialling anything is checked eagerly,
// because a connection that only fails under a decision fails as a denial: the
// dsn_env variable must be set and non-empty, the durations must parse, and a
// provider entry's connection: must name a declared connection. The pool itself
// is not dialled — sql.Open is lazy — so a build succeeds while the host's
// database is still starting.
//
// A connection or a provider entry that fails takes the whole build with it, and
// every pool opened so far is closed on the way out: a failed build strands
// nothing.
func (d *Document) BuildRegistryWithConnections(baseDir string, opts ...BuildOption) (*provider.Registry, *Connections, error) {
	var cfg buildConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	declared, err := d.fieldTypeIndex()
	if err != nil {
		return nil, nil, err
	}
	conns, err := d.openConnections(cfg.openConnection)
	if err != nil {
		return nil, nil, err
	}
	reg := provider.NewRegistry()
	// providers: is registered FIRST so the objects: section can see which types
	// are already file-backed. That ordering IS the precedence rule.
	for _, p := range d.Providers {
		if err := registerProvider(reg, p, baseDir, conns); err != nil {
			_ = conns.Close()
			return nil, nil, err
		}
	}
	if err := d.registerObjects(reg, cfg, declared); err != nil {
		_ = conns.Close()
		return nil, nil, err
	}
	// References are declared LAST, in their own pass, because a reference names
	// a target type that may be declared further down the file (or served by the
	// objects: section) — and requiring the target to already exist would make a
	// legal document depend on the order its entries happen to be written in.
	if err := d.declareReferences(reg); err != nil {
		_ = conns.Close()
		return nil, nil, err
	}
	return reg, conns, nil
}

// declareReferences applies every providers: entry's references: block to the
// built registry, after every type has been registered.
//
// Fields are declared in sorted order so a document with two bad references
// always fails on the same one, and the build is reproducible.
func (d *Document) declareReferences(reg *provider.Registry) error {
	for _, p := range d.Providers {
		if len(p.References) == 0 {
			continue
		}
		fields := make([]string, 0, len(p.References))
		for field := range p.References {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			if err := reg.DeclareReference(p.ObjectType, field, p.References[field]); err != nil {
				return err
			}
		}
	}
	return nil
}

// registerProvider builds one declared provider and registers it under its
// object-type with the cache options its TTL/MaxSize imply.
func registerProvider(reg *provider.Registry, p Provider, baseDir string, conns *Connections) error {
	if p.ObjectType == "" {
		return aerr.New(aerr.APERTURE_CONFIG_INVALID, "seed: provider is missing object_type")
	}
	var opts []provider.CacheOption
	if p.TTL != "" {
		ttl, err := time.ParseDuration(p.TTL)
		if err != nil {
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: provider has an invalid ttl",
				map[string]any{"object_type": p.ObjectType, "ttl": p.TTL})
		}
		opts = append(opts, provider.WithTTL(ttl))
	}
	if p.MaxSize != 0 {
		opts = append(opts, provider.WithMaxSize(p.MaxSize))
	}
	impl, err := buildObjectProvider(p, baseDir, conns)
	if err != nil {
		return err
	}
	// Register surfaces APERTURE_PROVIDER_INVALID for a duplicate object_type.
	return reg.Register(p.ObjectType, impl, opts...)
}

// buildObjectProvider constructs the ObjectProvider for a declared kind.
func buildObjectProvider(p Provider, baseDir string, conns *Connections) (provider.ObjectProvider, error) {
	switch p.Kind {
	case "sql":
		return buildSQLProvider(p, conns)
	case "csv":
		if p.Path == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: csv provider is missing path",
				map[string]any{"object_type": p.ObjectType})
		}
		path := p.Path
		if !filepath.IsAbs(path) && baseDir != "" {
			path = filepath.Join(baseDir, path)
		}
		return csvprovider.New(path), nil
	default:
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: unknown provider kind",
			map[string]any{"object_type": p.ObjectType, "kind": p.Kind})
	}
}
