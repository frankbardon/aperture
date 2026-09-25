package cli

import (
	"fmt"
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
// per-instance fact — see connectionRoute.
//
// objects: and attributes: are the local document's own, untouched: both carry
// DATA rather than a pointer to data, and they belong to the instance whose seed
// file lists them.

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

// wiringDocument projects a non-empty shared wiring set into the four wiring
// sections of a seed.Document, carrying local's two DATA sections (objects: and
// attributes:) through unchanged.
//
// local is this instance's own seed document — possibly empty — and is read for
// exactly two things: the data sections, and the ROUTE for each connection name
// the shared manifest declares. Its own four WIRING sections are not read here:
// with shared wiring present the database is what the registries are built from.
// (Letting the local file ADD an object type or a slot the database never
// declared, and refusing a collision, is a separate rule with its own story; this
// function is where that layering will compose, not where it is decided.)
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

	if len(set.Providers) > 0 {
		doc.Providers = make([]seed.Provider, 0, len(set.Providers))
		for _, p := range set.Providers {
			doc.Providers = append(doc.Providers, wiringSeedProvider(p))
		}
	}
	doc.FieldTypes = wiringSeedFieldTypes(set.FieldTypes)
	if len(set.AttributeProviders) > 0 {
		doc.AttributeProviders = make([]seed.AttributeProvider, 0, len(set.AttributeProviders))
		for _, ap := range set.AttributeProviders {
			doc.AttributeProviders = append(doc.AttributeProviders, wiringSeedAttributeProvider(ap))
		}
	}
	return doc, nil
}

// wiringSeedProvider converts one stored provider entry into its seed form.
//
// Path is left empty deliberately, and there is nothing to leave out: kind: csv
// cannot be SHARED wiring (a filesystem path is machine-local), so the shared
// tables have no path column and a stored entry can never want one. A row that
// nevertheless spells kind: csv — hand-written, or written by an older build — is
// refused by seed's own buildObjectProvider for the missing path, which is the
// right refusal for the wrong reason; the reason is stated at the push, where the
// mistake is made.
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
// A name with no route at all is not refused here. It resolves to a dsn_env:
// naming a variable, and seed.resolveConnection refuses an unset one by name with
// APERTURE_SQL_PROVIDER_CONNECTION — the same refusal, with the same remedy, that
// a local file's own connection raises.
func connectionRoutes(manifest []model.WiringConnection, local *seed.Document) (map[string]seed.Connection, error) {
	if len(manifest) == 0 {
		return nil, nil
	}
	out := make(map[string]seed.Connection, len(manifest))
	// Only the CONVENTIONAL names are checked for a collision. A local document
	// may legitimately point two connections at one dsn_env: — two logical names
	// over one server is two pools, which the seed path has always allowed — but
	// two DB-declared names that DERIVE the same variable is an ambiguity this
	// convention created, and resolving it either way would route one of them
	// somewhere nobody asked for.
	derived := make(map[string][]string, len(manifest))
	for _, c := range manifest {
		if declared, ok := localConnection(local, c.Name); ok {
			out[c.Name] = declared
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
	return out, nil
}

// localConnection returns this instance's declared route for name, if its seed
// document declares one.
//
// The entry is returned verbatim, DSNLiteral included. A literal dsn: is refused
// by seed.Parse before a document is usable for anything, so a parsed file cannot
// carry one, and re-deriving that refusal here would be the same rule in two
// places — the one thing wiring_project.go's own doc comment warns against.
func localConnection(local *seed.Document, name string) (seed.Connection, bool) {
	if local == nil || len(local.Connections) == 0 {
		return seed.Connection{}, false
	}
	c, ok := local.Connections[name]
	return c, ok
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
