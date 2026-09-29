package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// Object declares one host object's metadata INLINE in the seed document, so a
// small or fixed object set needs no separate CSV file beside the seed:
//
//	objects:
//	  - id: account:acme/brand:1
//	    metadata:
//	      tier: gold
//	      seats: 5
//	      tags: [premium, launch]
//	      owner:
//	        dept: eng
//	        lead: alice
//
// Like the providers section — and for the same reason — this is runtime WIRING,
// not model state. BuildRegistry turns the declared objects into an in-memory
// provider.Static registered under each object-type; Apply never writes a row for
// them, and because the model is exported by reading storage back, an export does
// not reproduce them.
//
// Unlike the providers section, this one is LOCAL: it is one of the two wiring
// sections `aperture wiring push` does not share, and the reason is the "never
// persist provider data as source of truth" Non-Goal (export.go) rather than a gap
// in the schema. A providers: entry is a POINTER to where metadata lives and is
// safe to copy to a second instance; the entries here are the metadata, so sharing
// them would make Aperture's own database the source of truth for a host's domain
// data. The seed FILE is therefore the whole source of truth for inline object
// metadata, and an instance that needs metadata its peers also need reaches it
// through a providers: entry, which IS shared. See skills/shared-wiring.md.
//
// The object-type is DERIVED from the identity's terminal segment
// (account:acme/brand:1 is a "brand"), never declared separately — one fact in
// one place cannot disagree with itself.
type Object struct {
	// ID is the object's canonical identity (e.g. "brand:1" or
	// "account:acme/brand:1"). Its terminal segment's type is the object-type the
	// entry is registered under.
	ID string `yaml:"id" json:"id"`
	// Metadata is the object's attribute bag, carried as raw JSON so the YAML and
	// JSON paths decode it identically and numbers keep their exact form until
	// they are normalised (see decodeObjectMetadata). Legal shapes are the shared
	// value model's: a scalar, an array of scalars, or an object one further level
	// deep. Omit it for an object with no metadata.
	Metadata json.RawMessage `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// objectGroup is one object-type's inline entries, in declaration order.
type objectGroup struct {
	typ     string
	objects []provider.Object
}

// registerObjects builds the document's inline objects into in-memory providers
// and registers one per object-type, in first-declaration order.
//
// It runs in two passes, and the split is the point: EVERY inline entry is
// validated (pass one) before any precedence decision is taken (pass two), so a
// malformed declaration fails the load whether or not its type is ultimately
// discarded in favour of a providers: entry. Validation that depended on wiring
// would mean a document silently stopped being checked the day someone added a
// CSV for one of its types. The field_types: declarations are applied in that
// same first pass, for the same reason: a declared date is validated and
// canonicalised even for a type whose entries lose to a providers: entry.
//
// Precedence is TYPE-LEVEL and total: a type already served by the providers:
// section keeps that file-backed provider, and every inline entry for that type
// is discarded — no merge at any granularity and no fallback for an id the file
// lacks. That is the DEFAULT, because pointing a type at a CSV while its inline
// entries are still in the file is an ordinary migration step, not a fault; a
// document that booted yesterday must not stop booting because a providers: row
// was added. StrictProviderCollision makes the collision fatal for hosts that
// would rather read it as an authoring mistake, and ProviderCollisions lets any
// host report the discard.
func (d *Document) registerObjects(reg *provider.Registry, cfg buildConfig, declared map[string]map[string]string) error {
	groups, err := d.groupObjects(declared)
	if err != nil {
		return err
	}
	if cfg.strictProviderCollision {
		// Collisions are collected rather than reported one at a time, so an
		// author fixing a document sees every ambiguous type in one pass. Only
		// the object TYPE is named: an object id can embed an account
		// ("account:acme/brand:1"), and an error message is the wrong place for
		// it.
		var collided []string
		for _, g := range groups {
			if reg.Has(g.typ) {
				collided = append(collided, g.typ)
			}
		}
		if len(collided) > 0 {
			slices.Sort(collided)
			// The types ride in the MESSAGE as well as the context, because the
			// message is what a CLI prints and what lands in a bug report.
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				fmt.Sprintf("seed: object type(s) %s are declared in both the providers: and objects: "+
					"sections; the providers: entry wins and every inline entry for that type is "+
					"discarded entirely (no merge at any granularity, no fallback for an id the file "+
					"lacks) — remove one of the two declarations, or drop "+
					"seed.StrictProviderCollision() to accept the discard",
					strings.Join(collided, ", ")),
				map[string]any{"object_types": collided})
		}
	}

	for _, g := range groups {
		if reg.Has(g.typ) {
			// The file-backed provider wins the whole type. Nothing from these
			// entries reaches the registry, not even for ids the file lacks.
			continue
		}
		impl, err := provider.NewStatic(g.objects)
		if err != nil {
			return err
		}
		// TTL 0 never expires: inline metadata is fixed for the life of the
		// process, so a freshness window would only buy re-reads of a value that
		// cannot have changed.
		if err := reg.Register(g.typ, impl, provider.WithTTL(0)); err != nil {
			return err
		}
	}
	return nil
}

// ProviderCollisions reports the object types this document declares in BOTH its
// providers: and objects: sections — exactly the types whose inline entries
// BuildRegistry discards in favour of the file-backed provider — sorted, and
// empty when there is no overlap.
//
// It exists because the discard is the DEFAULT and must not be silent. There is
// no logging path out of this package (a library that logs is a library that
// picks your logger), so the collision is returned as a fact the host can
// surface however it already surfaces things:
//
//	reg, err := doc.BuildRegistry(dir)
//	if err != nil {
//		return err
//	}
//	if types := doc.ProviderCollisions(); len(types) > 0 {
//		slog.Warn("seed: inline objects discarded, providers: entry wins",
//			"object_types", types)
//	}
//
// It reads the document only — no file IO, no registry — so it is safe to call
// before or after BuildRegistry, and calling it twice costs nothing shared. An
// entry whose id does not parse is skipped rather than reported here: it is not
// a collision, and BuildRegistry already fails the load on it with
// APERTURE_IDENTITY_INVALID.
//
// Only object TYPES are returned, never ids: an id can embed an account
// ("account:acme/brand:1"), and this value is destined for a log line.
func (d *Document) ProviderCollisions() []string {
	if len(d.Objects) == 0 || len(d.Providers) == 0 {
		return nil
	}
	filed := make(map[string]bool, len(d.Providers))
	for _, p := range d.Providers {
		if p.ObjectType != "" {
			filed[p.ObjectType] = true
		}
	}
	var collided []string
	seen := make(map[string]bool, len(filed))
	for _, o := range d.Objects {
		id, err := identity.Parse(o.ID)
		if err != nil {
			continue
		}
		typ := terminalType(id)
		if filed[typ] && !seen[typ] {
			seen[typ] = true
			collided = append(collided, typ)
		}
	}
	slices.Sort(collided)
	return collided
}

// groupObjects validates every inline entry and groups them by object-type.
//
// Objects of DIFFERENT types may be interleaved freely in the file — they are
// grouped here, and the groups come back in first-declaration order — but within
// a type, declaration order is preserved, because that is the order List and
// Query return.
//
// Ids are deduplicated across the WHOLE section, not per type: two entries with
// the same id are the same object declared twice however they are spelled, and
// the second would silently shadow the first.
//
// declared is the validated field_types: index (object-type -> field -> type),
// nil when the document declares none. It is applied AFTER the value model has
// accepted the metadata, because a declared date is a string scalar either way:
// the declaration narrows which strings are legal, it never widens the model.
func (d *Document) groupObjects(declared map[string]map[string]string) ([]objectGroup, error) {
	if len(d.Objects) == 0 {
		return nil, nil
	}
	index := make(map[string]int, len(d.Objects)) // object-type -> groups slot
	var groups []objectGroup
	seen := make(map[string]bool, len(d.Objects))

	for _, o := range d.Objects {
		if o.ID == "" {
			return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID, "seed: object is missing id")
		}
		id, err := identity.Parse(o.ID)
		if err != nil {
			return nil, err // passes through APERTURE_IDENTITY_INVALID
		}
		key := id.String()
		if seen[key] {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: object is declared more than once", map[string]any{"id": key})
		}
		seen[key] = true

		md, err := decodeObjectMetadata(key, o.Metadata)
		if err != nil {
			return nil, err
		}
		typ := terminalType(id)
		if err := applyFieldTypes(key, typ, md, declared[typ]); err != nil {
			return nil, err
		}
		slot, ok := index[typ]
		if !ok {
			slot = len(groups)
			index[typ] = slot
			groups = append(groups, objectGroup{typ: typ})
		}
		groups[slot].objects = append(groups[slot].objects, provider.Object{ID: id, Metadata: md})
	}
	return groups, nil
}

// terminalType returns the object-type an identity belongs to: the type of its
// terminal segment. It is the one derivation of an object's type in this package.
func terminalType(id identity.Identity) string {
	segs := id.Segments()
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1].Type
}

// decodeObjectMetadata turns one entry's raw metadata into a validated
// provider.Metadata.
//
// Two things happen here that the raw decode does not do on its own:
//
//   - NUMBERS are normalised the same way every other loader normalises them —
//     an exact integer that fits int64 becomes an int64, anything else a float64.
//     YAML would otherwise hand over an int and JSON a float64, so the same
//     document would answer `object.seats == 5` differently depending on the
//     format it was written in, and differently from the identical value loaded
//     from a CSV :int or :json column. A silent disagreement between two loaders
//     is the failure mode this normalisation exists to remove.
//   - Every field is checked against the shared value model at LOAD time, so a
//     malformed value fails the seed rather than surfacing as an evaluation error
//     on the Check hot path.
//
// Every failure is APERTURE_CONFIG_INVALID naming the object id and, where the
// fault is in one field, the field — never the value, which is host data. A
// value-model rejection is wrapped so the inner APERTURE_METADATA_INVALID (with
// its path/type/depth context) stays in the chain.
func decodeObjectMetadata(id string, raw json.RawMessage) (provider.Metadata, error) {
	if len(raw) == 0 {
		return provider.Metadata{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber is the house pattern (rules/ast.go decodeScalar, csvprovider's
	// json column): defer the int/float choice to normalizeNumbers rather than
	// floating every integer on the way in.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
			"seed: object %q has undecodable metadata", id)
	}
	if v == nil {
		// An explicit `metadata:` with no value is an object with no fields, not
		// an error — the same reading as omitting the key.
		return provider.Metadata{}, nil
	}
	md, ok := v.(map[string]any)
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: object metadata must be a mapping of field names to values",
			map[string]any{"id": id, "kind": jsonKind(v)})
	}
	// Fields are walked in sorted order so a document with several offending
	// values always reports the same one.
	fields := make([]string, 0, len(md))
	for name := range md {
		fields = append(fields, name)
	}
	slices.Sort(fields)
	for _, name := range fields {
		val, err := normalizeNumbers(id, name, md[name])
		if err != nil {
			return nil, err
		}
		// The shared value model is the single authority on what a metadata value
		// may be; the seed loader never re-implements those rules.
		if err := provider.ValidateField(name, val); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
				"seed: object %q metadata field %q is not a legal metadata value", id, name)
		}
		md[name] = val
	}
	return md, nil
}

// normalizeNumbers rewrites every json.Number in a decoded value, at every depth,
// to the int64/float64 the rest of Aperture compares against. The containers are
// the ones just decoded for this entry, so rewriting them in place shares nothing
// with any other entry.
//
// It is shared by the objects: and attributes: sections — there is one value
// model, so there is one normalisation. id names whichever entry is being
// decoded; the message therefore says "metadata" rather than "object metadata".
func normalizeNumbers(id, field string, v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil // matches a CSV :int column and a :json integer
		}
		if f, err := x.Float64(); err == nil {
			return f, nil // matches a CSV :float column and a :json non-integer
		}
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: metadata holds a number no int64 or float64 can represent",
			map[string]any{"id": id, "field": field})
	case map[string]any:
		for k, elem := range x {
			nv, err := normalizeNumbers(id, field, elem)
			if err != nil {
				return nil, err
			}
			x[k] = nv
		}
		return x, nil
	case []any:
		for i, elem := range x {
			nv, err := normalizeNumbers(id, field, elem)
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
		return x, nil
	default:
		return v, nil
	}
}

// jsonKind names the JSON kind of a decoded value, so a rejection can say what
// the document held without repeating the value itself.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
