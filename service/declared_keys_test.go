package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/storage/memory"
)

// E3-S3 at the facade: the two definition-time surfaces (ValidateRule/PutRule and
// EvaluateRulePreview) apply ONE declared-key gate, and the collapse from slots to
// rule roots is asserted directly, because both of its halves widen access silently
// when they drift.

func declaredKeysService(t *testing.T, sets map[provider.AttributeSlot]model.DeclaredKeys) *Service {
	t.Helper()
	store := memory.New()
	if err := store.Setup(context.Background()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return New(engine.New(store), WithStorage(store), WithDeclaredAttributeKeys(sets))
}

func ruleReading(t *testing.T, path string) model.Rule {
	t.Helper()
	return ruleFrom(t, rules.Compare(rules.OpEq, rules.Var(path), rules.Lit("x")))
}

// ruleWholeBag is the compilable form of a WHOLE-BAG read: hasKey takes a map on
// the left, so `hasKey(principal, "clearance")` type-checks where
// `principal == "x"` does not. It is the real escape hatch a declared set has to
// close, not a hypothetical one.
func ruleWholeBag(t *testing.T, root string) model.Rule {
	t.Helper()
	return ruleFrom(t, rules.Compare(rules.OpHasKey, rules.Var(root), rules.Lit("clearance")))
}

func ruleFrom(t *testing.T, ast *rules.Node) model.Rule {
	t.Helper()
	raw, err := json.Marshal(ast)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return model.Rule{Name: "r", AST: raw}
}

func declared(keys ...string) model.DeclaredKeys {
	return model.DeclaredKeys{Declared: true, Keys: keys}
}

// TestAFacadeThatDeclaresNothingValidatesEveryRuleItUsedTo is the
// backward-compatibility bar, at the surface every rule author reaches: with no
// WithDeclaredAttributeKeys, no slot declares, and a rule may read any attribute
// key at all — including the whole bag. That is what keeps every rule already in
// the repository, its fixtures, and every deployment's stored rules valid.
func TestAFacadeThatDeclaresNothingValidatesEveryRuleItUsedTo(t *testing.T) {
	ctx := context.Background()
	for _, svc := range []*Service{
		New(engine.New(memory.New()), WithStorage(memory.New())), // the option never passed
		declaredKeysService(t, nil),                              // passed, empty
		declaredKeysService(t, map[provider.AttributeSlot]model.DeclaredKeys{
			// Present but NOT declared: the zero value of the column.
			provider.AttributeSlotUser: {},
		}),
	} {
		for _, path := range []string{
			"principal.clearance", "principal.anything.at.all", "account.plan",
		} {
			if err := svc.ValidateRule(ctx, ruleReading(t, path)); err != nil {
				t.Errorf("ValidateRule(%s) with nothing declared: %v", path, err)
			}
		}
		for _, root := range []string{"principal", "account"} {
			if err := svc.ValidateRule(ctx, ruleWholeBag(t, root)); err != nil {
				t.Errorf("ValidateRule(whole %s bag) with nothing declared: %v", root, err)
			}
		}
	}
}

func TestValidateRuleRefusesAnUndeclaredKey(t *testing.T) {
	ctx := context.Background()
	svc := declaredKeysService(t, map[provider.AttributeSlot]model.DeclaredKeys{
		provider.AttributeSlotUser:    declared("department"),
		provider.AttributeSlotMachine: declared("department"),
		provider.AttributeSlotAccount: declared("plan"),
	})

	if err := svc.ValidateRule(ctx, ruleReading(t, "principal.department")); err != nil {
		t.Fatalf("a declared key must validate: %v", err)
	}
	if err := svc.ValidateRule(ctx, ruleReading(t, "account.plan")); err != nil {
		t.Fatalf("a declared account key must validate: %v", err)
	}
	if err := svc.ValidateRule(ctx, ruleReading(t, "principal.id")); err != nil {
		t.Fatalf("the floor must validate: %v", err)
	}

	err := svc.ValidateRule(ctx, ruleReading(t, "principal.clearance"))
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("ValidateRule CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE (err=%v)", got, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "clearance") || !strings.Contains(msg, "user") {
		t.Errorf("refusal must name the key and the slot, got %q", msg)
	}

	// The whole-bag read is refused too: `hasKey(principal, ...)` reads whatever the
	// bag carries, so left legal it is the one expression that makes a declared set
	// decorative.
	if got := aerr.CodeOf(svc.ValidateRule(ctx, ruleWholeBag(t, "principal"))); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("whole-bag read: CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", got)
	}
}

// TestPutRuleAppliesTheSameGateAsValidateRule is the half that actually stops a bad
// rule from shipping. If save and check disagreed, the editor would report a rule
// clean and then store it — or refuse a rule its own check just passed.
func TestPutRuleAppliesTheSameGateAsValidateRule(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Enough authority for a system-tier mutation to reach validation at all:
	// PutRule gates BEFORE it validates, so an unauthorized actor would report
	// AUTHZ_DENIED and prove nothing about the declared-key gate.
	mustPut(t, store.PutAccount(ctx, model.Account{ID: "acme", Name: "acme"}))
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))
	eng := engine.New(store)
	svc := New(eng, WithStorage(store), WithGate(authz.NewGate(eng)),
		WithDeclaredAttributeKeys(map[provider.AttributeSlot]model.DeclaredKeys{
			provider.AttributeSlotUser:    declared("department"),
			provider.AttributeSlotMachine: declared("department"),
		}))
	actor := Actor{Principal: "alice", Account: "acme"}

	err := svc.PutRule(ctx, actor, ruleReading(t, "principal.clearance"))
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("PutRule CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE (err=%v)", got, err)
	}
	if _, getErr := svc.GetRule(ctx, "r"); getErr == nil {
		t.Fatal("a refused rule must not have been stored")
	}
	if err := svc.PutRule(ctx, actor, ruleReading(t, "principal.department")); err != nil {
		t.Fatalf("PutRule with a declared key: %v", err)
	}
}

// TestEvaluateRulePreviewRefusesAnUndeclaredKey pins the other definition-time
// surface. A preview that answered for a rule the save will refuse would teach an
// author that the rule works.
func TestEvaluateRulePreviewRefusesAnUndeclaredKey(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	svc := New(engine.New(store),
		WithProviders(newHiredAtRegistry(t)),
		WithDeclaredAttributeKeys(map[provider.AttributeSlot]model.DeclaredKeys{
			provider.AttributeSlotUser:    declared("department"),
			provider.AttributeSlotMachine: declared("department"),
		}))

	ast := rules.Compare(rules.OpEq, rules.Var("principal.clearance"), rules.Lit("secret"))
	_, err := svc.EvaluateRulePreview(ctx, ast, "staff:1")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("EvaluateRulePreview CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE (err=%v)", got, err)
	}

	// A declared key still previews, so the gate has not broken the surface.
	ok := rules.Compare(rules.OpEq, rules.Var("principal.department"), rules.Lit("eng"))
	if _, err := svc.EvaluateRulePreview(ctx, ok, "staff:1"); err != nil {
		t.Fatalf("a declared key must still preview: %v", err)
	}
}

// TestThePrincipalRootIsEnforcedOnlyWhenBothSlotsDeclare is the load-bearing half
// of the slot-to-root collapse. Both directions are failures:
//
//   - one declaring slot treated as enough would start enforcing machine principals
//     on the strength of a set only the user slot agreed to, which breaks the
//     per-slot opt-in the story promises;
//   - intersecting instead of unioning would refuse `principal.fleet` for a key the
//     machine slot really does guarantee, which is the documented way to write a
//     kind-specific rule.
func TestThePrincipalRootIsEnforcedOnlyWhenBothSlotsDeclare(t *testing.T) {
	cases := []struct {
		name     string
		sets     map[provider.AttributeSlot]model.DeclaredKeys
		declared bool
		permits  []string
		refuses  []string
	}{
		{
			name:     "only the user slot declares",
			sets:     map[provider.AttributeSlot]model.DeclaredKeys{provider.AttributeSlotUser: declared("department")},
			declared: false,
			permits:  []string{"department", "clearance", "anything"},
		},
		{
			name: "only the machine slot declares",
			sets: map[provider.AttributeSlot]model.DeclaredKeys{
				provider.AttributeSlotMachine: declared("fleet"),
			},
			declared: false,
			permits:  []string{"fleet", "clearance"},
		},
		{
			name: "both declare — the union is permitted",
			sets: map[provider.AttributeSlot]model.DeclaredKeys{
				provider.AttributeSlotUser:    declared("department"),
				provider.AttributeSlotMachine: declared("fleet"),
			},
			declared: true,
			permits:  []string{"department", "fleet"},
			refuses:  []string{"clearance"},
		},
		{
			name: "both declare, one empty — the union is the other's set",
			sets: map[provider.AttributeSlot]model.DeclaredKeys{
				provider.AttributeSlotUser:    declared("department"),
				provider.AttributeSlotMachine: {Declared: true},
			},
			declared: true,
			permits:  []string{"department"},
			refuses:  []string{"fleet"},
		},
		{
			name: "both declare empty — nothing but the floor",
			sets: map[provider.AttributeSlot]model.DeclaredKeys{
				provider.AttributeSlotUser:    {Declared: true},
				provider.AttributeSlotMachine: {Declared: true},
			},
			declared: true,
			refuses:  []string{"department", "fleet"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := declaredAttributeKeys(tc.sets).Principal
			if set.Declared != tc.declared {
				t.Fatalf("Principal.Declared = %v, want %v", set.Declared, tc.declared)
			}
			ctx := context.Background()
			svc := declaredKeysService(t, tc.sets)
			for _, key := range tc.permits {
				if err := svc.ValidateRule(ctx, ruleReading(t, "principal."+key)); err != nil {
					t.Errorf("principal.%s must validate: %v", key, err)
				}
			}
			for _, key := range tc.refuses {
				err := svc.ValidateRule(ctx, ruleReading(t, "principal."+key))
				if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
					t.Errorf("principal.%s: CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", key, got)
				}
			}
			// The floor is readable in every one of these arrangements.
			if err := svc.ValidateRule(ctx, ruleReading(t, "principal.id")); err != nil {
				t.Errorf("principal.id must validate: %v", err)
			}
		})
	}
}

// TestTheAccountRootIsEnforcedByItsOwnSlot proves the two roots are independent:
// one slot backs `account`, so declaring it neither waits for nor affects the
// principal slots.
func TestTheAccountRootIsEnforcedByItsOwnSlot(t *testing.T) {
	ctx := context.Background()
	svc := declaredKeysService(t, map[provider.AttributeSlot]model.DeclaredKeys{
		provider.AttributeSlotAccount: declared("plan"),
	})
	if err := svc.ValidateRule(ctx, ruleReading(t, "account.plan")); err != nil {
		t.Fatalf("a declared account key must validate: %v", err)
	}
	if got := aerr.CodeOf(svc.ValidateRule(ctx, ruleReading(t, "account.region"))); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("account.region: CodeOf = %q, want APERTURE_RULE_UNDECLARED_ATTRIBUTE", got)
	}
	// The principal slots declare nothing, so `principal` keeps today's leniency.
	if err := svc.ValidateRule(ctx, ruleReading(t, "principal.clearance")); err != nil {
		t.Fatalf("the principal root must be unaffected: %v", err)
	}
}
