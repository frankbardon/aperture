package auth

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// TestDevAuthenticator_BearerIsPrincipal asserts the dev/static adapter maps a
// non-empty bearer straight to the principal id with no claims — the property
// that makes Aperture runnable with no external IdP.
func TestDevAuthenticator_BearerIsPrincipal(t *testing.T) {
	authn := NewDev()
	principal, claims, err := authn.Authenticate(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Authenticate: unexpected error: %v", err)
	}
	if principal != "alice" {
		t.Errorf("principal = %q, want %q", principal, "alice")
	}
	if len(claims) != 0 {
		t.Errorf("claims = %v, want empty (dev has no claims to map)", claims)
	}
}

// TestDevAuthenticator_EmptyBearerUnauthenticated asserts even the dev adapter
// fails closed: an empty bearer is APERTURE_UNAUTHENTICATED, not principal "".
func TestDevAuthenticator_EmptyBearerUnauthenticated(t *testing.T) {
	_, _, err := NewDev().Authenticate(context.Background(), "")
	if err == nil {
		t.Fatal("Authenticate(\"\"): want error, got nil")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("code = %s, want %s", got, aerr.APERTURE_UNAUTHENTICATED)
	}
}
