// Package delegation implements "bestow" (FR-15): a designated principal grants
// a permission it already holds to another principal in the same account,
// WITHOUT privilege escalation or cross-account leakage.
//
// Delegation is itself a permission. A principal "may delegate" within an account
// when its effective grant set contains an allow grant on the reserved
// DelegateAction whose object pattern covers the object being bestowed. So the
// right to delegate is scoped exactly like any other permission — a delegate
// grant over "account:acme/project:atlas/**" authorizes bestowing only within
// that subtree — and it is just a normal grant on a normal permission, with no
// special storage.
//
// The bestow rule is conjunctive and fail-closed. A delegator may bestow a grant
// G only when ALL of the following hold, checked against its (delegator,
// account) effective grant set from E3-S1:
//
//   - account membership: the delegator is a member of G's account. A bestow
//     stamped to any other account is rejected (cross-account leakage guard).
//   - may-delegate: an effective allow grant D_del exists with
//     D_del.permission.action == DelegateAction and D_del.object ⊇ G.object.
//   - delegatable: G's permission is flagged Delegatable.
//   - subset: an effective allow grant D exists with the SAME action and scope
//     strategy as G's permission and D.object ⊇ G.object — i.e. G is a subset of
//     the delegator's own authority, never broader.
//
// "⊇" is identity.Contains: G's object pattern is equal-or-more-specific than /
// contained within D's. A more-specific pattern under D is a subset; a
// broader-or-disjoint one is not. The containment test is conservative (sound,
// possibly incomplete), so when authority cannot be proven the bestow is DENIED.
//
// Bestowed grants are ordinary account-scoped grants: the engine treats them
// identically to any other grant and they vanish outside their account, because
// every grant query is account-scoped (E3-S1). Revoke is the inverse mutation,
// gated by the same authority check so a delegator can withdraw only grants it
// could itself have bestowed.
//
// Bestow and Revoke are mutations: E4-S2 audits them and E4-S1 exposes them over
// Twirp. They live here as a service-level operation so those surfaces wrap this
// one code path, and the pure subset rule (canBestow) is unit-testable without
// storage.
package delegation

import (
	"context"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
)

// DelegateAction is the reserved action verb that represents the "may delegate"
// right. A principal that holds an effective allow grant on a permission with
// this action may bestow grants within that grant's object scope. An object type
// opts a resource into delegation by declaring this verb and a permission on it.
const DelegateAction = "aperture.delegate"

// Service is the delegation facade: it bestows and revokes account-scoped grants
// on behalf of a delegator, enforcing the fail-closed subset rule. It is the
// single code path E4-S1 (Twirp) and E6-S3 (UI) drive.
type Service struct {
	store model.Storage
	eng   *engine.Engine
	now   func() time.Time
}

// Option configures a Service at construction.
type Option func(*Service)

// WithClock overrides the timestamp source stamped onto bestowed grants. It
// exists for deterministic tests; production uses the default time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns a Service backed by store for persistence and eng for resolving
// the delegator's effective (account-scoped) grant set.
func New(store model.Storage, eng *engine.Engine, opts ...Option) *Service {
	s := &Service{store: store, eng: eng, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Bestow grants `grant` on behalf of `delegator`, but only if the delegator's
// authority within grant.AccountID covers it (membership + may-delegate +
// delegatable + subset). On success the grant is stamped with the service clock
// and persisted as an ordinary account-scoped grant. On any authority failure it
// returns an APERTURE_DELEGATION_* coded error and writes nothing — bestow fails
// closed.
func (s *Service) Bestow(ctx context.Context, delegator string, grant model.Grant) error {
	if delegator == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "delegation: delegator is empty")
	}
	if err := model.ValidateGrant(grant); err != nil {
		return err
	}

	if _, err := s.authorize(ctx, delegator, grant); err != nil {
		return err
	}

	now := s.now()
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = now
	}
	grant.UpdatedAt = now
	if err := s.store.PutGrant(ctx, grant); err != nil {
		return err
	}
	return nil
}

// Revoke withdraws the grant identified by grantID on behalf of delegator. A
// delegator may revoke only a grant it could itself bestow now — the same
// authority check Bestow applies — so revocation cannot be used to reach across
// accounts or beyond the delegator's own scope. A missing grant is
// APERTURE_NOT_FOUND.
func (s *Service) Revoke(ctx context.Context, delegator string, grantID string) error {
	if delegator == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "delegation: delegator is empty")
	}
	if grantID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "delegation: grant id is empty")
	}
	grant, err := s.store.GetGrant(ctx, grantID)
	if err != nil {
		return err // NOT_FOUND when the grant is unknown.
	}
	if _, err := s.authorize(ctx, delegator, grant); err != nil {
		return err
	}
	return s.store.DeleteGrant(ctx, grantID)
}

// authorize enforces the full bestow rule for grant on behalf of delegator and,
// on success, returns the grant's resolved permission. It is shared by Bestow and
// Revoke so the two mutations apply one identical, fail-closed authority check.
func (s *Service) authorize(ctx context.Context, delegator string, grant model.Grant) (model.Permission, error) {
	// Cross-account guard: a delegator may only act within an account it belongs
	// to. Even a multi-account delegator's authority in another account is moot —
	// the effective-grant query below is scoped to grant.AccountID — but rejecting
	// a non-member up front gives a precise, defence-in-depth error.
	member, err := s.store.IsMember(ctx, delegator, grant.AccountID)
	if err != nil {
		return model.Permission{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"delegation: failed to check delegator membership", err)
	}
	if !member {
		return model.Permission{}, aerr.WithContext(aerr.APERTURE_DELEGATION_DENIED,
			"delegator is not a member of the grant's account",
			map[string]any{"reason": "cross_account", "delegator": delegator, "account": grant.AccountID})
	}

	targetPerm, err := s.store.GetPermission(ctx, grant.PermissionID)
	if err != nil {
		return model.Permission{}, err // NOT_FOUND when the permission is unknown.
	}

	held, err := s.effectiveAllows(ctx, grant.AccountID, delegator)
	if err != nil {
		return model.Permission{}, err
	}
	if err := canBestow(grant, targetPerm, held); err != nil {
		return model.Permission{}, err
	}
	return targetPerm, nil
}

// authority pairs one of the delegator's effective grants with its resolved
// permission, so the subset rule can read both the object pattern (from the
// grant) and the action / scope strategy (from the permission).
type authority struct {
	grant      model.Grant
	permission model.Permission
}

// effectiveAllows loads the delegator's account-scoped effective grants and pairs
// each ALLOW grant with its permission. Deny grants are dropped: a deny restricts
// the delegator, it never confers authority to hand on. A grant whose permission
// no longer exists is also dropped (a dangling reference confers nothing).
func (s *Service) effectiveAllows(ctx context.Context, account, delegator string) ([]authority, error) {
	grants, err := s.eng.EffectiveGrants(ctx, account, delegator)
	if err != nil {
		return nil, err
	}
	out := make([]authority, 0, len(grants))
	perms := make(map[string]model.Permission, len(grants))
	for _, g := range grants {
		if g.Effect != model.EffectAllow {
			continue
		}
		perm, ok := perms[g.PermissionID]
		if !ok {
			p, err := s.store.GetPermission(ctx, g.PermissionID)
			if err != nil {
				if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
					continue // dangling permission reference: inert.
				}
				return nil, err
			}
			perm = p
			perms[g.PermissionID] = perm
		}
		out = append(out, authority{grant: g, permission: perm})
	}
	return out, nil
}

// canBestow is the pure subset rule: it reports whether a delegator holding the
// effective allow authorities `held` may bestow `target` (resolved permission
// `targetPerm`). It performs NO storage access, so the security-critical rule is
// unit-testable in isolation. It returns nil to allow, or an APERTURE_DELEGATION_*
// coded error naming the first failed condition.
//
// Order of checks (each fails closed):
//  1. effect — only an allow grant may be bestowed (a delegated deny is out of
//     scope for this story; see FOLLOWUPS).
//  2. delegatable — the permission must be flagged Delegatable.
//  3. may-delegate — held must include an allow on DelegateAction whose object
//     covers target's object.
//  4. subset — held must include an allow with target's action AND scope strategy
//     whose object covers target's object.
func canBestow(target model.Grant, targetPerm model.Permission, held []authority) error {
	if target.Effect != model.EffectAllow {
		return aerr.WithContext(aerr.APERTURE_DELEGATION_DENIED,
			"only allow grants may be bestowed",
			map[string]any{"reason": "non_allow_effect", "grant": target.ID, "effect": string(target.Effect)})
	}
	if !targetPerm.Delegatable {
		return aerr.WithContext(aerr.APERTURE_DELEGATION_NOT_DELEGATABLE,
			"the permission is not flagged delegatable",
			map[string]any{"grant": target.ID, "permission": targetPerm.ID})
	}

	targetPat, err := identity.ParsePattern(target.Object)
	if err != nil {
		return err
	}

	// (3) may-delegate: the delegator must hold the right to delegate over the
	// target object.
	if !coversObject(held, DelegateAction, "", targetPat, false) {
		return aerr.WithContext(aerr.APERTURE_DELEGATION_DENIED,
			"delegator holds no may-delegate right covering the grant's object",
			map[string]any{"reason": "no_delegate_right", "grant": target.ID, "object": target.Object})
	}

	// (4) subset: the delegator must already hold the underlying permission, at the
	// same scope strategy, over a scope that contains the target object.
	if !coversObject(held, targetPerm.Action, targetPerm.ScopeStrategy, targetPat, true) {
		return aerr.WithContext(aerr.APERTURE_DELEGATION_DENIED,
			"bestowed grant is not a subset of the delegator's effective grants",
			map[string]any{
				"reason":         "not_subset",
				"grant":          target.ID,
				"action":         targetPerm.Action,
				"scope_strategy": targetPerm.ScopeStrategy,
				"object":         target.Object,
			})
	}
	return nil
}

// coversObject reports whether `held` contains an allow authority whose
// permission has the given action — and, when matchStrategy is set, the given
// scope strategy — and whose object pattern Contains targetPat. A held grant with
// an unparseable object pattern is skipped defensively (writes validate patterns,
// so this is a corrupt-storage guard that fails closed rather than panicking).
func coversObject(held []authority, action, strategy string, targetPat identity.Pattern, matchStrategy bool) bool {
	for _, a := range held {
		if a.permission.Action != action {
			continue
		}
		if matchStrategy && a.permission.ScopeStrategy != strategy {
			continue
		}
		heldPat, err := identity.ParsePattern(a.grant.Object)
		if err != nil {
			continue
		}
		if identity.Contains(heldPat, targetPat) {
			return true
		}
	}
	return false
}
