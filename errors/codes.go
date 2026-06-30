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
	// APERTURE_NOT_FOUND — a referenced principal, role, object, or grant does
	// not exist in the active account scope.
	APERTURE_NOT_FOUND Code = "APERTURE_NOT_FOUND"
	// APERTURE_STORAGE — the underlying Storage implementation returned an error
	// (query, write, or schema setup).
	APERTURE_STORAGE Code = "APERTURE_STORAGE"
	// APERTURE_CONFIG_INVALID — configuration (env vars or YAML) was read but is
	// malformed or internally inconsistent.
	APERTURE_CONFIG_INVALID Code = "APERTURE_CONFIG_INVALID"
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
}

// AllCodes is the registry every gate walks. Append new codes here; the
// Registry table guards consistency.
var AllCodes = []Code{
	APERTURE_BOOT,
	APERTURE_UNIMPLEMENTED,
	APERTURE_INVALID_INPUT,
	APERTURE_NOT_FOUND,
	APERTURE_STORAGE,
	APERTURE_CONFIG_INVALID,
}

// Message returns the canonical message for a code, or empty when the code has
// no Registry entry.
func Message(code Code) string {
	return Registry[code].Message
}
