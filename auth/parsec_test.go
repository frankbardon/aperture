package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	parsecauth "github.com/frankbardon/parsec/auth"

	aerr "github.com/frankbardon/aperture/errors"
)

// brokerKeyring stands in for a running parsec broker: it builds a signing
// keyring with one active key, persists it to <dir>/keyring.json (exactly where
// the broker's StateDir keeps it), and returns the path plus an issuer that
// mints tokens against that ring. Aperture's parsec adapter loads the SAME file,
// so the per-key id (kid) matches and verification succeeds — the genuine
// shared-keyring integration, no network.
func brokerKeyring(t *testing.T, dir string) (path string, issuer *parsecauth.Issuer) {
	t.Helper()
	ring := parsecauth.NewKeyRing()
	if _, err := ring.Generate(); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path = filepath.Join(dir, parsecauth.KeyringFileName)
	if err := parsecauth.SaveKeyRing(path, ring); err != nil {
		t.Fatalf("save keyring: %v", err)
	}
	signer, err := parsecauth.NewSigner(ring)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return path, parsecauth.NewIssuer(signer)
}

func mintAccess(t *testing.T, issuer *parsecauth.Issuer, sub string) string {
	t.Helper()
	token, _, err := issuer.IssueAccessForChannels(sub, []string{"public:demo"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	return token
}

// TestParsec_VerifyValid mints a broker access token and asserts the adapter,
// pointed at the broker's keyring file, verifies it and resolves the subject to
// the principal id.
func TestParsec_VerifyValid(t *testing.T) {
	path, issuer := brokerKeyring(t, t.TempDir())
	authn, err := NewParsec(ParsecOptions{KeyringPath: path})
	if err != nil {
		t.Fatalf("NewParsec: %v", err)
	}

	token := mintAccess(t, issuer, "machine:ci-bot")
	principal, claims, err := authn.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: unexpected error: %v", err)
	}
	if principal != "machine:ci-bot" {
		t.Errorf("principal = %q, want %q", principal, "machine:ci-bot")
	}
	if claims["sub"] != "machine:ci-bot" {
		t.Errorf("claims[sub] = %v, want machine:ci-bot", claims["sub"])
	}
}

// TestParsec_StateDirResolvesKeyring asserts the StateDir option resolves
// keyring.json inside the broker's state directory (the orbit serve.go shape).
func TestParsec_StateDirResolvesKeyring(t *testing.T) {
	dir := t.TempDir()
	_, issuer := brokerKeyring(t, dir)
	authn, err := NewParsec(ParsecOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("NewParsec: %v", err)
	}
	principal, _, err := authn.Authenticate(context.Background(), mintAccess(t, issuer, "user:alice"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != "user:alice" {
		t.Errorf("principal = %q, want user:alice", principal)
	}
}

// TestParsec_ForeignKeyring asserts a token minted by a DIFFERENT broker keyring
// fails verification with APERTURE_INVALID_TOKEN — the adapter trusts only
// tokens signed by the keyring it loaded.
func TestParsec_ForeignKeyring(t *testing.T) {
	apertureRing, _ := brokerKeyring(t, t.TempDir())
	_, foreignIssuer := brokerKeyring(t, t.TempDir())

	authn, err := NewParsec(ParsecOptions{KeyringPath: apertureRing})
	if err != nil {
		t.Fatalf("NewParsec: %v", err)
	}
	token := mintAccess(t, foreignIssuer, "machine:ci-bot")
	_, _, err = authn.Authenticate(context.Background(), token)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_TOKEN {
		t.Fatalf("foreign keyring: code = %s, want %s (err=%v)", got, aerr.APERTURE_INVALID_TOKEN, err)
	}
}

func TestNewParsec_RequiresKeyringSource(t *testing.T) {
	_, err := NewParsec(ParsecOptions{})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("no keyring source: code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
}
