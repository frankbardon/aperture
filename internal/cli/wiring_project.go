package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
)

// The seed -> model projection behind `aperture wiring push`, and every rule a
// push refuses on.
//
// # Why this lives in internal/cli
//
// There is no import edge between seed/ and storage/, in either direction, and
// there must not be one: seed/ links a Postgres driver for HOST provider
// connections and storage/ links one for Aperture's own backend, and the two
// share no connection handling. internal/cli is the one package that already
// imports both, so the projection and the write meet here and nowhere else.
//
// # Why the validation lives here and not in model.ValidateWiringSet
//
// Storage validates STRUCTURE ONLY — an empty key, a negative cache bound, a
// duplicate within a section — and says so in its own doc comment. It
// deliberately does not check that a Kind is implemented, that a Connection
// appears in the manifest, that a DeclaredType is one of the two legal words, or
// that a TTL parses as a duration, because those are questions about the
// VOCABULARY a wiring builder owns rather than about the shape of a row. Asking
// them in both places would be two rules that can disagree, and the one that
// disagrees silently is the one in the layer nobody reads.
//
// So every refusal a push makes is here, ahead of the write, and the write
// itself (model.Storage.ReplaceWiring) is all-or-nothing. A push either validates
// completely and then replaces the whole set in one transaction, or it changes
// nothing at all.
//
// # The four sections, and the two that are NOT here
//
// Shared (projected below): providers:, field_types:, connections:,
// attribute_providers:.
//
// Local (deliberately untouched): objects: and attributes:. Both carry DATA
// rather than a pointer to data — inline object metadata and inline subject bags
// — and both belong to the instance whose seed file lists them. A push reads
// neither, and the other twelve model-state sections of the document are not
// wiring at all.

const (
	// wiringKindSQL is the database-backed kind: statements run against a named
	// entry of the pushed connections: manifest. It is the only kind that can be
	// SHARED wiring, because the only machine-local thing it names is a connection
	// NAME, which every instance resolves from its own environment.
	wiringKindSQL = "sql"
	// wiringKindCSV is the file-backed kind. Legal in a local seed document and
	// refused in shared wiring — see APERTURE_WIRING_KIND_UNSHAREABLE.
	wiringKindCSV = "csv"
)

// wiringFieldTypeDate and wiringFieldTypeDateTime are the field_types:
// vocabulary: the CSV loader's column-suffix words with the colon removed.
//
// They restate seed/fields.go's own two constants, which are unexported, and the
// restatement is pinned behaviourally rather than by sharing a symbol:
// TestPushAndTheSeedBuilderAgreeOnTheFieldTypeVocabulary runs the same document
// through this projection and through seed's BuildRegistry and requires both to
// refuse it. Exporting the constants instead would mean a spelling change had to
// remember to visit two packages; the test means it cannot.
const (
	wiringFieldTypeDate     = "date"
	wiringFieldTypeDateTime = "datetime"
)

// wiringFieldTypeExpected names the whole accepted vocabulary, for a diagnostic
// that has to say what WOULD have been accepted.
const wiringFieldTypeExpected = wiringFieldTypeDate + " or " + wiringFieldTypeDateTime

// wiringFromDocument projects the four shared wiring sections out of doc,
// validates every one of them, and stamps the result.
//
// now stamps CreatedAt and UpdatedAt on every row of the set, to ONE instant. A
// push is a whole-set REPLACE — ReplaceWiring empties the five tables and writes
// the new set — so "created" means "created by this push", and giving the rows of
// one push different instants would invent a history the write does not have.
//
// The document arrives already parsed, so a literal dsn: has normally been
// refused by seed.Parse before this is reached. It is checked again anyway: this
// function is the push's contract, and a Document assembled in Go rather than
// parsed from a file would otherwise slip a credential past the one rule that
// exists to keep credentials out of shared storage.
func wiringFromDocument(doc *seed.Document, now time.Time) (model.WiringSet, error) {
	if doc == nil {
		return model.WiringSet{}, aerr.New(aerr.APERTURE_INVALID_INPUT,
			"cli: no seed document to push wiring from")
	}
	if err := refuseLiteralWiringDSN(doc); err != nil {
		return model.WiringSet{}, err
	}

	set := model.WiringSet{}
	declared := make(map[string]struct{}, len(doc.Connections))
	for name := range doc.Connections {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return model.WiringSet{}, aerr.New(aerr.APERTURE_CONFIG_INVALID,
				"cli: the wiring's connections: block declares an entry with an empty name")
		}
		declared[trimmed] = struct{}{}
		set.Connections = append(set.Connections, model.WiringConnection{
			Name:      trimmed,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	for _, p := range doc.Providers {
		row, err := wiringProviderRow(p, declared, now)
		if err != nil {
			return model.WiringSet{}, err
		}
		set.Providers = append(set.Providers, row)
	}

	for _, ft := range doc.FieldTypes {
		rows, err := wiringFieldTypeRows(ft, now)
		if err != nil {
			return model.WiringSet{}, err
		}
		set.FieldTypes = append(set.FieldTypes, rows...)
	}

	for _, ap := range doc.AttributeProviders {
		row, err := wiringAttributeProviderRow(ap, declared, now)
		if err != nil {
			return model.WiringSet{}, err
		}
		set.AttributeProviders = append(set.AttributeProviders, row)
	}

	set.Sort()
	// The structural pass runs here as well as inside ReplaceWiring, so a
	// duplicate object type or a negative max_size is reported before the store is
	// even asked. ValidateWiringSet's refusals are already coded, so they are
	// returned as they are — wrapping would re-stamp them.
	if err := model.ValidateWiringSet(set); err != nil {
		return model.WiringSet{}, err
	}
	return set, nil
}

// wiringToDocument is wiringFromDocument's INVERSE: the deployed wiring set
// rendered back as the four seed sections, so `aperture wiring pull` emits a
// document `aperture wiring push` accepts unchanged.
//
// It is the one projection in this direction, and E5-S2's `wiring diff` must not
// grow a second. A diff does not need one: both sides of that comparison are
// model.WiringSet values — the store's, straight from GetWiring, and the local
// document's, through wiringFromDocument — and comparing them there rather than
// comparing rendered documents is what keeps the two commands agreeing about what
// "the same wiring" means. It also side-steps the dsn_env: asymmetry below, which
// is a property of the FILE and not of the wiring.
//
// # Every field that is dropped is dropped because the store does not hold it
//
//   - CreatedAt / UpdatedAt: seed has no key for a stamp, and it should not. A push
//     restamps the whole set to one instant, so a stamp is a fact about the last
//     push rather than about the wiring — which is also what makes a pull byte-
//     stable across pushes.
//   - Connection.DSNEnv, and the whole pool tuning: never stored, by design. The
//     manifest is names and nothing else. It therefore comes back as `dsn_env: ""`,
//     which is a re-pushable document and NOT a bootable one: an instance built
//     from this file refuses at BuildRegistryWithConnections with a coded error
//     naming the unset variable. That is the right failure — loud, at boot, naming
//     the per-instance fact the operator has to supply — and it is the visible shape
//     of the rule that a credential's variable name is per-instance too.
//   - Provider.Path / AttributeProvider.Path: kind: csv is refused at push, so no
//     stored entry has one.
//   - DeclaredKeys: see wiringUnexpressedDeclaredKeys.
func wiringToDocument(set model.WiringSet) *seed.Document {
	doc := &seed.Document{}

	if len(set.Connections) > 0 {
		doc.Connections = make(map[string]seed.Connection, len(set.Connections))
		for _, c := range set.Connections {
			doc.Connections[c.Name] = seed.Connection{}
		}
	}

	for _, p := range set.Providers {
		entry := seed.Provider{
			ObjectType: p.ObjectType,
			Kind:       p.Kind,
			Connection: p.Connection,
			GetOne:     p.GetOne,
			GetAll:     p.GetAll,
			IDColumn:   p.IDColumn,
			TTL:        p.TTL,
			MaxSize:    p.MaxSize,
		}
		if len(p.References) > 0 {
			entry.References = make(map[string]string, len(p.References))
			for _, r := range p.References {
				entry.References[r.Field] = r.TargetType
			}
		}
		doc.Providers = append(doc.Providers, entry)
	}

	// field_types: is stored one row per (object type, field) and spelled one entry
	// per object type with a fields: map. The rows arrive sorted by object type then
	// field, so walking them in order and starting a new entry whenever the object
	// type changes re-groups them without a second sort — and the entry order is the
	// stored order, which is what makes the emitted document byte-stable.
	for _, ft := range set.FieldTypes {
		if n := len(doc.FieldTypes); n > 0 && doc.FieldTypes[n-1].ObjectType == ft.ObjectType {
			doc.FieldTypes[n-1].Fields[ft.Field] = ft.DeclaredType
			continue
		}
		doc.FieldTypes = append(doc.FieldTypes, seed.FieldType{
			ObjectType: ft.ObjectType,
			Fields:     map[string]string{ft.Field: ft.DeclaredType},
		})
	}

	for _, ap := range set.AttributeProviders {
		doc.AttributeProviders = append(doc.AttributeProviders, seed.AttributeProvider{
			Subject:    ap.Subject,
			Kind:       ap.Kind,
			Connection: ap.Connection,
			GetOne:     ap.GetOne,
			GetAll:     ap.GetAll,
			IDColumn:   ap.IDColumn,
			TTL:        ap.TTL,
			MaxSize:    ap.MaxSize,
		})
	}
	return doc
}

// wiringUnexpressedDeclaredKeys names the slots whose DECLARED KEY SET a pulled
// document cannot yet carry, so a pull can say so out loud instead of dropping it.
//
// The seed attribute_providers: schema has no key for a declared set yet — it
// gains one in its own story — while the column and model.DeclaredKeys have existed
// since the schema was created, because Setup creates and never migrates. So a slot
// whose set was written straight to storage round-trips through a pull as NOT
// DECLARED, and a re-push would clear it.
//
// That is a real gap in the fixed point, and it is reported rather than hidden: the
// two states DeclaredKeys exists to keep apart are "opted out of enforcement" and
// "opted in and permits nothing", and silently turning the second into the first is
// precisely the collapse the type was made a struct to prevent. Refusing the pull
// instead would be worse — it would make the command unusable for the other three
// sections over a field nothing enforces yet.
//
// WHEN THE SEED KEY LANDS, this function and its warning go away in the same
// change that starts emitting the key. A warning left behind after the document can
// express the set would be a false alarm on every pull.
func wiringUnexpressedDeclaredKeys(set model.WiringSet) []string {
	var slots []string
	for _, ap := range set.AttributeProviders {
		if ap.DeclaredKeys.Declared {
			slots = append(slots, ap.Subject)
		}
	}
	return slots
}

// refuseLiteralWiringDSN refuses a literal dsn: anywhere in the wiring being
// pushed, in exactly the posture seed.Parse already takes: the offending entry is
// named and the value never is.
//
// It reuses APERTURE_SQL_PROVIDER_DSN_LITERAL rather than minting a push-specific
// code, because the harm and the remedy are identical — a password was written
// somewhere it will be read by something other than the process that needs it —
// and the registry fixups for that code already say to move it to dsn_env: and
// rotate the credential. A second code would mean two sets of fixups for one
// mistake.
//
// Pushing one is strictly worse than committing one: the shared-wiring schema has
// no column for a DSN and none for a dsn_env variable NAME either, so there is
// nowhere for it to go, and an operator who wrote one is describing a per-instance
// fact in a place every instance reads.
func refuseLiteralWiringDSN(doc *seed.Document) error {
	var connections []string
	for name, c := range doc.Connections {
		if strings.TrimSpace(c.DSNLiteral) != "" {
			connections = append(connections, strings.TrimSpace(name))
		}
	}
	if len(connections) > 0 {
		sort.Strings(connections)
		return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL,
			fmt.Sprintf("cli: wiring connection %s carries a literal dsn: key; use dsn_env: naming the environment variable each instance reads the DSN from, because shared wiring holds the connection's NAME and nothing else",
				strings.Join(quoteEach(connections), ", ")),
			map[string]any{"connections": connections, "forbidden_key": "dsn", "use_instead": "dsn_env"})
	}
	var subjects []string
	for _, ap := range doc.AttributeProviders {
		if strings.TrimSpace(ap.DSNLiteral) != "" {
			subjects = append(subjects, strings.TrimSpace(ap.Subject))
		}
	}
	if len(subjects) == 0 {
		return nil
	}
	sort.Strings(subjects)
	return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL,
		fmt.Sprintf("cli: the wiring's attribute provider for subject %s carries a literal dsn: key; a provider entry never carries credentials — declare the database once in connections: with dsn_env: and name it with connection:",
			strings.Join(quoteEach(subjects), ", ")),
		map[string]any{"subjects": subjects, "forbidden_key": "dsn", "use_instead": "connection"})
}

// wiringProviderRow converts one providers: entry into its shared-wiring row.
func wiringProviderRow(p seed.Provider, declared map[string]struct{}, now time.Time) (model.WiringProvider, error) {
	objectType := strings.TrimSpace(p.ObjectType)
	if objectType == "" {
		return model.WiringProvider{}, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"cli: the wiring's providers: block has an entry with no object_type")
	}
	where := map[string]any{"object_type": objectType}
	if err := checkWiringKind(p.Kind, "providers", where); err != nil {
		return model.WiringProvider{}, err
	}
	connection, err := wiringConnectionName(p.Connection, declared, "provider", where)
	if err != nil {
		return model.WiringProvider{}, err
	}
	if err := checkWiringTTL(p.TTL, "provider", where); err != nil {
		return model.WiringProvider{}, err
	}

	refs := make([]model.WiringReference, 0, len(p.References))
	for field, target := range p.References {
		field, target = strings.TrimSpace(field), strings.TrimSpace(target)
		if field == "" || target == "" {
			return model.WiringProvider{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"cli: the wiring's provider declares a reference with an empty field name or target type",
				map[string]any{"object_type": objectType, "field": field, "target_type": target})
		}
		refs = append(refs, model.WiringReference{Field: field, TargetType: target})
	}
	// A references: map has no order, so field order is the canonical one and the
	// rows are sorted before they are stored. That is what makes a push and a read
	// back byte-stable rather than dependent on Go's map iteration.
	model.SortWiringReferences(refs)

	return model.WiringProvider{
		ObjectType: objectType,
		Kind:       strings.TrimSpace(p.Kind),
		Connection: connection,
		GetOne:     p.GetOne,
		GetAll:     p.GetAll,
		IDColumn:   p.IDColumn,
		TTL:        strings.TrimSpace(p.TTL),
		MaxSize:    p.MaxSize,
		References: refs,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// wiringFieldTypeRows flattens one field_types: entry into one row per field.
func wiringFieldTypeRows(ft seed.FieldType, now time.Time) ([]model.WiringFieldType, error) {
	objectType := strings.TrimSpace(ft.ObjectType)
	if objectType == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"cli: the wiring's field_types: block has an entry with no object_type")
	}
	rows := make([]model.WiringFieldType, 0, len(ft.Fields))
	for field, declaredType := range ft.Fields {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"cli: the wiring's field_types: entry has an empty field name",
				map[string]any{"object_type": objectType})
		}
		// An unrecognised spelling is REFUSED, never stored: a declaration the
		// builder will quietly skip reads exactly like one it honoured while
		// validating nothing, and pushing it puts that silence in every instance.
		if declaredType != wiringFieldTypeDate && declaredType != wiringFieldTypeDateTime {
			// Named in the MESSAGE and not only in the context map: the CLI prints a
			// refusal with %v, so anything an operator has to act on that lives only
			// in Context is invisible exactly where it is read.
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				fmt.Sprintf("cli: the wiring declares field %q of object type %q as %q, which is not a declared field type; write %s",
					field, objectType, declaredType, wiringFieldTypeExpected),
				map[string]any{
					"object_type": objectType, "field": field,
					"type": declaredType, "expected": wiringFieldTypeExpected,
				})
		}
		rows = append(rows, model.WiringFieldType{
			ObjectType:   objectType,
			Field:        field,
			DeclaredType: declaredType,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
	}
	return rows, nil
}

// wiringAttributeProviderRow converts one attribute_providers: entry into its
// shared-wiring row.
func wiringAttributeProviderRow(ap seed.AttributeProvider, declared map[string]struct{}, now time.Time) (model.WiringAttributeProvider, error) {
	// The slot is parsed through provider.ParseAttributeSlot rather than compared
	// against a list restated here, so the CLI cannot accept a fourth slot the
	// registry could never serve — the closed set is what stops a host declaring a
	// party the engine cannot fetch, which surfaces as an empty bag, which is a
	// silent denial.
	//
	// Its refusal is REPLACED rather than wrapped, and with the same code. Wrapping
	// would re-stamp and leave two coded errors in the chain; and the library's
	// message ("provider: not an attribute slot") carries the offending value in its
	// Context rather than in its text, while the CLI prints a refusal with %v — so
	// the one thing the operator has to fix would be invisible exactly where it is
	// read. The legal set is still DERIVED (slotList reads provider.AttributeSlots),
	// never restated.
	subject := strings.TrimSpace(ap.Subject)
	slot, err := provider.ParseAttributeSlot(subject)
	if err != nil {
		return model.WiringAttributeProvider{}, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			fmt.Sprintf("cli: the wiring declares an attribute provider for subject %q, which is not an attribute slot; the slots are %s",
				subject, slotList()),
			map[string]any{"subject": subject, "slots": slotList()})
	}
	where := map[string]any{"subject": slot.String()}
	if err := checkWiringKind(ap.Kind, "attribute_providers", where); err != nil {
		return model.WiringAttributeProvider{}, err
	}
	connection, err := wiringConnectionName(ap.Connection, declared, "attribute provider", where)
	if err != nil {
		return model.WiringAttributeProvider{}, err
	}
	if err := checkWiringTTL(ap.TTL, "attribute provider", where); err != nil {
		return model.WiringAttributeProvider{}, err
	}
	return model.WiringAttributeProvider{
		Subject:    slot.String(),
		Kind:       strings.TrimSpace(ap.Kind),
		Connection: connection,
		GetOne:     ap.GetOne,
		GetAll:     ap.GetAll,
		IDColumn:   ap.IDColumn,
		TTL:        strings.TrimSpace(ap.TTL),
		MaxSize:    ap.MaxSize,
		// DeclaredKeys is left NOT DECLARED, explicitly, because the seed document
		// has no key for it yet: the attribute_providers: schema gains the field in
		// its own story, and the two states a DeclaredKeys distinguishes — "not
		// declared" (opts the slot out of enforcement) and "declared empty" (opts in
		// and permits nothing) — must not be collapsed on the way through. An absent
		// YAML key is the zero value here; a present-but-empty one will be
		// model.DeclaredKeys{Declared: true}.
		DeclaredKeys: model.DeclaredKeys{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// checkWiringKind refuses a kind that cannot be shared wiring, and an unknown
// kind.
//
// The two refusals are deliberately different codes. kind: csv is IMPLEMENTED and
// works perfectly in a local seed — it is unshareable, and the remedy is to move
// the entry to kind: sql or leave it out of the push. An unknown kind is a typo or
// a vocabulary the build does not have, which is the same APERTURE_CONFIG_INVALID
// the seed builder raises for it.
func checkWiringKind(kind, section string, where map[string]any) error {
	switch strings.TrimSpace(kind) {
	case wiringKindSQL:
		return nil
	case wiringKindCSV:
		ctx := copyWhere(where)
		ctx["section"] = section
		ctx["kind"] = wiringKindCSV
		return aerr.WithContext(aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
			fmt.Sprintf("cli: %s entry %s selects kind: csv, which cannot be SHARED wiring — its data source is a filesystem path, a relative one resolves against the seed file's own directory and an absolute one is a guess about the other instance's disk, so the shared-wiring schema has no path column at all; read the same data through kind: sql and a connections: entry, or keep this entry in the LOCAL seed document",
				section, describeWhere(where)),
			ctx)
	case "":
		ctx := copyWhere(where)
		ctx["section"] = section
		return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("cli: %s entry %s declares no kind", section, describeWhere(where)), ctx)
	default:
		ctx := copyWhere(where)
		ctx["section"] = section
		ctx["kind"] = strings.TrimSpace(kind)
		ctx["expected"] = wiringKindSQL
		return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("cli: %s entry %s declares an unknown kind %q; shared wiring accepts kind: sql",
				section, describeWhere(where), strings.TrimSpace(kind)), ctx)
	}
}

// wiringConnectionName resolves an entry's connection: against the pushed
// manifest and returns the name to store.
//
// The undeclared case cannot be caught any lower down. The column carries no
// foreign key on purpose — an entry of a non-database kind names no connection,
// and absence is the empty string rather than a NULL — and one connections: entry
// is one POOL, so a name with no manifest entry does not fall back to a default:
// it fails the registry build on the next boot of every instance that reads this
// wiring. The declared names are listed in the refusal because the mistake is
// almost always a typo, and the answer is usually visible in the list.
func wiringConnectionName(raw string, declared map[string]struct{}, what string, where map[string]any) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		// Every kind shared wiring accepts is database-backed, so a missing
		// connection is a missing pool. This mirrors the seed builder's own refusal
		// for the same entry, code included.
		ctx := copyWhere(where)
		return "", aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("cli: wiring %s %s names no connection; name an entry of the document's connections: block",
				what, describeWhere(where)), ctx)
	}
	if _, ok := declared[name]; ok {
		return name, nil
	}
	ctx := copyWhere(where)
	ctx["connection"] = name
	ctx["declared"] = sortedKeys(declared)
	return "", aerr.WithContext(aerr.APERTURE_WIRING_CONNECTION_UNDECLARED,
		fmt.Sprintf("cli: wiring %s %s names connection %q, which the pushed connections: manifest does not declare (declared: %s)",
			what, describeWhere(where), name, renderDeclared(declared)), ctx)
}

// checkWiringTTL refuses a ttl: that is not a Go duration.
//
// The TTL is STORED as the text the operator wrote — a read back has to be
// re-pushable byte for byte, and a time.Duration would round-trip "30s" as
// "30000000000" — so nothing below this layer ever parses it. If a push accepted
// "30" it would be accepted here, stored, read back, and then fail the registry
// build on every instance at once.
func checkWiringTTL(raw, what string, where map[string]any) error {
	ttl := strings.TrimSpace(raw)
	if ttl == "" {
		return nil
	}
	if _, err := time.ParseDuration(ttl); err != nil {
		ctx := copyWhere(where)
		ctx["ttl"] = ttl
		return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			fmt.Sprintf("cli: wiring %s %s has an invalid ttl %q; write a Go duration such as \"30s\" or \"5m\"",
				what, describeWhere(where), ttl), ctx)
	}
	return nil
}

// checkWiringAgainstModel is the half of the validation that needs the STORE: the
// model-state checks a document alone cannot answer.
//
// Both refusals here are foreign-key facts stated actionably. The database would
// refuse the same writes on its own — apt_wiring_providers.object_type carries a
// real edge ON DELETE RESTRICT — but "constraint failed" does not tell an operator
// which of the two names in the statement was wrong, and a raw violation arrives
// with the generic storage fixups rather than "declare this object type first".
//
// The order matters. An empty store is checked BEFORE the per-entry types,
// because a store with nothing in it would otherwise report the first provider's
// object type as missing — true, but the wrong sentence: the operator has not
// mistyped a type, they have pointed at a database that holds no model at all,
// and usually at the wrong database.
func checkWiringAgainstModel(ctx context.Context, store model.Storage, set model.WiringSet) error {
	types, err := store.ListObjectTypes(ctx)
	if err != nil {
		if aerr.CodeOf(err) != "" {
			return err
		}
		return aerr.Wrap(aerr.APERTURE_STORAGE, "cli: reading the store's object types", err)
	}
	if len(types) == 0 {
		return aerr.WithContext(aerr.APERTURE_WIRING_NO_MODEL_STATE,
			"cli: the target store declares no object types, so it holds no model state for this wiring to be wiring FOR; apply the model state first (aperture import, or aperture serve --seed) and then push the wiring — and check the --store DSN, because a typo names an empty database Setup will happily create",
			map[string]any{"object_types": 0})
	}
	known := make(map[string]struct{}, len(types))
	for _, ot := range types {
		known[ot.Name] = struct{}{}
	}
	// set.Providers is already sorted, so a set with two unknown types always
	// reports the same one and the refusal is reproducible.
	for _, p := range set.Providers {
		if _, ok := known[p.ObjectType]; ok {
			continue
		}
		return aerr.WithContext(aerr.APERTURE_WIRING_OBJECT_TYPE_UNKNOWN,
			fmt.Sprintf("cli: the wiring declares a provider for object type %q, which the store's object_types table has no row for; declare the type with the model state before pushing wiring that serves it", p.ObjectType),
			map[string]any{"object_type": p.ObjectType})
	}
	// field_types: is deliberately NOT checked against the table. A declaration may
	// name a type whose objects a LOCAL seed lists inline, which needs no
	// object_types row at all, and apt_wiring_field_types.object_type carries no
	// foreign key for exactly that reason. Checking it here would refuse a
	// declaration every loader accepts.
	return nil
}

// copyWhere clones an error-context map so two refusals built from the same
// location cannot share (and mutate) one map.
func copyWhere(where map[string]any) map[string]any {
	out := make(map[string]any, len(where)+3)
	for k, v := range where {
		out[k] = v
	}
	return out
}

// describeWhere renders an entry's location for a message: the object type for a
// providers: entry, the slot for an attribute_providers: one.
func describeWhere(where map[string]any) string {
	if v, ok := where["object_type"]; ok {
		return fmt.Sprintf("for object type %q", v)
	}
	if v, ok := where["subject"]; ok {
		return fmt.Sprintf("for subject %q", v)
	}
	return "(unnamed)"
}

// sortedKeys returns a name set in a stable order, for an error context.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderDeclared spells the manifest for a message. "(none)" rather than an empty
// list, because "declared: " reads as a formatting bug.
func renderDeclared(set map[string]struct{}) string {
	names := sortedKeys(set)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(quoteEach(names), ", ")
}

// quoteEach quotes each name so a multi-name message stays readable. It is the
// CLI's own copy of the same courtesy seed extends: nothing is shared across the
// package boundary for three lines.
func quoteEach(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}
