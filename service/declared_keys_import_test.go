package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
)

// Import is a rule-WRITING path, and the declared-key gate's whole argument is that
// a local attribute layer's extra keys are inert because no rule naming one can be
// saved. These assert the gate covers this path too — for a while it did not, and
// the "by construction ... inert" paragraph in rules/declared.go was false because
// of it.

// seedRuleReading is a seed-document rule whose AST reads one attribute path, which
// is the shape the gate has to catch in a document rather than in a model.Rule.
func seedRuleReading(t *testing.T, name, path string) seed.Rule {
	t.Helper()
	raw, err := json.Marshal(rules.Compare(rules.OpEq, rules.Var(path), rules.Lit("x")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return seed.Rule{Name: name, AST: raw}
}

// importingService is a facade wired for Import — store, gate and an admin actor,
// since requireMutator refuses a facade with no gate — with a declared key set and a
// counting store, so a refusal can be shown to have opened no transaction.
func importingService(t *testing.T, sets map[provider.AttributeSlot]model.DeclaredKeys) (*Service, *countingStore) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))

	counting := &countingStore{Storage: store}
	eng := engine.New(counting)
	return New(eng, WithStorage(counting), WithGate(authz.NewGate(eng)),
		WithDeclaredAttributeKeys(sets)), counting
}

// TestImportRefusesARuleReadingAnUndeclaredAttributeKey is the bypass test. Both
// principal slots declare, so the root is enforced; the rule names a key neither
// declared. It must be refused with the same code PutRule uses, and nothing from
// the document may land.
func TestImportRefusesARuleReadingAnUndeclaredAttributeKey(t *testing.T) {
	svc, counting := importingService(t, map[provider.AttributeSlot]model.DeclaredKeys{
		provider.AttributeSlotUser:    declared("department"),
		provider.AttributeSlotMachine: declared("department"),
	})
	doc := portDoc()
	doc.Rules = []seed.Rule{seedRuleReading(t, "leak", "principal.clearance")}

	err := svc.Import(context.Background(), Actor{Principal: "alice", Account: "acme"}, doc)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("code = %q, want %q (err = %v)", got, aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE, err)
	}
	if !strings.Contains(err.Error(), "clearance") || !strings.Contains(err.Error(), "leak") {
		t.Errorf("refusal must name the key and the rule; got %v", err)
	}
	if counting.atomics != 0 {
		t.Errorf("a refused import opened %d transaction(s); the gate must run before the transaction", counting.atomics)
	}
}

// TestAnImportThatDeclaresNothingAcceptsEveryRuleItUsedTo is the opt-in bar, at the
// surface. A deployment that has not opted in must be able to import exactly what it
// could before declaring existed — otherwise every such deployment breaks at once.
func TestAnImportThatDeclaresNothingAcceptsEveryRuleItUsedTo(t *testing.T) {
	for _, sets := range []map[provider.AttributeSlot]model.DeclaredKeys{
		nil,
		{},
		// One principal slot declaring is NOT enough for the principal root: the
		// other slot guarantees nothing, so the root stays unenforced.
		{provider.AttributeSlotUser: declared("department")},
	} {
		svc, _ := importingService(t, sets)
		doc := portDoc()
		doc.Rules = []seed.Rule{seedRuleReading(t, "anything", "principal.whatever")}
		if err := svc.Import(context.Background(), Actor{Principal: "alice", Account: "acme"}, doc); err != nil {
			t.Fatalf("declaring %v refused a rule it used to accept: %v", sets, err)
		}
	}
}

// TestAnImportRefusalCarriesExactlyOneApertureCode — Wrap re-stamps, so a gate that
// wrapped an already-coded refusal would bury the key-and-slot fixups under a
// generic import code. Chain depth is what catches that; the code alone does not.
func TestAnImportRefusalCarriesExactlyOneApertureCode(t *testing.T) {
	svc, _ := importingService(t, map[provider.AttributeSlot]model.DeclaredKeys{
		provider.AttributeSlotUser:    declared(),
		provider.AttributeSlotMachine: declared(),
	})
	doc := portDoc()
	doc.Rules = []seed.Rule{seedRuleReading(t, "r", "principal.clearance")}

	err := svc.Import(context.Background(), Actor{Principal: "alice", Account: "acme"}, doc)
	if err == nil {
		t.Fatal("a declared-empty set must refuse every non-floor key")
	}
	if n := codedDepth(err); n != 1 {
		t.Fatalf("chain carries %d Aperture-coded errors, want exactly 1: %v", n, err)
	}
}

// TestTheFloorIsImportableUnderADeclaredEmptySet — principal.id is a floor key,
// present with no provider wired and never part of a declared set. An import of the
// single most common rule in existence must not be refused by a wiring change that
// has nothing to say about it.
func TestTheFloorIsImportableUnderADeclaredEmptySet(t *testing.T) {
	svc, _ := importingService(t, map[provider.AttributeSlot]model.DeclaredKeys{
		provider.AttributeSlotUser:    declared(),
		provider.AttributeSlotMachine: declared(),
		provider.AttributeSlotAccount: declared(),
	})
	doc := portDoc()
	doc.Rules = []seed.Rule{seedRuleReading(t, "owner", "principal.id")}
	if err := svc.Import(context.Background(), Actor{Principal: "alice", Account: "acme"}, doc); err != nil {
		t.Fatalf("the floor was refused: %v", err)
	}
}

// TestThePrincipalRootsSlotSetIsDerivedNotSpelledOut — the collapse used to name the
// two principal slots as literals. A fourth slot would then have been left out of
// both the union and the every-slot-must-declare bar, the second of which enforces a
// set the new slot never agreed to. Deriving makes both follow from the closed set.
func TestThePrincipalRootsSlotSetIsDerivedNotSpelledOut(t *testing.T) {
	all := provider.AttributeSlots()
	got := principalAttributeSlots()

	if len(got) != len(all)-1 {
		t.Fatalf("principal slots = %v (%d), want every slot but the account one out of %v", got, len(got), all)
	}
	if slices.Contains(got, provider.AttributeSlotAccount) {
		t.Errorf("the account slot backs the account root, not the principal root; got %v", got)
	}
	for _, slot := range all {
		if slot == provider.AttributeSlotAccount {
			continue
		}
		if !slices.Contains(got, slot) {
			t.Errorf("slot %q is not the account slot and so must back the principal root; got %v", slot, got)
		}
	}
}

// codedDepth counts the Aperture-coded errors in a chain. A same-code re-stamp is
// invisible to CodeOf, so depth is what proves the gate passed the refusal through
// rather than re-wrapping it and burying the key-and-slot fixups.
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
