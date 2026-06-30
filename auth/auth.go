// Package auth turns an incoming external credential into a known Aperture
// principal. Authentication is ALWAYS external: Aperture consumes credentials,
// it never issues them (there is no login or credential-issuance surface here).
//
// The package is structured exactly like orbit's auth seam — a small
// Authenticator interface plus a set of interchangeable adapters, with the
// chosen adapter selected by configuration and applied to HTTP requests as
// middleware (see internal/server). Three adapters ship:
//
//   - dev/static (NewDev) — trusts any non-empty bearer as the principal id, so
//     Aperture runs with NO external IdP (fixtures, demos, CI). It is the
//     default when no IdP is configured, modelled on orbit's devAuthenticator.
//   - oidc (NewOIDC) — verifies a bearer JWT (signature, issuer, audience,
//     expiry) against an OIDC provider's published metadata/JWKS using the
//     standard pure-Go github.com/coreos/go-oidc verifier.
//   - parsec (NewParsec) — verifies a token minted by the
//     github.com/frankbardon/parsec token broker against its signing keyring
//     (orbit's realtime/token-broker pattern).
//
// Every adapter resolves the principal id through the SAME configurable
// claim→principal mapping (PrincipalClaim): which verified claim — sub, email,
// or a custom claim — becomes the Aperture principal id is configuration, not
// code. The dev adapter is the one exception by construction: the bearer is the
// principal, so there is no claim to map.
package auth

import (
	"context"
	"fmt"

	aerr "github.com/frankbardon/aperture/errors"
)

// Claims is the set of verified assertions an authenticator extracted from a
// credential, keyed by claim name. It is always a generic map so the
// claim→principal mapping (and downstream consumers, e.g. the engine in E4-S1)
// can read any claim uniformly regardless of which adapter produced it. A dev
// authenticator returns a nil/empty map — the bearer alone identifies the
// principal.
type Claims map[string]any

// Authenticator turns an external bearer credential into an Aperture principal
// id plus the verified claims behind it. The input is the raw bearer token (the
// value after "Bearer " in the Authorization header); the middleware in
// internal/server extracts it from the *http.Request before calling this. This
// mirrors orbit's tokenbroker.Authenticator, whose Authenticate also takes a
// bearer string — Aperture adds the claim map so the principal mapping is
// configurable and downstream surfaces can read the verified assertions.
//
// Implementations MUST fail closed: a missing, malformed, or unverifiable
// credential is an error (APERTURE_UNAUTHENTICATED / APERTURE_INVALID_TOKEN),
// never a silently-empty principal.
type Authenticator interface {
	// Authenticate verifies bearer and returns the resolved principal id and the
	// verified claims. It returns APERTURE_UNAUTHENTICATED when no principal can
	// be derived (empty bearer, or a verified token missing the principal claim)
	// and APERTURE_INVALID_TOKEN when a presented credential fails verification.
	Authenticate(ctx context.Context, bearer string) (principalID string, claims Claims, err error)
}

// Principal is the authenticated identity attached to a request context by the
// middleware: the resolved principal id and the claims it was derived from.
type Principal struct {
	// ID is the Aperture principal id the credential resolved to.
	ID string
	// Claims are the verified assertions behind the principal (empty for dev).
	Claims Claims
}

// ctxKey is the unexported context key the resolved Principal is stored under.
type ctxKey struct{}

// WithPrincipal returns a child context carrying p. The middleware calls it
// after a successful Authenticate so handlers (and, in E4-S1, the decision
// surface) can recover the caller via PrincipalFromContext.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFromContext recovers the Principal attached by the middleware. ok is
// false for an anonymous request (no credential presented) — callers that
// require a principal treat !ok as APERTURE_UNAUTHENTICATED.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// mapPrincipal applies the configurable claim→principal mapping: it reads the
// claim named by principalClaim from claims and returns its string value as the
// principal id. It is the single place every verifying adapter (oidc, parsec)
// derives the principal, so "which claim is the principal id" stays
// configuration. A missing or non-string/empty claim is APERTURE_UNAUTHENTICATED
// — the token verified, but it does not name a principal Aperture can use.
func mapPrincipal(claims Claims, principalClaim string) (string, error) {
	raw, ok := claims[principalClaim]
	if !ok {
		return "", aerr.WithContext(aerr.APERTURE_UNAUTHENTICATED,
			"auth: token is missing the configured principal claim",
			map[string]any{"principal_claim": principalClaim})
	}
	s, ok := raw.(string)
	if !ok {
		return "", aerr.WithContext(aerr.APERTURE_UNAUTHENTICATED,
			"auth: principal claim is not a string",
			map[string]any{"principal_claim": principalClaim, "type": fmt.Sprintf("%T", raw)})
	}
	if s == "" {
		return "", aerr.WithContext(aerr.APERTURE_UNAUTHENTICATED,
			"auth: principal claim is empty",
			map[string]any{"principal_claim": principalClaim})
	}
	return s, nil
}
