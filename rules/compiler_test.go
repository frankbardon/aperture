package rules

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

func TestCompileAndEvalOverMetadata(t *testing.T) {
	c := NewCompiler()
	rule := And(
		Compare(OpEq, Var("object.classification"), Lit("public")),
		Compare(OpGe, Var("object.version"), Lit(2)),
	)
	compiled, err := c.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	ctx := context.Background()
	cases := []struct {
		name string
		md   map[string]any
		want bool
	}{
		{"match", map[string]any{"classification": "public", "version": 3}, true},
		{"wrong class", map[string]any{"classification": "secret", "version": 3}, false},
		{"low version", map[string]any{"classification": "public", "version": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compiled.Eval(ctx, Input{Object: tc.md})
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("eval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvalEqualityAgainstMissingFieldIsFalse documents that an equality against a
// metadata field the object lacks reads as nil and yields false — not an error.
func TestEvalEqualityAgainstMissingFieldIsFalse(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpEq, Var("object.classification"), Lit("public")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := compiled.Eval(context.Background(), Input{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Fatalf("equality against a missing field should be false")
	}
}

// TestEvalOrderedComparisonAgainstMissingFieldErrors documents that an ordered
// comparison (>=, <, …) against a field the object lacks is an APERTURE_RULE_EVAL
// error — the rule assumes a field the object does not carry. The scope resolver
// treats it as a non-decision rather than silently selecting.
func TestEvalOrderedComparisonAgainstMissingFieldErrors(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpGe, Var("object.version"), Lit(2)))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = compiled.Eval(context.Background(), Input{Object: map[string]any{}})
	if err == nil {
		t.Fatalf("expected an eval error for an ordered comparison against a missing field")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_EVAL {
		t.Fatalf("code = %q, want APERTURE_RULE_EVAL", code)
	}
}

func TestEvalUsesPrincipalActionAndFunctions(t *testing.T) {
	c := NewCompiler()
	rule := And(
		Compare(OpEq, Var("principal.id"), Lit("alice")),
		Compare(OpEq, Var("action"), Lit("read")),
		Compare(OpEq, Call("lower", Var("object.owner")), Lit("alice")),
		Compare(OpIn, Var("object.region"), List(Lit("us"), Lit("eu"))),
	)
	compiled, err := c.Compile(rule)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	in := Input{
		Object:    map[string]any{"owner": "ALICE", "region": "eu"},
		Principal: map[string]any{"id": "alice"},
		Action:    "read",
	}
	got, err := compiled.Eval(context.Background(), in)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf("expected rule to select; got false")
	}
}

// TestEvalIsPure proves evaluation does not mutate the supplied metadata map and
// is deterministic across repeated runs over the same snapshot.
func TestEvalIsPure(t *testing.T) {
	c := NewCompiler()
	compiled, err := c.Compile(Compare(OpEq, Var("object.tier"), Lit("gold")))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	md := map[string]any{"tier": "gold"}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		got, err := compiled.Eval(ctx, Input{Object: md})
		if err != nil || !got {
			t.Fatalf("run %d: got=%v err=%v", i, got, err)
		}
	}
	if len(md) != 1 || md["tier"] != "gold" {
		t.Fatalf("evaluation mutated the metadata snapshot: %v", md)
	}
}

func TestCompileValidationFailures(t *testing.T) {
	c := NewCompiler()
	cases := []struct {
		name string
		node *Node
		code aerr.Code
	}{
		{
			name: "unknown variable root",
			node: Compare(OpEq, Var("subject.id"), Lit("x")),
			code: aerr.APERTURE_RULE_UNKNOWN_VARIABLE,
		},
		{
			name: "structurally invalid",
			node: And(Var("object.x")),
			code: aerr.APERTURE_RULE_INVALID,
		},
		{
			name: "type error: string compared to number",
			node: Compare(OpLt, Var("action"), Lit(5)),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
		{
			name: "type error: non-boolean result",
			node: Lit(5),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
		{
			name: "type error: unknown function",
			node: Compare(OpEq, Call("frobnicate", Var("object.x")), Lit("y")),
			code: aerr.APERTURE_RULE_TYPE_ERROR,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Compile(tc.node)
			if err == nil {
				t.Fatalf("expected compile error, got nil")
			}
			if code := aerr.CodeOf(err); code != tc.code {
				t.Fatalf("code = %q, want %q (err: %v)", code, tc.code, err)
			}
		})
	}
}

// TestDisabledBuiltinsKeepEvalDeterministic proves no nondeterministic builtin
// (e.g. now()) is reachable: referencing one is rejected at compile time, so a
// rule can never read wall-clock state.
func TestDisabledBuiltinsKeepEvalDeterministic(t *testing.T) {
	c := NewCompiler()
	_, err := c.Compile(Compare(OpEq, Call("now"), Lit("x")))
	if err == nil {
		t.Fatalf("now() must not be callable from a rule")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_RULE_TYPE_ERROR {
		t.Fatalf("code = %q, want APERTURE_RULE_TYPE_ERROR", code)
	}
}

func TestHostFunctionRegistration(t *testing.T) {
	c := NewCompiler(Function("riskScore", func(args ...any) (any, error) {
		// pure: deterministic over its argument
		s, _ := args[0].(string)
		return len(s), nil
	}))
	compiled, err := c.Compile(Compare(OpGt, Call("riskScore", Var("object.label")), Lit(2)))
	if err != nil {
		t.Fatalf("compile with host function: %v", err)
	}
	got, err := compiled.Eval(context.Background(), Input{Object: map[string]any{"label": "abcd"}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Fatalf("riskScore('abcd')=4 > 2 should be true")
	}
}
