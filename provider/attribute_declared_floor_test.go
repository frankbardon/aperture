// This file is deliberately in the EXTERNAL test package. The declared-set
// suppression it asserts happens in provider, and the floor that has to survive it
// is stamped in rules — which imports provider, so an in-package test could not
// reach it without a cycle. An external test package depends on both and creates
// none, and the non-test firewall (TestProviderPackageImportsOnlyIdentityAndErrors)
// is untouched because it scans only non-test files.
package provider_test

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// declaredFloorEngine wires the revocation case — hole (b) — with `declared` as the
// shared layer's declared set, and returns a rules.Engine over the five rules the
// cases ask.
//
// The shared directory has bob and has DROPPED alice. One instance's file still
// lists her, with an `id` and a `kind` of its own on top of her clearance and team,
// which is the innocent collision the engine's floor exists for.
func declaredFloorEngine(t *testing.T, declared []string) *rules.Engine {
	t.Helper()
	reg := provider.NewAttributeRegistry()
	reg.MustRegister(provider.AttributeSlotUser,
		mustStatic(t, provider.AttributeRecord{ID: "bob", Attributes: provider.Metadata{"clearance": int64(1)}}),
		provider.WithDeclaredKeys(declared))
	reg.MustRegisterLocal(provider.AttributeSlotUser,
		mustStatic(t, provider.AttributeRecord{ID: "alice", Attributes: provider.Metadata{
			"id":        "local-surrogate",
			"kind":      "machine",
			"clearance": int64(9),
			"team":      "atlas",
		}}))
	return rules.NewEngine(rules.MapSource{
		"is-alice":     {AST: rules.Compare(rules.OpEq, rules.Var("principal.id"), rules.Lit("alice"))},
		"is-user":      {AST: rules.Compare(rules.OpEq, rules.Var("principal.kind"), rules.Lit("user"))},
		"cleared":      {AST: rules.Compare(rules.OpEq, rules.Var("principal.clearance"), rules.Lit(int64(9)))},
		"on-the-team":  {AST: rules.Compare(rules.OpEq, rules.Var("principal.team"), rules.Lit("atlas"))},
		"still-graded": {AST: rules.Compare(rules.OpEq, rules.Var("principal.clearance"), rules.Lit(int64(1)))},
	}, nil, rules.WithPrincipalResolver(reg))
}

// TestTheFloorStillStampsOverASuppressedKey asserts the three tiers in the one
// direction they compose — floor over shared over local — now that the middle tier
// can SUPPRESS the bottom one.
//
// The declared set here is `clearance` ALONE, which is what makes the floor half
// live: `id` and `kind` are undeclared, so the local file's surrogates survive the
// merge and reach the engine, and the only thing that can stop a rule reading them
// is the floor being stamped LAST. (Declare them too and suppression removes them
// before rules ever sees them, which asserts nothing about the floor — the sibling
// case below is the one that covers declaring them.)
//
// Four things come out of it, each a different failure:
//
//   - `principal.id` is the real principal. The floor is stamped last over whatever
//     the resolver returned and is never part of any declared set, so a local bag
//     spelling `id` cannot be read. If it could, `principal.id == object.owner` — the
//     most common rule there is — would compare one machine's surrogate.
//   - `principal.kind` is the same, and it is the key a rule uses to state its
//     dependence on a directory, so a bag that could redefine it would make that
//     statement unreliable.
//   - `principal.clearance` is GONE, or the revocation did not happen.
//   - `principal.team` still answers, or suppression has become the discard this
//     layering replaced.
//
// It goes through rules.Engine.Selected because the floor is the thing under test
// and provider cannot see it. rules/principal_floor_test.go and
// rules/account_floor_test.go still pass unchanged, which is the other half: the
// floor's own behaviour did not move.
func TestTheFloorStillStampsOverASuppressedKey(t *testing.T) {
	ctx := context.Background()
	obj := identity.MustParse("document:1")
	eng := declaredFloorEngine(t, []string{"clearance"})

	selected := func(t *testing.T, rule, principal string) bool {
		t.Helper()
		ok, err := eng.Selected(ctx, rule, obj, "acme", "user", principal, "read")
		if err != nil {
			t.Fatalf("Selected(%s, %s): %v", rule, principal, err)
		}
		return ok
	}

	if !selected(t, "is-alice", "alice") {
		t.Error("principal.id did not read the real principal: the FLOOR is stamped LAST over " +
			"the merged bag and is never part of a declared set, so no bag may be read for it")
	}
	if !selected(t, "is-user", "alice") {
		t.Error("principal.kind did not read the real kind; a local bag's `kind` must not be " +
			"readable, or a rule cannot state its own dependence on a directory")
	}
	if selected(t, "cleared", "alice") {
		t.Error("principal.clearance still read the local file's 9 for a subject the shared " +
			"directory has dropped: the declared key was not revoked")
	}
	if !selected(t, "on-the-team", "alice") {
		t.Error("principal.team stopped answering; it is outside the declared set, and " +
			"suppression scoped to the set is what keeps the local layer a layer")
	}
	// The control: a subject the shared directory DOES list still reads its value, so
	// the suppression above is about the missing record and not about the slot.
	if !selected(t, "still-graded", "bob") {
		t.Error("the shared layer's own clearance stopped being read")
	}
}

// TestDeclaringAFloorKeyIsRedundantRatherThanAnError: `id` and `kind` are the
// engine's floor, always present and never part of any declared set, so naming them
// in a declared set is neither required nor a refusal — merely redundant.
//
// The case is here because the tempting "tidy-up" is to refuse a declared set that
// names a floor key, or to special-case it in the merge. Either would be a
// wiring-time refusal over a declaration that cannot change a verdict, and the
// second would put the floor's key names in a second place. This asserts that the
// set is taken as written and the floor is unaffected by what it says.
func TestDeclaringAFloorKeyIsRedundantRatherThanAnError(t *testing.T) {
	ctx := context.Background()
	obj := identity.MustParse("document:1")
	eng := declaredFloorEngine(t, []string{"id", "kind", "clearance"})

	for _, rule := range []string{"is-alice", "is-user"} {
		ok, err := eng.Selected(ctx, rule, obj, "acme", "user", "alice", "read")
		if err != nil {
			t.Fatalf("Selected(%s): a declared set naming a floor key must not be a refusal: %v", rule, err)
		}
		if !ok {
			t.Errorf("%s did not select; the floor is stamped over the merged bag whatever the "+
				"declared set says about its key names", rule)
		}
	}
}

func mustStatic(t *testing.T, records ...provider.AttributeRecord) provider.AttributeProvider {
	t.Helper()
	p, err := provider.NewStaticAttributes(records)
	if err != nil {
		t.Fatalf("NewStaticAttributes: %v", err)
	}
	return p
}
