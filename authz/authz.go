// Package authz is the authorization gate the model-mutation API (E4-S1) calls
// before every mutation. It expresses Aperture's internal administrative
// authority — the right to change the system itself — as ordinary identities in
// the scheme, in two tiers, and enforces the required tier on each mutation
// (FR-17).
//
// Authority is in-scheme, never a parallel permission system. An admin right is
// just an allow grant on a reserved admin action verb (AdminAction) whose object
// pattern covers a tier's authority identity. The gate decides authority by
// resolving that grant through the SAME engine that answers every other
// question — engine.Check / engine.Explain — so an admin check is an ordinary
// decision: wildcard-resolvable, auditable, and explainable, with no
// special-cased bypass path. This follows the idiom E3-S2 (delegation:
// aperture.delegate) and E3-S3 (impersonation: aperture.impersonate.*)
// established — a reserved action verb plus an identity-pattern-scoped grant.
//
// Two tiers:
//
//   - SYSTEM (global) — addressed as system:* . Permits managing the global
//     schema: object-types, permission types, roles, providers, templates, and
//     rules, plus tenancy (accounts) and global principals. A holder is anyone
//     whose effective allow grants in their active account include an allow on
//     AdminAction whose object pattern covers system:schema (the tier anchor) —
//     e.g. a grant on system:* or the all-covering **.
//
//   - ACCOUNT — addressed as account:<acct>/admin:* . Permits managing grants and
//     delegation WITHIN ONE account only. A holder is anyone whose effective
//     allow grants IN THAT account include an allow on AdminAction whose object
//     pattern covers account:<acct>/admin:all (the per-account anchor). Because the
//     check resolves against the target account, an account-admin of account A is
//     refused any mutation scoped to account B (grants are account-scoped, so A's
//     authority is never even loaded for B, and account:A/admin:* does not cover
//     account:B/admin:*): account-admin authority is confined to its own account.
//
// SYSTEM SUPERSEDES ACCOUNT. The account-tier confinement above binds
// account-admins to each other. A SYSTEM-admin, however, may drive any
// account-tier mutation in any account — including a freshly-created account
// that holds no admin grants of its own yet. Authorize checks system-admin
// first for account-tier mutations and only falls back to the per-account
// check for non-system actors (RequireAccountAdmin itself stays confined, so
// the "is this principal an account-admin of X" question keeps its narrow
// answer; only the mutation gate treats system as the super-tier). This mirrors
// the system-or-account precedent in service/audit.go's authorizeAuditRead.
//
// A note on the address spelling. The brief addresses the tiers as `system:**`
// and `account:<acct>/admin:**`. The identity grammar only accepts `**` as a
// standalone path SEGMENT (e.g. account:acme/**), not inside an id component, so
// the in-scheme spelling of the tier authority is the single-component wildcard
// the grammar supports: system:* and account:<acct>/admin:* . A broader holder
// (account:acme/**, or the all-covering **) still resolves, so the authority is
// genuinely wildcard-resolvable. A principal that wants BOTH tiers at once holds
// an allow on AdminAction over ** , which covers every tier anchor.
//
// The gate keeps the mutation→tier mapping in ONE legible place (mutationTier):
// E4-S1 names each mutation it is about to perform and the gate resolves the
// required tier and enforces it. The gate holds no state beyond the engine and
// is safe for concurrent use to the degree the engine is.
package authz

import (
	"context"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
)

// AdminAction is the reserved action verb that represents administrative
// authority. A principal holds an admin tier when its effective allow grant set
// includes an allow grant on a permission with this action whose object pattern
// covers that tier's authority identity. It is analogous to delegation's
// DelegateAction and impersonation's Augment/BecomeAction: the right to
// administer is an ordinary permission, scoped exactly like any other.
const AdminAction = "aperture.admin"

// The tier authority anchors — the concrete identities a tier's authority is
// resolved against. They are ordinary object identities the engine matches an
// admin grant's object pattern against, so the authority check is an ordinary
// engine.Check (no bespoke matching). systemAnchor represents the global schema;
// accountAnchor builds the per-account administrative authority identity.
const systemAnchor = "system:schema"

// accountAnchor returns the administrative-authority identity for one account:
// account:<acct>/admin:all . An account-admin grant (account:<acct>/admin:*, or
// broader) covers it; an admin grant scoped to a different account does not, which
// is what confines account-admin authority to its own account.
func accountAnchor(account string) string {
	return "account:" + account + "/admin:all"
}

// Tier is one of the two administrative authority tiers a mutation can require.
type Tier int

const (
	// TierSystem is global schema-administration authority (system:*).
	TierSystem Tier = iota
	// TierAccount is per-account grant/delegation administration authority
	// (account:<acct>/admin:*), confined to a single account.
	TierAccount
)

// String renders the tier for diagnostics and error context.
func (t Tier) String() string {
	switch t {
	case TierSystem:
		return "system"
	case TierAccount:
		return "account"
	default:
		return "unknown"
	}
}

// Mutation names a model-mutating operation. It is the key E4-S1 passes to the
// gate so the gate, not each endpoint, owns the mutation→tier policy. The set is
// the authoritative list of operations the system gates; E4-S1 maps each Twirp /
// CLI mutation endpoint onto one of these.
type Mutation string

const (
	// ---- System tier: managing the global schema + tenancy. ----

	// MutationPutObjectType / MutationDeleteObjectType manage object types.
	MutationPutObjectType    Mutation = "put_object_type"
	MutationDeleteObjectType Mutation = "delete_object_type"
	// MutationPutPermission / MutationDeletePermission manage permission types.
	MutationPutPermission    Mutation = "put_permission"
	MutationDeletePermission Mutation = "delete_permission"
	// MutationPutRole / MutationDeleteRole manage roles.
	MutationPutRole    Mutation = "put_role"
	MutationDeleteRole Mutation = "delete_role"
	// MutationPutGroup / MutationDeleteGroup manage groups (global membership
	// containers).
	MutationPutGroup    Mutation = "put_group"
	MutationDeleteGroup Mutation = "delete_group"
	// MutationPutPrincipal / MutationDeletePrincipal manage principals (global
	// identities).
	MutationPutPrincipal    Mutation = "put_principal"
	MutationDeletePrincipal Mutation = "delete_principal"
	// MutationPutProvider / MutationDeleteProvider manage object providers.
	MutationPutProvider    Mutation = "put_provider"
	MutationDeleteProvider Mutation = "delete_provider"
	// MutationPutTemplate / MutationDeleteTemplate manage templates.
	MutationPutTemplate    Mutation = "put_template"
	MutationDeleteTemplate Mutation = "delete_template"
	// MutationPutRule / MutationDeleteRule manage rules.
	MutationPutRule    Mutation = "put_rule"
	MutationDeleteRule Mutation = "delete_rule"
	// MutationPutAccount / MutationDeleteAccount manage tenancy (accounts).
	MutationPutAccount    Mutation = "put_account"
	MutationDeleteAccount Mutation = "delete_account"
	// MutationImport applies a whole declarative state file (E5-S2): it can create
	// or replace any global-schema entity, so it is the most privileged mutation
	// there is and sits at the system tier alongside the schema mutations. (Export
	// is a system-tier READ, gated directly via RequireSystemAdmin, not a mutation.)
	MutationImport Mutation = "import"

	// ---- Account tier: managing grants + delegation WITHIN one account. ----

	// MutationPutGrant / MutationDeleteGrant manage grants in an account.
	MutationPutGrant    Mutation = "put_grant"
	MutationDeleteGrant Mutation = "delete_grant"
	// MutationBestow / MutationRevoke are the delegation mutations (E3-S2) in an
	// account.
	MutationBestow Mutation = "bestow"
	MutationRevoke Mutation = "revoke"
	// MutationPutMembership / MutationDeleteMembership manage who belongs to an
	// account.
	MutationPutMembership    Mutation = "put_membership"
	MutationDeleteMembership Mutation = "delete_membership"
)

// mutationTier is the single source of truth mapping every gated mutation to the
// authority tier that guards it. E4-S1 adds a row here when it adds a mutation;
// the gate reads it so the policy lives in exactly one place.
var mutationTier = map[Mutation]Tier{
	MutationPutObjectType:    TierSystem,
	MutationDeleteObjectType: TierSystem,
	MutationPutPermission:    TierSystem,
	MutationDeletePermission: TierSystem,
	MutationPutRole:          TierSystem,
	MutationDeleteRole:       TierSystem,
	MutationPutGroup:         TierSystem,
	MutationDeleteGroup:      TierSystem,
	MutationPutPrincipal:     TierSystem,
	MutationDeletePrincipal:  TierSystem,
	MutationPutProvider:      TierSystem,
	MutationDeleteProvider:   TierSystem,
	MutationPutTemplate:      TierSystem,
	MutationDeleteTemplate:   TierSystem,
	MutationPutRule:          TierSystem,
	MutationDeleteRule:       TierSystem,
	MutationPutAccount:       TierSystem,
	MutationDeleteAccount:    TierSystem,
	MutationImport:           TierSystem,

	MutationPutGrant:         TierAccount,
	MutationDeleteGrant:      TierAccount,
	MutationBestow:           TierAccount,
	MutationRevoke:           TierAccount,
	MutationPutMembership:    TierAccount,
	MutationDeleteMembership: TierAccount,
}

// TierOf reports the authority tier a mutation requires, and whether the
// mutation is known to the gate. An unknown mutation has no policy and the gate
// fails closed on it (Authorize refuses it).
func TierOf(m Mutation) (Tier, bool) {
	t, ok := mutationTier[m]
	return t, ok
}

// Actor is who is attempting a mutation: the principal id and the active account
// the principal is operating in. Account is the account whose (account-scoped)
// grants the actor's authority is resolved from for a SYSTEM-tier check — a
// system-admin grant is an ordinary account-scoped grant on system:* , global in
// what it administers (the schema is not under any account). For an ACCOUNT-tier
// check the TARGET account governs instead, so confinement holds regardless of
// Actor.Account.
type Actor struct {
	// Principal is the id of the principal attempting the mutation. Mandatory.
	Principal string
	// Account is the active account the actor is operating in. Mandatory for a
	// system-tier check (it is where the actor's system:* grant is resolved).
	Account string
}

// Gate is the authorization gate. It holds only the decision engine, resolving
// every authority question through engine.Check / engine.Explain so an admin
// check is an ordinary decision with no special-cased bypass.
type Gate struct {
	eng *engine.Engine
}

// NewGate returns a Gate that resolves authority through eng.
func NewGate(eng *engine.Engine) *Gate {
	return &Gate{eng: eng}
}

// Authorize enforces the authority tier required by mutation m on behalf of
// actor. For an account-tier mutation, account is the account the mutation
// targets (e.g. the grant's AccountID) and MUST be supplied; for a system-tier
// mutation it is ignored. It returns nil when the actor holds the required tier,
// or an APERTURE_AUTHZ_DENIED coded error (fail-closed) when it does not. An
// unknown mutation is refused with APERTURE_INVALID_INPUT — the gate never
// permits an operation it has no policy for.
func (g *Gate) Authorize(ctx context.Context, actor Actor, m Mutation, account string) error {
	tier, ok := TierOf(m)
	if !ok {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"authz: unknown mutation has no authority policy",
			map[string]any{"mutation": string(m)})
	}
	switch tier {
	case TierSystem:
		return g.RequireSystemAdmin(ctx, actor)
	case TierAccount:
		if account == "" {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"authz: account-tier mutation requires a target account",
				map[string]any{"mutation": string(m)})
		}
		// System-admin supersedes account-admin: a system-admin may drive every
		// account-tier mutation, including in a freshly-created account that has
		// no admin grants of its own yet. Fall back to the account-admin check so
		// its richer denial context surfaces for non-system actors. Mirrors the
		// system-or-account precedent in service/audit.go's authorizeAuditRead.
		if err := g.RequireSystemAdmin(ctx, actor); err == nil {
			return nil
		}
		return g.RequireAccountAdmin(ctx, actor, account)
	default:
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"authz: mutation maps to an unknown tier",
			map[string]any{"mutation": string(m), "tier": tier.String()})
	}
}

// RequireSystemAdmin returns nil when actor holds system-admin authority — an
// effective allow grant on AdminAction whose object pattern covers system:* —
// resolved through the engine in the actor's active account. Otherwise it
// returns APERTURE_AUTHZ_DENIED. The check is an ordinary engine.Check against
// the system anchor identity, so it is auditable and explainable like any
// decision (see ExplainSystemAdmin).
func (g *Gate) RequireSystemAdmin(ctx context.Context, actor Actor) error {
	if err := validateActor(actor, true); err != nil {
		return err
	}
	dec, err := g.eng.Check(ctx, g.systemRequest(actor))
	if err != nil {
		return err
	}
	if dec.Allow {
		return nil
	}
	return aerr.WithContext(aerr.APERTURE_AUTHZ_DENIED,
		"actor does not hold system-admin authority",
		map[string]any{
			"tier":      TierSystem.String(),
			"actor":     actor.Principal,
			"account":   actor.Account,
			"authority": systemAnchor,
			"reason":    "no_system_admin",
			"decision":  dec.Reason,
		})
}

// RequireAccountAdmin returns nil when actor holds account-admin authority in the
// TARGET account — an effective allow grant on AdminAction, resolved IN account,
// whose object pattern covers account:<account>/admin:* . Otherwise it returns
// APERTURE_AUTHZ_DENIED. Because authority is resolved in the target account, an
// admin of a DIFFERENT account is denied here (its grants are not even loaded,
// and its admin pattern does not cover this account's anchor): account-admin
// authority is confined to its own account.
func (g *Gate) RequireAccountAdmin(ctx context.Context, actor Actor, account string) error {
	if err := validateActor(actor, false); err != nil {
		return err
	}
	if account == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "authz: target account is empty")
	}
	dec, err := g.eng.Check(ctx, g.accountRequest(actor, account))
	if err != nil {
		return err
	}
	if dec.Allow {
		return nil
	}
	return aerr.WithContext(aerr.APERTURE_AUTHZ_DENIED,
		"actor does not hold account-admin authority in the target account",
		map[string]any{
			"tier":      TierAccount.String(),
			"actor":     actor.Principal,
			"account":   account,
			"authority": accountAnchor(account),
			"reason":    "no_account_admin",
			"decision":  dec.Reason,
		})
}

// ExplainSystemAdmin returns the engine Trace for the system-admin authority
// decision — the full derivation behind RequireSystemAdmin — proving the admin
// check resolves through the normal engine and is explainable on admin
// identities. The verdict in the returned Trace matches RequireSystemAdmin.
func (g *Gate) ExplainSystemAdmin(ctx context.Context, actor Actor) (engine.Trace, error) {
	if err := validateActor(actor, true); err != nil {
		return engine.Trace{}, err
	}
	return g.eng.Explain(ctx, g.systemRequest(actor))
}

// ExplainAccountAdmin returns the engine Trace for the account-admin authority
// decision in the target account — the full derivation behind
// RequireAccountAdmin. It proves account-admin authority is explainable on the
// per-account admin identity through the normal engine.
func (g *Gate) ExplainAccountAdmin(ctx context.Context, actor Actor, account string) (engine.Trace, error) {
	if err := validateActor(actor, false); err != nil {
		return engine.Trace{}, err
	}
	if account == "" {
		return engine.Trace{}, aerr.New(aerr.APERTURE_INVALID_INPUT, "authz: target account is empty")
	}
	return g.eng.Explain(ctx, g.accountRequest(actor, account))
}

// systemRequest builds the engine request that resolves system-admin authority:
// the reserved admin action on the system anchor, scoped to the actor's active
// account.
func (g *Gate) systemRequest(actor Actor) engine.Request {
	return engine.Request{
		Account:   actor.Account,
		Principal: actor.Principal,
		Action:    AdminAction,
		Object:    systemAnchor,
	}
}

// accountRequest builds the engine request that resolves account-admin authority
// for the target account: the reserved admin action on that account's admin
// anchor, scoped to the target account itself (the source of confinement).
func (g *Gate) accountRequest(actor Actor, account string) engine.Request {
	return engine.Request{
		Account:   account,
		Principal: actor.Principal,
		Action:    AdminAction,
		Object:    accountAnchor(account),
	}
}

// validateActor rejects an actor missing a required field. needAccount is set for
// system-tier checks, where the actor's active account is where its system:*
// grant is resolved; account-tier checks resolve in the target account instead,
// so Actor.Account is not required there.
func validateActor(actor Actor, needAccount bool) error {
	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "authz: actor principal is empty")
	}
	if needAccount && actor.Account == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "authz: actor active account is empty")
	}
	return nil
}
