package server

import (
	"net/http"
	"strings"

	"github.com/frankbardon/aperture/auth"
	aerr "github.com/frankbardon/aperture/errors"
)

// Authenticate wraps next with the request authentication middleware: it
// resolves the incoming credential to an Aperture principal (via authn) and
// attaches it to the request context, so downstream handlers — and, in E4-S1,
// the full Twirp/HTTP decision surface — can recover the caller with
// auth.PrincipalFromContext.
//
// The bearer is read from the Authorization header. The middleware deliberately
// distinguishes three cases so adding it to the serve wiring does not break the
// existing decision surface (whose enforcement lands in E4-S1):
//
//   - No credential presented → the request proceeds ANONYMOUSLY (no principal
//     in context). This keeps the unauthenticated /check flow working: E3-S5
//     resolves a principal when one is offered; it does not yet REQUIRE one.
//   - A credential is presented and verifies → the resolved principal is
//     attached and the request proceeds.
//   - A credential is presented but FAILS verification → the request is refused
//     401 with the coded error (APERTURE_INVALID_TOKEN / APERTURE_UNAUTHENTICATED).
//     A bad token is a hard failure, never silently downgraded to anonymous.
//
// A nil authn returns next unwrapped — auth is opt-in at the wiring layer.
func Authenticate(authn auth.Authenticator, next http.Handler) http.Handler {
	if authn == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := bearerToken(r)
		if !ok {
			// Anonymous: no credential offered. Proceed without a principal.
			next.ServeHTTP(w, r)
			return
		}
		principalID, claims, err := authn.Authenticate(r.Context(), bearer)
		if err != nil {
			code := aerr.CodeOf(err)
			if code == "" {
				code = aerr.APERTURE_UNAUTHENTICATED
			}
			writeError(w, http.StatusUnauthorized, code, err.Error())
			return
		}
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{ID: principalID, Claims: claims})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. ok is false when the header is absent, not a Bearer scheme, or carries
// an empty token — every one of which is treated as "no credential presented".
// The scheme match is case-insensitive per RFC 7235.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
