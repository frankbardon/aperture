// Package errors holds Aperture's error taxonomy. Every failure surfaced by
// the library is an APERTURE_* coded error so the CLI, Twirp, and MCP surfaces
// can translate it to a transport-appropriate status without string-matching
// human-readable messages.
//
// Codes are SCREAMING_SNAKE, namespaced with the APERTURE_ prefix, and each one
// carries a Message + Fixup metadata entry in Registry (the orbit pattern). An
// error that already carries an APERTURE_* code passes through Aperture's
// wrapping verbatim — CodeOf recovers the existing code and it is never
// re-stamped.
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
	// APERTURE_PROVIDER_INVALID — an ObjectProvider registration is malformed: an
	// empty object-type key, a nil provider, or a duplicate registration for a
	// type that already has a provider. Raised by the provider registry at
	// registration time, before any object metadata can be fetched.
	APERTURE_PROVIDER_INVALID Code = "APERTURE_PROVIDER_INVALID"
	// APERTURE_PROVIDER_UNREGISTERED — metadata for an object-type was requested
	// (fetch, enumerate, or invalidate) but no ObjectProvider is registered for
	// that type. The object-type is the identity's terminal segment type.
	APERTURE_PROVIDER_UNREGISTERED Code = "APERTURE_PROVIDER_UNREGISTERED"
	// APERTURE_PROVIDER_FETCH — a host ObjectProvider's Fetch/List/Query returned
	// a plain (uncoded) error. The cause is wrapped verbatim; provider errors that
	// already carry an APERTURE_* code (e.g. APERTURE_NOT_FOUND for an
	// absent object) pass through unwrapped instead.
	APERTURE_PROVIDER_FETCH Code = "APERTURE_PROVIDER_FETCH"
	// APERTURE_RULE_INVALID — a rule AST is structurally malformed: an unknown
	// node type, a logical node with the wrong child count, a comparison missing
	// an operand, an empty/ill-typed literal, or a variable reference whose path
	// is not a dotted identifier. Raised by AST validation before a rule can be
	// compiled.
	APERTURE_RULE_INVALID Code = "APERTURE_RULE_INVALID"
	// APERTURE_RULE_UNKNOWN_VARIABLE — a rule references a variable whose root is
	// not one of the exposed context roots (object, principal, account, action).
	// Raised by AST validation before evaluation, so a typo'd or unbound variable
	// is caught at compile time rather than silently reading nil.
	APERTURE_RULE_UNKNOWN_VARIABLE Code = "APERTURE_RULE_UNKNOWN_VARIABLE"
	// APERTURE_RULE_TYPE_ERROR — a rule fails the expression type-checker at
	// compile time: a type-incompatible comparison, a non-boolean result, or a
	// call to a function that is not registered. Surfaced before evaluation so an
	// ill-typed rule never reaches the hot path.
	APERTURE_RULE_TYPE_ERROR Code = "APERTURE_RULE_TYPE_ERROR"
	// APERTURE_RULE_EVAL — a compiled rule failed at evaluation time: the
	// expression runtime returned an error, or the result was not a boolean. The
	// underlying cause is wrapped verbatim.
	APERTURE_RULE_EVAL Code = "APERTURE_RULE_EVAL"
	// APERTURE_RULE_NOT_FOUND — a scope strategy named a rule reference that the
	// configured rule source cannot resolve. Raised before evaluation when the
	// rule-backed inclusive/exclusive path looks up its rule.
	APERTURE_RULE_NOT_FOUND Code = "APERTURE_RULE_NOT_FOUND"
	// APERTURE_DELEGATION_DENIED — a delegator tried to bestow (or revoke) a grant
	// that exceeds the authority they hold in the active account: it is not a
	// subset of their own effective allow grants, they hold no "may delegate"
	// right over the target object, or the grant is stamped to an account they are
	// not a member of (a cross-account bestow). The Context "reason" names which
	// of these failed. Bestow fails closed — when authority cannot be proven, it
	// is denied.
	APERTURE_DELEGATION_DENIED Code = "APERTURE_DELEGATION_DENIED"
	// APERTURE_DELEGATION_NOT_DELEGATABLE — a delegator tried to bestow a grant on
	// a permission that is not flagged delegatable. Delegation is opt-in per
	// permission; an unflagged permission can never be handed on, regardless of
	// the delegator's own authority.
	APERTURE_DELEGATION_NOT_DELEGATABLE Code = "APERTURE_DELEGATION_NOT_DELEGATABLE"
	// APERTURE_IMPERSONATION_DENIED — an operator tried to start an impersonation
	// session it is not authorized to open: the operator or the target is not a
	// member of the active account (cross-account impersonation is refused), the
	// operator holds no impersonation right whose object covers the target, or a
	// become session was requested while the operator holds only the weaker
	// augment right. The Context "reason" names which guard failed. Start fails
	// closed — when authority cannot be proven, no session is issued.
	APERTURE_IMPERSONATION_DENIED Code = "APERTURE_IMPERSONATION_DENIED"
	// APERTURE_IMPERSONATION_EXPIRED — a time-boxed impersonation session was
	// presented past its expiry. The elevation is dropped: a surface that guards
	// on the session up front gets this code, while the engine's decision path
	// fails closed to the operator's own (un-elevated) authority rather than
	// erroring. Either way an expired session never elevates.
	APERTURE_IMPERSONATION_EXPIRED Code = "APERTURE_IMPERSONATION_EXPIRED"
	// APERTURE_UNAUTHENTICATED — a request could not be resolved to a known
	// principal: no credential was presented where one is required, or the
	// configured authenticator could not derive a principal id from the presented
	// credential (e.g. an empty bearer to the dev authenticator, or a verified
	// token missing the configured principal claim). It is distinct from
	// APERTURE_AUTHZ_DENIED — the caller is unknown, not under-privileged.
	APERTURE_UNAUTHENTICATED Code = "APERTURE_UNAUTHENTICATED"
	// APERTURE_INVALID_TOKEN — a presented bearer credential failed verification:
	// a malformed JWT, a bad signature, an unknown/mismatched issuer or audience,
	// an expired token, or a parsec-broker token that does not verify against the
	// configured keyring. The credential was supplied but is not trustworthy, so
	// the request is refused rather than treated as anonymous.
	APERTURE_INVALID_TOKEN Code = "APERTURE_INVALID_TOKEN"
	// APERTURE_TEMPLATE_INVALID — a provisioning template is structurally
	// malformed at DEFINITION time: an empty name, a version below 1, a parameter
	// with an empty/duplicate name or an unknown type, no template grants, a
	// template grant missing its subject/permission/effect/object, a malformed
	// ${param} reference token, or a grant that references a parameter the template
	// does not declare. Caught when the template is put, so a bad template can
	// never reach apply.
	APERTURE_TEMPLATE_INVALID Code = "APERTURE_TEMPLATE_INVALID"
	// APERTURE_TEMPLATE_PARAM — a template APPLY supplied bad parameters: a
	// required parameter is missing, an argument names a parameter the template
	// does not declare, or a value fails its declared type (a segment-typed value
	// that is not a legal identity component). Raised at apply time, before any
	// grant is expanded or written, so a bad parameter set never partially applies.
	APERTURE_TEMPLATE_PARAM Code = "APERTURE_TEMPLATE_PARAM"
	// APERTURE_AUTHZ_DENIED — an actor attempted a model mutation without holding
	// the admin authority tier that gates it: a system-tier (schema) mutation
	// without effective system-admin authority over system:*, or an account-tier
	// (grants/delegation) mutation without effective account-admin authority over
	// account:<acct>/admin:* in the TARGET account. Account-admin authority is
	// confined to its own account — an admin of one account is denied a mutation
	// scoped to another. The authority is resolved through the ordinary engine (an
	// effective-grant Check on the reserved admin action against the tier's
	// authority identity), so the denial is auditable and explainable like any
	// other decision. The gate fails closed — when the required tier cannot be
	// proven, the mutation is refused.
	APERTURE_AUTHZ_DENIED Code = "APERTURE_AUTHZ_DENIED"
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
	APERTURE_PROVIDER_INVALID: {
		Message: "object provider registration is invalid",
		Fixups: []string{
			"Register a non-nil provider under a non-empty object-type key.",
			"Register each object type at most once; check for a duplicate registration.",
		},
	},
	APERTURE_PROVIDER_UNREGISTERED: {
		Message: "no object provider is registered for the object type",
		Fixups: []string{
			"Register an ObjectProvider for the object type before fetching its metadata.",
			"Confirm the object identity's terminal segment type matches a registered provider key.",
		},
	},
	APERTURE_PROVIDER_FETCH: {
		Message: "object provider returned an error",
		Fixups: []string{
			"Inspect the wrapped cause for the underlying provider failure.",
			"Return APERTURE_NOT_FOUND from the provider for an object that does not exist.",
		},
	},
	APERTURE_RULE_INVALID: {
		Message: "rule AST is malformed",
		Fixups: []string{
			"Give each logical node the right child count: and/or take two or more, not takes exactly one.",
			"Give every comparison a left and right operand, and every literal a scalar value.",
			"Write variable references as dotted identifier paths, e.g. object.classification.",
		},
	},
	APERTURE_RULE_UNKNOWN_VARIABLE: {
		Message: "rule references an unknown variable",
		Fixups: []string{
			"Reference variables under a known context root: object, principal, account, or action.",
			"Check for a typo in the variable's root segment.",
		},
	},
	APERTURE_RULE_TYPE_ERROR: {
		Message: "rule failed expression type checking",
		Fixups: []string{
			"Compare compatible types and make the rule evaluate to a boolean.",
			"Call only functions registered with the rules engine.",
		},
	},
	APERTURE_RULE_EVAL: {
		Message: "rule evaluation failed",
		Fixups: []string{
			"Inspect the wrapped cause for the underlying evaluation failure.",
			"Ensure the rule expression yields a boolean for the supplied context.",
		},
	},
	APERTURE_RULE_NOT_FOUND: {
		Message: "the referenced rule was not found",
		Fixups: []string{
			"Confirm the rule reference exists in the configured rule source.",
		},
	},
	APERTURE_DELEGATION_DENIED: {
		Message: "the delegator may not bestow this grant",
		Fixups: []string{
			"Bestow only grants that are a subset of your own effective allow grants in the account (same action and scope strategy, an equal-or-more-specific object pattern).",
			"Confirm you hold a 'may delegate' right whose object pattern covers the grant's object.",
			"Bestow grants only within an account you are a member of; cross-account bestowal is rejected.",
		},
	},
	APERTURE_DELEGATION_NOT_DELEGATABLE: {
		Message: "the permission is not flagged delegatable",
		Fixups: []string{
			"Set Delegatable on the permission definition to allow it to be bestowed.",
		},
	},
	APERTURE_IMPERSONATION_DENIED: {
		Message: "the operator may not impersonate this target",
		Fixups: []string{
			"Impersonate only within an account both the operator and the target are members of; cross-account impersonation is refused.",
			"Confirm the operator holds an impersonation right (augment or become) whose object pattern covers the target's identity.",
			"Become mode requires the stronger become right; an augment right alone cannot become a target.",
		},
	},
	APERTURE_IMPERSONATION_EXPIRED: {
		Message: "the impersonation session has expired",
		Fixups: []string{
			"Start a fresh impersonation session; sessions are time-boxed and expire automatically.",
		},
	},
	APERTURE_UNAUTHENTICATED: {
		Message: "the request could not be resolved to a known principal",
		Fixups: []string{
			"Present a credential: send an Authorization: Bearer <token> header.",
			"With the dev/static authenticator the bearer IS the principal id; send a non-empty value.",
			"Confirm the verified token carries the configured principal claim (APERTURE_AUTH_PRINCIPAL_CLAIM, default 'sub').",
		},
	},
	APERTURE_INVALID_TOKEN: {
		Message: "the presented bearer credential failed verification",
		Fixups: []string{
			"Confirm the token is a well-formed JWT signed by the configured issuer's keys.",
			"Check the token issuer and audience match APERTURE_OIDC_ISSUER and APERTURE_OIDC_AUDIENCE, and that it has not expired.",
			"For a parsec adapter, confirm the token was minted by the broker sharing the configured keyring/secret.",
		},
	},
	APERTURE_TEMPLATE_INVALID: {
		Message: "the provisioning template is malformed",
		Fixups: []string{
			"Give the template a non-empty name, a version of at least 1, and at least one grant.",
			"Declare every parameter a grant references; write references as ${name} with a declared parameter.",
			"Give each template grant a valid subject, a permission id, an allow/deny effect, and a non-empty object pattern.",
		},
	},
	APERTURE_TEMPLATE_PARAM: {
		Message: "the template apply supplied invalid parameters",
		Fixups: []string{
			"Supply a value for every parameter the template declares, and no parameters it does not.",
			"A segment-typed parameter value must be a legal identity component: letters, digits, and -._~@+ only.",
		},
	},
	APERTURE_AUTHZ_DENIED: {
		Message: "the actor lacks the admin authority tier required for this mutation",
		Fixups: []string{
			"Schema mutations (permission types, roles, object-types, providers, templates, rules) require system-admin authority: an allow grant on the admin action whose object covers system:*.",
			"Grant and delegation mutations require account-admin authority in the TARGET account: an allow grant on the admin action whose object covers account:<acct>/admin:*.",
			"Account-admin authority is confined to its own account; obtain authority in the account the mutation targets, or hold a broader (e.g. **) grant.",
		},
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
	APERTURE_PROVIDER_INVALID,
	APERTURE_PROVIDER_UNREGISTERED,
	APERTURE_PROVIDER_FETCH,
	APERTURE_RULE_INVALID,
	APERTURE_RULE_UNKNOWN_VARIABLE,
	APERTURE_RULE_TYPE_ERROR,
	APERTURE_RULE_EVAL,
	APERTURE_RULE_NOT_FOUND,
	APERTURE_DELEGATION_DENIED,
	APERTURE_DELEGATION_NOT_DELEGATABLE,
	APERTURE_IMPERSONATION_DENIED,
	APERTURE_IMPERSONATION_EXPIRED,
	APERTURE_UNAUTHENTICATED,
	APERTURE_INVALID_TOKEN,
	APERTURE_TEMPLATE_INVALID,
	APERTURE_TEMPLATE_PARAM,
	APERTURE_AUTHZ_DENIED,
}

// Message returns the canonical message for a code, or empty when the code has
// no Registry entry.
func Message(code Code) string {
	return Registry[code].Message
}
