// Package model defines Aperture's RBAC domain model and the persistence
// boundary (the Storage interface) that every backend implements.
//
// The model is deliberately small and explicit. Six entities make up the
// authorization graph:
//
//   - ObjectType — a protected resource type (the "type" half of an identity
//     segment, e.g. "document") with a declared, validated set of action verbs.
//   - Permission — an (action, scope-strategy) pair bound to one object type.
//     Declaring a permission against an action the object type does not declare
//     is rejected (typed-action validation, APERTURE_ACTION_UNDECLARED).
//   - Principal — a user or machine, addressable by the identity scheme.
//   - Role — a named bundle of permissions; principals are assigned roles.
//   - Group — a collection of principals that can itself be a grant subject.
//   - Grant — a binding of a subject (principal | role | group) to a permission,
//     scoped to an object-identity PATTERN and an Effect (allow | deny), and
//     STAMPED with the account it belongs to. Account stamping is the mechanism
//     that guarantees cross-account isolation (enforced later in E3-S1).
//
// Grants store the identity pattern in string form so wildcard grants are
// first-class; the decision engine (E1-S4) parses the pattern with the identity
// package and resolves matches with deny-overrides + specificity tiebreak.
//
// The model couples only to the leaf packages errors/ and identity/. It has no
// storage, engine, or transport dependencies, so the Storage interface here is
// the single seam the in-memory and SQLite backends — and a future Postgres
// backend — implement, with no backend-specific concepts leaking into it.
package model

import "time"

// PrincipalKind enumerates the categories of principal Aperture authorizes.
type PrincipalKind string

const (
	// PrincipalUser is a human end-user.
	PrincipalUser PrincipalKind = "user"
	// PrincipalMachine is a non-human caller (service account, CI bot, agent).
	PrincipalMachine PrincipalKind = "machine"
)

// Valid reports whether k is a recognised principal kind.
func (k PrincipalKind) Valid() bool {
	return k == PrincipalUser || k == PrincipalMachine
}

// Effect is the polarity of a grant: it either allows or denies.
type Effect string

const (
	// EffectAllow grants access for the matched action + object pattern.
	EffectAllow Effect = "allow"
	// EffectDeny withholds access; a matching deny overrides allows at equal or
	// broader specificity (deny-overrides, resolved in E1-S4).
	EffectDeny Effect = "deny"
)

// Valid reports whether e is a recognised effect.
func (e Effect) Valid() bool {
	return e == EffectAllow || e == EffectDeny
}

// SubjectKind enumerates what a grant can be bound to.
type SubjectKind string

const (
	// SubjectPrincipal binds a grant directly to a single principal.
	SubjectPrincipal SubjectKind = "principal"
	// SubjectRole binds a grant to every principal assigned the role.
	SubjectRole SubjectKind = "role"
	// SubjectGroup binds a grant to every principal in the group.
	SubjectGroup SubjectKind = "group"
)

// Valid reports whether k is a recognised subject kind.
func (k SubjectKind) Valid() bool {
	return k == SubjectPrincipal || k == SubjectRole || k == SubjectGroup
}

// Subject identifies what a grant applies to: a principal, role, or group, by
// its id. It is a value type so it is comparable and cheap to pass in the
// engine's subject-set expansion.
type Subject struct {
	Kind SubjectKind
	ID   string
}

// ObjectType is a protected resource type. Name is the identity-segment type
// (e.g. "document", "project"); Actions is the declared, validated set of verbs
// permissions may name for this type. The verb set is closed: typed-action
// validation rejects any permission action absent from it.
type ObjectType struct {
	// Name is the identity-segment type this object type governs.
	Name string
	// Actions is the declared verb set. Order is not significant; duplicates are
	// ignored by validation.
	Actions []string
	// Description is optional human-readable documentation.
	Description string
	// CreatedAt / UpdatedAt are stamped by the service layer and persisted
	// verbatim. Present for forward-compatibility with audit (E4-S2).
	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasAction reports whether action is in the object type's declared verb set.
func (ot ObjectType) HasAction(action string) bool {
	for _, a := range ot.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// Permission is an (action, scope-strategy) pair bound to one object type. The
// action MUST be declared by the object type (typed-action validation).
// ScopeStrategy is an opaque reference at this layer — the resolvers that turn a
// strategy into a concrete object set arrive in E2-S1; storage only records the
// ref.
type Permission struct {
	// ID is the permission's stable identifier (caller-assigned).
	ID string
	// ObjectType references ObjectType.Name.
	ObjectType string
	// Action is the verb; it must be in the object type's declared set.
	Action string
	// ScopeStrategy is an opaque scope-strategy reference, resolved in E2-S1.
	ScopeStrategy string
	// Description is optional human-readable documentation.
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Principal is a user or machine, addressable by the identity scheme. Identity
// is its canonical identity string (e.g. "user:alice", "machine:ci-bot").
// RoleIDs are the roles assigned to the principal; the decision engine expands
// them — together with the principal's groups — into the subject set it
// resolves grants against.
type Principal struct {
	// ID is the principal's stable identifier (caller-assigned).
	ID string
	// Kind is user or machine.
	Kind PrincipalKind
	// Identity is the principal's canonical identity-scheme address.
	Identity string
	// DisplayName is optional human-readable label.
	DisplayName string
	// RoleIDs are the roles assigned to this principal.
	RoleIDs   []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Role is a named bundle of permissions. Principals are assigned roles
// (Principal.RoleIDs); a role may also be a grant subject directly.
type Role struct {
	// ID is the role's stable identifier (caller-assigned).
	ID string
	// Name is the human-readable role name.
	Name string
	// Description is optional documentation.
	Description string
	// PermissionIDs is the bundle of permissions the role confers.
	PermissionIDs []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Group is a collection of principals that can itself hold grants (be a grant
// subject). MemberPrincipalIDs are the principals in the group.
type Group struct {
	// ID is the group's stable identifier (caller-assigned).
	ID string
	// Name is the human-readable group name.
	Name string
	// Description is optional documentation.
	Description string
	// MemberPrincipalIDs are the principals that belong to the group.
	MemberPrincipalIDs []string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Grant binds a subject (principal | role | group) to a permission, scoped to an
// object-identity pattern and an effect, and STAMPED with the account it belongs
// to. AccountID is mandatory: it is the cross-account isolation boundary — grant
// queries are always account-scoped so a grant stamped to one account can never
// surface in another (enforced end-to-end in E3-S1).
//
// Object is an identity PATTERN in string form (e.g.
// "account:acme/project:atlas/**"), so wildcard grants are first-class. The
// engine parses it with identity.ParsePattern; later a scope resolver (E2-S1)
// can produce the grant's object set instead of a literal pattern.
type Grant struct {
	// ID is the grant's stable identifier (caller-assigned), globally unique.
	ID string
	// AccountID stamps the grant to an account. Mandatory.
	AccountID string
	// Subject is what the grant applies to.
	Subject Subject
	// PermissionID references the granted Permission.
	PermissionID string
	// Object is the identity pattern the grant scopes to (string form).
	Object string
	// Effect is allow or deny.
	Effect Effect
	// CreatedAt / UpdatedAt are stamped by the service layer and persisted
	// verbatim. Present for forward-compatibility with audit (E4-S2).
	CreatedAt time.Time
	UpdatedAt time.Time
}
