package engine

import (
	"context"
	"fmt"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
)

// Mode is the impersonation mode a decision is resolved under.
type Mode string

const (
	// ModeNone is the absence of impersonation: the ordinary path, where a
	// principal acts purely as itself. The zero value, so a zero
	// ImpersonationContext is inert.
	ModeNone Mode = ""
	// ModeAugment ADDS the target's effective permissions to the operator's own:
	// the decision resolves over the union of both subject sets, but the operator
	// keeps acting under its OWN identity. Use it to "see what they can see" while
	// retaining your own authority and audit identity.
	ModeAugment Mode = "augment"
	// ModeBecome FULLY assumes the target's identity for the decision: the
	// decision resolves over the target's subject set ALONE, as if the target had
	// asked. The operator's own grants do not apply. Become is the strictly
	// stronger mode and is gated by a stronger right (see the impersonation
	// package). The audit trail still records the real operator.
	ModeBecome Mode = "become"
)

// Valid reports whether m is a recognised impersonation mode (none counts as
// valid — it is the inert default).
func (m Mode) Valid() bool {
	return m == ModeNone || m == ModeAugment || m == ModeBecome
}

// ImpersonationContext is the decision-context DECORATOR that carries an
// impersonation session into the engine. It NEVER mutates stored grants: it only
// steers which subject set the engine resolves a decision over, then records who
// really acted so the decision can be audited.
//
//   - RealActor is the operator — the principal that truly issued the request and
//     under whose identity audit attributes the action.
//   - EffectiveSubject is the target — the principal whose authority the decision
//     borrows (augment ADDS it to the operator's; become uses it ALONE).
//   - Mode selects augment vs become.
//   - ExpiresAt is the session's hard time-box. The engine enforces it with its
//     injected clock: a presented-but-expired context fails closed to NO
//     elevation (the operator's own authority), never to the target's.
//
// The shape is the audit linkage E4-S2 consumes: every decision made under an
// active session surfaces it on Decision.Impersonation / Trace.Impersonation and
// (for middleware) via the context helpers below.
type ImpersonationContext struct {
	// RealActor is the operator's principal id (the audit identity).
	RealActor string
	// EffectiveSubject is the target's principal id (whose authority is used).
	EffectiveSubject string
	// Mode is augment or become (or none, which is inert).
	Mode Mode
	// ExpiresAt is the session's expiry instant; at or after it the session is
	// expired and confers no elevation.
	ExpiresAt time.Time
}

// active reports whether ic is an in-force impersonation as of now: a real
// augment/become mode that has not yet expired. A none-mode or expired context
// is inert, so the engine resolves the request as the plain operator.
func (ic ImpersonationContext) active(now time.Time) bool {
	if ic.Mode != ModeAugment && ic.Mode != ModeBecome {
		return false
	}
	return now.Before(ic.ExpiresAt)
}

type impersonationKey struct{}

// WithImpersonation returns a context carrying ic, so downstream layers — most
// importantly the audit layer (E4-S2) — can read the real actor and effective
// subject of any decision made while it is set. The engine's *As entry points
// set this on the context they evaluate under; surfaces may also set it before
// calling so audit middleware wrapping the engine sees it.
func WithImpersonation(ctx context.Context, ic ImpersonationContext) context.Context {
	return context.WithValue(ctx, impersonationKey{}, ic)
}

// ImpersonationFromContext returns the impersonation context carried by ctx, if
// any. ok is false when no impersonation was set (the ordinary path).
func ImpersonationFromContext(ctx context.Context) (ImpersonationContext, bool) {
	ic, ok := ctx.Value(impersonationKey{}).(ImpersonationContext)
	return ic, ok
}

// CheckAs resolves a Check under an impersonation decorator (FR-16). It is the
// impersonation-aware sibling of Check: the operator presents a session as ic
// and the engine resolves the effective subject set accordingly. The
// non-impersonated path is byte-for-byte unchanged — when ic is inert (none mode
// or expired) CheckAs delegates straight to Check, so an EXPIRED session fails
// closed to the operator's own authority with NO elevation.
//
// For an active session:
//   - the request's principal MUST be the operator (req.Principal == ic.RealActor);
//   - augment resolves over operator∪target subjects, become over target alone;
//   - the operator and target must BOTH be members of the active account, else
//     the decision is a fail-closed deny (cross-account impersonation refused);
//   - the returned Decision carries ic on Decision.Impersonation for audit.
func (e *Engine) CheckAs(ctx context.Context, req Request, ic ImpersonationContext) (Decision, error) {
	if !ic.active(e.now()) {
		return e.Check(ctx, req)
	}
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}
	if err := validateImpersonation(req.Principal, ic); err != nil {
		return Decision{}, err
	}
	object, err := identity.Parse(req.Object)
	if err != nil {
		return Decision{}, err
	}

	subjects, ok, err := e.elevatedSubjects(ctx, req.Account, ic)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return crossAccountImpersonationDeny(req, ic), nil
	}

	ctx = WithImpersonation(ctx, ic)
	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return Decision{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for impersonated subjects", err)
	}
	permCache := make(map[string]*model.Permission, len(grants))
	dec, err := e.evaluate(ctx, req, object, grants, permCache)
	if err != nil {
		return Decision{}, err
	}
	dec.Impersonation = cloneIC(ic)
	return dec, nil
}

// EnumerateAs is the impersonation-aware sibling of Enumerate: it lists the
// objects the EFFECTIVE subject set may act on. Inert ic delegates to Enumerate
// (an expired session enumerates only the operator's own access). For an active
// session a cross-account boundary violation fails closed to the empty set.
func (e *Engine) EnumerateAs(ctx context.Context, req EnumerateRequest, ic ImpersonationContext) ([]string, error) {
	if !ic.active(e.now()) {
		return e.Enumerate(ctx, req)
	}
	if err := validateEnumerateRequest(req); err != nil {
		return nil, err
	}
	if err := validateImpersonation(req.Principal, ic); err != nil {
		return nil, err
	}
	query, err := identity.ParsePattern(req.Pattern)
	if err != nil {
		return nil, err
	}
	subjects, ok, err := e.elevatedSubjects(ctx, req.Account, ic)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{}, nil
	}
	ctx = WithImpersonation(ctx, ic)
	return e.enumerateWithSubjects(ctx, req, query, subjects)
}

// ExplainAs is the impersonation-aware sibling of Explain: it returns the full
// derivation resolved over the effective subject set, with ic attached to the
// Trace so the diagnostic shows both the real operator (Trace.Request.Principal)
// and the effective subject set (Trace.Subjects). Inert ic delegates to Explain.
// A cross-account boundary violation yields a fail-closed deny trace.
func (e *Engine) ExplainAs(ctx context.Context, req Request, ic ImpersonationContext) (Trace, error) {
	if !ic.active(e.now()) {
		return e.Explain(ctx, req)
	}
	if err := validateRequest(req); err != nil {
		return Trace{}, err
	}
	if err := validateImpersonation(req.Principal, ic); err != nil {
		return Trace{}, err
	}
	object, err := identity.Parse(req.Object)
	if err != nil {
		return Trace{}, err
	}
	subjects, ok, err := e.elevatedSubjects(ctx, req.Account, ic)
	if err != nil {
		return Trace{}, err
	}
	if !ok {
		dec := crossAccountImpersonationDeny(req, ic)
		return Trace{Request: req, Decision: dec, Considered: []GrantEvaluation{}, Impersonation: cloneIC(ic)}, nil
	}
	ctx = WithImpersonation(ctx, ic)
	tr, err := e.explainWithSubjects(ctx, req, object, subjects)
	if err != nil {
		return Trace{}, err
	}
	tr.Impersonation = cloneIC(ic)
	return tr, nil
}

// elevatedSubjects computes the subject set an ACTIVE impersonation resolves
// over and enforces the cross-account boundary. The caller has already verified
// ic.active. ok is false (a fail-closed refusal) when the operator or the target
// is not a member of account — neither mode may cross an account boundary. On
// ok, augment yields operator∪target subjects and become yields the target's
// subjects alone.
func (e *Engine) elevatedSubjects(ctx context.Context, account string, ic ImpersonationContext) (subjects []model.Subject, ok bool, err error) {
	opMember, err := e.store.IsMember(ctx, ic.RealActor, account)
	if err != nil {
		return nil, false, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to check operator membership for impersonation", err)
	}
	tgtMember, err := e.store.IsMember(ctx, ic.EffectiveSubject, account)
	if err != nil {
		return nil, false, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to check target membership for impersonation", err)
	}
	if !opMember || !tgtMember {
		return nil, false, nil
	}

	target, err := e.subjectSet(ctx, ic.EffectiveSubject)
	if err != nil {
		return nil, false, err
	}
	if ic.Mode == ModeBecome {
		return target, true, nil
	}

	// Augment: union the operator's own subject set with the target's, deduped so
	// a subject shared by both (e.g. a common group) is consulted once.
	operator, err := e.subjectSet(ctx, ic.RealActor)
	if err != nil {
		return nil, false, err
	}
	return unionSubjects(operator, target), true, nil
}

// unionSubjects returns the deduplicated union of two subject sets, preserving
// the first set's order then appending the second set's new members.
func unionSubjects(a, b []model.Subject) []model.Subject {
	seen := make(map[model.Subject]struct{}, len(a)+len(b))
	out := make([]model.Subject, 0, len(a)+len(b))
	for _, s := range a {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// validateImpersonation rejects a malformed active impersonation decorator: an
// empty target, or a request whose principal is not the operator named as the
// real actor. The operator always presents the session under its OWN identity,
// so a mismatch is a caller bug (APERTURE_INVALID_INPUT), not a deny.
func validateImpersonation(reqPrincipal string, ic ImpersonationContext) error {
	if !ic.Mode.Valid() {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"engine: unknown impersonation mode %q", ic.Mode)
	}
	if ic.RealActor == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: impersonation real actor is empty")
	}
	if ic.EffectiveSubject == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: impersonation effective subject is empty")
	}
	if reqPrincipal != ic.RealActor {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"engine: request principal %q must be the impersonation operator %q",
			reqPrincipal, ic.RealActor)
	}
	return nil
}

// crossAccountImpersonationDeny is the fail-closed verdict when an impersonation
// session is presented across an account boundary (operator or target not a
// member of the active account). It names no deciding grant — the refusal
// precedes grant evaluation — and still carries the attempted context for audit.
func crossAccountImpersonationDeny(req Request, ic ImpersonationContext) Decision {
	return Decision{
		Allow: false,
		Reason: fmt.Sprintf(
			"impersonation refused: operator %q and target %q must both be members of active account %q",
			ic.RealActor, ic.EffectiveSubject, req.Account),
		Impersonation: cloneIC(ic),
	}
}

// cloneIC copies ic onto the heap so callers receive a distinct pointer per
// decision (the value is small and immutable once returned).
func cloneIC(ic ImpersonationContext) *ImpersonationContext {
	c := ic
	return &c
}
