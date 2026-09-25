package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// slotNames renders the closed set of attribute subjects for a message and a
// context, in provider.AttributeSlots() order. The provider package has the same
// helper unexported; this is the seed's spelling of the same closed set, and it
// is derived from AttributeSlots() rather than written out, so a fourth slot
// could never be legal here and unnamed in the error that refuses it.
func slotNames() []string {
	slots := provider.AttributeSlots()
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.String())
	}
	return out
}

// Attribute declares one SUBJECT's attribute bag INLINE in the seed document, so
// a deployment with a handful of principals needs no directory, no CSV, and no Go
// to make `principal.department` mean something:
//
//	attributes:
//	  - subject: user
//	    id: alice
//	    metadata:
//	      department: eng
//	      clearance: 3
//	      teams: [platform, infra]
//	  - subject: machine
//	    id: ci-runner
//	    metadata: {department: eng}
//	  - subject: account
//	    id: acme
//	    metadata: {plan: enterprise}
//
// It is the attribute seam's counterpart of the objects: section, and it is the
// same KIND of thing: runtime WIRING, not model state. BuildAttributeRegistry
// turns the block into a live *provider.AttributeRegistry backed by an in-memory
// provider.StaticAttributes per slot; Apply writes no row for it, and because the
// model is exported by reading storage back, an export does not reproduce it. The
// seed FILE is the source of truth for it, exactly as auth config is.
//
// # Why this is its own key, and not a metadata: field on principals:/accounts:
//
// principals: and accounts: are MODEL STATE — rows Apply writes and Export reads
// back. An attribute bag is not: it belongs to the host's directory, and Aperture
// never persists it (there is no column for it, by design — see the effort's "no
// schema change" fence). Hanging the bag off a model entry would put wiring inside
// state, and the export would then be one of two bad things: lossy, because it
// silently dropped the bags it could not read back out of storage, or untruthful,
// because it invented them. A separate key keeps the line where every other
// wiring section already keeps it.
type Attribute struct {
	// Subject names the attribute SLOT this entry belongs to: "user" or "machine"
	// for a principal, "account" for the tenant a decision is made in. The set is
	// closed — it is the parties a decision has — and an unknown value is
	// APERTURE_ATTRIBUTE_SLOT_UNKNOWN naming the three legal ones. It is spelled
	// subject: rather than kind: because "account" is a slot but not a principal
	// KIND, and a key called kind: would read as model.PrincipalKind.
	Subject string `yaml:"subject" json:"subject"`
	// ID is the bare attribute KEY the bag belongs to: a principal id for the user
	// and machine slots, an account id for the account slot. It is the host's own
	// handle and Aperture never parses it — unlike an objects: entry's id, which
	// is a segmented identity whose terminal segment names its type. There is no
	// type to derive here: the slot is declared, because a bare key has nothing to
	// derive one from.
	ID string `yaml:"id" json:"id"`
	// Metadata is the attribute bag, carried as raw JSON so the YAML and JSON
	// paths decode it identically and numbers keep their exact form until they are
	// normalised (see decodeAttributeMetadata). Legal shapes are the shared value
	// model's — a scalar, an array of scalars, or an object one further level deep
	// — because there is exactly ONE value model in Aperture and an attribute bag
	// is a value in it. Omit it for a subject that is declared but carries nothing.
	Metadata json.RawMessage `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// HasAttributeSources reports whether the document declares ANY attribute source
// at all — the one question a host asks before deciding whether to wire an
// attribute registry into its decision stack.
//
// It lives HERE, beside the sections it counts, rather than in the caller that
// asks it. internal/cli's hasObjectSources records the bug this placement exists
// to prevent: that gate was written over `providers:` alone and then a second
// object-metadata section (`objects:`) was added, so a seed declaring only inline
// metadata built a populated registry that nothing ever read — the data was
// invisible to rules and to scope enumeration, with no error anywhere. The gate
// was in a different package from the field list, so adding the field did not
// look like touching the gate.
//
// The second section has now landed: `attribute_providers:` (kind: csv | sql,
// see attribute_provider.go) is OR'd in below, in this method, and it counts
// exactly as much as an inline entry does — a seed whose only attribute source is
// an external one must wire the resolvers, or the directory it named would be
// read by nothing. Any THIRD section owes the same one-line edit here, and it is
// one edit in one place because the gate lives beside the fields it counts.
func (d *Document) HasAttributeSources() bool {
	if d == nil {
		return false
	}
	return len(d.Attributes) > 0 || len(d.AttributeProviders) > 0
}

// BuildAttributeRegistry constructs a live *provider.AttributeRegistry from the
// document's TWO attribute wiring sections, the way BuildRegistry constructs an
// object registry from providers: and objects::
//
//   - attribute_providers: declares an EXTERNAL source per slot (kind: csv | sql,
//     see attribute_provider.go), and
//   - attributes: declares bags INLINE, served per slot by an in-memory
//     provider.StaticAttributes registered with a TTL of 0 (inline data is fixed
//     for the life of the process, so a freshness window would only buy re-reads
//     of a value that cannot have changed).
//
// A slot claimed by BOTH sections is not an error and not a discard: the two are
// registered as the slot's two LAYERS — the attribute_providers: entry as the
// SHARED layer, the inline block as the LOCAL one — and a fetch reads their merge
// with the shared layer winning every key both serve, so an inline bag can add a
// key the external source does not carry and can never override one it does. The
// layered slots are reported by Document.AttributeCollisions and the rule itself
// is provider.AttributeLayer's.
//
// It always returns a usable registry — empty when the document declares neither
// section — so a caller can wire it unconditionally. An empty registry is not
// the same as no registry: a fetch against an unfilled slot reports
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, which the rules layer reads as "the
// host knows nothing here" and evaluates against the floor bag rather than
// failing the decision.
//
// Slots are filled in provider.AttributeSlots() order, not in the order the file
// happens to list them, so a document with two bad slots always fails on the same
// one and a build is reproducible. Within a slot, an inline section's DECLARATION
// ORDER is preserved — it is the order Enumerate returns.
//
// A missing or unknown subject:, a missing id:, a duplicate (subject, id) pair,
// metadata that is not a mapping, and a number no int64 or float64 can represent
// are APERTURE_CONFIG_INVALID naming the entry. So are a missing or unknown
// kind:, a csv entry with no path:, a sql entry with no get_one: or no
// connection:, a slot declared twice, and an unparseable ttl:. A value the shared
// value model rejects keeps the model's own APERTURE_METADATA_INVALID (see
// decodeAttributeMetadata for why that code is not replaced); an unknown
// subject: keeps APERTURE_ATTRIBUTE_SLOT_UNKNOWN; a connection: naming nothing
// the connections: block declares is APERTURE_SQL_PROVIDER_CONNECTION; and the
// account wildcard "*" as an id is APERTURE_ATTRIBUTE_PROVIDER_INVALID from the
// provider that refuses it — this loader does not restate the rules about what a
// KEY may be, because provider is their one authority.
//
// baseDir is the directory relative paths resolve against — typically the seed
// file's directory, "" for the process CWD. It is what a kind: csv entry's path:
// is joined to when it is not absolute; it was reserved in this signature one
// story before it was read, so that adding the file-backed sections broke no
// caller.
//
// # A kind: sql entry needs the document's pools, which this form does not have
//
// connections: is ONE shared pool set: the object providers and the attribute
// providers read through the same pools, and opening a second set behind the
// caller's back would double every deployment's connections and hand nothing back
// to close them. So this form passes no connections at all, and a kind: sql
// attribute provider fails here with APERTURE_SQL_PROVIDER_CONNECTION naming
// BuildAttributeRegistryWithConnections — the form that takes the *Connections
// BuildRegistryWithConnections already returned. A document whose attribute
// sources are inline or kind: csv needs no pool and builds fine through this one.
func (d *Document) BuildAttributeRegistry(baseDir string) (*provider.AttributeRegistry, error) {
	return d.buildAttributeRegistry(baseDir, nil, openAttributeSource)
}

// groupAttributes validates every inline entry and groups the records by slot.
//
// Entries for different subjects may be interleaved freely in the file — they are
// grouped here — but within a slot, declaration order is preserved.
//
// Ids are deduplicated PER SLOT, not across the whole section, and that is the
// one place this section deliberately differs from objects:. An objects: id is a
// full identity, so the same string twice is the same object twice however it is
// spelled. An attribute key is a bare opaque handle from a different namespace
// per slot: a tenant with the account id "acme" and a service principal called
// "acme" are two unrelated subjects, and refusing the second would be refusing a
// legal deployment. Within a slot the collision is real — the last writer would
// silently win, which is how one subject's attributes become another's.
func (d *Document) groupAttributes() (map[provider.AttributeSlot][]provider.AttributeRecord, error) {
	if len(d.Attributes) == 0 {
		return nil, nil
	}
	groups := make(map[provider.AttributeSlot][]provider.AttributeRecord, len(provider.AttributeSlots()))
	seen := make(map[provider.AttributeSlot]map[string]bool, len(provider.AttributeSlots()))

	for _, a := range d.Attributes {
		if a.Subject == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: attribute entry is missing subject",
				map[string]any{"id": a.ID})
		}
		// ParseAttributeSlot is the one crossing point between a bare string and a
		// slot this registry serves, and it stays the authority on the closed set:
		// its CODE is what this refusal reports, so the entry keeps
		// APERTURE_ATTRIBUTE_SLOT_UNKNOWN's fixups ("use one of the three declared
		// slots") rather than APERTURE_CONFIG_INVALID's generic ones. Only the
		// MESSAGE is the seed's, because the offending subject and the entry it
		// belongs to are the one thing the provider cannot know — and the message
		// is what a CLI prints and what lands in a bug report.
		slot, err := provider.ParseAttributeSlot(a.Subject)
		if err != nil {
			return nil, aerr.WithContext(aerr.CodeOf(err),
				fmt.Sprintf("seed: attribute entry %q declares an unknown subject %q; the subjects are %s",
					a.ID, a.Subject, strings.Join(slotNames(), ", ")),
				map[string]any{"subject": a.Subject, "id": a.ID, "subjects": slotNames()})
		}
		if a.ID == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: attribute entry is missing id",
				map[string]any{"subject": a.Subject})
		}
		if seen[slot] == nil {
			seen[slot] = make(map[string]bool)
		}
		if seen[slot][a.ID] {
			// Named here rather than left to NewStaticAttributes because the fault
			// is in the FILE: an author fixing it wants both halves of the pair,
			// since the same id under a different subject is perfectly legal.
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: attribute subject is declared more than once",
				map[string]any{"subject": a.Subject, "id": a.ID})
		}
		seen[slot][a.ID] = true

		md, err := decodeAttributeMetadata(a.Subject, a.ID, a.Metadata)
		if err != nil {
			return nil, err
		}
		groups[slot] = append(groups[slot], provider.AttributeRecord{ID: a.ID, Attributes: md})
	}
	return groups, nil
}

// decodeAttributeMetadata turns one entry's raw metadata into a validated
// provider.Metadata. It is decodeObjectMetadata's sibling and does the same two
// things the raw decode does not:
//
//   - NUMBERS are normalised the way every loader normalises them — an exact
//     integer that fits int64 becomes an int64, anything else a float64 — so
//     `principal.clearance == 3` answers the same whether the bag was authored in
//     YAML, in JSON, or (once E3-S1 lands) read from a CSV :int column.
//   - The bag is checked against the shared value model at LOAD time, through
//     provider.ValidateMetadata, so a shape the expression evaluator cannot
//     survive never reaches a Check. For an attribute bag that gate matters more
//     than it does for an object: an object's bag is read by the rules evaluating
//     against that one object, while a principal's is read by every rule against
//     every object in the decision.
//
// # Why a value-model rejection keeps the value model's code
//
// Every other malformed seed entry is APERTURE_CONFIG_INVALID, and this one is
// not. aerr.Wrap RE-STAMPS — it builds a fresh CodedError with whatever code it
// is handed, and CodeOf reports the outermost — so wrapping the rejection
// APERTURE_CONFIG_INVALID would replace APERTURE_METADATA_INVALID, whose fixups
// name the legal shapes, the depth cap and the size cap, with "the configuration
// is invalid", whose fixups are true of every bad seed there is. The guard keeps
// whatever the value model decided and only ADDS the entry it came from, which is
// the single fact the value model cannot know: it validates a bag, not a
// document.
func decodeAttributeMetadata(subject, id string, raw json.RawMessage) (provider.Metadata, error) {
	if len(raw) == 0 {
		return provider.Metadata{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber is the house pattern (rules/ast.go decodeScalar, csvprovider's
	// json column, decodeObjectMetadata): defer the int/float choice to
	// normalizeNumbers rather than floating every integer on the way in.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_CONFIG_INVALID, err,
			"seed: attributes for %s %q are undecodable", subject, id)
	}
	if v == nil {
		// An explicit `metadata:` with no value is a subject with no attributes,
		// not an error — the same reading as omitting the key.
		return provider.Metadata{}, nil
	}
	md, ok := v.(map[string]any)
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: attribute metadata must be a mapping of field names to values",
			map[string]any{"subject": subject, "id": id, "kind": jsonKind(v)})
	}
	// Fields are walked in sorted order so a bag with several offending values
	// always reports the same one.
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
		md[name] = val
	}
	// One call for the whole bag, not one per field: ValidateMetadata is the
	// documented entry point a loader calls once per subject, and it already walks
	// the fields in sorted order.
	if err := provider.ValidateMetadata(md); err != nil {
		if code := aerr.CodeOf(err); code != "" {
			return nil, aerr.Wrapf(code, err,
				"seed: attributes for %s %q are rejected by the value model", subject, id)
		}
		return nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
			"seed: attributes for %s %q are rejected by the value model", subject, id)
	}
	return md, nil
}
