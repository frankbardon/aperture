package identity

import (
	"sort"
	"testing"
)

func TestSpecificityOrdering(t *testing.T) {
	// All of these patterns match account:acme/project:atlas (depth 2) except
	// where noted; the slice is written from MOST to LEAST specific, and the
	// test asserts Specificity is strictly decreasing in that order.
	ordered := []string{
		"account:acme/project:atlas", // fully literal (4 literal components)
		"account:acme/project:*",     // 3 literal components
		"account:acme/*",             // 2 literals + bare single-segment wildcard
		"account:acme/**",            // 2 literals + recursive wildcard (least specific)
	}
	prev := Specificity(MustParsePattern(ordered[0]))
	for i := 1; i < len(ordered); i++ {
		cur := Specificity(MustParsePattern(ordered[i]))
		if !(prev > cur) {
			t.Fatalf("specificity not strictly decreasing: %q(%d) should be > %q(%d)",
				ordered[i-1], prev, ordered[i], cur)
		}
		prev = cur
	}
}

func TestSpecificityFavorsLongerLiteralPath(t *testing.T) {
	deep := Specificity(MustParsePattern("account:acme/project:atlas/document:42"))
	shallow := Specificity(MustParsePattern("account:acme/project:atlas"))
	if !(deep > shallow) {
		t.Fatalf("deeper literal path should be more specific: deep=%d shallow=%d", deep, shallow)
	}
}

func TestSpecificitySingleStarBeatsDoubleStar(t *testing.T) {
	// For an identity of depth 2, both match; "*" is more specific than "**".
	single := MustParsePattern("account:acme/*")
	double := MustParsePattern("account:acme/**")
	if !MoreSpecific(single, double) {
		t.Fatalf("'*' should rank above '**': single=%d double=%d",
			Specificity(single), Specificity(double))
	}
}

func TestCompareContract(t *testing.T) {
	a := MustParsePattern("account:acme/project:atlas")
	b := MustParsePattern("account:acme/project:*")
	if Compare(a, b) != 1 {
		t.Fatalf("Compare(literal, wildcard) = %d, want 1", Compare(a, b))
	}
	if Compare(b, a) != -1 {
		t.Fatalf("Compare(wildcard, literal) = %d, want -1", Compare(b, a))
	}
	if Compare(a, a) != 0 {
		t.Fatalf("Compare(a, a) = %d, want 0", Compare(a, a))
	}
}

func TestCompareAsSortKeyIsDeterministic(t *testing.T) {
	patterns := []Pattern{
		MustParsePattern("account:acme/**"),
		MustParsePattern("account:acme/project:atlas"),
		MustParsePattern("account:acme/*"),
		MustParsePattern("account:acme/project:*"),
	}
	// Sort most-specific first.
	sort.SliceStable(patterns, func(i, j int) bool {
		return Compare(patterns[i], patterns[j]) > 0
	})
	want := []string{
		"account:acme/project:atlas",
		"account:acme/project:*",
		"account:acme/*",
		"account:acme/**",
	}
	for i, p := range patterns {
		if p.String() != want[i] {
			t.Fatalf("sorted[%d] = %q, want %q", i, p.String(), want[i])
		}
	}
}

func TestSpecificity_SetCountsAsLiteral(t *testing.T) {
	set := MustParsePattern("brand:{1,5,23}")
	wild := MustParsePattern("brand:*")
	lit := MustParsePattern("brand:5")
	// A set is a fixed (non-wild) component, so it outranks a wildcard...
	if !MoreSpecific(set, wild) {
		t.Error("brand:{1,5,23} should be more specific than brand:*")
	}
	// ...and ranks equal to a single literal id (both pin type+id components).
	if Compare(set, lit) != 0 {
		t.Errorf("set vs literal specificity = %d, want 0", Compare(set, lit))
	}
}
