package auth

import (
	"context"
	"os"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// Mode selects which authenticator adapter is constructed. Selection is
// configuration, not code: an operator chooses dev, oidc, or parsec via
// APERTURE_AUTH_MODE (or the serve --auth flag), and Config.Build returns the
// matching Authenticator.
type Mode string

const (
	// ModeDev is the dev/static adapter: bearer == principal id. The default.
	ModeDev Mode = "dev"
	// ModeOIDC is the OIDC/JWT adapter.
	ModeOIDC Mode = "oidc"
	// ModeParsec is the parsec token-broker adapter.
	ModeParsec Mode = "parsec"
)

// Config is the surface-neutral authentication configuration. It is populated
// from environment variables (ConfigFromEnv) or set directly (e.g. from a YAML
// document a later story unmarshals into it), then turned into an Authenticator
// by Build. The zero value selects the dev adapter — the documented default
// that makes Aperture runnable with no external IdP.
type Config struct {
	// Mode picks the adapter (dev|oidc|parsec). Empty defaults to dev.
	Mode Mode
	// PrincipalClaim is the verified claim mapped to the principal id for the
	// oidc and parsec adapters (sub, email, custom). Empty defaults to "sub".
	// The dev adapter ignores it (the bearer is the principal).
	PrincipalClaim string

	// OIDC settings (Mode == oidc).
	OIDCIssuer   string
	OIDCAudience string
	OIDCJWKSURL  string

	// Parsec settings (Mode == parsec). ParsecKeyringPath / ParsecStateDir locate
	// the broker's persisted signing keyring (keyring.json) that Aperture verifies
	// brokered tokens against — the broker and Aperture must share it.
	ParsecKeyringPath string
	ParsecStateDir    string
}

// Environment variable names. All under the APERTURE_ namespace per convention.
const (
	EnvMode           = "APERTURE_AUTH_MODE"
	EnvPrincipalClaim = "APERTURE_AUTH_PRINCIPAL_CLAIM"
	EnvOIDCIssuer     = "APERTURE_OIDC_ISSUER"
	EnvOIDCAudience   = "APERTURE_OIDC_AUDIENCE"
	EnvOIDCJWKSURL    = "APERTURE_OIDC_JWKS_URL"
	EnvParsecKeyring  = "APERTURE_PARSEC_KEYRING"
	EnvParsecStateDir = "APERTURE_PARSEC_STATE_DIR"
)

// ConfigFromEnv reads the auth configuration from APERTURE_* environment
// variables. An unset APERTURE_AUTH_MODE selects the dev adapter, so a process
// with no auth configuration at all still gets a working (dev) authenticator —
// the default that keeps fixtures, demos, and CI runnable with no IdP.
func ConfigFromEnv() Config {
	return Config{
		Mode:              Mode(strings.ToLower(strings.TrimSpace(os.Getenv(EnvMode)))),
		PrincipalClaim:    os.Getenv(EnvPrincipalClaim),
		OIDCIssuer:        os.Getenv(EnvOIDCIssuer),
		OIDCAudience:      os.Getenv(EnvOIDCAudience),
		OIDCJWKSURL:       os.Getenv(EnvOIDCJWKSURL),
		ParsecKeyringPath: os.Getenv(EnvParsecKeyring),
		ParsecStateDir:    os.Getenv(EnvParsecStateDir),
	}
}

// Build constructs the configured Authenticator. The oidc adapter performs
// network discovery at build time, so Build takes a ctx. An unrecognised mode
// is APERTURE_CONFIG_INVALID.
func (c Config) Build(ctx context.Context) (Authenticator, error) {
	switch c.mode() {
	case ModeDev:
		return NewDev(), nil
	case ModeOIDC:
		return NewOIDC(ctx, OIDCOptions{
			Issuer:         c.OIDCIssuer,
			Audience:       c.OIDCAudience,
			JWKSURL:        c.OIDCJWKSURL,
			PrincipalClaim: c.PrincipalClaim,
		})
	case ModeParsec:
		return NewParsec(ParsecOptions{
			KeyringPath:    c.ParsecKeyringPath,
			StateDir:       c.ParsecStateDir,
			PrincipalClaim: c.PrincipalClaim,
		})
	default:
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"auth: unknown auth mode",
			map[string]any{"mode": string(c.Mode), "valid": "dev|oidc|parsec"})
	}
}

// mode returns the configured mode, defaulting to dev when unset.
func (c Config) mode() Mode {
	if c.Mode == "" {
		return ModeDev
	}
	return c.Mode
}
