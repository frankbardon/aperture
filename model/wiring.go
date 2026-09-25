package model

import (
	"encoding/json"
	"sort"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// This file is the model half of SHARED WIRING: the five tables described under
// "The five shared-wiring tables" in skills/storage-schema.md, expressed as Go
// entities so a second instance can read a decision's wiring out of the database
// it already shares instead of out of a seed file it does not have.
//
// WIRING IS NOT MODEL STATE, and the distinction is worth keeping in view while
// reading what follows. Model state is who exists and who may do what; wiring is
// where a decision's object metadata and attribute bags are read FROM. The types
// here are persisted, but nothing in them participates in a decision directly —
// they are the instructions for BUILDING the registries that do.
//
// WHY THESE TYPES LIVE IN model/ AND NOT IN seed/. There is no import edge
// between seed/ and storage/, in either direction, and there must not be one:
// seed/ links a Postgres driver for HOST provider connections and storage/ links
// one for Aperture's own backend, and the two share no connection handling. A
// wiring type both sides need therefore belongs here, in the package both already
// import, and the seed document's own structs are converted to and from these at
// the internal/cli boundary — the one package that imports both.
//
// WHAT IS DELIBERATELY ABSENT. There is no DSN, no credential, not even a
// dsn_env: variable NAME, and no filesystem path. Those are per-instance facts:
// each instance resolves its own credentials, sizes its own pool, and may reach
// the same logical database through a different host, while a stored path is a
// guess about the other instance's filesystem. The schema has no column for any
// of them, so there is nothing here to carry one.
//
// A TTL IS A STRING. It is the Go duration TEXT the operator wrote ("30s",
// "5m"), carried verbatim rather than parsed to a time.Duration, because this is
// wiring an operator pushes and reads back and the read back has to be
// re-pushable byte for byte — a duration would round-trip "30s" as
// "30000000000". A duration is not an instant: CreatedAt/UpdatedAt below are
// governed by storage/storagetime exactly as every other stamp is, and they are
// the only instants in these types.

// WiringConnection is one row of apt_wiring_connections: the MANIFEST OF NAMES a
// provider entry may cite. It carries the name and nothing else — see the file
// comment above for why a DSN, a dsn_env variable name and the pool tuning are
// all per-instance facts with no column here.
type WiringConnection struct {
	// Name is the connection's identity: the string a WiringProvider or a
	// WiringAttributeProvider names in its Connection field. Non-empty.
	Name string
	// CreatedAt / UpdatedAt are stamped by the layer that pushes the wiring and
	// persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WiringProvider is one row of apt_wiring_providers plus the reference rows it
// owns: the database-backed form of a seed document's providers: entry. It is
// keyed by ObjectType because a type may be served at most once.
type WiringProvider struct {
	// ObjectType is the type whose object metadata this entry serves, and the
	// entry's identity. Non-empty.
	ObjectType string
	// Kind selects the implementation the wiring builder instantiates. Storage
	// records it verbatim and does not police the vocabulary: which kinds are
	// legal in SHARED wiring is a push-validation question (kind: csv is refused
	// there, because a filesystem path is machine-local), and answering it here
	// would put the rule in two places.
	Kind string
	// Connection names the WiringConnection this entry reads through, or the
	// empty string for a kind that reads no database. Absence is the EMPTY STRING
	// and never a nil pointer, matching the column: that choice is what rules out
	// a foreign key on it, and the name is resolved when the wiring is BUILT,
	// where an unknown one is a coded error naming the typo and the manifest it is
	// missing from.
	Connection string
	// GetOne / GetAll / IDColumn are the statement set for a database-backed kind,
	// each the empty string when the kind does not use it.
	GetOne   string
	GetAll   string
	IDColumn string
	// TTL is the cache freshness window as the Go duration TEXT the operator
	// wrote. See the file comment for why it is not a time.Duration.
	TTL string
	// MaxSize caps cached entries for this type; 0 means the registry default.
	// Negative is refused.
	MaxSize int
	// References is this entry's references: map, flattened into rows: each names
	// a metadata field this provider serves and the object type whose identities
	// that field holds. A map has no order, so the persisted rows carry no
	// sequence and a read back is sorted by Field — that is what makes a
	// push-and-read-back round trip byte-stable.
	//
	// The reference rows are OWNED by this entry, exactly as a principal's role
	// list is owned by the principal: the schema's ON DELETE CASCADE removes them
	// with it, and there is no way to write one without writing the entry.
	References []WiringReference
	// CreatedAt / UpdatedAt are stamped by the layer that pushes the wiring and
	// persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WiringReference is one row of apt_wiring_provider_references: a declaration
// that a metadata field a provider serves holds identities of another object
// type. It is the one wiring entity with NO timestamps, for the reason the other
// owned child tables have none — its history is its provider entry's, and the
// CASCADE edge means it cannot outlive it.
type WiringReference struct {
	// Field is the metadata field name holding the identities. Non-empty, and
	// unique within one WiringProvider.
	Field string
	// TargetType is the object type whose identities the field holds. Non-empty.
	//
	// It is resolved against the REGISTRY the wiring builds, not against any
	// table: the target must be a type that registry serves, and an inline
	// objects: type belongs to that set without appearing in a wiring row at all.
	// That is why the column carries no foreign key and why storage does not check
	// it here.
	TargetType string
}

// WiringFieldType is one row of apt_wiring_field_types: a declaration that a
// metadata field holds a date or a datetime. It is the field_types: section,
// flattened one row per (ObjectType, Field).
type WiringFieldType struct {
	// ObjectType is the type whose objects the declaration applies to. Non-empty.
	ObjectType string
	// Field is the metadata field being declared. Non-empty, and unique within an
	// object type.
	Field string
	// DeclaredType is the field's declared type — the CSV loader's column-suffix
	// vocabulary with the colon removed ("date", "datetime"). Non-empty; the
	// vocabulary itself is checked where the wiring is built, not here.
	DeclaredType string
	// CreatedAt / UpdatedAt are stamped by the layer that pushes the wiring and
	// persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WiringAttributeProvider is one row of apt_wiring_attribute_providers: the
// database-backed form of an attribute_providers: entry, one per attribute SLOT.
type WiringAttributeProvider struct {
	// Subject is the attribute slot this entry fills, and the entry's identity.
	// It is spelled Subject rather than Slot for the reason the seed key is:
	// "account" is a slot but not a principal KIND, and Kind is already this
	// entry's implementation selector. Non-empty; the closed slot vocabulary is
	// checked where the wiring is built.
	Subject string
	// Kind selects the implementation. Recorded verbatim, as WiringProvider.Kind
	// is.
	Kind string
	// Connection names the WiringConnection this entry reads through, or the
	// empty string. One connection row is one pool, however many entries of
	// either kind name it.
	Connection string
	// GetOne / GetAll / IDColumn are the statement set, each the empty string
	// when unused.
	//
	// GetAll is legitimately empty HERE where an object provider must declare it:
	// a slot with no enumeration statement is FETCH-ONLY, every decision path
	// works unchanged, and only the system-tier admin read refuses. That is a
	// feature — it lets a host serve the attributes of the principal being decided
	// about without exposing its whole user table to an enumeration.
	GetOne   string
	GetAll   string
	IDColumn string
	// TTL is this slot's cache freshness window as Go duration TEXT.
	TTL string
	// MaxSize caps cached bags for this slot; 0 means the registry default.
	// Negative is refused.
	MaxSize int
	// DeclaredKeys is the OPTIONAL declared key set: the attribute keys this
	// shared slot guarantees. It is a DeclaredKeys rather than a []string because
	// "not declared" and "declared empty" are different answers and a plain slice
	// loses one of them the first time a clone helper collapses an empty slice to
	// nil. See DeclaredKeys.
	DeclaredKeys DeclaredKeys
	// CreatedAt / UpdatedAt are stamped by the layer that pushes the wiring and
	// persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeclaredKeys is an OPTIONAL set of attribute key names: the keys a shared
// attribute slot guarantees. The optionality is the whole point of the type, and
// it is a struct rather than a []string because the two states a slice would
// conflate are DIFFERENT ANSWERS:
//
//   - NOT DECLARED (Declared == false) opts the slot out of enforcement. It
//     behaves exactly as a slot did before the column existed.
//   - DECLARED EMPTY (Declared == true, no Keys) opts IN and permits no keys at
//     all.
//
// A nil-versus-empty slice would express the same distinction and lose it
// silently: storage/memory's cloneStrings returns nil for an empty input, and
// encoding/json renders a nil slice as null and an empty one as [], so any of the
// three layers a set passes through could flatten "declared empty" into "never
// declared" with nothing going red. Declared is the bit that cannot be dropped by
// accident.
//
// The stored shape is a plain list of names with NO per-key type information —
// the simplest form that round-trips — because the metadata value model already
// governs shape, and a second typing mechanism is a second place for two
// declarations to disagree.
//
// Nothing enforces a declared set yet; the column and this type carry it because
// Setup creates and never migrates, so adding either later would be a second hard
// schema break.
type DeclaredKeys struct {
	// Declared reports whether a set was declared at all. When it is false, Keys
	// MUST be empty — Validate refuses the contradiction rather than guessing
	// which half was meant.
	Declared bool
	// Keys is the declared set, in the order it was declared. Non-empty names, no
	// duplicates.
	Keys []string
}

// Encode renders the set in the one form both dialects' declared_keys column
// holds: the EMPTY STRING when nothing was declared, and a JSON array of the
// names — including "[]" — when something was.
//
// Both SQL backends call this rather than each spelling the encoding out, which
// is the one place this package departs from "every statement is written twice,
// so a reader can hold the two files side by side". The reason is that the two
// spellings here are different answers rather than two renderings of one value:
// a twin pair that drifted would not produce a visibly wrong row, it would
// produce a slot that silently stopped being enforced.
func (d DeclaredKeys) Encode() (string, error) {
	if !d.Declared {
		return "", nil
	}
	keys := d.Keys
	if keys == nil {
		// json.Marshal renders a nil slice as null. The declared-empty spelling is
		// "[]", so the empty set is written explicitly.
		keys = []string{}
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return "", aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "marshal declared attribute keys", err)
	}
	return string(b), nil
}

// ParseDeclaredKeys is Encode's read-side counterpart: the empty string decodes
// to a set that was never declared, and any JSON array decodes to one that was.
func ParseDeclaredKeys(raw string) (DeclaredKeys, error) {
	if raw == "" {
		return DeclaredKeys{}, nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return DeclaredKeys{}, aerr.Wrap(aerr.APERTURE_INVALID_INPUT,
			"unmarshal declared attribute keys", err)
	}
	if keys == nil {
		// A stored "null" is not a spelling this package writes, but decoding it to
		// a declared-empty set keeps Declared true: the column was not empty, so
		// something was declared.
		keys = []string{}
	}
	return DeclaredKeys{Declared: true, Keys: keys}, nil
}

// Clone deep-copies the key slice so a stored set cannot be mutated through a
// caller's reference (or the reverse). It preserves Declared and the
// empty-versus-nil distinction that Declared exists to survive: a declared-empty
// set clones to a declared-empty set.
func (d DeclaredKeys) Clone() DeclaredKeys {
	if !d.Declared {
		return DeclaredKeys{}
	}
	out := DeclaredKeys{Declared: true, Keys: []string{}}
	if len(d.Keys) > 0 {
		out.Keys = make([]string, len(d.Keys))
		copy(out.Keys, d.Keys)
	}
	return out
}

// WiringSet is the WHOLE of a deployment's shared wiring: every row of all five
// tables, read and written as one value.
//
// It is one type rather than five independent collections because the wiring is
// only meaningful whole. A provider entry naming a connection the manifest does
// not list is not half-valid wiring, it is broken wiring, and an instance that
// booted against a set written half-way would build a registry missing exactly
// the entries whose write failed — while reporting nothing. So the write is an
// all-or-nothing REPLACE of the whole set (Storage.ReplaceWiring) and the read
// hands the whole set back from one consistent snapshot (Storage.GetWiring).
type WiringSet struct {
	Connections        []WiringConnection
	Providers          []WiringProvider
	FieldTypes         []WiringFieldType
	AttributeProviders []WiringAttributeProvider
}

// IsEmpty reports whether the set carries no wiring at all — the state a
// database is in before anything has been pushed to it, and the state that tells
// a booting instance to fall back to its local seed file.
func (w WiringSet) IsEmpty() bool {
	return len(w.Connections) == 0 && len(w.Providers) == 0 &&
		len(w.FieldTypes) == 0 && len(w.AttributeProviders) == 0
}

// Sort orders every section, and every provider's reference rows, into the one
// canonical order a read returns. A backend's List* and GetWiring return sorted
// output, so a push-and-read-back round trip is stable regardless of the order
// the wiring was written in.
func (w *WiringSet) Sort() {
	SortWiringConnections(w.Connections)
	SortWiringProviders(w.Providers)
	SortWiringFieldTypes(w.FieldTypes)
	SortWiringAttributeProviders(w.AttributeProviders)
}

// SortWiringConnections orders connections by name.
func SortWiringConnections(cs []WiringConnection) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
}

// SortWiringProviders orders provider entries by object type, and each entry's
// reference rows by field name.
func SortWiringProviders(ps []WiringProvider) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].ObjectType < ps[j].ObjectType })
	for i := range ps {
		SortWiringReferences(ps[i].References)
	}
}

// SortWiringReferences orders a provider's reference rows by field name. A
// references: map has no order of its own, so field order IS the canonical one.
func SortWiringReferences(rs []WiringReference) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Field < rs[j].Field })
}

// SortWiringFieldTypes orders field-type declarations by object type, then field.
func SortWiringFieldTypes(fs []WiringFieldType) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].ObjectType != fs[j].ObjectType {
			return fs[i].ObjectType < fs[j].ObjectType
		}
		return fs[i].Field < fs[j].Field
	})
}

// SortWiringAttributeProviders orders attribute-provider entries by slot.
func SortWiringAttributeProviders(as []WiringAttributeProvider) {
	sort.Slice(as, func(i, j int) bool { return as[i].Subject < as[j].Subject })
}

// ---- Validation ----
//
// Every Validate* below is STRUCTURAL: it checks the things the tables cannot
// hold — an empty key, a negative cache bound, a duplicate — and nothing else.
// It deliberately does NOT check that a Kind is one the builder implements, that
// a Connection name appears in the manifest, that a DeclaredType is one of the
// two legal words, or that a TTL parses as a duration. Those are answered where
// the wiring is BUILT, against the registry and the vocabulary the builder owns,
// and duplicating them here would put each rule in two places that can disagree.
//
// The one apparent exception is not one: Connection is not checked against
// Connections because the column deliberately carries no foreign key, for the
// reason stated on the field.

// ValidateWiringConnection checks a connection manifest entry is well-formed.
func ValidateWiringConnection(c WiringConnection) error {
	if c.Name == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "wiring connection name is empty")
	}
	return nil
}

// ValidateWiringProvider checks a provider entry and its reference rows are
// well-formed.
func ValidateWiringProvider(p WiringProvider) error {
	if p.ObjectType == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "wiring provider object type is empty")
	}
	if p.Kind == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring provider declares no kind",
			map[string]any{"object_type": p.ObjectType})
	}
	if p.MaxSize < 0 {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring provider max_size is negative",
			map[string]any{"object_type": p.ObjectType, "max_size": p.MaxSize})
	}
	seen := make(map[string]struct{}, len(p.References))
	for _, r := range p.References {
		if r.Field == "" {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring provider reference has an empty field name",
				map[string]any{"object_type": p.ObjectType})
		}
		if r.TargetType == "" {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring provider reference names no target type",
				map[string]any{"object_type": p.ObjectType, "field": r.Field})
		}
		if _, dup := seen[r.Field]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring provider declares the same reference field twice",
				map[string]any{"object_type": p.ObjectType, "field": r.Field})
		}
		seen[r.Field] = struct{}{}
	}
	return nil
}

// ValidateWiringFieldType checks a field-type declaration is well-formed.
func ValidateWiringFieldType(ft WiringFieldType) error {
	if ft.ObjectType == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "wiring field type object type is empty")
	}
	if ft.Field == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring field type field name is empty",
			map[string]any{"object_type": ft.ObjectType})
	}
	if ft.DeclaredType == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring field type declares no type",
			map[string]any{"object_type": ft.ObjectType, "field": ft.Field})
	}
	return nil
}

// ValidateWiringAttributeProvider checks an attribute-provider entry, including
// its declared key set, is well-formed.
func ValidateWiringAttributeProvider(ap WiringAttributeProvider) error {
	if ap.Subject == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "wiring attribute provider subject is empty")
	}
	if ap.Kind == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring attribute provider declares no kind",
			map[string]any{"subject": ap.Subject})
	}
	if ap.MaxSize < 0 {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring attribute provider max_size is negative",
			map[string]any{"subject": ap.Subject, "max_size": ap.MaxSize})
	}
	if !ap.DeclaredKeys.Declared && len(ap.DeclaredKeys.Keys) > 0 {
		// The contradiction is refused rather than resolved: keys with Declared
		// false is either a forgotten flag or a stray slice, and guessing would
		// silently pick one of two opposite meanings.
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"wiring attribute provider carries declared keys but declares no key set",
			map[string]any{"subject": ap.Subject})
	}
	seen := make(map[string]struct{}, len(ap.DeclaredKeys.Keys))
	for _, k := range ap.DeclaredKeys.Keys {
		if k == "" {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring attribute provider declares an empty attribute key",
				map[string]any{"subject": ap.Subject})
		}
		if _, dup := seen[k]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring attribute provider declares the same attribute key twice",
				map[string]any{"subject": ap.Subject, "key": k})
		}
		seen[k] = struct{}{}
	}
	return nil
}

// ValidateWiringSet validates every entry in the set and additionally refuses a
// DUPLICATE KEY within any section.
//
// The duplicates are checked here rather than left to each table's PRIMARY KEY
// on purpose. A key collision inside one pushed set is a malformed push, not a
// database failure: APERTURE_INVALID_INPUT names the duplicated key and says so,
// where the alternative would be APERTURE_STORAGE_CONSTRAINT from two dialects
// and a third refusal hand-written to match them in a backend with no keys at
// all. Every backend calls this before it writes anything, so all three refuse
// the same set with the same code.
func ValidateWiringSet(set WiringSet) error {
	names := make(map[string]struct{}, len(set.Connections))
	for _, c := range set.Connections {
		if err := ValidateWiringConnection(c); err != nil {
			return err
		}
		if _, dup := names[c.Name]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring declares the same connection twice",
				map[string]any{"connection": c.Name})
		}
		names[c.Name] = struct{}{}
	}
	types := make(map[string]struct{}, len(set.Providers))
	for _, p := range set.Providers {
		if err := ValidateWiringProvider(p); err != nil {
			return err
		}
		if _, dup := types[p.ObjectType]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring declares a provider for the same object type twice",
				map[string]any{"object_type": p.ObjectType})
		}
		types[p.ObjectType] = struct{}{}
	}
	fields := make(map[string]struct{}, len(set.FieldTypes))
	for _, ft := range set.FieldTypes {
		if err := ValidateWiringFieldType(ft); err != nil {
			return err
		}
		key := ft.ObjectType + "\x00" + ft.Field
		if _, dup := fields[key]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring declares the same field type twice",
				map[string]any{"object_type": ft.ObjectType, "field": ft.Field})
		}
		fields[key] = struct{}{}
	}
	slots := make(map[string]struct{}, len(set.AttributeProviders))
	for _, ap := range set.AttributeProviders {
		if err := ValidateWiringAttributeProvider(ap); err != nil {
			return err
		}
		if _, dup := slots[ap.Subject]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"wiring declares the same attribute slot twice",
				map[string]any{"subject": ap.Subject})
		}
		slots[ap.Subject] = struct{}{}
	}
	return nil
}
