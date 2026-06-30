// Package errors holds Aperture's error taxonomy. Every failure surfaced by
// the library is an APERTURE_* coded error so the CLI, Twirp, and MCP surfaces
// can translate it to a transport-appropriate status without string-matching
// human-readable messages.
//
// Codes are SCREAMING_SNAKE, namespaced with the APERTURE_ prefix, and each one
// carries a Message + Fixup metadata entry in Registry (the pulse/orbit
// pattern). Pulse's own *CodedError values pass through verbatim and keep their
// upstream codes; Aperture never re-stamps them.
package errors

// Code is a typed identifier for an Aperture-domain error.
type Code string

// Aperture-domain codes. Each MUST have a Registry entry with a Message and
// either at least one Fixup or FixupNotApplicable=true. Gated by
// TestCodesHaveFixups. Append new codes to AllCodes when you add them.
const (
	// APERTURE_BOOT — aperture failed during startup wiring (config, storage,
	// engine, or auth assembly in the serve command).
	APERTURE_BOOT Code = "APERTURE_BOOT"
	// APERTURE_UNIMPLEMENTED — the caller invoked a surface that is recognised
	// but not yet wired. Placeholder CLI commands return this until their story
	// lands.
	APERTURE_UNIMPLEMENTED Code = "APERTURE_UNIMPLEMENTED"
	// APERTURE_INVALID_INPUT — caller-supplied input failed validation before
	// any decision or mutation was attempted.
	APERTURE_INVALID_INPUT Code = "APERTURE_INVALID_INPUT"
	// APERTURE_IDENTITY_INVALID — an object-identity or pattern string is
	// malformed: empty input or segment, a segment missing its `type:id` colon,
	// an empty type/id component, or an illegal character. Raised by the
	// identity grammar parser before the value can be matched or stored.
	APERTURE_IDENTITY_INVALID Code = "APERTURE_IDENTITY_INVALID"
	// APERTURE_NOT_FOUND — a referenced principal, role, object, or grant does
	// not exist in the active account scope.
	APERTURE_NOT_FOUND Code = "APERTURE_NOT_FOUND"
	// APERTURE_STORAGE — the underlying Storage implementation returned an error
	// (query, write, or schema setup).
	APERTURE_STORAGE Code = "APERTURE_STORAGE"
	// APERTURE_CONFIG_INVALID — configuration (env vars or YAML) was read but is
	// malformed or internally inconsistent.
	APERTURE_CONFIG_INVALID Code = "APERTURE_CONFIG_INVALID"
	// APERTURE_ACTION_UNDECLARED — a permission was declared against an action
	// verb that the target object type does not declare in its validated verb
	// set. Typed-action validation rejects free-form actions before a permission
	// can be persisted or granted.
	APERTURE_ACTION_UNDECLARED Code = "APERTURE_ACTION_UNDECLARED"
	// APERTURE_SCOPE_INVALID — a permission's scope-strategy reference is
	// malformed: an unparseable spec, an unknown parameter, an empty value, or a
	// strategy whose required configuration (e.g. an inclusive/exclusive id-list
	// or rule) is missing. Raised by the scope resolver before a grant's object
	// membership can be decided.
	APERTURE_SCOPE_INVALID Code = "APERTURE_SCOPE_INVALID"
	// APERTURE_SCOPE_UNKNOWN_STRATEGY — a grant's permission names a scope
	// strategy key that is not registered in the active scope registry. Built-in
	// keys are literal, implicit, inclusive, and exclusive; host code may register
	// more.
	APERTURE_SCOPE_UNKNOWN_STRATEGY Code = "APERTURE_SCOPE_UNKNOWN_STRATEGY"
	// APERTURE_SCOPE_LISTER_UNCONFIGURED — an implicit or exclusive resolver was
	// asked to enumerate ("all objects of the type"), but no ObjectLister is
	// configured. Enumeration is supplied by the object provider in E2-S2; until
	// then Members returns this code. Contains never needs the lister.
	APERTURE_SCOPE_LISTER_UNCONFIGURED Code = "APERTURE_SCOPE_LISTER_UNCONFIGURED"
	// APERTURE_SCOPE_RULE_UNCONFIGURED — an inclusive or exclusive resolver was
	// configured with a rule reference, but no RuleEvaluator is wired. Rule-backed
	// scope membership lands in E2-S3; until then the rule path returns this code.
	APERTURE_SCOPE_RULE_UNCONFIGURED Code = "APERTURE_SCOPE_RULE_UNCONFIGURED"
)

// Metadata describes an Aperture code: the canonical human-readable Message and
// the actionable Fixup hints surfaced to operators. FixupNotApplicable marks a
// code for which no operator action is meaningful (e.g. an internal invariant).
type Metadata struct {
	// Message is the canonical one-line summary for the code.
	Message string
	// Fixups are short, actionable remediation hints.
	Fixups []string
	// FixupNotApplicable is true when no operator remediation is meaningful.
	FixupNotApplicable bool
}

// Registry maps every Aperture code to its metadata. It is the single source of
// truth for messages + fixups; TestCodesHaveFixups guards that AllCodes and
// Registry stay in lockstep.
var Registry = map[Code]Metadata{
	APERTURE_BOOT: {
		Message: "aperture failed to start",
		Fixups: []string{
			"Check the APERTURE_* environment variables and any --config file.",
			"Confirm the storage backend (memory or sqlite) is reachable.",
		},
	},
	APERTURE_UNIMPLEMENTED: {
		Message:            "this surface is not yet implemented",
		FixupNotApplicable: true,
	},
	APERTURE_INVALID_INPUT: {
		Message: "input failed validation",
		Fixups: []string{
			"Re-check the request shape against the command or API contract.",
		},
	},
	APERTURE_IDENTITY_INVALID: {
		Message: "object identity is malformed",
		Fixups: []string{
			"Use type:id segments joined by '/', e.g. account:acme/project:atlas/document:42.",
			"Ensure no segment is empty and every segment carries a ':' with a non-empty type and id.",
			"Remove illegal characters; types and ids allow letters, digits, and -._~@+ only ('*' marks a wildcard in patterns).",
		},
	},
	APERTURE_NOT_FOUND: {
		Message: "the referenced entity was not found",
		Fixups: []string{
			"Confirm the identifier exists in the current account scope.",
		},
	},
	APERTURE_STORAGE: {
		Message: "the storage backend returned an error",
		Fixups: []string{
			"Inspect the wrapped cause for the underlying storage failure.",
		},
	},
	APERTURE_CONFIG_INVALID: {
		Message: "configuration is invalid",
		Fixups: []string{
			"Validate the YAML config and APERTURE_* env vars against the docs.",
		},
	},
	APERTURE_ACTION_UNDECLARED: {
		Message: "action is not declared on the object type",
		Fixups: []string{
			"Add the action verb to the object type's declared action set, or grant a verb the type already declares.",
			"List the object type's actions to see the validated verb set.",
		},
	},
	APERTURE_SCOPE_INVALID: {
		Message: "scope strategy reference is malformed",
		Fixups: []string{
			"Use 'strategy' or 'strategy;param=value' form, e.g. inclusive;ids=account:acme/document:42.",
			"Give an inclusive/exclusive strategy an 'ids' list or a 'rule' reference; implicit takes no configuration.",
		},
	},
	APERTURE_SCOPE_UNKNOWN_STRATEGY: {
		Message: "scope strategy is not registered",
		Fixups: []string{
			"Use a built-in strategy (literal, implicit, inclusive, exclusive) or register the custom key with the scope registry.",
		},
	},
	APERTURE_SCOPE_LISTER_UNCONFIGURED: {
		Message:            "scope enumeration requires an object lister that is not configured",
		FixupNotApplicable: true,
	},
	APERTURE_SCOPE_RULE_UNCONFIGURED: {
		Message:            "scope rule path requires a rule evaluator that is not configured",
		FixupNotApplicable: true,
	},
}

// AllCodes is the registry every gate walks. Append new codes here; the
// Registry table guards consistency.
var AllCodes = []Code{
	APERTURE_BOOT,
	APERTURE_UNIMPLEMENTED,
	APERTURE_INVALID_INPUT,
	APERTURE_IDENTITY_INVALID,
	APERTURE_NOT_FOUND,
	APERTURE_STORAGE,
	APERTURE_CONFIG_INVALID,
	APERTURE_ACTION_UNDECLARED,
	APERTURE_SCOPE_INVALID,
	APERTURE_SCOPE_UNKNOWN_STRATEGY,
	APERTURE_SCOPE_LISTER_UNCONFIGURED,
	APERTURE_SCOPE_RULE_UNCONFIGURED,
}

// Message returns the canonical message for a code, or empty when the code has
// no Registry entry.
func Message(code Code) string {
	return Registry[code].Message
}
