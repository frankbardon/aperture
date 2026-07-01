package auth

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
)

// devAuthenticator trusts any non-empty bearer as the principal id. It is the
// adapter that makes Aperture runnable with NO external IdP — fixtures, demos,
// and CI present the principal id directly as the bearer ("Authorization:
// Bearer alice" resolves to principal "alice"). It is modelled on orbit's
// devAuthenticator and is the default when no IdP is configured.
//
// It performs NO verification and issues NO credentials: it is a development /
// single-tenant trust shortcut, not a security boundary. Never select it for a
// deployment that faces untrusted callers.
type devAuthenticator struct{}

// NewDev returns the dev/static authenticator: bearer == principal id.
func NewDev() Authenticator { return devAuthenticator{} }

// Authenticate maps the bearer straight to the principal id. There is no claim
// to map (the bearer IS the principal), so the returned claims are nil. An empty
// bearer is APERTURE_UNAUTHENTICATED — even the dev adapter fails closed.
func (devAuthenticator) Authenticate(_ context.Context, bearer string) (string, Claims, error) {
	if bearer == "" {
		return "", nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"auth: empty bearer presented to the dev authenticator")
	}
	return bearer, nil, nil
}
