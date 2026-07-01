package auth

import (
	"context"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	aerr "github.com/frankbardon/aperture/errors"
)

// OIDCOptions configures the OIDC/JWT authenticator. Issuer and Audience are
// the security-critical pair: the verifier rejects any token whose `iss` is not
// Issuer or whose `aud` does not contain Audience. JWKSURL is optional — when
// empty the provider's signing keys are discovered from
// <Issuer>/.well-known/openid-configuration; when set, that JWKS endpoint is
// used directly (skips discovery, e.g. for a provider without a discovery doc).
type OIDCOptions struct {
	// Issuer is the IdP's issuer URL (the expected `iss` claim and the discovery
	// root). Required.
	Issuer string
	// Audience is the expected `aud` claim — Aperture's client identifier at the
	// IdP. Required: an empty audience would accept tokens minted for any client.
	Audience string
	// JWKSURL, when set, is the provider's JWKS endpoint; verification fetches
	// keys from it directly instead of discovering them from Issuer.
	JWKSURL string
	// PrincipalClaim names the verified claim mapped to the Aperture principal id
	// (sub, email, or a custom claim). Defaults to "sub" when empty.
	PrincipalClaim string
	// Now overrides the expiry clock; nil means time.Now. Primarily for tests.
	Now func() time.Time
}

// oidcAuthenticator verifies bearer JWTs against an OIDC provider's published
// keys and maps a verified claim to the principal id.
type oidcAuthenticator struct {
	verifier       *oidc.IDTokenVerifier
	principalClaim string
}

// NewOIDC builds an OIDC/JWT authenticator. It establishes the provider's
// signing keys up front — via JWKS discovery against opts.Issuer, or from
// opts.JWKSURL when given — so per-request verification is offline against the
// cached key set. Discovery performs network I/O, so it takes a ctx. A missing
// issuer or audience is APERTURE_CONFIG_INVALID; a discovery failure is
// APERTURE_BOOT.
func NewOIDC(ctx context.Context, opts OIDCOptions) (Authenticator, error) {
	if opts.Issuer == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID, "auth: oidc issuer is required")
	}
	if opts.Audience == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"auth: oidc audience is required (an empty audience accepts tokens for any client)")
	}

	cfg := &oidc.Config{ClientID: opts.Audience}
	if opts.Now != nil {
		cfg.Now = opts.Now
	}

	var verifier *oidc.IDTokenVerifier
	if opts.JWKSURL != "" {
		keySet := oidc.NewRemoteKeySet(ctx, opts.JWKSURL)
		verifier = oidc.NewVerifier(opts.Issuer, keySet, cfg)
	} else {
		provider, err := oidc.NewProvider(ctx, opts.Issuer)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_BOOT,
				"auth: oidc provider discovery failed", err)
		}
		verifier = provider.Verifier(cfg)
	}
	return &oidcAuthenticator{verifier: verifier, principalClaim: principalClaimOr(opts.PrincipalClaim)}, nil
}

// Authenticate verifies bearer as an OIDC ID token — signature against the
// provider keys, `iss` == Issuer, `aud` ⊇ Audience, and unexpired — then maps
// the configured claim to the principal id. Any verification failure is
// APERTURE_INVALID_TOKEN; a verified token that does not carry the principal
// claim is APERTURE_UNAUTHENTICATED.
func (a *oidcAuthenticator) Authenticate(ctx context.Context, bearer string) (string, Claims, error) {
	if bearer == "" {
		return "", nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED, "auth: empty bearer")
	}
	idToken, err := a.verifier.Verify(ctx, bearer)
	if err != nil {
		return "", nil, aerr.Wrap(aerr.APERTURE_INVALID_TOKEN, "auth: oidc token verification failed", err)
	}
	var claims Claims
	if err := idToken.Claims(&claims); err != nil {
		return "", nil, aerr.Wrap(aerr.APERTURE_INVALID_TOKEN, "auth: decoding oidc claims failed", err)
	}
	principal, err := mapPrincipal(claims, a.principalClaim)
	if err != nil {
		return "", nil, err
	}
	return principal, claims, nil
}

// principalClaimOr returns claim, or the default "sub" when claim is empty.
func principalClaimOr(claim string) string {
	if claim == "" {
		return "sub"
	}
	return claim
}
