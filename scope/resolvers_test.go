package scope

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// fakeLister is a test ObjectLister that returns a fixed set of identities of a
// type, honouring the bounding pattern and limit the resolver passes.
type fakeLister struct {
	objects []identity.Identity
}

func (l fakeLister) List(_ context.Context, _ string, pattern identity.Pattern, limit int) ([]identity.Identity, error) {
	out := make([]identity.Identity, 0, len(l.objects))
	for _, o := range l.objects {
		if pattern.Matches(o) {
			out = append(out, o)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeRules selects objects whose canonical string is in the selected set,
// regardless of the rule ref — enough to exercise the rule seam.
type fakeRules struct {
	selected map[string]bool
}

func (r fakeRules) Selected(_ context.Context, _ string, object identity.Identity, _, _ string) (bool, error) {
	return r.selected[object.String()], nil
}

func mustResolve(t *testing.T, r *Registry, gc GrantContext, deps Deps) ScopeResolver {
	t.Helper()
	res, err := r.Resolve(gc, deps)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", gc.Spec.Strategy, err)
	}
	return res
}

func gc(strategy, pattern, objType string, ids []string, rule string) GrantContext {
	return GrantContext{
		Pattern:    identity.MustParsePattern(pattern),
		ObjectType: objType,
		Spec:       Spec{Strategy: strategy, IDs: ids, Rule: rule},
	}
}

// --- implicit ---

func TestImplicitContains(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{})
	ctx := context.Background()

	// Any document under acme is a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:42")); !ok {
		t.Errorf("implicit should cover account:acme/document:42")
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas/document:7")); !ok {
		t.Errorf("implicit should cover a nested document under acme")
	}
	// A non-document object of a different terminal type is not a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas")); ok {
		t.Errorf("implicit document scope must not cover a project terminal")
	}
	// Outside the pattern scope: not a member.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:other/document:1")); ok {
		t.Errorf("implicit must not cover objects outside the pattern scope")
	}
}

func TestImplicitRejectsConfig(t *testing.T) {
	_, err := newImplicitResolver(gc(StrategyImplicit, "**", "document", []string{"document:1"}, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("implicit with ids: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestImplicitMembersNeedsLister(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{})
	_, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED {
		t.Fatalf("implicit Members without lister: code = %q, want APERTURE_SCOPE_LISTER_UNCONFIGURED", code)
	}
}

func TestImplicitMembersWithLister(t *testing.T) {
	r := DefaultRegistry()
	lister := fakeLister{objects: []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:2"),
		identity.MustParse("account:other/document:3"),
	}}
	res := mustResolve(t, r, gc(StrategyImplicit, "account:acme/**", "document", nil, ""), Deps{Lister: lister})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("members = %v, want the two acme documents", got)
	}
}

// --- inclusive ---

func TestInclusiveListContains(t *testing.T) {
	r := DefaultRegistry()
	ids := []string{"account:acme/document:42", "account:acme/document:99"}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", ids, ""), Deps{})
	ctx := context.Background()

	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:42")); !ok {
		t.Errorf("inclusive should cover a listed object")
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:7")); ok {
		t.Errorf("inclusive must not cover an unlisted object")
	}
	// Listed but outside the pattern scope: not covered (pattern bounds the list).
	res2 := mustResolve(t, r, gc(StrategyInclusive, "account:acme/project:atlas/**", "document",
		[]string{"account:acme/document:42"}, ""), Deps{})
	if ok, _ := res2.Contains(ctx, identity.MustParse("account:acme/document:42")); ok {
		t.Errorf("inclusive must not cover a listed object outside the pattern scope")
	}
}

func TestInclusiveRequiresIdsOrRule(t *testing.T) {
	_, err := newInclusiveResolver(gc(StrategyInclusive, "**", "document", nil, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("inclusive with no config: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestInclusiveMembersListPath(t *testing.T) {
	r := DefaultRegistry()
	ids := []string{"account:acme/document:42", "account:acme/document:99", "account:other/document:1"}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", ids, ""), Deps{})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	// Only the two acme documents fall within the pattern scope; no lister needed.
	if len(got) != 2 {
		t.Fatalf("members = %v, want 2 acme documents", got)
	}
}

func TestInclusiveRulePath(t *testing.T) {
	r := DefaultRegistry()
	rules := fakeRules{selected: map[string]bool{"account:acme/document:5": true}}
	res := mustResolve(t, r, gc(StrategyInclusive, "account:acme/**", "document", nil, "myrule"),
		Deps{Rules: rules})
	ctx := context.Background()

	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:5")); err != nil || !ok {
		t.Errorf("rule-selected object should be covered (ok=%v err=%v)", ok, err)
	}
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:6")); ok {
		t.Errorf("non-selected object must not be covered")
	}
}

func TestInclusiveRulePathUnconfigured(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyInclusive, "**", "document", nil, "myrule"), Deps{})
	_, err := res.Contains(context.Background(), identity.MustParse("document:1"))
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_RULE_UNCONFIGURED {
		t.Fatalf("rule path without evaluator: code = %q, want APERTURE_SCOPE_RULE_UNCONFIGURED", code)
	}
}

// --- exclusive ---

func TestExclusiveListContains(t *testing.T) {
	r := DefaultRegistry()
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document",
		[]string{"account:acme/document:7"}, ""), Deps{})
	ctx := context.Background()

	// Of-type, in scope, not excluded: covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:1")); !ok {
		t.Errorf("exclusive should cover a non-excluded document")
	}
	// Excluded: not covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/document:7")); ok {
		t.Errorf("exclusive must not cover an excluded document")
	}
	// Wrong terminal type: not covered (exclusive is all-OF-TYPE minus list).
	if ok, _ := res.Contains(ctx, identity.MustParse("account:acme/project:atlas")); ok {
		t.Errorf("exclusive document scope must not cover a project terminal")
	}
	// Outside the pattern scope: not covered.
	if ok, _ := res.Contains(ctx, identity.MustParse("account:other/document:1")); ok {
		t.Errorf("exclusive must not cover objects outside the pattern scope")
	}
}

func TestExclusiveRequiresConfig(t *testing.T) {
	_, err := newExclusiveResolver(gc(StrategyExclusive, "**", "document", nil, ""), Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("exclusive with no config: code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}

func TestExclusiveMembersWithLister(t *testing.T) {
	r := DefaultRegistry()
	lister := fakeLister{objects: []identity.Identity{
		identity.MustParse("account:acme/document:1"),
		identity.MustParse("account:acme/document:7"), // excluded
		identity.MustParse("account:acme/document:9"),
	}}
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document",
		[]string{"account:acme/document:7"}, ""), Deps{Lister: lister})
	got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("members = %v, want documents 1 and 9 (7 excluded)", got)
	}
	for _, o := range got {
		if o.String() == "account:acme/document:7" {
			t.Errorf("excluded document leaked into Members")
		}
	}
}

func TestExclusiveRulePath(t *testing.T) {
	r := DefaultRegistry()
	rules := fakeRules{selected: map[string]bool{"account:acme/document:3": true}}
	res := mustResolve(t, r, gc(StrategyExclusive, "account:acme/**", "document", nil, "quarantine"),
		Deps{Rules: rules})
	ctx := context.Background()

	// Rule selects document:3 for exclusion.
	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:3")); err != nil || ok {
		t.Errorf("rule-excluded object must not be covered (ok=%v err=%v)", ok, err)
	}
	if ok, err := res.Contains(ctx, identity.MustParse("account:acme/document:4")); err != nil || !ok {
		t.Errorf("non-excluded object should be covered (ok=%v err=%v)", ok, err)
	}
}
