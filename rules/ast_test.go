package rules

import (
	"encoding/json"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// sampleRule is a representative AST that exercises every node kind, used by the
// round-trip and render tests.
func sampleRule() *Node {
	return And(
		Compare(OpEq, Var("object.classification"), Lit("public")),
		Or(
			Compare(OpGe, Var("principal.clearance"), Lit(3)),
			Not(Compare(OpEq, Var("action"), Lit("delete"))),
		),
		Compare(OpIn, Var("object.region"), List(Lit("us"), Lit("eu"))),
		Compare(OpEq, Call("lower", Var("object.owner")), Lit("alice")),
	)
}

func TestASTRoundTrip(t *testing.T) {
	n := sampleRule()

	first, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back Node
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	second, err := json.Marshal(&back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("round-trip not stable:\n first  = %s\n second = %s", first, second)
	}

	// The decoded AST must still render to the same expression as the original,
	// proving structure survives the round-trip, not just bytes.
	srcA, err := n.Expr()
	if err != nil {
		t.Fatalf("Expr original: %v", err)
	}
	srcB, err := back.Expr()
	if err != nil {
		t.Fatalf("Expr decoded: %v", err)
	}
	if srcA != srcB {
		t.Fatalf("decoded AST renders differently:\n %q\n %q", srcA, srcB)
	}
}

// TestLiteralRoundTripFalsyValues guards the trap that omitempty would drop a
// false / 0 / "" literal: those must survive the round-trip.
func TestLiteralRoundTripFalsyValues(t *testing.T) {
	for _, v := range []any{false, 0, "", nil} {
		n := Compare(OpEq, Var("object.flag"), Lit(v))
		first, err := json.Marshal(n)
		if err != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		var back Node
		if err := json.Unmarshal(first, &back); err != nil {
			t.Fatalf("unmarshal %v: %v", v, err)
		}
		second, err := json.Marshal(&back)
		if err != nil {
			t.Fatalf("re-marshal %v: %v", v, err)
		}
		if string(first) != string(second) {
			t.Fatalf("falsy literal %#v not stable: %s vs %s", v, first, second)
		}
		if back.Right == nil || len(back.Right.Value) == 0 {
			t.Fatalf("falsy literal %#v lost its value on round-trip: %s", v, first)
		}
	}
}

func TestExprRender(t *testing.T) {
	src, err := sampleRule().Expr()
	if err != nil {
		t.Fatalf("Expr: %v", err)
	}
	want := `((object.classification == "public") && ((principal.clearance >= 3) || !((action == "delete"))) && (object.region in ["us", "eu"]) && (lower(object.owner) == "alice"))`
	if src != want {
		t.Fatalf("render mismatch:\n got  %s\n want %s", src, want)
	}
}

func TestParseFromJSON(t *testing.T) {
	// The wire shape an editor / state file would hand us.
	const doc = `{"type":"compare","op":"eq","left":{"type":"var","name":"object.tier"},"right":{"type":"literal","value":"gold"}}`
	var n Node
	if err := json.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	src, err := n.Expr()
	if err != nil {
		t.Fatalf("Expr: %v", err)
	}
	if want := `(object.tier == "gold")`; src != want {
		t.Fatalf("render = %q, want %q", src, want)
	}
}

func TestValidateFailures(t *testing.T) {
	cases := []struct {
		name string
		node *Node
		code aerr.Code
	}{
		{"and too few children", And(Var("object.x")), aerr.APERTURE_RULE_INVALID},
		{"not wrong arity", &Node{Type: NodeNot, Children: []*Node{Var("object.x"), Var("object.y")}}, aerr.APERTURE_RULE_INVALID},
		{"compare missing operand", &Node{Type: NodeCompare, Op: OpEq, Left: Var("object.x")}, aerr.APERTURE_RULE_INVALID},
		{"compare bad op", Compare("LIKE", Var("object.x"), Lit(1)), aerr.APERTURE_RULE_INVALID},
		{"in needs list", Compare(OpIn, Var("object.x"), Lit("y")), aerr.APERTURE_RULE_INVALID},
		{"var bad path", Var("object..x"), aerr.APERTURE_RULE_INVALID},
		{"var unknown root", Var("subject.id"), aerr.APERTURE_RULE_UNKNOWN_VARIABLE},
		{"literal empty", &Node{Type: NodeLiteral}, aerr.APERTURE_RULE_INVALID},
		{"literal composite", &Node{Type: NodeLiteral, Value: json.RawMessage(`[1,2]`)}, aerr.APERTURE_RULE_INVALID},
		{"unknown node type", &Node{Type: "frob"}, aerr.APERTURE_RULE_INVALID},
		{"call bad name", Call("", Var("object.x")), aerr.APERTURE_RULE_INVALID},
		{"nil node", nil, aerr.APERTURE_RULE_INVALID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.node.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if code := aerr.CodeOf(err); code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	if err := sampleRule().Validate(); err != nil {
		t.Fatalf("sampleRule should validate: %v", err)
	}
	// Each allowed root is accepted.
	for _, root := range []string{"object.a", "principal.b", "account.c", "action"} {
		if err := Var(root).Validate(); err != nil {
			t.Errorf("root %q should validate: %v", root, err)
		}
	}
}
