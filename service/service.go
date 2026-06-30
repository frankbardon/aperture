// Package service is the thin decision facade that every surface — the CLI
// check command, the HTTP /check endpoint, and (in E4-S1) the Twirp service —
// calls instead of touching the engine directly. It exists so the surfaces
// share ONE code path with ONE fail-closed policy: the rule for turning an
// engine error into a rendered decision lives here, not duplicated per surface.
//
// Fail-closed rendering (per the engine's error contract):
//
//   - A genuine input-validation error (APERTURE_INVALID_INPUT /
//     APERTURE_IDENTITY_INVALID) is returned to the caller verbatim, so the CLI
//     renders a usage error and HTTP returns 400. The caller asked an
//     ill-formed question; that is not a deny.
//   - Every other engine error (an unknown principal surfaces as
//     APERTURE_NOT_FOUND, a storage fault as APERTURE_STORAGE, ...) is rendered
//     fail-closed as a DENY, with the underlying error folded into the reason.
//     A decision point must never fail open.
//   - A clean engine result passes through unchanged.
//
// E4-S1 note: the Twirp handler should call exactly this facade —
// service.New(eng).Check(ctx, service.Query{...}) returning service.Result — so
// the gRPC/Twirp surface inherits the same fail-closed semantics for free.
package service

import (
	"context"
	"fmt"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
)

// Query is a single authorization question in surface-neutral form. It mirrors
// engine.Request but is the type the CLI and HTTP layers marshal to/from, so the
// engine's Request type stays an engine-internal concern.
type Query struct {
	// Account is the active account the decision is scoped to.
	Account string
	// Principal is the id of the principal asking.
	Principal string
	// Action is the verb being attempted.
	Action string
	// Object is the canonical object-identity string.
	Object string
}

// Result is a rendered decision: the verdict, a human-readable reason, and the
// ids of the deciding grant(s). It is the value each surface serializes.
type Result struct {
	// Allow is the verdict: true permits, false denies.
	Allow bool
	// Reason explains the verdict (names the deciding grants, or the fail-closed
	// cause on an operational error).
	Reason string
	// DecidingGrantIDs are the grant ids that produced an allow/deny verdict;
	// empty on a default-deny or a fail-closed deny.
	DecidingGrantIDs []string
}

// Service is the decision facade over an engine.
type Service struct {
	eng *engine.Engine
}

// New returns a Service backed by eng.
func New(eng *engine.Engine) *Service {
	return &Service{eng: eng}
}

// Check answers q. It returns an error ONLY for genuine input-validation
// failures (which surfaces render as 400 / usage errors); every other engine
// failure is folded into a fail-closed DENY Result with a nil error, so a
// decision point never fails open.
func (s *Service) Check(ctx context.Context, q Query) (Result, error) {
	dec, err := s.eng.Check(ctx, engine.Request{
		Account:   q.Account,
		Principal: q.Principal,
		Action:    q.Action,
		Object:    q.Object,
	})
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_INVALID_INPUT, aerr.APERTURE_IDENTITY_INVALID:
			// The caller asked an ill-formed question — surface it as an error.
			return Result{}, err
		default:
			// Operational failure (unknown principal, storage fault, ...): deny.
			return Result{
				Allow:  false,
				Reason: fmt.Sprintf("fail-closed deny: %v", err),
			}, nil
		}
	}
	return Result{
		Allow:            dec.Allow,
		Reason:           dec.Reason,
		DecidingGrantIDs: dec.DecidingGrantIDs,
	}, nil
}
