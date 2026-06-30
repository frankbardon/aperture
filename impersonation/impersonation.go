// Package impersonation implements "act on behalf of another user" (FR-16): a
// privileged operator borrows a target's authority for a decision, in one of two
// modes, under hard guardrails. It is the start/gating half of the feature; the
// engine's *As entry points (engine.CheckAs/EnumerateAs/ExplainAs) are the
// decision half that consumes a started session.
//
// Two modes, one strictly stronger than the other:
//
//   - AUGMENT adds the target's effective permissions to the operator's own. The
//     operator keeps acting under its OWN identity; the decision resolves over the
//     union of both subject sets.
//   - BECOME fully assumes the target's identity: the decision resolves over the
//     target's subject set alone, as if the target had asked. The operator's own
//     grants do not apply.
//
// Impersonation is itself a permission, scoped exactly like any other. Two
// reserved action verbs gate the two modes — AugmentAction and BecomeAction — and
// an operator "may impersonate" a target within an account when its effective
// allow grant set holds the matching right whose object pattern covers the
// target principal's identity. BECOME REQUIRES THE STRICTLY STRONGER right:
// holding the become right implies the augment right (it can do either), but the
// augment right alone can never become a target. This is the separate-gating-right
// rule the story requires.
//
// Start gates and issues a TIME-BOXED session; the guardrails are conjunctive and
// fail closed. An operator may open a session over a target only when ALL hold,
// checked against the operator's (operator, account) effective grants:
//
//   - account boundary: BOTH the operator and the target are members of the active
//     account. A session spanning accounts is refused (no cross-account
//     impersonation). This is independently re-checked by the engine on every
//     decision, so a forged context cannot bypass it.
//   - right held: an effective allow grant on the mode's action (or, for augment,
//     the stronger become action) whose object pattern covers the target's
//     identity.
//
// On success the session carries an expiry (now + ttl, injected clock so tests
// never touch the wall clock). The session is presented to the engine as an
// engine.ImpersonationContext; an expired one confers NO elevation. Every
// resulting decision records BOTH the real operator and the effective subject for
// the audit layer (E4-S2) — impersonation is never silent.
//
// The gating rule (mayStart) is pure and storage-free, so the security-critical
// check is unit-testable in isolation.
package impersonation

import (
	"context"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
)

// The reserved impersonation action verbs. An object type opts a target into
// being impersonable by declaring these verbs and a permission on each; an
// operator is granted that permission to gain the right. They are analogous to
// delegation's DelegateAction.
const (
	// AugmentAction is the right to open an AUGMENT session: borrow the target's
	// permissions while keeping your own identity. The weaker of the two.
	AugmentAction = "aperture.impersonate.augment"
	// BecomeAction is the right to open a BECOME session: fully assume the
	// target's identity. The strictly stronger right — it also satisfies augment.
	BecomeAction = "aperture.impersonate.become"
)

// DefaultTTL is the time-box applied to a session when no WithTTL is given. It is
// deliberately short: impersonation is a privileged, transient act.
const DefaultTTL = 15 * time.Minute

// Session is a started, time-boxed impersonation grant. It records the operator
// (RealActor), the target (Subject, a principal id), the account it is scoped to,
// the mode, and its lifetime. It is presented to the engine via Context; an
// expired session confers no elevation.
type Session struct {
	// RealActor is the operator's principal id — the audit identity.
	RealActor string
	// Subject is the target's principal id — whose authority is borrowed.
	Subject string
	// Account is the active account the session is scoped to. Both principals are
	// members of it; the engine refuses to resolve the session in any other.
	Account string
	// Mode is augment or become.
	Mode engine.Mode
	// StartedAt is when the session was issued (the service clock).
	StartedAt time.Time
	// ExpiresAt is the hard time-box: at or after it the session is expired.
	ExpiresAt time.Time
}

// Active reports whether the session is still in force as of now.
func (s Session) Active(now time.Time) bool {
	return now.Before(s.ExpiresAt)
}

// Context returns the engine decorator for the session — the value passed to
// engine.CheckAs/EnumerateAs/ExplainAs. It carries the expiry, so the engine
// enforces the time-box itself; an expired context confers no elevation.
func (s Session) Context() engine.ImpersonationContext {
	return engine.ImpersonationContext{
		RealActor:        s.RealActor,
		EffectiveSubject: s.Subject,
		Mode:             s.Mode,
		ExpiresAt:        s.ExpiresAt,
	}
}

// Live returns the engine decorator for the session, or APERTURE_IMPERSONATION_EXPIRED
// when it has already expired as of now. It is the hard-error guard for surfaces
// that want to reject an expired session up front rather than silently resolving
// with no elevation (the engine's own fail-closed behaviour). Either way an
// expired session never elevates.
func (s Session) Live(now time.Time) (engine.ImpersonationContext, error) {
	if !s.Active(now) {
		return engine.ImpersonationContext{}, aerr.WithContext(
			aerr.APERTURE_IMPERSONATION_EXPIRED,
			"impersonation session has expired",
			map[string]any{
				"operator":   s.RealActor,
				"target":     s.Subject,
				"expires_at": s.ExpiresAt,
			})
	}
	return s.Context(), nil
}

// Service starts impersonation sessions, enforcing the start-time guardrails. It
// is the single code path E4-S1 (Twirp) and the operator surfaces drive, so the
// gating is applied in exactly one place.
type Service struct {
	store model.Storage
	eng   *engine.Engine
	now   func() time.Time
	ttl   time.Duration
}

// Option configures a Service at construction.
type Option func(*Service)

// WithClock overrides the session clock (StartedAt/ExpiresAt). It exists for
// deterministic tests; production uses time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithTTL overrides the session time-box. A non-positive ttl is ignored (the
// default stands), so a session is always bounded.
func WithTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// New returns a Service backed by store for membership/principal/permission
// lookups and eng for resolving the operator's effective (account-scoped) grant
// set.
func New(store model.Storage, eng *engine.Engine, opts ...Option) *Service {
	s := &Service{store: store, eng: eng, now: time.Now, ttl: DefaultTTL}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start opens a time-boxed session for operator to impersonate target within
// account, in mode. It enforces the full guardrail set (account boundary + the
// mode's gating right) and fails closed with an APERTURE_IMPERSONATION_DENIED
// coded error — naming the failed guard in Context["reason"] — when authority
// cannot be proven. On success the session is bounded by the service clock + ttl.
func (s *Service) Start(ctx context.Context, operator, target, account string, mode engine.Mode) (*Session, error) {
	switch {
	case operator == "":
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "impersonation: operator is empty")
	case target == "":
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "impersonation: target is empty")
	case account == "":
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "impersonation: account is empty")
	}
	if mode != engine.ModeAugment && mode != engine.ModeBecome {
		return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"impersonation: mode must be augment or become, got %q", mode)
	}

	// Account boundary: the operator must be a member of the account it acts in.
	opMember, err := s.store.IsMember(ctx, operator, account)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"impersonation: failed to check operator membership", err)
	}
	if !opMember {
		return nil, aerr.WithContext(aerr.APERTURE_IMPERSONATION_DENIED,
			"operator is not a member of the active account",
			map[string]any{"reason": "operator_not_member", "operator": operator, "account": account})
	}

	// The target must exist and be a member of the SAME account — no cross-account
	// impersonation. GetPrincipal also yields the target's identity for the
	// right-coverage check below.
	targetPrincipal, err := s.store.GetPrincipal(ctx, target)
	if err != nil {
		return nil, err // NOT_FOUND when the target is unknown.
	}
	tgtMember, err := s.store.IsMember(ctx, target, account)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"impersonation: failed to check target membership", err)
	}
	if !tgtMember {
		return nil, aerr.WithContext(aerr.APERTURE_IMPERSONATION_DENIED,
			"target is not a member of the active account",
			map[string]any{"reason": "cross_account", "target": target, "account": account})
	}

	// Gating right: the operator must hold the mode's right over the target.
	held, err := s.effectiveAllows(ctx, account, operator)
	if err != nil {
		return nil, err
	}
	targetIdent, err := identity.ParsePattern(targetPrincipal.Identity)
	if err != nil {
		// A stored principal's identity must parse; a corrupt one fails closed.
		return nil, aerr.Wrapf(aerr.APERTURE_IMPERSONATION_DENIED, err,
			"impersonation: target %q has an unparseable identity %q", target, targetPrincipal.Identity)
	}
	if err := mayStart(mode, targetIdent, held); err != nil {
		return nil, err
	}

	now := s.now()
	return &Session{
		RealActor: operator,
		Subject:   target,
		Account:   account,
		Mode:      mode,
		StartedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}, nil
}

// authority pairs one of the operator's effective grants with its resolved
// permission, so the gating rule can read the object pattern (from the grant) and
// the action (from the permission).
type authority struct {
	grant      model.Grant
	permission model.Permission
}

// effectiveAllows loads the operator's account-scoped effective grants and pairs
// each ALLOW grant with its permission. Deny grants confer no right, and a grant
// whose permission no longer exists is inert.
func (s *Service) effectiveAllows(ctx context.Context, account, operator string) ([]authority, error) {
	grants, err := s.eng.EffectiveGrants(ctx, account, operator)
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

// mayStart is the pure gating rule: it reports whether an operator holding the
// effective allow authorities `held` may open a session of `mode` over a target
// whose identity is targetIdent. It performs NO storage access, so the
// security-critical right check is unit-testable in isolation. It returns nil to
// allow, or an APERTURE_IMPERSONATION_DENIED coded error naming the missing right.
//
// The strictly-stronger rule:
//   - BECOME requires an allow on BecomeAction covering the target.
//   - AUGMENT requires an allow on AugmentAction OR BecomeAction covering the
//     target — so a become-right holder can also augment (it implies augment),
//     but an augment-right holder can never become.
func mayStart(mode engine.Mode, targetIdent identity.Pattern, held []authority) error {
	switch mode {
	case engine.ModeBecome:
		if coversTarget(held, BecomeAction, targetIdent) {
			return nil
		}
		return aerr.WithContext(aerr.APERTURE_IMPERSONATION_DENIED,
			"operator holds no become right covering the target",
			map[string]any{"reason": "no_become_right", "action": BecomeAction})
	case engine.ModeAugment:
		if coversTarget(held, BecomeAction, targetIdent) || coversTarget(held, AugmentAction, targetIdent) {
			return nil
		}
		return aerr.WithContext(aerr.APERTURE_IMPERSONATION_DENIED,
			"operator holds no augment right covering the target",
			map[string]any{"reason": "no_augment_right", "action": AugmentAction})
	default:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"impersonation: unknown mode %q", mode)
	}
}

// coversTarget reports whether `held` contains an allow authority whose
// permission has the given action and whose object pattern Contains targetIdent.
// A held grant with an unparseable object pattern is skipped defensively (writes
// validate patterns, so this is a corrupt-storage guard that fails closed).
func coversTarget(held []authority, action string, targetIdent identity.Pattern) bool {
	for _, a := range held {
		if a.permission.Action != action {
			continue
		}
		heldPat, err := identity.ParsePattern(a.grant.Object)
		if err != nil {
			continue
		}
		if identity.Contains(heldPat, targetIdent) {
			return true
		}
	}
	return false
}
