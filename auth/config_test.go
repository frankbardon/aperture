package auth

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// TestConfig_DefaultIsDev asserts the zero config (and an unset
// APERTURE_AUTH_MODE) builds the dev adapter — the documented default that keeps
// Aperture runnable with no IdP.
func TestConfig_DefaultIsDev(t *testing.T) {
	authn, err := Config{}.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	principal, _, err := authn.Authenticate(context.Background(), "alice")
	if err != nil || principal != "alice" {
		t.Fatalf("default adapter is not dev: principal=%q err=%v", principal, err)
	}
}

func TestConfigFromEnv_SelectsAndMaps(t *testing.T) {
	t.Setenv(EnvMode, "dev")
	t.Setenv(EnvPrincipalClaim, "email")
	t.Setenv(EnvOIDCIssuer, "https://idp.example")
	t.Setenv(EnvOIDCAudience, "aperture")

	cfg := ConfigFromEnv()
	if cfg.Mode != ModeDev {
		t.Errorf("Mode = %q, want dev", cfg.Mode)
	}
	if cfg.PrincipalClaim != "email" {
		t.Errorf("PrincipalClaim = %q, want email", cfg.PrincipalClaim)
	}
	if cfg.OIDCIssuer != "https://idp.example" || cfg.OIDCAudience != "aperture" {
		t.Errorf("OIDC config not read: %+v", cfg)
	}
}

// TestConfig_ClaimMappingFlowsToAdapter asserts the claim-mapping config is not
// merely parsed but threaded into the constructed adapter: a parsec adapter
// built with a custom PrincipalClaim reads that claim for the principal id.
func TestConfig_ClaimMappingFlowsToAdapter(t *testing.T) {
	path, _ := brokerKeyring(t, t.TempDir())
	a, err := NewParsec(ParsecOptions{KeyringPath: path, PrincipalClaim: "typ"})
	if err != nil {
		t.Fatalf("NewParsec: %v", err)
	}
	pa, ok := a.(*parsecAuthenticator)
	if !ok {
		t.Fatalf("NewParsec returned %T, want *parsecAuthenticator", a)
	}
	if pa.principalClaim != "typ" {
		t.Errorf("principalClaim = %q, want typ (claim mapping must flow from config)", pa.principalClaim)
	}
}

func TestConfig_UnknownModeIsConfigError(t *testing.T) {
	_, err := Config{Mode: "nope"}.Build(context.Background())
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("unknown mode: code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
}
