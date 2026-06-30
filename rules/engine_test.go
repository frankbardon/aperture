package rules

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/scope"
)

// fakeFetcher is an in-memory MetadataFetcher keyed by identity string.
type fakeFetcher map[string]map[string]any

func (f fakeFetcher) Fetch(_ context.Context, id identity.Identity) (map[string]any, error) {
	md, ok := f[id.String()]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND, "no such object",
			map[string]any{"id": id.String()})
	}
	return md, nil
}

func newTestEngine() *Engine {
	source := MapSource{
		"public": {Name: "public", AST: Compare(OpEq, Var("object.classification"), Lit("public"))},
		"owner":  {Name: "owner", AST: Compare(OpEq, Var("object.owner"), Var("principal.id"))},
		"reads":  {Name: "reads", AST: Compare(OpEq, Var("action"), Lit("read"))},
	}
	fetcher := fakeFetcher{
		"account:acme/document:1": {"classification": "public", "owner": "alice"},
		"account:acme/document:2": {"classification": "secret", "owner": "bob"},
	}
	return NewEngine(source, fetcher)
}

func TestEngineSelected(t *testing.T) {
	eng := newTestEngine()
	ctx := context.Background()
	doc1 := identity.MustParse("account:acme/document:1")
	doc2 := identity.MustParse("account:acme/document:2")

	cases := []struct {
		name      string
		rule      string
		object    identity.Identity
		principal string
		action    string
		want      bool
	}{
		{"public selects public doc", "public", doc1, "alice", "read", true},
		{"public rejects secret doc", "public", doc2, "alice", "read", false},
		{"owner matches principal", "owner", doc1, "alice", "read", true},
		{"owner rejects non-owner", "owner", doc1, "bob", "read", false},
		{"action read matches", "reads", doc1, "alice", "read", true},
		{"action read rejects write", "reads", doc1, "alice", "write", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eng.Selected(ctx, tc.rule, tc.object, tc.principal, tc.action)
			if err != nil {
				t.Fatalf("Selected: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Selected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEngineRuleNotFound(t *testing.T) {
	eng := newTestEngine()
	_, err := eng.Selected(context.Background(), "missing",
		identity.MustParse("account:acme/document:1"), "alice", "read")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_NOT_FOUND {
		t.Fatalf("code = %q, want APERTURE_RULE_NOT_FOUND", code)
	}
}

func TestEngineSurfacesMetadataError(t *testing.T) {
	eng := newTestEngine()
	// document:99 has no metadata in the fetcher.
	_, err := eng.Selected(context.Background(), "public",
		identity.MustParse("account:acme/document:99"), "alice", "read")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %q, want APERTURE_NOT_FOUND (metadata error surfaced)", code)
	}
}

func TestEngineSurfacesInvalidRule(t *testing.T) {
	source := MapSource{
		"bad": {Name: "bad", AST: Compare(OpEq, Var("subject.id"), Lit("x"))},
	}
	eng := NewEngine(source, fakeFetcher{"document:1": {}})
	_, err := eng.Selected(context.Background(), "bad", identity.MustParse("document:1"), "alice", "read")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
		t.Fatalf("code = %q, want APERTURE_RULE_UNKNOWN_VARIABLE", code)
	}
}

func TestEngineEvalIsCachedAcrossSelected(t *testing.T) {
	eng := newTestEngine()
	ctx := context.Background()
	doc1 := identity.MustParse("account:acme/document:1")
	for i := 0; i < 3; i++ {
		if _, err := eng.Selected(ctx, "public", doc1, "alice", "read"); err != nil {
			t.Fatalf("Selected: %v", err)
		}
	}
	st := eng.CacheStats()
	if st.Entries != 1 {
		t.Fatalf("repeated Selected on one rule must cache a single program; stats = %+v", st)
	}
	if st.Hits < 2 {
		t.Fatalf("expected cache hits across repeated Selected; stats = %+v", st)
	}
}

// TestEngineSatisfiesScopeRuleEvaluator wires the Engine as the rule-backed scope
// evaluator and drives it through the real inclusive/exclusive resolvers — the
// end-to-end integration E2-S1 left a seam for.
func TestEngineSatisfiesScopeRuleEvaluator(t *testing.T) {
	eng := newTestEngine()
	ctx := context.Background()
	reg := scope.DefaultRegistry()
	pattern := identity.MustParsePattern("account:acme/**")

	// Inclusive, rule-backed: covers an object the rule selects.
	incl := scope.GrantContext{
		Pattern:    pattern,
		ObjectType: "document",
		Spec:       scope.Spec{Strategy: scope.StrategyInclusive, Rule: "public"},
		Principal:  "alice",
		Action:     "read",
	}
	inclRes, err := reg.Resolve(incl, scope.Deps{Rules: eng})
	if err != nil {
		t.Fatalf("resolve inclusive: %v", err)
	}
	if ok, err := inclRes.Contains(ctx, identity.MustParse("account:acme/document:1")); err != nil || !ok {
		t.Fatalf("inclusive rule should cover the public document (ok=%v err=%v)", ok, err)
	}
	if ok, _ := inclRes.Contains(ctx, identity.MustParse("account:acme/document:2")); ok {
		t.Fatalf("inclusive rule must not cover the secret document")
	}

	// Exclusive, rule-backed: excludes an object the rule selects.
	excl := scope.GrantContext{
		Pattern:    pattern,
		ObjectType: "document",
		Spec:       scope.Spec{Strategy: scope.StrategyExclusive, Rule: "public"},
		Principal:  "alice",
		Action:     "read",
	}
	exclRes, err := reg.Resolve(excl, scope.Deps{Rules: eng})
	if err != nil {
		t.Fatalf("resolve exclusive: %v", err)
	}
	// document:1 is public → the rule selects it → exclusive excludes it.
	if ok, err := exclRes.Contains(ctx, identity.MustParse("account:acme/document:1")); err != nil || ok {
		t.Fatalf("exclusive rule must exclude the public document (ok=%v err=%v)", ok, err)
	}
	// document:2 is secret → not selected → exclusive covers it.
	if ok, err := exclRes.Contains(ctx, identity.MustParse("account:acme/document:2")); err != nil || !ok {
		t.Fatalf("exclusive rule should cover the non-selected document (ok=%v err=%v)", ok, err)
	}
}

func TestEnginePrincipalResolver(t *testing.T) {
	source := MapSource{
		"clear": {AST: Compare(OpGe, Var("principal.clearance"), Lit(3))},
	}
	eng := NewEngine(source, nil, WithPrincipalResolver(principalResolverFunc(
		func(_ context.Context, p string) (map[string]any, error) {
			clearance := map[string]int{"alice": 5, "bob": 1}
			return map[string]any{"id": p, "clearance": clearance[p]}, nil
		})))
	ctx := context.Background()
	obj := identity.MustParse("document:1")

	if ok, err := eng.Selected(ctx, "clear", obj, "alice", "read"); err != nil || !ok {
		t.Fatalf("alice (clearance 5) should pass (ok=%v err=%v)", ok, err)
	}
	if ok, err := eng.Selected(ctx, "clear", obj, "bob", "read"); err != nil || ok {
		t.Fatalf("bob (clearance 1) should fail (ok=%v err=%v)", ok, err)
	}
}

type principalResolverFunc func(ctx context.Context, principal string) (map[string]any, error)

func (f principalResolverFunc) Attributes(ctx context.Context, principal string) (map[string]any, error) {
	return f(ctx, principal)
}
