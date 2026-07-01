package model

import (
	"encoding/json"
	"sort"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// Rule is a named, persisted rule definition: a stable reference name, optional
// documentation, and the rule AST in its canonical JSON form. The AST is the
// EXACT serialization the rules package (E2-S3) marshals — a rules.Node tree —
// carried here as an opaque json.RawMessage so the model layer stays decoupled
// from the rules engine while persisting the engine's own format byte-for-byte.
// The node editor (E7-S2) and the export/import state file (E5-S2) read and
// write this same AST; there is no second rule format.
//
// A Rule is identified by its Name (the reference a scope strategy resolves),
// so PutRule upserts on the name. Rule is a PUBLIC contract: E5-S2 serializes
// exactly this shape, so the field set is additive-only.
type Rule struct {
	// Name is the rule's stable reference name; it is the rule's identity. A scope
	// strategy names it, the engine resolves it through the rule source backed by
	// this storage, and the editor saves it. Non-empty.
	Name string
	// Description is optional human-readable documentation.
	Description string
	// AST is the rule's decision tree in the canonical JSON form the rules package
	// marshals (a rules.Node). It is stored verbatim so a round-trip is byte-stable
	// and the editor's format is preserved exactly. Non-empty and a JSON object.
	AST json.RawMessage
	// CreatedAt / UpdatedAt are stamped by the service layer and persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidateRule checks a rule is structurally well-formed at DEFINITION time: a
// non-empty name and an AST that is present and a JSON object. It deliberately
// does NOT type-check or compile the AST — that is the rules engine's job, run
// at import/edit time by the layer that owns the rules package — so the model
// stays free of an engine dependency. A nil/empty or non-object AST is rejected
// here so a structurally broken rule never reaches storage.
func ValidateRule(r Rule) error {
	if r.Name == "" {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule name is empty")
	}
	if len(r.AST) == 0 {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule has no AST", map[string]any{"rule": r.Name})
	}
	if !json.Valid(r.AST) {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule AST is not valid JSON", map[string]any{"rule": r.Name})
	}
	// The AST must be a JSON object (a rules.Node), not an array or scalar.
	trimmed := trimSpace(r.AST)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule AST must be a JSON object", map[string]any{"rule": r.Name})
	}
	return nil
}

// SortRules orders rules by name so list and export output is stable across
// backends.
func SortRules(rs []Rule) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
}

// trimSpace strips leading JSON whitespace so the object-shape check inspects the
// first significant byte without pulling in strings/bytes for one call.
func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return b[i:]
		}
	}
	return b[i:]
}
