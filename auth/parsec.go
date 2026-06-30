package auth

import (
	"context"
	"encoding/json"
	"path/filepath"

	parsecauth "github.com/frankbardon/parsec/auth"

	aerr "github.com/frankbardon/aperture/errors"
)

// ParsecOptions configures the parsec token-broker authenticator. The adapter
// verifies a token minted by a github.com/frankbardon/parsec token broker
// against the broker's signing keyring, then maps a claim to the principal id.
// This is orbit's realtime/token-broker pattern: the broker (orbit mounts it
// under /parsec, persisting its keyring under StateDir) issues a client a
// short-lived access token; Aperture loads that same keyring and verifies the
// token to learn who the caller is.
//
// The keyring is the integration seam: the broker and Aperture must share it,
// because a parsec key carries a per-key id (kid) the verifier matches the
// token against. Point Aperture at the broker's persisted keyring exactly one of
// two ways (KeyringPath wins): KeyringPath is the broker's keyring.json directly;
// StateDir is the broker's state directory, inside which keyring.json is
// resolved (the orbit serve.go shape — StateDir = <DataDir>/parsec).
type ParsecOptions struct {
	// KeyringPath is a path to the broker's keyring.json (its persisted signing
	// keys).
	KeyringPath string
	// StateDir is the broker's state directory; keyring.json is resolved inside
	// it. Used when KeyringPath is empty.
	StateDir string
	// PrincipalClaim names the verified claim mapped to the principal id.
	// Defaults to "sub" (parsec stamps the user id into `sub`).
	PrincipalClaim string
	// ExpectedType is the parsec token type to accept; defaults to access (the
	// client connection token a caller presents).
	ExpectedType parsecauth.Type
}

// parsecAuthenticator verifies parsec-broker JWTs against a keyring.
type parsecAuthenticator struct {
	verifier       *parsecauth.Verifier
	expected       parsecauth.Type
	principalClaim string
}

// NewParsec builds the parsec token-broker authenticator. It loads the broker's
// signing keyring (from Secret, KeyringPath, or StateDir) and constructs a
// verifier that follows the ring by reference, so a broker key rotation that
// rewrites the ring takes effect without restarting Aperture. A missing keyring
// source is APERTURE_CONFIG_INVALID; a load failure is APERTURE_BOOT.
func NewParsec(opts ParsecOptions) (Authenticator, error) {
	ring, err := loadParsecKeyRing(opts)
	if err != nil {
		return nil, err
	}
	verifier, err := parsecauth.NewVerifier(ring)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "auth: parsec verifier init failed", err)
	}
	expected := opts.ExpectedType
	if expected == "" {
		expected = parsecauth.TypeAccess
	}
	return &parsecAuthenticator{
		verifier:       verifier,
		expected:       expected,
		principalClaim: principalClaimOr(opts.PrincipalClaim),
	}, nil
}

// loadParsecKeyRing resolves the broker keyring from the configured source.
func loadParsecKeyRing(opts ParsecOptions) (*parsecauth.KeyRing, error) {
	switch {
	case opts.KeyringPath != "":
		ring, err := parsecauth.LoadKeyRing(opts.KeyringPath)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_BOOT, "auth: load parsec keyring failed", err)
		}
		return ring, nil
	case opts.StateDir != "":
		ring, err := parsecauth.LoadKeyRing(filepath.Join(opts.StateDir, parsecauth.KeyringFileName))
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_BOOT, "auth: load parsec keyring from state dir failed", err)
		}
		return ring, nil
	default:
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"auth: parsec adapter needs a keyring source (keyring path or broker state dir)")
	}
}

// Authenticate verifies bearer as a parsec broker token of the expected type,
// then maps the configured claim to the principal id. A verification failure
// (bad signature, wrong type, expired) is APERTURE_INVALID_TOKEN; a verified
// token missing the principal claim is APERTURE_UNAUTHENTICATED.
func (a *parsecAuthenticator) Authenticate(_ context.Context, bearer string) (string, Claims, error) {
	if bearer == "" {
		return "", nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED, "auth: empty bearer")
	}
	pc, err := a.verifier.Verify(bearer, a.expected)
	if err != nil {
		return "", nil, aerr.Wrap(aerr.APERTURE_INVALID_TOKEN, "auth: parsec token verification failed", err)
	}
	claims, err := parsecClaimsToMap(pc)
	if err != nil {
		return "", nil, err
	}
	principal, err := mapPrincipal(claims, a.principalClaim)
	if err != nil {
		return "", nil, err
	}
	return principal, claims, nil
}

// parsecClaimsToMap converts the parsec Claims struct into a generic Claims map
// (via its JSON shape) so the shared claim→principal mapping reads parsec tokens
// the same way it reads OIDC tokens.
func parsecClaimsToMap(pc parsecauth.Claims) (Claims, error) {
	raw, err := json.Marshal(pc)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_INVALID_TOKEN, "auth: encoding parsec claims failed", err)
	}
	var claims Claims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_INVALID_TOKEN, "auth: decoding parsec claims failed", err)
	}
	return claims, nil
}
