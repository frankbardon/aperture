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

func TestPatternSet_Matches(t *testing.T) {
	p := MustParsePattern("brand:{1,5,23}")
	for _, id := range []string{"brand:1", "brand:5", "brand:23"} {
		if !p.Matches(MustParse(id)) {
			t.Errorf("%s should match brand:{1,5,23}", id)
		}
	}
	for _, id := range []string{"brand:2", "brand:6", "app:1", "brand:11"} {
		if p.Matches(MustParse(id)) {
			t.Errorf("%s should NOT match brand:{1,5,23}", id)
		}
	}
}

func TestPatternSet_TypeComponentAndNested(t *testing.T) {
	// Set in the type component.
	p := MustParsePattern("{app,brand}:5")
	if !p.Matches(MustParse("app:5")) || !p.Matches(MustParse("brand:5")) {
		t.Error("type set should match app:5 and brand:5")
	}
	if p.Matches(MustParse("dish:5")) {
		t.Error("type set should not match dish:5")
	}
	// Set nested under a path segment.
	q := MustParsePattern("account:acme/brand:{1,2}")
	if !q.Matches(MustParse("account:acme/brand:2")) {
		t.Error("nested set should match account:acme/brand:2")
	}
	if q.Matches(MustParse("account:acme/brand:3")) {
		t.Error("nested set should not match account:acme/brand:3")
	}
}

func TestPatternSet_ParseErrors(t *testing.T) {
	for _, bad := range []string{"brand:{}", "brand:{1,,2}", "brand:{1,*}", "brand:{a/b}"} {
		if _, err := ParsePattern(bad); err == nil {
			t.Errorf("ParsePattern(%q) should fail", bad)
		}
	}
	// Duplicates are dropped, not an error.
	p, err := ParsePattern("brand:{1,1,5}")
	if err != nil {
		t.Fatalf("dup set: %v", err)
	}
	if !p.Matches(MustParse("brand:1")) || !p.Matches(MustParse("brand:5")) {
		t.Error("dedup set should still match its members")
	}
}

func TestPatternSet_StringRoundTrips(t *testing.T) {
	const s = "brand:{1,5,23}"
	if got := MustParsePattern(s).String(); got != s {
		t.Errorf("String() = %q, want %q", got, s)
	}
}

func TestPatternSet_Expand(t *testing.T) {
	got, ok := MustParsePattern("brand:{1,5,23}").Expand()
	if !ok {
		t.Fatal("brand:{1,5,23} should be enumerable")
	}
	want := []string{"brand:1", "brand:5", "brand:23"}
	if len(got) != len(want) {
		t.Fatalf("expand = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("expand[%d] = %s, want %s", i, got[i].String(), want[i])
		}
	}

	// Cross-product of two set components.
	cross, ok := MustParsePattern("{app,brand}:{1,2}").Expand()
	if !ok || len(cross) != 4 {
		t.Fatalf("cross-product expand = %v (ok=%v), want 4", cross, ok)
	}

	// A wildcard makes it non-enumerable.
	if _, ok := MustParsePattern("brand:*").Expand(); ok {
		t.Error("brand:* must not be enumerable")
	}
	if _, ok := MustParsePattern("brand:{1,5}/**").Expand(); ok {
		t.Error("a ** pattern must not be enumerable")
	}
}

func TestPatternSet_Contains(t *testing.T) {
	// A set subsumes a member and a sub-set.
	if !MustParsePattern("brand:{1,5,23}").Contains(MustParsePattern("brand:5")) {
		t.Error("{1,5,23} should contain 5")
	}
	if !MustParsePattern("brand:{1,5,23}").Contains(MustParsePattern("brand:{1,5}")) {
		t.Error("{1,5,23} should contain {1,5}")
	}
	// It does NOT subsume a non-member or an overlapping-but-not-subset set.
	if MustParsePattern("brand:{1,5,23}").Contains(MustParsePattern("brand:3")) {
		t.Error("{1,5,23} should not contain 3")
	}
	if MustParsePattern("brand:{1,5}").Contains(MustParsePattern("brand:{5,9}")) {
		t.Error("{1,5} should not contain {5,9}")
	}
	// A wildcard subsumes a set; a set does not subsume a wildcard.
	if !MustParsePattern("brand:*").Contains(MustParsePattern("brand:{1,5}")) {
		t.Error("brand:* should contain brand:{1,5}")
	}
	if MustParsePattern("brand:{1,5}").Contains(MustParsePattern("brand:*")) {
		t.Error("brand:{1,5} should not contain brand:*")
	}
}
