package seed

import (
	"fmt"
	"slices"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// FieldType declares the type of selected metadata fields for ONE object type,
// so an inline objects: entry gets the same load-time validation a typed CSV
// column gets:
//
//	field_types:
//	  - object_type: brand
//	    fields:
//	      hired_at: date
//	      last_seen: datetime
//
//	objects:
//	  - id: account:acme/brand:1
//	    metadata:
//	      hired_at: "2026-03-04"
//
// # Quote date values in YAML
//
// The quotes above are not decoration. YAML resolves an UNQUOTED calendar day as
// a !!timestamp, and the seed loader's YAML path normalises through JSON, so
// `hired_at: 2026-03-04` reaches this loader already widened to
// "2026-03-04T00:00:00Z" — a timestamp, which a field declared "date" rejects.
// That widening happens in the YAML decoder, before any part of this package can
// see the document, so it cannot be undone here: by the time the value arrives,
// a day the author wrote and a midnight the author wrote are the same bytes.
// Quoting keeps the value the string it is meant to be, and the rejection says
// so when the instant is exactly midnight. A JSON seed has no such trap — JSON
// has no date type — so the two formats agree on every quoted value.
//
// # Why the section exists
//
// The objects: section spells the metadata value model DIRECTLY in YAML — no
// encoding, no delimiter, no type suffix — which is its whole appeal, and also
// the one thing it cannot express. A CSV header can say "hired_at:date"; a YAML
// mapping has nowhere to put that, so `hired_at: 2026-02-30` loads happily as an
// ordinary string and only becomes visible months later as a rule that silently
// never matches. This section is the missing declaration, and nothing more.
//
// # Why it is a separate top-level section
//
// The obvious home looks like the existing object_types: entries, and it is the
// wrong one: object_types: is MODEL STATE — Apply writes a row for each, and
// Export rebuilds them by reading storage back. A field-type declaration is
// runtime WIRING, exactly like providers: and objects:, and hanging wiring off a
// model entity would mean every export silently dropped it. So it sits beside
// the two sections it belongs with, in the same shape as providers: — a list of
// entries each naming its object_type — rather than as a map keyed by type, so
// that a duplicate declaration is a detectable error rather than a last-one-wins
// merge, and so the three wiring sections read alike.
//
// It is nonetheless one of the four SHARED wiring sections and objects: is not: a
// declaration here says how to READ a field, which is the same kind of fact a
// providers: entry carries and is safe to copy to a second instance, where an
// objects: entry carries the field's VALUE. `aperture wiring push` writes it to
// apt_wiring_field_types and `aperture wiring pull` reads it back under this key.
// Apply still writes nothing for it and an export still reproduces none of it — see
// skills/shared-wiring.md for why those are three different questions.
//
// # What it deliberately is not
//
// It is a DATE-TYPE declaration, not a general metadata schema. There is no
// required:, no default:, no enum:, no pattern:, no int/float/bool, and no
// nested field path. The value model already governs shape, depth, and size; the
// only thing it cannot govern is which strings a host means as dates, because
// nothing in a string says so. Everything else here would be a second schema
// language competing with the value model.
//
// Consequently a declared field is NOT a required field. Declaring a type says
// what the field must look like IF it is present; an object that omits it is
// perfectly valid, and so is an object type that has no objects: entries at all
// (a seed may declare field types for a type served by a providers: entry).
type FieldType struct {
	// ObjectType is the type whose objects: entries the declaration applies to
	// (e.g. "brand"), matched against each object's identity terminal-segment
	// type exactly as providers: is. Each type may be declared at most once.
	ObjectType string `yaml:"object_type" json:"object_type"`
	// Fields maps a metadata field name to its declared type: "date" or
	// "datetime", lower-case and exact. The spelling is the CSV loader's column
	// suffix vocabulary with the colon removed — the same two words mean the
	// same two things in both loaders, and neither accepts an alias.
	Fields map[string]string `yaml:"fields" json:"fields"`
}

const (
	// fieldTypeDate declares a field to hold a calendar day, stored as the
	// canonical provider.DateLayout string.
	fieldTypeDate = "date"
	// fieldTypeDateTime declares a field to hold an instant, stored as the
	// canonical provider.DateTimeLayout string. It is a separate declaration
	// rather than a modifier on the day form for the same reason the CSV loader
	// keeps ":date" and ":datetime" apart: a field is a day or it is an instant,
	// and holding both in one field is exactly what the declaration rejects.
	fieldTypeDateTime = "datetime"
	// fieldTypeExpected names the whole accepted vocabulary, for a diagnostic
	// that has to say what WOULD have been accepted.
	fieldTypeExpected = fieldTypeDate + " or " + fieldTypeDateTime
)

// fieldTypeIndex validates the field_types: section and returns it as
// object-type -> field -> declared type.
//
// It runs before anything is registered, so a malformed declaration fails the
// build even when the document declares no objects at all: a type nobody
// currently has inline entries for is a normal thing to declare (the entries may
// be arriving, or the type may be served by a providers: entry), and a typo in
// one must not wait for an entry to expose it.
//
// The section is validated in sorted order at both levels, so a document with
// several offending declarations always reports the same one.
func (d *Document) fieldTypeIndex() (map[string]map[string]string, error) {
	if len(d.FieldTypes) == 0 {
		return nil, nil
	}
	index := make(map[string]map[string]string, len(d.FieldTypes))
	for _, ft := range d.FieldTypes {
		if ft.ObjectType == "" {
			return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
				"seed: field type declaration is missing object_type")
		}
		if _, dup := index[ft.ObjectType]; dup {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"seed: object type declares field types more than once; merge the two entries",
				map[string]any{"object_type": ft.ObjectType})
		}
		names := make([]string, 0, len(ft.Fields))
		for name := range ft.Fields {
			names = append(names, name)
		}
		slices.Sort(names)

		fields := make(map[string]string, len(ft.Fields))
		for _, name := range names {
			typ := ft.Fields[name]
			if name == "" {
				return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
					"seed: field type declaration has an empty field name",
					map[string]any{"object_type": ft.ObjectType})
			}
			// An unrecognised spelling is REJECTED, never ignored: a declaration
			// the loader quietly skipped would read exactly like one it honoured
			// while validating nothing, which is the failure this section exists
			// to remove. "timestamp", "Date", and "time" are all errors.
			if typ != fieldTypeDate && typ != fieldTypeDateTime {
				return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
					"seed: unknown declared field type",
					map[string]any{
						"object_type": ft.ObjectType, "field": name,
						"type": typ, "expected": fieldTypeExpected,
					})
			}
			fields[name] = typ
		}
		index[ft.ObjectType] = fields
	}
	return index, nil
}

// applyFieldTypes validates and canonicalises one object's declared fields in
// place, after the value model has already accepted the metadata.
//
// Three behaviours are shared verbatim with the CSV loader, because two loaders
// that disagree about what a date is are worse than either one alone:
//
//   - The stored value is the CANONICAL text, not the text as written, so
//     "2026-03-04T01:02:03.456Z" and "2026-03-04T01:02:03" both become
//     "2026-03-04T01:02:03Z" and two objects naming one instant are one string —
//     which is what makes a Filter.Fields equality predicate over the field mean
//     anything.
//   - The declared type fixes the GRANULARITY. A "date" field rejects a
//     timestamp and a "datetime" field rejects a bare day, rather than quietly
//     widening the day to midnight; the implicit repair is precisely the silent
//     assumption the declaration exists to refuse. Write the midnight out.
//   - An EMPTY value omits the field rather than storing an empty string,
//     following the CSV loader's empty-cell rule. An absent date is meaningfully
//     different from any date, and a zero time would silently satisfy every
//     "before <anything>" rule ever written against the field.
//
// A field that is absent from the object's metadata is skipped, not faulted:
// the section declares a TYPE, never a requirement.
//
// Every rejection is APERTURE_CONFIG_INVALID naming the object id and the field,
// and carries the provider.DateReason for a parse failure so a caller can branch
// on the cause. It NEVER carries the value: a date is frequently personal data —
// a birth date, a termination date — and an error is a thing that gets logged.
// That is also why the value model's own error is classified and then dropped
// rather than wrapped; wrapping would additionally hide the reason, since
// provider.DateReasonOf reads the OUTERMOST coded error.
func applyFieldTypes(id, typ string, md provider.Metadata, decl map[string]string) error {
	if len(decl) == 0 || len(md) == 0 {
		return nil
	}
	// Declared fields are walked in sorted order so an object with several
	// offending values always reports the same one.
	names := make([]string, 0, len(decl))
	for name := range decl {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		raw, present := md[name]
		if !present {
			continue // a declared type is not a requirement
		}
		declared := decl[name]
		s, ok := raw.(string)
		if !ok {
			// A YAML mapping can hold any shape, so unlike a CSV cell this case
			// exists — and it deserves its own diagnostic rather than a
			// confusing parse failure about a value that was never text.
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				fmt.Sprintf("seed: object %q metadata field %q is declared %s but holds a %s, not a string",
					id, name, declared, metadataKind(raw)),
				map[string]any{
					"id": id, "object_type": typ, "field": name,
					"type": declared, "kind": metadataKind(raw), "expected": dateLayoutFor(declared),
				})
		}
		if s == "" {
			delete(md, name)
			continue
		}
		v, err := provider.ParseDateValue(s)
		if err != nil {
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				fmt.Sprintf("seed: object %q metadata field %q cannot be parsed as its declared type %s (expected %s)",
					id, name, declared, dateLayoutFor(declared)),
				map[string]any{
					"id": id, "field": name, "type": declared,
					"reason": string(provider.DateReasonOf(err)), "expected": dateLayoutFor(declared),
				})
		}
		if want := dateGranularityFor(declared); v.Granularity() != want {
			// Not a parse failure, so no "reason" key: provider.DateReasonOf
			// correctly reports "" for this error, exactly as it does for the
			// CSV loader's granularity rejection.
			ctx := map[string]any{
				"id": id, "field": name, "type": declared,
				"granularity": v.Granularity().String(), "expected": dateLayoutFor(declared),
			}
			msg := fmt.Sprintf(
				"seed: object %q metadata field %q is a valid date of the wrong granularity for its declared type %s (expected %s)",
				id, name, declared, dateLayoutFor(declared))
			if declared == fieldTypeDate && isMidnightUTC(v.Time()) {
				// Overwhelmingly the likeliest cause, and baffling without a
				// pointer: YAML resolves an UNQUOTED calendar day as a
				// !!timestamp, which reaches this loader as midnight UTC. The
				// hint names the fix, never the value.
				msg += "; note that an unquoted calendar day in YAML is resolved as a timestamp — quote it"
				ctx["hint"] = "quote the value so YAML keeps it a string"
			}
			return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID, msg, ctx)
		}
		md[name] = v.String()
	}
	return nil
}

// isMidnightUTC reports whether t is exactly midnight — the fingerprint of a
// calendar day YAML widened into a timestamp on the way in.
func isMidnightUTC(t time.Time) bool {
	h, m, s := t.Clock()
	return h == 0 && m == 0 && s == 0
}

// dateLayoutFor names the one canonical layout a declared type accepts, for a
// diagnostic. It is the layout, never a value.
func dateLayoutFor(declared string) string {
	if declared == fieldTypeDateTime {
		return provider.DateTimeLayout
	}
	return provider.DateLayout
}

// dateGranularityFor is the granularity a declared type says its values carry.
func dateGranularityFor(declared string) provider.DateGranularity {
	if declared == fieldTypeDateTime {
		return provider.GranularityDateTime
	}
	return provider.GranularityDate
}

// metadataKind names the shape of an already-normalised metadata value, so a
// rejection can say what the document held without repeating the value itself.
// It is the post-normalizeNumbers counterpart of jsonKind, which sees
// json.Number rather than the int64/float64 the loader stores.
func metadataKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
