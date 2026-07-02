// Package filter applies server-side field predicates to a list of entity JSON
// bodies before they are returned to a client. It is the business logic behind
// the admin UI's data-grid filters: the client sends a Spec (a set of
// predicates), the server evaluates it against each entity and returns only the
// matches, so filtered-out rows never cross the wire.
//
// The entities are heterogeneous (accounts, principals, roles, grants, ...), so
// predicates address fields by their JSON key and evaluation is dynamic (over
// the decoded map), not per-type. Text comparisons are case-insensitive. Array
// fields (e.g. a principal's Roles) match if ANY element satisfies the predicate.
package filter

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Operators. The set is deliberately small ("core"): equality, substring,
// prefix, and emptiness. An unknown operator makes its predicate a no-op.
const (
	OpEq       = "eq"       // field equals value (case-insensitive)
	OpContains = "contains" // field contains value (case-insensitive substring)
	OpStarts   = "starts"   // field starts with value (case-insensitive prefix)
	OpEmpty    = "empty"    // field is empty / absent (value ignored)
)

// Predicate is one field test.
type Predicate struct {
	Field string
	Op    string
	Value string
}

// Spec is a set of predicates and how to combine them.
type Spec struct {
	Predicates []Predicate
	// MatchAny combines predicates with OR when true, AND (the default) when false.
	MatchAny bool
}

// Empty reports whether the spec would constrain nothing (no usable predicates),
// so callers can skip work entirely.
func (s Spec) Empty() bool {
	for _, p := range s.Predicates {
		if p.usable() {
			return false
		}
	}
	return true
}

func (p Predicate) usable() bool {
	if strings.TrimSpace(p.Field) == "" {
		return false
	}
	switch p.Op {
	case OpEq, OpContains, OpStarts:
		return p.Value != ""
	case OpEmpty:
		return true
	default:
		return false // unknown op => not usable => ignored
	}
}

// Apply returns the subset of entities (each a JSON object body) that satisfy the
// spec. An empty spec returns the input unchanged. Entities whose JSON does not
// decode to an object are kept (fail-open: a filter must never silently hide a
// row it could not evaluate).
func Apply(entities []string, spec Spec) []string {
	preds := make([]Predicate, 0, len(spec.Predicates))
	for _, p := range spec.Predicates {
		if p.usable() {
			preds = append(preds, p)
		}
	}
	if len(preds) == 0 {
		return entities
	}
	out := make([]string, 0, len(entities))
	for _, body := range entities {
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			out = append(out, body) // undecodable => keep
			continue
		}
		if matches(obj, preds, spec.MatchAny) {
			out = append(out, body)
		}
	}
	return out
}

func matches(obj map[string]any, preds []Predicate, any bool) bool {
	for _, p := range preds {
		ok := evalPredicate(obj, p)
		if any && ok {
			return true
		}
		if !any && !ok {
			return false
		}
	}
	// AND with all true, or OR with all false.
	return !any
}

// evalPredicate tests one predicate against the decoded entity. A field that is
// an array satisfies a value predicate if ANY element does; emptiness is true for
// an absent field, an empty string, or an empty array.
func evalPredicate(obj map[string]any, p Predicate) bool {
	raw, present := obj[p.Field]
	if p.Op == OpEmpty {
		return isEmpty(present, raw)
	}
	needle := strings.ToLower(p.Value)
	for _, s := range stringValues(raw) {
		if testString(strings.ToLower(s), p.Op, needle) {
			return true
		}
	}
	return false
}

func testString(hay, op, needle string) bool {
	switch op {
	case OpEq:
		return hay == needle
	case OpContains:
		return strings.Contains(hay, needle)
	case OpStarts:
		return strings.HasPrefix(hay, needle)
	default:
		return false
	}
}

func isEmpty(present bool, raw any) bool {
	if !present || raw == nil {
		return true
	}
	switch v := raw.(type) {
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	default:
		return false
	}
}

// stringValues flattens a field value into the string(s) to test: a scalar
// becomes one string; an array becomes one string per element.
func stringValues(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			out = append(out, scalarString(e))
		}
		return out
	default:
		return []string{scalarString(v)}
	}
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers decode to float64; render integers without a trailing .0.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
