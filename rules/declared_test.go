package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// E3-S3: a rule reading an undeclared key on a DECLARING slot is refused.
//
// The property is "identical always" by construction. A local attribute layer may
// add keys the shared directory does not carry, and the declared set makes those
// keys unreachable from any rule the deployment can validate — so they are inert
// rather than a second answer to a deployment-wide grant. These tests pin the three
// cases the story names (declared key passes, undeclared key is refused,
// non-declaring slot passes anything) plus the four edges where a wrong
// implementation is invisible: the floor, the declared-EMPTY set, the nested path,
// and the whole-bag read.

// declaring is the set under test: the `principal` root declaring two keys, built
// the way service.WithDeclaredAttributeKeys builds it (with the slot names, because
// those are what the refusal has to name).
func declaringPrincipal(keys ...string) DeclaredAttributeKeys {
	return DeclaredAttributeKeys{
		Principal: DeclaredKeySet{Declared: true, Keys: keys, Slots: []string{"user", "machine"}},
	}
}

func declaringAccount(keys ...string) DeclaredAttributeKeys {
	return DeclaredAttributeKeys{
		Account: DeclaredKeySet{Declared: true, Keys: keys, Slots: []string{"account"}},
	}
}

func TestADeclaredKeyIsReadable(t *testing.T) {
	ast := And(
		Compare(OpEq, Var("principal.department"), Lit("eng")),
		Compare(OpEq, Var("principal.clearance"), Lit("secret")),
	)
	if err := CheckDeclaredAttributeKeys(ast, declaringPrincipal("department", "clearance")); err != nil {
		t.Fatalf("a rule reading only declared keys must validate: %v", err)
	}
}

func TestAnUndeclaredKeyOnADeclaringSlotIsRefused(t *testing.T) {
	ast := Compare(OpEq, Var("principal.clearance"), Lit("secret"))
	err := CheckDeclaredAttributeKeys(ast, declaringPrincipal("department"))
	if err == nil {
		t.Fatal("a rule reading an undeclared key must be refused")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", got)
	}
	// One coded error in the chain: a re-stamp is invisible to CodeOf, so depth is
	// what proves the refusal was classified once.
	if d := codedDepth(err); d != 1 {
		t.Errorf("coded chain depth = %d, want exactly 1 (err=%v)", d, err)
	}
	// The MESSAGE has to carry the key and the slot, because that is the pair an
	// author acts on and a CLI prints a coded error with %v. A remedy that lives
	// only in Context is invisible exactly where it is read.
	msg := err.Error()
	for _, want := range []string{"clearance", "user", "machine", "declared_keys:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message %q does not name %q", msg, want)
		}
	}
	var ce *aerr.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("refusal is not a *aerr.CodedError: %v", err)
	}
	if got := ce.Context["key"]; got != "clearance" {
		t.Errorf(`Context["key"] = %v, want "clearance"`, got)
	}
	if got := ce.Context["path"]; got != "principal.clearance" {
		t.Errorf(`Context["path"] = %v, want "principal.clearance"`, got)
	}
}

func TestANonDeclaringSlotPermitsAnyKey(t *testing.T) {
	// The backward-compatibility bar: a zero-value declaration set is what every
	// deployment that has not opted in has, and it must refuse nothing at all.
	ast := And(
		Compare(OpEq, Var("principal.whatever"), Lit("x")),
		Compare(OpEq, Var("account.anything"), Lit("y")),
		Compare(OpNe, Var("principal"), Lit("z")),
	)
	if err := CheckDeclaredAttributeKeys(ast, DeclaredAttributeKeys{}); err != nil {
		t.Fatalf("a slot that declares nothing must keep today's leniency: %v", err)
	}
	if (DeclaredAttributeKeys{}).Enforcing() {
		t.Error("the zero value must not report itself as enforcing")
	}
	// And per root: declaring `principal` must not start enforcing `account`.
	if err := CheckDeclaredAttributeKeys(
		Compare(OpEq, Var("account.plan"), Lit("enterprise")),
		declaringPrincipal("department"),
	); err != nil {
		t.Fatalf("declaring the principal root must not enforce the account root: %v", err)
	}
	if err := CheckDeclaredAttributeKeys(
		Compare(OpEq, Var("principal.tier"), Lit("gold")),
		declaringAccount("plan"),
	); err != nil {
		t.Fatalf("declaring the account root must not enforce the principal root: %v", err)
	}
}

func TestTheFloorIsNotPartOfAnyDeclaredSet(t *testing.T) {
	// `principal.id == object.owner` is the single most common rule there is, and
	// the floor bags stamp id/kind LAST over whatever a provider returned, so those
	// keys are present in every deployment whether a provider is wired or not.
	// Refusing them over a wiring change that says nothing about them would break
	// ownership rules everywhere.
	ast := And(
		Compare(OpEq, Var("principal.id"), Var("object.owner")),
		Compare(OpEq, Var("principal.kind"), Lit("user")),
		Compare(OpEq, Var("account.id"), Var("object.account")),
	)
	decls := DeclaredAttributeKeys{
		Principal: DeclaredKeySet{Declared: true, Keys: []string{"department"}, Slots: []string{"user", "machine"}},
		Account:   DeclaredKeySet{Declared: true, Keys: []string{"plan"}, Slots: []string{"account"}},
	}
	if err := CheckDeclaredAttributeKeys(ast, decls); err != nil {
		t.Fatalf("the floor keys must be readable under any declared set: %v", err)
	}
	// Including under a DECLARED-EMPTY set, which permits nothing else.
	empty := DeclaredAttributeKeys{
		Principal: DeclaredKeySet{Declared: true, Slots: []string{"user", "machine"}},
		Account:   DeclaredKeySet{Declared: true, Slots: []string{"account"}},
	}
	if err := CheckDeclaredAttributeKeys(ast, empty); err != nil {
		t.Fatalf("the floor keys must survive a declared-empty set: %v", err)
	}
}

func TestADeclaredEmptySetPermitsNothingButTheFloor(t *testing.T) {
	// Declared-empty is the state a len(Keys) test loses, and losing it turns a
	// root that permits NO key into one that permits every key.
	empty := DeclaredAttributeKeys{
		Principal: DeclaredKeySet{Declared: true, Slots: []string{"user", "machine"}},
	}
	if !empty.Enforcing() {
		t.Error("a declared-empty set must report itself as enforcing")
	}
	err := CheckDeclaredAttributeKeys(Compare(OpEq, Var("principal.department"), Lit("eng")), empty)
	if aerr.CodeOf(err) != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("a declared-empty set must refuse every non-floor key, got %v", err)
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("a declared-empty refusal must say the set is empty, got %q", err)
	}
}

func TestOnlyTheFirstSegmentPastTheRootIsDeclared(t *testing.T) {
	// A declared key names a top-level key of a bag, and the value under it may be
	// a nested metadata value. Comparing the whole dotted path instead would refuse
	// every nested read of a declared key.
	ast := Compare(OpEq, Var("principal.metadata.department"), Lit("eng"))
	if err := CheckDeclaredAttributeKeys(ast, declaringPrincipal("metadata")); err != nil {
		t.Fatalf("a declared key must permit a nested read under it: %v", err)
	}
	// And the reverse: declaring the LEAF does not permit the parent.
	if err := CheckDeclaredAttributeKeys(ast, declaringPrincipal("department")); err == nil {
		t.Fatal("declaring the leaf segment must not permit reading a different top-level key")
	}
}

func TestAWholeBagReadIsRefusedByADeclaringRoot(t *testing.T) {
	// `principal` with no path hands the rule the whole bag, so it reads whatever
	// the bag happens to carry — including every key a local layer added. Left
	// legal, it is the one expression that makes a declared set decorative.
	ast := Compare(OpEq, Var("principal"), Var("object.owner_bag"))
	err := CheckDeclaredAttributeKeys(ast, declaringPrincipal("department"))
	if aerr.CodeOf(err) != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("a whole-bag read must be refused by a declaring root, got %v", err)
	}
	if !strings.Contains(err.Error(), "WHOLE") {
		t.Errorf("the whole-bag refusal must say so, got %q", err)
	}
	// But not by a root that declares nothing: the opt-out is total.
	if err := CheckDeclaredAttributeKeys(ast, DeclaredAttributeKeys{}); err != nil {
		t.Fatalf("a whole-bag read must still pass when nothing is declared: %v", err)
	}
}

func TestAKeyIsRefusedWhereverTheRuleNamesIt(t *testing.T) {
	// The gate is on what the rule NAMES, not on what a comparison did — the same
	// notion readsBeyondFloor uses, because they share walkVarFields. A key behind
	// an `&&` that short-circuits, or inside a list, or under a Not, is still a key
	// the rule's text depends on.
	decls := declaringPrincipal("department")
	cases := map[string]*Node{
		"short-circuited by an &&": And(
			Compare(OpEq, Var("principal.department"), Lit("legal")),
			Compare(OpEq, Var("principal.clearance"), Lit("secret")),
		),
		"under a Not": Not(Compare(OpEq, Var("principal.clearance"), Lit("secret"))),
		"inside a list": Compare(OpIn, Var("principal.department"),
			List(Var("principal.clearance"), Lit("eng"))),
		"on the right of a comparison": Compare(OpEq, Var("object.owner"), Var("principal.clearance")),
	}
	for name, ast := range cases {
		t.Run(name, func(t *testing.T) {
			if code := aerr.CodeOf(CheckDeclaredAttributeKeys(ast, decls)); code != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
				t.Fatalf("CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", code)
			}
		})
	}
}

func TestAPrefixThatIsNotTheRootIsNotEnforced(t *testing.T) {
	// `principality` shares a prefix with `principal` without sharing a root. It is
	// not a legal variable root at all, so it never reaches enforcement — but the
	// walk must not claim it either, or a declared set would refuse a path it has
	// nothing to say about.
	if err := CheckDeclaredAttributeKeys(
		Compare(OpEq, Var("account.plan"), Lit("enterprise")),
		declaringAccount("plan"),
	); err != nil {
		t.Fatalf("the account root's own declared key must pass: %v", err)
	}
	for _, path := range []string{"principality.secret", "accounts.secret", "object.principal_tier"} {
		if err := CheckDeclaredAttributeKeys(
			Compare(OpEq, Var(path), Lit("x")),
			DeclaredAttributeKeys{
				Principal: DeclaredKeySet{Declared: true, Slots: []string{"user", "machine"}},
				Account:   DeclaredKeySet{Declared: true, Slots: []string{"account"}},
			},
		); err != nil {
			t.Errorf("%s is not an attribute read and must not be refused: %v", path, err)
		}
	}
}

func TestValidateASTDeclaringReportsStructureFirst(t *testing.T) {
	// Compile-then-declare: a rule that is not yet a rule has a structural error to
	// fix first, and "principal.tier is not declared" would point its author at the
	// wrong line.
	broken := json.RawMessage(`{"type":"and","children":[{"type":"var","name":"principal.tier"}]}`)
	err := ValidateASTDeclaring(broken, declaringPrincipal("department"))
	if err == nil {
		t.Fatal("a structurally invalid rule must be refused")
	}
	if got := aerr.CodeOf(err); got == aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("CodeOf = %q, want the structural code first", got)
	}
}

func TestValidateASTDeclaringEnforcesAnOtherwiseValidRule(t *testing.T) {
	raw, err := json.Marshal(Compare(OpEq, Var("principal.clearance"), Lit("secret")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// ValidateAST — what every caller had before the set existed — must still pass.
	if err := ValidateAST(raw); err != nil {
		t.Fatalf("ValidateAST must be unchanged for a compilable rule: %v", err)
	}
	if code := aerr.CodeOf(ValidateASTDeclaring(raw, declaringPrincipal("department"))); code != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", code)
	}
	if err := ValidateASTDeclaring(raw, declaringPrincipal("clearance")); err != nil {
		t.Fatalf("a declared key must validate through ValidateASTDeclaring: %v", err)
	}
}

// codedDepth counts the Aperture-coded errors in a chain. A same-code re-stamp is
// invisible to CodeOf, so depth is what proves the refusal was classified once.
func codedDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}
