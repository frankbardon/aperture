package identity

import "testing"

func TestContains(t *testing.T) {
	cases := []struct {
		name  string
		outer string
		inner string
		want  bool
	}{
		// Reflexive: a pattern contains itself.
		{"identical literal", "account:acme/document:42", "account:acme/document:42", true},
		{"identical wildcard", "account:acme/**", "account:acme/**", true},

		// More-specific inner under a broader outer: contained.
		{"concrete under double star", "account:acme/**", "account:acme/project:atlas/document:42", true},
		{"subtree under double star", "account:acme/**", "account:acme/project:atlas/**", true},
		{"single star covers literal", "account:acme/*", "account:acme/document:42", true},
		{"wild id covers fixed id", "account:acme/document:*", "account:acme/document:42", true},
		{"trailing star covers deeper concrete", "account:*/**", "account:acme/project:atlas/document:42", true},

		// Broader or disjoint inner: NOT contained (fail-closed).
		{"broader inner double star", "account:acme/project:atlas/**", "account:acme/**", false},
		{"disjoint account", "account:acme/**", "account:other/**", false},
		{"disjoint literal id", "account:acme/document:42", "account:acme/document:43", false},
		{"inner single star not under literal", "account:acme/document:42", "account:acme/*", false},
		{"inner wild id not under fixed", "account:acme/document:42", "account:acme/document:*", false},
		{"inner double star not under single width", "account:acme/document:42", "account:acme/**", false},
		{"shorter outer cannot cover", "account:acme", "account:acme/document:42", false},
		{"longer outer cannot cover shorter", "account:acme/document:42", "account:acme", false},

		// Single star vs single star, and double star one-or-more semantics.
		{"single star contains single star", "account:acme/*", "account:acme/*", true},
		{"double star requires at least one", "account:acme/**", "account:acme", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outer := MustParsePattern(tc.outer)
			inner := MustParsePattern(tc.inner)
			if got := Contains(outer, inner); got != tc.want {
				t.Fatalf("Contains(%q, %q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
			}
			// Method form must agree with the package function.
			if got := outer.Contains(inner); got != tc.want {
				t.Fatalf("(%q).Contains(%q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
			}
		})
	}
}

// TestContainsSoundnessVsMatches checks the core soundness property for concrete
// inners: whenever Contains(outer, inner) holds and inner is a concrete identity,
// outer must actually Match that identity. A containment that outran matching
// would be an escalation bug.
func TestContainsSoundnessVsMatches(t *testing.T) {
	concretes := []string{
		"account:acme/project:atlas/document:42",
		"account:acme/document:7",
		"account:other/project:zeta/document:1",
	}
	outers := []string{
		"account:acme/**", "account:*/**", "account:acme/project:atlas/**",
		"account:acme/document:*", "**", "account:acme/document:7",
	}
	for _, o := range outers {
		outer := MustParsePattern(o)
		for _, c := range concretes {
			id := MustParse(c)
			inner := MustParsePattern(c)
			if Contains(outer, inner) && !outer.Matches(id) {
				t.Fatalf("UNSOUND: Contains(%q, %q) true but outer does not Match the identity", o, c)
			}
		}
	}
}
