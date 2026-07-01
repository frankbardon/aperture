package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	aerr "github.com/frankbardon/aperture/errors"
)

const (
	testIssuer   = "https://idp.test.example"
	testAudience = "aperture-test"
)

// newTestOIDC builds an oidcAuthenticator wired to a StaticKeySet holding pub,
// verifying against testIssuer/testAudience with a fixed clock. It bypasses
// NewOIDC's network discovery so the JWT verification path is exercised offline
// — no IdP, no JWKS fetch.
func newTestOIDC(pub crypto.PublicKey, principalClaim string, now time.Time) *oidcAuthenticator {
	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{pub}}
	verifier := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{
		ClientID: testAudience,
		Now:      func() time.Time { return now },
	})
	return &oidcAuthenticator{verifier: verifier, principalClaim: principalClaimOr(principalClaim)}
}

// signRS256 produces a compact RS256-signed JWT carrying claims. It is enough
// for go-oidc's StaticKeySet to verify against the matching public key — the
// in-test signer the acceptance criteria call for (generate a key, sign tokens
// in-test, hit no network).
func signRS256(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signingInput := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + b64(sig)
}

func baseClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "user:alice",
		"email": "alice@example.com",
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

func TestOIDC_VerifyValid(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "sub", now)

	token := signRS256(t, key, baseClaims(now))
	principal, claims, err := authn.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: unexpected error: %v", err)
	}
	if principal != "user:alice" {
		t.Errorf("principal = %q, want %q", principal, "user:alice")
	}
	if claims["email"] != "alice@example.com" {
		t.Errorf("claims[email] = %v, want alice@example.com", claims["email"])
	}
}

func TestOIDC_Expired(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "sub", now)

	c := baseClaims(now)
	c["exp"] = now.Add(-time.Minute).Unix() // already expired
	token := signRS256(t, key, c)

	_, _, err := authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_TOKEN {
		t.Fatalf("expired token: code = %s, want %s (err=%v)", got, aerr.APERTURE_INVALID_TOKEN, err)
	}
}

func TestOIDC_BadIssuer(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "sub", now)

	c := baseClaims(now)
	c["iss"] = "https://evil.example" // not the configured issuer
	token := signRS256(t, key, c)

	_, _, err := authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_TOKEN {
		t.Fatalf("bad issuer: code = %s, want %s (err=%v)", got, aerr.APERTURE_INVALID_TOKEN, err)
	}
}

func TestOIDC_BadAudience(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "sub", now)

	c := baseClaims(now)
	c["aud"] = "some-other-client"
	token := signRS256(t, key, c)

	_, _, err := authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_TOKEN {
		t.Fatalf("bad audience: code = %s, want %s (err=%v)", got, aerr.APERTURE_INVALID_TOKEN, err)
	}
}

func TestOIDC_BadSignature(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048) // verifier trusts `other`, token signed by `key`
	now := time.Now()
	authn := newTestOIDC(&other.PublicKey, "sub", now)

	token := signRS256(t, key, baseClaims(now))
	_, _, err := authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_TOKEN {
		t.Fatalf("bad signature: code = %s, want %s (err=%v)", got, aerr.APERTURE_INVALID_TOKEN, err)
	}
}

// TestOIDC_ClaimMapping asserts the configurable claim→principal mapping: with
// PrincipalClaim="email" the email claim — not sub — becomes the principal id.
func TestOIDC_ClaimMapping(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "email", now)

	token := signRS256(t, key, baseClaims(now))
	principal, _, err := authn.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: unexpected error: %v", err)
	}
	if principal != "alice@example.com" {
		t.Errorf("principal = %q, want %q (mapped from email claim)", principal, "alice@example.com")
	}
}

// TestOIDC_MissingPrincipalClaim asserts a verified token that does not carry
// the configured principal claim is APERTURE_UNAUTHENTICATED (verified, but no
// principal Aperture can name) — distinct from APERTURE_INVALID_TOKEN.
func TestOIDC_MissingPrincipalClaim(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Now()
	authn := newTestOIDC(&key.PublicKey, "preferred_username", now)

	token := signRS256(t, key, baseClaims(now)) // has sub+email, not preferred_username
	_, _, err := authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_UNAUTHENTICATED {
		t.Fatalf("missing claim: code = %s, want %s (err=%v)", got, aerr.APERTURE_UNAUTHENTICATED, err)
	}
}

// TestNewOIDC_RequiresIssuerAndAudience asserts the security-critical config is
// mandatory: an empty issuer or audience is a coded config error, never a
// verifier that accepts anything.
func TestNewOIDC_RequiresIssuerAndAudience(t *testing.T) {
	if _, err := NewOIDC(context.Background(), OIDCOptions{Audience: "x"}); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Errorf("missing issuer: want APERTURE_CONFIG_INVALID, got %v", err)
	}
	if _, err := NewOIDC(context.Background(), OIDCOptions{Issuer: "https://x"}); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Errorf("missing audience: want APERTURE_CONFIG_INVALID, got %v", err)
	}
}
