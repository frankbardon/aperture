package errors

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestCodesHaveFixups asserts every Code in AllCodes has a Registry entry with
// a non-empty Message and at least one Fixup OR FixupNotApplicable=true. Mirrors
// the orbit discipline: a code with no remediation guidance is a bug.
func TestCodesHaveFixups(t *testing.T) {
	for _, code := range AllCodes {
		meta, ok := Registry[code]
		if !ok {
			t.Errorf("code %s missing Registry entry", code)
			continue
		}
		if meta.Message == "" {
			t.Errorf("code %s missing Message", code)
		}
		if !meta.FixupNotApplicable && len(meta.Fixups) == 0 {
			t.Errorf("code %s has neither Fixups nor FixupNotApplicable", code)
		}
	}
}

// TestRegistryHasNoOrphans asserts Registry contains nothing that AllCodes does
// not, so the two stay in lockstep.
func TestRegistryHasNoOrphans(t *testing.T) {
	known := make(map[Code]bool, len(AllCodes))
	for _, c := range AllCodes {
		known[c] = true
	}
	for code := range Registry {
		if !known[code] {
			t.Errorf("Registry has code %s that is not listed in AllCodes", code)
		}
	}
}

// TestCodesAreScreamingSnakeNamespaced asserts every code is SCREAMING_SNAKE and
// carries the APERTURE_ namespace prefix.
func TestCodesAreScreamingSnakeNamespaced(t *testing.T) {
	for _, code := range AllCodes {
		s := string(code)
		if !strings.HasPrefix(s, "APERTURE_") {
			t.Errorf("code %s is missing the APERTURE_ prefix", code)
		}
		if s != strings.ToUpper(s) {
			t.Errorf("code %s is not SCREAMING_SNAKE", code)
		}
	}
}

// TestCodedErrorUnwrap asserts the wrapped cause is reachable via errors.Is and
// the Code is recoverable via CodeOf through an fmt.Errorf wrap.
func TestCodedErrorUnwrap(t *testing.T) {
	sentinel := errors.New("disk gone")
	err := Wrap(APERTURE_STORAGE, "", sentinel)

	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is did not find the wrapped sentinel")
	}
	if got := CodeOf(err); got != APERTURE_STORAGE {
		t.Fatalf("CodeOf = %s, want %s", got, APERTURE_STORAGE)
	}
	// Survives an outer fmt.Errorf %w wrap.
	outer := fmt.Errorf("serve: %w", err)
	if got := CodeOf(outer); got != APERTURE_STORAGE {
		t.Fatalf("CodeOf(outer) = %s, want %s", got, APERTURE_STORAGE)
	}
}

// TestNewFallsBackToRegistryMessage asserts New with an empty message adopts the
// code's canonical Registry message.
func TestNewFallsBackToRegistryMessage(t *testing.T) {
	err := New(APERTURE_NOT_FOUND, "")
	if err.Msg != Message(APERTURE_NOT_FOUND) {
		t.Fatalf("New empty msg = %q, want registry message %q", err.Msg, Message(APERTURE_NOT_FOUND))
	}
}

// TestCodeOfPlainError asserts a non-coded error yields an empty Code.
func TestCodeOfPlainError(t *testing.T) {
	if got := CodeOf(errors.New("plain")); got != "" {
		t.Fatalf("CodeOf(plain) = %q, want empty", got)
	}
}
