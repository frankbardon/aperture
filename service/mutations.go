package service

import (
	"context"
	"time"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/model"
)

// This file extends the decision facade with the model-mutation surface: entity
// CRUD, grants, delegation, and impersonation. It is the single seam HTTP /
// Twirp / CLI share for writes, so the auth + admin-tier policy lives here once.
//
// Authorization layering:
//
//   - Schema entities (object-types, permissions, principals, roles, groups,
//     accounts) are SYSTEM-tier: the authz gate requires system-admin authority.
//   - Account-scoped entities (memberships, raw grants) are ACCOUNT-tier: the gate
//     requires account-admin authority IN THE TARGET account.
//   - Delegation (Bestow / Revoke) and Impersonation (Start) are NOT routed
//     through the admin gate — they carry their OWN finer-grained authorization
//     (the delegation subset rule / the impersonation guardrails), where the actor
//     is the delegator / operator, not an admin. Requiring admin tier on top would
//     defeat their purpose (delegation exists precisely so a non-admin may hand on
//     a subset of its own grants). See FOLLOWUPS.
//
// Every mutation requires an authenticated principal (a non-empty Actor.Principal
// / delegator / operator); an empty one is APERTURE_UNAUTHENTICATED. The Twirp
// surface overrides these with the identity the auth middleware resolved, so a
// caller can never spoof a different actor on the wire.

// Actor is the authenticated caller a mutation is attributed to and authorized
// against: the principal id and the active account it is operating in.
type Actor struct {
	// Principal is the authenticated principal id. Mandatory for any mutation.
	Principal string
	// Account is the active account. Required for a system-tier authority check
	// (where the actor's system:* grant is resolved); ignored for account-tier
	// checks, which resolve in the target account.
	Account string
}

func (a Actor) gateActor() authz.Actor {
	return authz.Actor{Principal: a.Principal, Account: a.Account}
}

// now is the facade clock for stamping CreatedAt/UpdatedAt on entity writes; it
// is time.Now in production. Delegation and impersonation carry their own clocks.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// requireStore reports the read/CRUD surface is wired, else APERTURE_UNIMPLEMENTED.
func (s *Service) requireStore() error {
	if s.store == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: entity surface is not wired (read-only facade)")
	}
	return nil
}

// requireMutator reports the gated-mutation surface (store + gate) is wired.
func (s *Service) requireMutator() error {
	if s.store == nil || s.gate == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED,
			"service: mutation surface is not wired (read-only facade)")
	}
	return nil
}

// authorize is the one place a gated mutation enforces an authenticated actor and
// the admin tier its kind requires. targetAccount is the account an account-tier
// mutation governs (ignored for system-tier).
func (s *Service) authorize(ctx context.Context, actor Actor, m authz.Mutation, targetAccount string) error {
	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: a mutation requires an authenticated principal")
	}
	return s.gate.Authorize(ctx, actor.gateActor(), m, targetAccount)
}

// stampPut sets UpdatedAt to now and CreatedAt to now when unset, honouring the
// model contract that the service layer stamps entity timestamps.
func (s *Service) stamp(created, updated *time.Time) {
	now := s.clock()
	if created.IsZero() {
		*created = now
	}
	*updated = now
}

// ---- ObjectType (system tier) ----

func (s *Service) PutObjectType(ctx context.Context, actor Actor, ot model.ObjectType) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutObjectType, ""); err != nil {
		return err
	}
	s.stamp(&ot.CreatedAt, &ot.UpdatedAt)
	return s.store.PutObjectType(ctx, ot)
}

func (s *Service) GetObjectType(ctx context.Context, name string) (model.ObjectType, error) {
	if err := s.requireStore(); err != nil {
		return model.ObjectType{}, err
	}
	return s.store.GetObjectType(ctx, name)
}

func (s *Service) ListObjectTypes(ctx context.Context) ([]model.ObjectType, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListObjectTypes(ctx)
}

func (s *Service) DeleteObjectType(ctx context.Context, actor Actor, name string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeleteObjectType, ""); err != nil {
		return err
	}
	return s.store.DeleteObjectType(ctx, name)
}

// ---- Permission (system tier) ----

func (s *Service) PutPermission(ctx context.Context, actor Actor, p model.Permission) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutPermission, ""); err != nil {
		return err
	}
	s.stamp(&p.CreatedAt, &p.UpdatedAt)
	return s.store.PutPermission(ctx, p)
}

func (s *Service) GetPermission(ctx context.Context, id string) (model.Permission, error) {
	if err := s.requireStore(); err != nil {
		return model.Permission{}, err
	}
	return s.store.GetPermission(ctx, id)
}

func (s *Service) ListPermissions(ctx context.Context) ([]model.Permission, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListPermissions(ctx)
}

func (s *Service) DeletePermission(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeletePermission, ""); err != nil {
		return err
	}
	return s.store.DeletePermission(ctx, id)
}

// ---- Principal (system tier) ----

func (s *Service) PutPrincipal(ctx context.Context, actor Actor, p model.Principal) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutPrincipal, ""); err != nil {
		return err
	}
	s.stamp(&p.CreatedAt, &p.UpdatedAt)
	return s.store.PutPrincipal(ctx, p)
}

func (s *Service) GetPrincipal(ctx context.Context, id string) (model.Principal, error) {
	if err := s.requireStore(); err != nil {
		return model.Principal{}, err
	}
	return s.store.GetPrincipal(ctx, id)
}

func (s *Service) ListPrincipals(ctx context.Context) ([]model.Principal, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListPrincipals(ctx)
}

func (s *Service) DeletePrincipal(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeletePrincipal, ""); err != nil {
		return err
	}
	return s.store.DeletePrincipal(ctx, id)
}

// ---- Role (system tier) ----

func (s *Service) PutRole(ctx context.Context, actor Actor, r model.Role) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutRole, ""); err != nil {
		return err
	}
	s.stamp(&r.CreatedAt, &r.UpdatedAt)
	return s.store.PutRole(ctx, r)
}

func (s *Service) GetRole(ctx context.Context, id string) (model.Role, error) {
	if err := s.requireStore(); err != nil {
		return model.Role{}, err
	}
	return s.store.GetRole(ctx, id)
}

func (s *Service) ListRoles(ctx context.Context) ([]model.Role, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListRoles(ctx)
}

func (s *Service) DeleteRole(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeleteRole, ""); err != nil {
		return err
	}
	return s.store.DeleteRole(ctx, id)
}

// ---- Group (system tier) ----

func (s *Service) PutGroup(ctx context.Context, actor Actor, g model.Group) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutGroup, ""); err != nil {
		return err
	}
	s.stamp(&g.CreatedAt, &g.UpdatedAt)
	return s.store.PutGroup(ctx, g)
}

func (s *Service) GetGroup(ctx context.Context, id string) (model.Group, error) {
	if err := s.requireStore(); err != nil {
		return model.Group{}, err
	}
	return s.store.GetGroup(ctx, id)
}

func (s *Service) ListGroups(ctx context.Context) ([]model.Group, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListGroups(ctx)
}

func (s *Service) DeleteGroup(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeleteGroup, ""); err != nil {
		return err
	}
	return s.store.DeleteGroup(ctx, id)
}

// ---- Account (system tier) ----

func (s *Service) PutAccount(ctx context.Context, actor Actor, a model.Account) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutAccount, ""); err != nil {
		return err
	}
	s.stamp(&a.CreatedAt, &a.UpdatedAt)
	return s.store.PutAccount(ctx, a)
}

func (s *Service) GetAccount(ctx context.Context, id string) (model.Account, error) {
	if err := s.requireStore(); err != nil {
		return model.Account{}, err
	}
	return s.store.GetAccount(ctx, id)
}

func (s *Service) ListAccounts(ctx context.Context) ([]model.Account, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListAccounts(ctx)
}

func (s *Service) DeleteAccount(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeleteAccount, ""); err != nil {
		return err
	}
	return s.store.DeleteAccount(ctx, id)
}

// ---- Membership (account tier; target account is the membership's account) ----

func (s *Service) PutMembership(ctx context.Context, actor Actor, m model.Membership) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutMembership, m.AccountID); err != nil {
		return err
	}
	s.stamp(&m.CreatedAt, &m.UpdatedAt)
	return s.store.PutMembership(ctx, m)
}

func (s *Service) DeleteMembership(ctx context.Context, actor Actor, principalID, accountID string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationDeleteMembership, accountID); err != nil {
		return err
	}
	return s.store.DeleteMembership(ctx, principalID, accountID)
}

// ---- Grant (account tier; target account is the grant's account) ----

func (s *Service) PutGrant(ctx context.Context, actor Actor, g model.Grant) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, authz.MutationPutGrant, g.AccountID); err != nil {
		return err
	}
	s.stamp(&g.CreatedAt, &g.UpdatedAt)
	return s.store.PutGrant(ctx, g)
}

func (s *Service) GetGrant(ctx context.Context, id string) (model.Grant, error) {
	if err := s.requireStore(); err != nil {
		return model.Grant{}, err
	}
	return s.store.GetGrant(ctx, id)
}

func (s *Service) ListGrants(ctx context.Context, accountID string) ([]model.Grant, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListGrants(ctx, accountID)
}

func (s *Service) DeleteGrant(ctx context.Context, actor Actor, id string) error {
	if err := s.requireMutator(); err != nil {
		return err
	}
	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: a mutation requires an authenticated principal")
	}
	// The grant's account governs the account-tier authority check, so it must be
	// loaded before the gate can resolve which account-admin authority is required.
	g, err := s.store.GetGrant(ctx, id)
	if err != nil {
		return err // NOT_FOUND when unknown.
	}
	if err := s.gate.Authorize(ctx, actor.gateActor(), authz.MutationDeleteGrant, g.AccountID); err != nil {
		return err
	}
	return s.store.DeleteGrant(ctx, id)
}

// ---- Delegation (own subset rule; actor = delegator, no admin gate) ----

// Bestow grants `grant` on behalf of delegator, enforcing the delegation subset
// rule (E3-S2). delegator must be the authenticated principal; the admin gate is
// deliberately NOT applied (delegation carries its own authorization).
func (s *Service) Bestow(ctx context.Context, delegator string, grant model.Grant) error {
	if s.deleg == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: delegation is not wired")
	}
	if delegator == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: bestow requires an authenticated delegator")
	}
	return s.deleg.Bestow(ctx, delegator, grant)
}

// Revoke withdraws the grant on behalf of delegator, enforcing the same subset
// rule Bestow applies.
func (s *Service) Revoke(ctx context.Context, delegator, grantID string) error {
	if s.deleg == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: delegation is not wired")
	}
	if delegator == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: revoke requires an authenticated delegator")
	}
	return s.deleg.Revoke(ctx, delegator, grantID)
}

// ---- Impersonation (own guardrails; actor = operator, no admin gate) ----

// ImpersonationStart opens a time-boxed session for operator to impersonate
// target in account, enforcing the impersonation guardrails (E3-S3). operator
// must be the authenticated principal.
func (s *Service) ImpersonationStart(ctx context.Context, operator, target, account string, mode engine.Mode) (*impersonation.Session, error) {
	if s.imperso == nil {
		return nil, aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: impersonation is not wired")
	}
	if operator == "" {
		return nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: impersonation requires an authenticated operator")
	}
	return s.imperso.Start(ctx, operator, target, account, mode)
}

// ImpersonationStop ends a session on behalf of operator. Impersonation sessions
// are stateless, time-boxed values (the client holds the session and presents it
// per decision), so there is no server-side session store to clear: Stop
// validates the operator and acknowledges. It exists so a surface has an explicit
// "I am done" call and a place for E4-S2 to audit the end of a session. See
// FOLLOWUPS for a stateful session registry.
func (s *Service) ImpersonationStop(ctx context.Context, operator string, _ *impersonation.Session) error {
	if s.imperso == nil {
		return aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: impersonation is not wired")
	}
	if operator == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: impersonation stop requires an authenticated operator")
	}
	return nil
}
