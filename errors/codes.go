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
	// APERTURE_STORAGE_SCHEMA_INCOMPATIBLE — an existing database was written by a
	// build of Aperture whose schema this build cannot read, and Setup refused it
	// at startup rather than let a read misinterpret the rows later. Aperture has
	// no migration tool and no schema versioning by design: a schema break is a
	// hard break, and the only remedy is a new database. Distinct from
	// APERTURE_STORAGE because nothing failed — the storage engine answered every
	// query correctly; it is the SHAPE of what it answered with that is wrong,
	// and the fix is an operator action, not a retry.
	APERTURE_STORAGE_SCHEMA_INCOMPATIBLE Code = "APERTURE_STORAGE_SCHEMA_INCOMPATIBLE"
	// APERTURE_STORAGE_CONSTRAINT — the database refused a write because it would
	// break a declared constraint, or the connection is not in a position to
	// enforce those constraints at all.
	//
	// In practice that is a foreign key: deleting an account that still has
	// members, a permission a grant still cites, or a principal that is still in a
	// group. Before Aperture's schema carried real foreign keys those deletes
	// SUCCEEDED and left the child rows behind, pointing at nothing — a grant
	// naming a permission that no longer exists is not a tidy leftover, it is
	// authority nobody can read or revoke. The refusal is the feature, and it is
	// deliberately distinct from APERTURE_STORAGE: nothing failed, no retry helps,
	// and the caller's next move is to delete the children first (or not to
	// delete the parent).
	//
	// The same code covers a SQLite connection whose foreign_keys pragma is off,
	// which Setup refuses up front: constraints that cannot be enforced are the
	// same problem as a constraint that was, one step earlier.
	APERTURE_STORAGE_CONSTRAINT Code = "APERTURE_STORAGE_CONSTRAINT"
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
	// (fetch, enumerate, invalidate, or an Enumerate metadata-field predicate)
	// but no ObjectProvider is registered for that type — or, for the predicate,
	// the engine was built with no metadata source at all (engine.WithMetadata).
	// The object-type is the identity's terminal segment type. A filtered
	// enumeration reports this rather than returning an empty list, because an
	// empty list reads as "no access" and would hide the misconfiguration.
	APERTURE_PROVIDER_UNREGISTERED Code = "APERTURE_PROVIDER_UNREGISTERED"
	// APERTURE_PROVIDER_FETCH — a host ObjectProvider's Fetch/List/Query returned
	// a plain (uncoded) error. The cause is wrapped verbatim; provider errors that
	// already carry an APERTURE_* code (e.g. APERTURE_NOT_FOUND for an
	// absent object) pass through unwrapped instead.
	APERTURE_PROVIDER_FETCH Code = "APERTURE_PROVIDER_FETCH"
	// APERTURE_PROVIDER_REFERENCE_INVALID — a declared object reference is not
	// usable: an empty object-type, field, or target; a target object-type with
	// no registered provider; a field declared twice on one type; or a request to
	// resolve a field no reference declares. A reference is an application-level
	// foreign key with no database constraint behind it, so the declaration is
	// checked where it is made — at registry build — rather than being discovered
	// by the first decision that followed it.
	APERTURE_PROVIDER_REFERENCE_INVALID Code = "APERTURE_PROVIDER_REFERENCE_INVALID"
	// APERTURE_PROVIDER_REFERENCE_MISMATCH — the VALUE of a declared reference
	// field does not point where the declaration says: it is neither an identity
	// string nor a list of them, it does not parse as a canonical identity, or
	// its terminal segment type is not the declared target ("team:7" in a field
	// declared to hold brands). It is an error rather than a skipped element on
	// purpose — an enumeration that silently dropped the value would read as "no
	// access" and hide the fault.
	APERTURE_PROVIDER_REFERENCE_MISMATCH Code = "APERTURE_PROVIDER_REFERENCE_MISMATCH"
	// APERTURE_ATTRIBUTE_SLOT_UNKNOWN — an attribute slot outside the closed set
	// (user, machine, account) was presented: registered, fetched, enumerated, or
	// parsed from a bare kind string. The set is closed because it enumerates the
	// parties a decision HAS, not the types a host happens to define, so an
	// unknown slot is a call-site bug rather than a wiring gap — and it fails
	// where it is presented rather than resolving to no provider and then to an
	// empty attribute bag, which reads as "this subject has no attributes" and
	// denies silently.
	APERTURE_ATTRIBUTE_SLOT_UNKNOWN Code = "APERTURE_ATTRIBUTE_SLOT_UNKNOWN"
	// APERTURE_ATTRIBUTE_PROVIDER_INVALID — an attribute provider registration or
	// an attribute key is unusable: a nil provider, a second provider for a slot
	// that already has one, a record declared twice, an empty key, or the account
	// wildcard "*" as a key. The wildcard is refused at the seam because the only
	// bag that could answer "the attributes of every account" is one account's
	// data served as another's.
	APERTURE_ATTRIBUTE_PROVIDER_INVALID Code = "APERTURE_ATTRIBUTE_PROVIDER_INVALID"
	// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED — attributes were requested for a
	// slot that is within the closed set but has no registered provider. It is
	// reported rather than answered with an empty bag: an empty bag is
	// indistinguishable from a subject that genuinely has no attributes, so a
	// missing wiring would surface as a rule quietly evaluating false.
	APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED Code = "APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED"
	// APERTURE_ATTRIBUTE_PROVIDER_FETCH — a host AttributeProvider's
	// Fetch/List/Query returned a plain (uncoded) error. The cause is wrapped
	// verbatim; an error already carrying an APERTURE_* code (notably
	// APERTURE_NOT_FOUND for an unknown key) passes through unwrapped instead, so
	// "there is no such principal" stays distinguishable from "the directory is
	// unreachable" — the two mean opposite things for a decision.
	APERTURE_ATTRIBUTE_PROVIDER_FETCH Code = "APERTURE_ATTRIBUTE_PROVIDER_FETCH"
	// APERTURE_SQL_PROVIDER_QUERY — a SQL-backed provider's statement did not
	// run: a connection, permission, syntax, placeholder-arity, or timeout
	// failure reported by the host's database. It covers both seams — an
	// ObjectProvider serving one object-type and an AttributeProvider serving one
	// attribute slot — because the failure and its remedy are the same; the
	// message names which. The driver's error is wrapped verbatim. This is an
	// OPERATIONAL failure, deliberately distinct from APERTURE_NOT_FOUND (the
	// object or subject is absent) — the registry must be able to tell "there is
	// no such object" from "the database is unreachable", because the two mean
	// opposite things for a decision.
	APERTURE_SQL_PROVIDER_QUERY Code = "APERTURE_SQL_PROVIDER_QUERY"
	// APERTURE_SQL_PROVIDER_AMBIGUOUS — a SQL-backed provider's "get one"
	// statement returned more than one row for a single object identity or a
	// single attribute key. The first row is never silently taken: which row won
	// would depend on an unspecified order, so the metadata or attribute bag —
	// and therefore the decision made from it — would vary between two otherwise
	// identical Checks.
	APERTURE_SQL_PROVIDER_AMBIGUOUS Code = "APERTURE_SQL_PROVIDER_AMBIGUOUS"
	// APERTURE_SQL_PROVIDER_SCAN — a row the host's database returned could not
	// be turned into object metadata or an attribute bag: an unnamed or
	// duplicated result column, a scan failure, a driver value of a Go type the
	// provider does not map, a []byte column that is not valid JSON, or a
	// timestamp the canonical date value model cannot represent. The statement
	// ran; its shape or its values are the problem, and the fix is a cast in the
	// SELECT list. The driver-value mapping is ONE table serving both seams, so
	// the rules are identical whichever provider read the column.
	APERTURE_SQL_PROVIDER_SCAN Code = "APERTURE_SQL_PROVIDER_SCAN"
	// APERTURE_SQL_PROVIDER_ROW_IDENTITY — a row returned by a SQL-backed
	// provider's "get all" statement did not yield a usable key: the result set
	// had no id column, or the row's id was NULL, empty, or not textual. For an
	// ObjectProvider the key is a full object IDENTITY, so the row also fails
	// when it does not parse as one, or when its terminal segment type is not the
	// object-type that provider serves. For an AttributeProvider the key is the
	// host's BARE subject id — an opaque handle with no grammar — so only the
	// textual checks apply.
	//
	// The key is composed by the developer inside the statement, and the two
	// seams spell it differently on purpose: 'brand:' || b.id AS id for an
	// object, u.id AS id for an attribute. Aperture cannot repair either — and
	// admitting a bad row would enumerate one object-type's rows under another's
	// and cache metadata under identities no Fetch of that provider could ever
	// return.
	APERTURE_SQL_PROVIDER_ROW_IDENTITY Code = "APERTURE_SQL_PROVIDER_ROW_IDENTITY"
	// APERTURE_SQL_PROVIDER_DSN_LITERAL — a declarative connection carries a
	// literal dsn: instead of naming an environment variable with dsn_env:. A
	// seed file is a committed artifact, so a DSN written into one is a password
	// written into version control, and that is only ever noticed afterwards.
	// The key is refused at PARSE time, before anything is opened, so the
	// document cannot be loaded by accident on the way to being fixed.
	APERTURE_SQL_PROVIDER_DSN_LITERAL Code = "APERTURE_SQL_PROVIDER_DSN_LITERAL"
	// APERTURE_SQL_PROVIDER_CONNECTION — a declared database connection could
	// not be resolved into a live pool, or a provider entry referenced one that
	// does not exist: an unset or empty DSN environment variable, an unparseable
	// pool setting, an unknown connection name, or a driver that refused to open
	// the handle. It is raised while the registry is being BUILT, never lazily on
	// the first query, because a connection that only fails under a decision
	// fails as a denial.
	//
	// A DSN is redacted out of anything this error carries: the whole point of
	// forbidding a literal dsn: is to keep the password out of committed and
	// logged text, and an error message is logged text.
	APERTURE_SQL_PROVIDER_CONNECTION Code = "APERTURE_SQL_PROVIDER_CONNECTION"
	// APERTURE_METADATA_INVALID — an object's metadata violates the shared
	// metadata value model: a value that is neither a scalar, a []any of
	// scalars, nor a map[string]any one level deeper; an array holding an object
	// or another array; a value nesting past the depth cap; or a value over the
	// per-value size cap. Raised by the loader (CSV, seed, a database provider)
	// at LOAD time, so a shape the expression evaluator cannot handle never
	// reaches a Check.
	APERTURE_METADATA_INVALID Code = "APERTURE_METADATA_INVALID"
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
	// APERTURE_RULE_UNDECLARED_ATTRIBUTE — a rule reads an attribute key off
	// `principal` or `account` that the deployment's shared wiring does not
	// DECLARE for that root. Raised by AST validation, never at decision time: the
	// point is that the rule is refused where it is authored rather than shipped
	// and then silently wrong somewhere else.
	//
	// A slot that declares no key set is not enforced at all, so this code cannot
	// be raised against a deployment that has declared nothing — which is every
	// deployment that has not opted in. A `principal` read is enforced only when
	// BOTH principal slots (user and machine) declare, because a rule cannot know
	// which kind will ask.
	//
	// What it prevents is an over-grant BETWEEN INSTANCES. A local attribute layer
	// may add keys the shared directory does not carry; a rule naming one of them
	// decides on the instance that has the key and reads a MISSING PATH on the
	// instance that does not — and a missing path neither denies nor errors, it
	// makes every predicate over it false, so an inclusive grant denies and an
	// EXCLUSIVE grant stops excluding, with nothing in any verdict, trace or note
	// to say why.
	APERTURE_RULE_UNDECLARED_ATTRIBUTE Code = "APERTURE_RULE_UNDECLARED_ATTRIBUTE"
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
	// APERTURE_ENTITY_UNMANAGED — a lifecycle write (create, update, or delete)
	// targeted an entity kind this deployment does not manage. The
	// APERTURE_MANAGE_ACCOUNTS / APERTURE_MANAGE_PRINCIPALS /
	// APERTURE_MANAGE_MEMBERSHIPS switches are deployment POSTURE, not
	// authorization: they say whether Aperture owns the kind's lifecycle at all,
	// so the refusal lands the same way for every caller — a full system-admin
	// included. It is deliberately distinct from APERTURE_AUTHZ_DENIED, because an
	// operator who cannot tell the two apart goes hunting through the grant table
	// for something a startup flag decided. The whole lifecycle is covered, not
	// creation alone (Aperture's entity writes are upserts), which is why the
	// message says "manage" rather than "create". Reads are unaffected, and the
	// decision path — Check / Enumerate / Explain — never consults the switches.
	APERTURE_ENTITY_UNMANAGED Code = "APERTURE_ENTITY_UNMANAGED"
	// APERTURE_WIRING_NO_MODEL_STATE — `aperture wiring push` was pointed at a
	// store that holds no model state at all, so there is nothing for the pushed
	// wiring to be wiring FOR.
	//
	// Wiring says where a decision reads object metadata and attribute bags FROM;
	// the model says who exists and who may do what. An empty store is therefore
	// almost always the wrong store — a typo'd DSN opens (and Setup creates) a
	// perfectly valid, perfectly empty database, and without this refusal the
	// wiring lands there, the push reports success, and the instance that actually
	// serves decisions never sees it. The refusal is deliberately not conditional
	// on the pushed set naming an object type: a connection-only push into an
	// empty database is the same mistake with fewer symptoms.
	APERTURE_WIRING_NO_MODEL_STATE Code = "APERTURE_WIRING_NO_MODEL_STATE"
	// APERTURE_WIRING_OBJECT_TYPE_UNKNOWN — a pushed provider entry serves an
	// object type the model's object-type table has no row for. The message names
	// the missing type.
	//
	// apt_wiring_providers.object_type carries a real foreign key ON DELETE
	// RESTRICT, so the database would refuse the row anyway — but a raw foreign-key
	// violation says "constraint failed", not "you have no object type called
	// dataset", and the operator has to go and read the schema to learn which of
	// the two names in the statement was the wrong one. This code is that refusal
	// with the type named.
	//
	// It applies to PROVIDER entries only. A field-type declaration may legally
	// name a type whose objects a seed lists inline, which needs no object-type
	// row at all, and apt_wiring_field_types.object_type carries no edge for
	// exactly that reason.
	APERTURE_WIRING_OBJECT_TYPE_UNKNOWN Code = "APERTURE_WIRING_OBJECT_TYPE_UNKNOWN"
	// APERTURE_WIRING_CONNECTION_UNDECLARED — a pushed provider or
	// attribute-provider entry names a connection the pushed connections: manifest
	// does not declare. The message names the undeclared connection and lists the
	// ones that were declared.
	//
	// The column carries no foreign key on purpose — an entry of a non-database
	// kind names no connection, and absence is the empty string rather than a NULL
	// — so nothing below this layer can catch the typo. It has to be caught at the
	// push, because one connections: entry is one POOL: a name with no manifest
	// entry does not fall back to a default pool, it fails the registry build on
	// the next boot of every instance that reads the wiring.
	APERTURE_WIRING_CONNECTION_UNDECLARED Code = "APERTURE_WIRING_CONNECTION_UNDECLARED"
	// APERTURE_WIRING_KIND_UNSHAREABLE — a pushed provider or attribute-provider
	// entry selects a kind that cannot be SHARED wiring, which today means
	// kind: csv.
	//
	// A csv entry's data source is a filesystem path, and a path is a machine-local
	// fact. A relative one is resolved against the seed FILE's own directory
	// (seed/provider.go, seed/attribute_provider.go), which database-sourced wiring
	// has none of; an absolute one is a guess about the other instance's disk. So
	// the shared-wiring schema has no path column at all, and an entry whose only
	// data source is a path has nothing to store. It is refused rather than stored
	// pathless, because a pathless csv entry would read back as wiring and then
	// serve nothing.
	//
	// kind: csv remains entirely legal in a LOCAL seed document. It is this
	// deployment's own file, and the instance that reads the seed is the instance
	// the path belongs to.
	//
	// It is raised at BOTH ends of the wiring, for the same condition and with the
	// same remedy: at the push, where the mistake is made, and on the BOOT that
	// reads the rows back, where a row that got there anyway — written by hand,
	// written by an older build, or written by a push that predates the check — is
	// refused before the instance serves a decision. Without the boot half such a
	// row reaches seed's own builder, which refuses it for the missing path: a true
	// statement whose remedy ("add a path:") cannot be carried out, because the
	// shared-wiring schema has no path column to add one to.
	APERTURE_WIRING_KIND_UNSHAREABLE Code = "APERTURE_WIRING_KIND_UNSHAREABLE"
	// APERTURE_WIRING_CONNECTION_UNROUTED — the shared wiring declares a connection
	// NAME this instance has no route for. The message names the connection and the
	// environment variable the conventional route reads its DSN from.
	//
	// The shared tables carry a connection's name and nothing else: which server,
	// which credential, how big a pool and how long a statement may take are
	// per-instance facts, and two instances may legitimately reach one logical
	// database differently. So each name is resolved locally — a host's
	// seed.WithConnectionOpener, a connections: entry under the same name in this
	// instance's own seed file, or the conventional APERTURE_CONNECTION_<NAME>_DSN
	// — and a name none of the three answers for is refused at boot.
	//
	// It is distinct from APERTURE_WIRING_CONNECTION_UNDECLARED, and the two are
	// opposite halves of one question. That one is a PUSH-time refusal: an entry
	// named a connection the pushed manifest does not list, which is a mistake in
	// the document being deployed and is the same mistake on every instance. This
	// one is a BOOT-time refusal: the manifest lists the name perfectly well and
	// THIS HOST has nowhere to point it, which is a per-instance fact and is
	// routinely true on one instance of a fleet and false on its peers.
	//
	// It is distinct from APERTURE_SQL_PROVIDER_CONNECTION, which seed raises for
	// an unset dsn_env, because the remedy differs and the remedy is the whole
	// point: that refusal sends an operator to a document's connections: block, and
	// a DB-declared name does not appear in this instance's document at all. An
	// operator told only "connection "main" reads its DSN from a variable that is
	// unset" greps a seed file that has never mentioned main, because the name came
	// out of a database somebody else pushed to.
	//
	// It is a boot refusal rather than a decision-time one because a connection
	// that fails under a decision does not fail AS a failure. An object provider
	// that cannot reach its database yields no metadata, and a rule reading
	// object.tier against absent metadata reads a missing path; an attribute
	// provider that cannot reach its database yields a nil bag under the leniency
	// contract, and a missing bag WIDENS an exclusive grant. Both authorize more
	// than the deployment asked for, and nothing in either verdict says a route was
	// missing.
	APERTURE_WIRING_CONNECTION_UNROUTED Code = "APERTURE_WIRING_CONNECTION_UNROUTED"
	// APERTURE_WIRING_LOCAL_COLLISION — this instance's LOCAL seed file declares
	// an object type or an attribute slot the shared wiring in its database
	// already declares. The message names the colliding entries and the two
	// sections that declare them.
	//
	// With wiring rows present the database is AUTHORITATIVE, and a local
	// declaration may only ADD an object type or a slot the database never
	// declared — which is the permanent situation of a Go host whose own object
	// providers no document can describe. A collision is refused rather than
	// resolved by precedence, in either direction, because both resolutions are
	// silent and both change what a decision reads: letting the database win
	// discards wiring somebody checked into this instance's file, and letting the
	// file win means one instance in a fleet answers from a source the others
	// cannot see. Neither shows up as an error on any later decision — it shows up
	// as a different verdict.
	//
	// It is distinct from APERTURE_PROVIDER_INVALID, which the registry raises for
	// the same overlap arriving from Go, because the remedies differ: this one
	// names two configuration sources an operator can edit, where that one names a
	// duplicate registration a developer has to remove.
	APERTURE_WIRING_LOCAL_COLLISION Code = "APERTURE_WIRING_LOCAL_COLLISION"
	// APERTURE_WIRING_NOTHING_DEPLOYED — `aperture wiring pull` was pointed at a
	// store that has no shared wiring in it at all.
	//
	// An empty set is not an error for `aperture wiring show`, which only DESCRIBES
	// it: nothing deployed is a real and useful answer, and the listing says so in
	// words. It is an error here, because a pull produces a FILE whose whole purpose
	// is to be pushed back — and a document carrying no wiring is a document that,
	// pushed, REPLACES the deployment's wiring with nothing.
	//
	// The two situations behind an empty read are opposites and indistinguishable
	// from the read alone: either nothing has been pushed to this store yet, or the
	// --store DSN names a database Setup has just created empty. Writing the file
	// anyway would commit the first reading of a situation that is usually the
	// second, and the mistake would only surface as a deployment-wide wiring wipe on
	// the next push.
	APERTURE_WIRING_NOTHING_DEPLOYED Code = "APERTURE_WIRING_NOTHING_DEPLOYED"
	// APERTURE_WIRING_OUTPUT_EXISTS — `aperture wiring pull --out` names a path
	// that already exists, and --force was not given.
	//
	// The file a pull writes is the file an operator diffs against version control,
	// so the likeliest thing at that path is the very document the pull is meant to
	// be compared with. Overwriting it silently would destroy the left-hand side of
	// the comparison and leave nothing to say it had ever been different. The
	// refusal is the default and --force is the way to say "yes, replace it".
	APERTURE_WIRING_OUTPUT_EXISTS Code = "APERTURE_WIRING_OUTPUT_EXISTS"
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
	APERTURE_STORAGE_SCHEMA_INCOMPATIBLE: {
		Message: "the existing database was written by an incompatible build and cannot be upgraded",
		Fixups: []string{
			"Recreate the database: move or delete the old file, start Aperture so Setup builds the schema fresh, then re-seed it.",
			"Point the store at a new, empty database if you need to keep the old file for reference.",
			"Do not expect an in-place upgrade: Aperture ships no migration tool and no schema versioning, so a schema break is a hard break by design.",
		},
	},
	APERTURE_STORAGE_CONSTRAINT: {
		Message: "the database refused the write because it would break referential integrity",
		Fixups: []string{
			"Delete the rows that reference the record first: a principal's memberships and group memberships, a permission's role assignments and grants, a role's principal assignments, an object type's permissions, an account's memberships and grants, and the grants that name a principal, role, or group as their subject.",
			"Creating a record? Create what it references first — a permission needs its object type, a membership and a group member need their principal, a grant and a role assignment need their permission, and a membership and a grant need their account.",
			"Refused on a grant's subject? subject_kind chooses the table subject_id must exist in — principal, role, or group. A grant naming a role id under kind \"group\" is refused even though that id exists elsewhere.",
			"Refused on an account id? It must name a real account, or be exactly \"*\" (the all-accounts wildcard). \"*\" is a sentinel, not an account: it needs no account record and cannot be given one.",
			"Ids are immutable in Aperture: to change one, create the new record, move the children, then delete the old one — an UPDATE that moved a key is refused rather than silently re-parenting.",
			"Seeing this from Setup rather than a write? The connection is not enforcing foreign keys. Open the store with sqlite.Open, which forces _pragma=foreign_keys(1) into the DSN whatever the caller passed.",
		},
	},
	APERTURE_CONFIG_INVALID: {
		Message: "configuration is invalid",
		Fixups: []string{
			"Validate the YAML config and APERTURE_* env vars against the docs.",
			"Enumerating an attribute slot that was declared without get_all: that slot is fetch-only by design, so add a get_all statement selecting a bare id, or read the slot through a fetch alone.",
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
			"Filtering an enumeration by metadata fields? Build the engine with engine.WithMetadata(registry) — the same provider registry the scope lister uses — or drop the field predicates.",
			"Searching object metadata by name? Search ALWAYS needs engine.WithMetadata(registry); unlike a field predicate it is not optional, and an empty result would read as \"no access\" rather than as the misconfiguration it is.",
		},
	},
	APERTURE_PROVIDER_FETCH: {
		Message: "object provider returned an error",
		Fixups: []string{
			"Inspect the wrapped cause for the underlying provider failure.",
			"Return APERTURE_NOT_FOUND from the provider for an object that does not exist.",
		},
	},
	APERTURE_PROVIDER_REFERENCE_INVALID: {
		Message: "a declared object reference is not usable",
		Fixups: []string{
			"Register a provider for the target object-type: a references: entry may only point at a type this registry serves.",
			"Declare each field at most once per object-type; several fields may point at the same target, but one field has one target.",
			"Declare the reference on the HOLDING side — the type whose provider actually returns the field — because that is the only side with a value to resolve.",
			"Resolving a field means declaring it first: reg.DeclareReference(\"dataset\", \"current_brands\", \"brand\"), or a references: entry in the provider's seed block.",
		},
	},
	APERTURE_PROVIDER_REFERENCE_MISMATCH: {
		Message: "a reference field's value does not identify an object of its declared target type",
		Fixups: []string{
			"Store FULL canonical identities in a reference field ('brand:1', 'account:acme/brand:1'), not bare primary keys — compose them where the data is loaded, e.g. SELECT 'brand:' || b.id.",
			"Make the field a string or a list of strings; a number, a map, or a list holding anything but strings cannot be an identity.",
			"Check the declared target against the values the field actually carries: an identity whose terminal segment type is not the declared type is rejected rather than skipped.",
		},
	},
	APERTURE_ATTRIBUTE_SLOT_UNKNOWN: {
		Message: "not an attribute slot",
		Fixups: []string{
			"Use one of the three declared slots: user, machine, or account (provider.AttributeSlotUser / AttributeSlotMachine / AttributeSlotAccount).",
			"Converting a principal kind that arrived as a bare string? Cross over with provider.ParseAttributeSlot so an unknown kind fails at the conversion instead of resolving to an empty attribute bag.",
			"The slot set is closed on purpose — it names the parties a decision has. Model a further distinction as a FIELD in the bag, not as a fourth slot.",
		},
	},
	APERTURE_ATTRIBUTE_PROVIDER_INVALID: {
		Message: "attribute provider registration or attribute key is invalid",
		Fixups: []string{
			"Register a non-nil provider, and at most one per slot; a duplicate is refused rather than replaced so one directory cannot silently shadow another.",
			"Declare each attribute key at most once within a provider.",
			"Fetch with a real key: a principal id for the user and machine slots, an account id for the account slot. An empty key names nobody.",
			"Resolve the account wildcard \"*\" to a concrete account before fetching attributes; it is never a legal attribute key.",
		},
	},
	APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED: {
		Message: "no attribute provider is registered for the slot",
		Fixups: []string{
			"Register an AttributeProvider for the slot before fetching its attributes.",
			"Check that the principal's kind maps to the slot you wired: a machine principal reads the machine slot, not the user slot.",
			"Deployment genuinely has no subjects of this kind? Then nothing should be fetching that slot — fix the caller rather than registering an empty provider.",
			"Seeing this from a decision? You should not: a Check/Enumerate/Explain against an unwired slot evaluates the floor bag and decides. This code reaches you only from a DIRECT attribute read (Fetch or Enumerate on the registry), so the caller to fix is that reader, not the decision path.",
		},
	},
	APERTURE_ATTRIBUTE_PROVIDER_FETCH: {
		Message: "attribute provider returned an error",
		Fixups: []string{
			"Inspect the wrapped cause for the underlying provider failure.",
			"Return APERTURE_NOT_FOUND from the provider for a key it does not know, so an unknown subject stays distinguishable from an unreachable directory.",
		},
	},
	APERTURE_SQL_PROVIDER_QUERY: {
		Message: "SQL provider could not run its statement",
		Fixups: []string{
			"Inspect the wrapped driver error for the underlying database failure.",
			"Check the statement's placeholder count: a fetch statement binds exactly one parameter — the identity's terminal segment value for an object provider, the bare subject id for an attribute provider.",
			"Use the placeholder syntax your engine speaks — Aperture passes placeholders through untouched and never rewrites $1 to ?.",
			"Confirm the database is reachable and the connection's role can read the table; raise Config.Timeout if the statement is legitimately slow.",
		},
	},
	APERTURE_SQL_PROVIDER_AMBIGUOUS: {
		Message: "SQL provider's fetch statement returned more than one row for one key",
		Fixups: []string{
			"Filter the fetch statement on a unique or primary key so one identity — or one subject id — selects at most one row.",
			"A join that fans out is the usual cause; aggregate or de-duplicate the fanned-out side instead of adding LIMIT 1, which would make the metadata depend on an unspecified row order.",
		},
	},
	APERTURE_SQL_PROVIDER_SCAN: {
		Message: "SQL provider could not read a row into metadata",
		Fixups: []string{
			"Give every selected expression a name, and alias duplicates: each result column becomes a metadata field keyed by its column name.",
			"Cast or serialise a column whose Go type the provider does not map (the driver value's type is named in the error, alongside the types that are mapped).",
			"A []byte column is decoded as JSON, never as a string: wrap an array in to_jsonb(...), and cast a numeric, uuid, or bytea to ::text, ::float8, or encode(...) in the statement.",
			"A list-valued field only arrives as a list when the statement casts it — SELECT to_jsonb(tags) AS tags, not SELECT tags, which yields the raw array literal as a string that silently matches nothing.",
		},
	},
	APERTURE_SQL_PROVIDER_ROW_IDENTITY: {
		Message: "SQL provider could not turn a row's id column into a usable key",
		Fixups: []string{
			"Object provider: compose the full identity in the get-all statement's id column — SELECT 'brand:' || b.id AS id — because a bare primary key is not an identity and Aperture supplies no template.",
			"Attribute provider: select the BARE subject id — SELECT u.id AS id — and never 'user:' || u.id, which is a legal opaque key that no principal id will ever match, so the slot enumerates and then answers nothing.",
			"Name the key column id, or set the provider's id column to the alias the statement actually uses.",
			"Make the id column textual and never NULL: cast a numeric or uuid key with ::text before concatenating it.",
			"Check that the identity's terminal segment type is the object-type this provider is registered under; a 'brand:1' row served by the 'dataset' provider is rejected rather than cached.",
		},
	},
	APERTURE_SQL_PROVIDER_DSN_LITERAL: {
		Message: "a declarative database connection carries a literal dsn instead of dsn_env",
		Fixups: []string{
			"Replace the connection's dsn: key with dsn_env: naming the environment variable that holds the DSN.",
			"Export the DSN in the process environment (or a .env file the deployment loads) rather than writing it into the seed file.",
			"Rotate the credential if a literal DSN was ever committed — the seed file is a version-controlled artifact.",
		},
	},
	APERTURE_SQL_PROVIDER_CONNECTION: {
		Message: "a declared database connection could not be resolved into a live pool",
		Fixups: []string{
			"Set the environment variable named by the connection's dsn_env: to a non-empty DSN before starting the process.",
			"Check that every kind: sql provider entry's connection: matches a name declared in the top-level connections: block.",
			"Give pool settings valid values: conn_max_lifetime and query_timeout are Go durations (\"30m\", \"5s\"), and query_timeout must be positive — there is no 'no timeout' setting.",
			"Verify the DSN's host, port, database, and credentials by connecting with psql; the driver's message is redacted here because a DSN parse failure commonly echoes the password.",
		},
	},
	APERTURE_METADATA_INVALID: {
		Message: "object metadata violates the metadata value model",
		Fixups: []string{
			"Make each field a scalar, a []any of scalars, or a map[string]any whose values are scalars, scalar arrays, or one further object level.",
			"Replace an array of objects with a scalar array (e.g. a list of ids) — arrays of objects are rejected at any position.",
			"Flatten a value that nests past the depth cap, or raise provider.ValueLimits.MaxDepth for the loader.",
			"Shorten a value over the per-value size cap, or raise provider.ValueLimits.MaxBytes for the loader.",
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
	APERTURE_RULE_UNDECLARED_ATTRIBUTE: {
		Message: "rule reads an attribute key the wiring does not declare",
		Fixups: []string{
			"Add the key to declared_keys: on the attribute_providers: entry for the slot the message names, then push the wiring again.",
			"Or stop reading the key in the rule: a key no shared entry declares is served by at most one instance's local layer, so a rule over it decides differently per instance.",
			"Read individual keys rather than the whole bag — a bare `principal` or `account` reference reads every key the bag happens to carry, which no declared set can cover.",
			"A slot that declares no key set is not enforced at all; remove declared_keys: from the entry to opt that slot back out.",
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
	APERTURE_ENTITY_UNMANAGED: {
		Message: "this deployment does not manage the entity kind the write targeted",
		Fixups: []string{
			"Set the switch for the kind named in the message — APERTURE_MANAGE_ACCOUNTS, APERTURE_MANAGE_PRINCIPALS, or APERTURE_MANAGE_MEMBERSHIPS — to true (the default), then RESTART aperture: the switches are read once at startup and never re-read.",
			"The three switches are independent; turning one on does not affect the others, so enable only the kind you meant to hand back to Aperture.",
			"Leaving a kind unmanaged is usually deliberate — those records are mastered by an upstream system. Make the change there and let it flow in, rather than flipping the switch.",
			"This is not a permission problem: no grant, role, or admin tier lifts it, and it refuses a system-admin exactly as it refuses anyone else.",
		},
	},
	APERTURE_WIRING_NO_MODEL_STATE: {
		Message: "the target store holds no model state, so there is nothing for the pushed wiring to be wiring for",
		Fixups: []string{
			"Apply model state to this store first — `aperture import --store <dsn>`, `aperture serve --store <dsn> --seed <file>`, or the mutation commands — and then push the wiring.",
			"Check the --store DSN: a typo names a database that does not exist yet, Setup creates it empty, and the wiring would land somewhere no instance reads.",
			"Two instances sharing wiring must share the MODEL too; wiring says where object metadata and attribute bags are read from, not who exists.",
		},
	},
	APERTURE_WIRING_OBJECT_TYPE_UNKNOWN: {
		Message: "a pushed provider entry serves an object type the model does not declare",
		Fixups: []string{
			"Declare the object type named in the message in the store's object_types: (apply the model state, then push the wiring again).",
			"Check the spelling against `aperture list object-type --store <dsn>`: the wiring's object_type: must equal the object type's name exactly.",
			"Field-type declarations are exempt — they may name a type whose objects a local seed lists inline — so only providers: entries need a row.",
		},
	},
	APERTURE_WIRING_CONNECTION_UNDECLARED: {
		Message: "a pushed entry names a connection the wiring's connections: manifest does not declare",
		Fixups: []string{
			"Add the connection named in the message to the document's connections: block, with dsn_env: naming the environment variable that holds its DSN.",
			"Or fix the entry's connection: to match one of the declared names the message lists — one connections: entry is one pool, so a typo opens no pool rather than a second one.",
			"Only the NAME is shared: every instance resolves that connection's DSN and pool settings from its own environment, so the manifest is a list of names and nothing else.",
		},
	},
	APERTURE_WIRING_KIND_UNSHAREABLE: {
		Message: "a pushed entry selects a kind that cannot be shared wiring",
		Fixups: []string{
			"Replace the kind: csv entry named in the message with kind: sql reading through a connections: entry, so every instance reaches the same data without a shared filesystem.",
			"Or leave that entry out of the pushed wiring and keep it in the LOCAL seed document, where the path belongs to the instance that reads it.",
			"A csv entry's only data source is a filesystem path; the shared-wiring schema has no path column, because a relative path resolves against the seed file's own directory and an absolute one is a guess about the other host's disk.",
			"On a BOOT this refusal means the row is already deployed — written by hand, or by a build that predates the check. Re-push a wiring document without it (`aperture wiring push`), or read the same data through kind: sql; there is no path column to add a path to.",
		},
	},
	APERTURE_WIRING_CONNECTION_UNROUTED: {
		Message: "the shared wiring declares a connection name this instance has no route for",
		Fixups: []string{
			"Export the environment variable the message names — APERTURE_CONNECTION_<NAME>_DSN — with this instance's DSN for that connection. It is the conventional route and needs no seed file.",
			"Or declare a connections: entry under the same name in this instance's --seed file, with dsn_env: naming a variable of your choosing; a local entry is that name's route, not a competing declaration, and it also carries the pool sizes and query_timeout.",
			"Or, in a Go host, supply seed.WithConnectionOpener and build the pool for that name yourself — the one seam a host needs, and the only one that never reads a DSN from the environment.",
			"Check which names this deployment expects with `aperture wiring show --store <dsn>`: the shared tables carry the connection NAME and nothing else, so every instance must supply its own route for each one.",
			"Do not work around it by removing the entry that uses the connection: an object provider that cannot reach its database yields no metadata, and an attribute provider that cannot yields a nil bag — which widens an exclusive grant rather than denying.",
		},
	},
	APERTURE_WIRING_LOCAL_COLLISION: {
		Message: "the local seed file declares an object type or attribute slot the shared wiring already declares",
		Fixups: []string{
			"Delete the local declaration named in the message: with wiring rows present the database is authoritative, and the local file may only ADD an object type or slot the database never declared.",
			"Or remove the shared declaration instead — push a wiring document that omits it (`aperture wiring push --store <dsn> --seed <file>`) — if the local one is the wiring you actually want the fleet to use.",
			"Check which side declares what with `aperture wiring show --store <dsn>`, then read the same section of this instance's --seed file.",
			"A host registering its own providers in Go gets the same refusal from the registry as APERTURE_PROVIDER_INVALID: the rule is about the registry, not about which syntax declared the entry.",
		},
	},
	APERTURE_WIRING_NOTHING_DEPLOYED: {
		Message: "the store has no shared wiring deployed, so there is nothing to pull",
		Fixups: []string{
			"Check the --store DSN first: a typo names a database that does not exist yet, Setup creates it empty, and an empty read is exactly what that looks like.",
			"If the DSN is right, nothing has been pushed to this deployment yet — run `aperture wiring push --seed <file> --store <dsn>`, and every instance sharing the database will read it.",
			"`aperture wiring show --store <dsn>` describes an empty store without refusing, which is the command to use when you only want to know whether anything is deployed.",
			"A pull is refused rather than writing an empty document because that document, pushed back, would replace the deployment's wiring with nothing.",
		},
	},
	APERTURE_WIRING_OUTPUT_EXISTS: {
		Message: "the --out path already exists and would be overwritten",
		Fixups: []string{
			"Write to a new path and compare the two files yourself — the existing file is usually the version-controlled document the pull is meant to be diffed against.",
			"Pass --force to replace the file deliberately.",
			"Nothing was read from the store and nothing was written: the path is checked before the wiring is fetched, so a refusal here leaves both the file and the deployment untouched.",
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
	APERTURE_STORAGE_SCHEMA_INCOMPATIBLE,
	APERTURE_STORAGE_CONSTRAINT,
	APERTURE_CONFIG_INVALID,
	APERTURE_ACTION_UNDECLARED,
	APERTURE_SCOPE_INVALID,
	APERTURE_SCOPE_UNKNOWN_STRATEGY,
	APERTURE_SCOPE_LISTER_UNCONFIGURED,
	APERTURE_SCOPE_RULE_UNCONFIGURED,
	APERTURE_PROVIDER_INVALID,
	APERTURE_PROVIDER_UNREGISTERED,
	APERTURE_PROVIDER_FETCH,
	APERTURE_PROVIDER_REFERENCE_INVALID,
	APERTURE_PROVIDER_REFERENCE_MISMATCH,
	APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
	APERTURE_ATTRIBUTE_PROVIDER_INVALID,
	APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED,
	APERTURE_ATTRIBUTE_PROVIDER_FETCH,
	APERTURE_SQL_PROVIDER_QUERY,
	APERTURE_SQL_PROVIDER_AMBIGUOUS,
	APERTURE_SQL_PROVIDER_SCAN,
	APERTURE_SQL_PROVIDER_ROW_IDENTITY,
	APERTURE_SQL_PROVIDER_DSN_LITERAL,
	APERTURE_SQL_PROVIDER_CONNECTION,
	APERTURE_METADATA_INVALID,
	APERTURE_RULE_INVALID,
	APERTURE_RULE_UNKNOWN_VARIABLE,
	APERTURE_RULE_TYPE_ERROR,
	APERTURE_RULE_EVAL,
	APERTURE_RULE_NOT_FOUND,
	APERTURE_RULE_UNDECLARED_ATTRIBUTE,
	APERTURE_DELEGATION_DENIED,
	APERTURE_DELEGATION_NOT_DELEGATABLE,
	APERTURE_IMPERSONATION_DENIED,
	APERTURE_IMPERSONATION_EXPIRED,
	APERTURE_UNAUTHENTICATED,
	APERTURE_INVALID_TOKEN,
	APERTURE_TEMPLATE_INVALID,
	APERTURE_TEMPLATE_PARAM,
	APERTURE_AUTHZ_DENIED,
	APERTURE_ENTITY_UNMANAGED,
	APERTURE_WIRING_NO_MODEL_STATE,
	APERTURE_WIRING_OBJECT_TYPE_UNKNOWN,
	APERTURE_WIRING_CONNECTION_UNDECLARED,
	APERTURE_WIRING_KIND_UNSHAREABLE,
	APERTURE_WIRING_CONNECTION_UNROUTED,
	APERTURE_WIRING_LOCAL_COLLISION,
	APERTURE_WIRING_NOTHING_DEPLOYED,
	APERTURE_WIRING_OUTPUT_EXISTS,
}

// Message returns the canonical message for a code, or empty when the code has
// no Registry entry.
func Message(code Code) string {
	return Registry[code].Message
}
