package identity

import (
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

func TestParsePatternRoundTrip(t *testing.T) {
	cases := []string{
		"account:acme",
		"account:acme/project:*",
		"account:acme/*",
		"account:acme/**",
		"*/document:42",
		"**",
		"account:*/project:*/document:*",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			p, err := ParsePattern(s)
			if err != nil {
				t.Fatalf("ParsePattern(%q): %v", s, err)
			}
			if got := p.String(); got != s {
				t.Fatalf("String() = %q, want %q", got, s)
			}
		})
	}
}

func TestParsePatternRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"account:acme/",
		"/account:acme",
		"account:acme//project:*",
		"account:acme/project", // missing colon, not a wildcard
		":acme",
		"account:",
		"account:ac me",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := ParsePattern(s)
			if aerr.CodeOf(err) != aerr.APERTURE_IDENTITY_INVALID {
				t.Fatalf("ParsePattern(%q) code = %q, want APERTURE_IDENTITY_INVALID", s, aerr.CodeOf(err))
			}
		})
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		id      string
		want    bool
	}{
		// Exact literal.
		{"account:acme", "account:acme", true},
		{"account:acme", "account:other", false},
		{"account:acme/project:atlas", "account:acme/project:atlas", true},

		// Depth must match for purely literal / single patterns.
		{"account:acme", "account:acme/project:atlas", false},
		{"account:acme/project:atlas", "account:acme", false},

		// "project:*" — literal type, wildcard id; single segment depth.
		{"account:acme/project:*", "account:acme/project:atlas", true},
		{"account:acme/project:*", "account:acme/project:other", true},
		{"account:acme/project:*", "account:acme/document:atlas", false},            // type mismatch
		{"account:acme/project:*", "account:acme/project:atlas/document:42", false}, // too deep

		// Bare "*" matches exactly one segment of any type/id.
		{"account:acme/*", "account:acme/project:atlas", true},
		{"account:acme/*", "account:acme/document:42", true},
		{"account:acme/*", "account:acme", false},                           // needs one more segment
		{"account:acme/*", "account:acme/project:atlas/document:42", false}, // matches exactly one

		// "*" in type position.
		{"*/document:42", "account:document/document:42", true},
		{"*:atlas", "project:atlas", true},
		{"*:atlas", "project:other", false},

		// "**" matches one-or-more segments recursively.
		{"account:acme/**", "account:acme/project:atlas", true},
		{"account:acme/**", "account:acme/project:atlas/document:42", true},
		{"account:acme/**", "account:acme", false}, // ** requires at least one beneath
		{"**", "account:acme", true},
		{"**", "account:acme/project:atlas/document:42", true},

		// "**" in the middle.
		{"account:acme/**/document:42", "account:acme/project:atlas/document:42", true},
		{"account:acme/**/document:42", "account:acme/project:atlas/folder:x/document:42", true},
		{"account:acme/**/document:42", "account:acme/document:42", false}, // ** needs >=1 between
		{"account:acme/**/document:42", "account:acme/project:atlas/document:99", false},

		// Combined wildcards.
		{"account:*/project:*/document:*", "account:acme/project:atlas/document:42", true},
		{"account:*/project:*/document:*", "account:acme/project:atlas", false},
	}
	for _, c := range cases {
		t.Run(c.pattern+"~"+c.id, func(t *testing.T) {
			p := MustParsePattern(c.pattern)
			id := MustParse(c.id)
			if got := p.Matches(id); got != c.want {
				t.Fatalf("MustParsePattern(%q).Matches(%q) = %v, want %v", c.pattern, c.id, got, c.want)
			}
			// Package-level Match must agree with the method.
			if got := Match(p, id); got != c.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", c.pattern, c.id, got, c.want)
			}
		})
	}
}
