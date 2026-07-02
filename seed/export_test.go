package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// fullDocument is a complete declarative model exercising every entity kind the
// state file captures — including templates and a rule AST — so the round-trip
// and idempotency assertions cover the whole surface, not just the seed subset.
func fullDocument() *Document {
	return &Document{
		Accounts: []Account{{ID: "acme", Name: "Acme Corp", Description: "demo tenant"}},
		Memberships: []Membership{
			{Principal: "alice", Account: "acme"},
			{Principal: "bob", Account: "acme"},
		},
		ObjectTypes: []ObjectType{
			{Name: "document", Description: "a doc", Actions: []string{"read", "write"}},
		},
		Permissions: []Permission{
			{ID: "perm-read", ObjectType: "document", Action: "read", Description: "read docs"},
			{ID: "perm-write", ObjectType: "document", Action: "write", Delegatable: true},
		},
		Principals: []Principal{
			{ID: "alice", Kind: "user", Identity: "user:alice", DisplayName: "Alice", Roles: []string{"editor"}},
			{ID: "bob", Kind: "user", Identity: "user:bob", DisplayName: "Bob"},
		},
		Roles: []Role{
			{ID: "editor", Name: "Editor", Description: "edits", Permissions: []string{"perm-read", "perm-write"}},
		},
		Groups: []Group{
			{ID: "eng", Name: "Engineering", Members: []string{"alice", "bob"}},
		},
		Grants: []Grant{
			{
				ID: "g-read", Account: "acme",
				Subject:    Subject{Kind: "group", ID: "eng"},
				Permission: "perm-read", Object: "account:acme/**", Effect: "allow",
			},
		},
		Templates: []Template{
			{
				Name: "onboard", Version: 1, Description: "onboard a member",
				Params: []TemplateParam{{Name: "account", Type: "segment"}},
				Grants: []TemplateGrant{{
					Subject:    Subject{Kind: "principal", ID: "${account}-member"},
					Permission: "perm-read", Object: "account:${account}/**", Effect: "allow",
				}},
			},
		},
		Rules: []Rule{
			{
				Name:        "public-only",
				Description: "select public docs",
				AST: json.RawMessage(`{ "type": "compare", "op": "eq",
					"left": {"type":"var","name":"object.classification"},
					"right": {"type":"literal","value":"public"} }`),
			},
		},
	}
}

func freshStore(t *testing.T) model.Storage {
	t.Helper()
	s := memory.New()
	if err := s.Setup(context.Background()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return s
}

// applyExport applies doc into a fresh store and re-exports it, returning the
// canonical JSON bytes of the re-export — the stable, comparable state file.
func applyExport(t *testing.T, doc *Document) []byte {
	t.Helper()
	ctx := context.Background()
	s := freshStore(t)
	if err := doc.Apply(ctx, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	exp, err := Export(ctx, s)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	b, err := Marshal(exp, FormatJSON)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestRoundTripFidelity proves export -> import -> export yields a byte-identical
// state file (stable key ordering + canonical rule AST).
func TestRoundTripFidelity(t *testing.T) {
	doc := fullDocument()
	b1 := applyExport(t, doc)

	// Import b1 into a fresh store and export again.
	reparsed, err := Parse(b1, FormatJSON)
	if err != nil {
		t.Fatalf("parse b1: %v", err)
	}
	b2 := applyExport(t, reparsed)

	if !bytes.Equal(b1, b2) {
		t.Fatalf("round trip not byte-stable:\n--- first ---\n%s\n--- second ---\n%s", b1, b2)
	}
}

// TestIdempotentReimport proves re-importing the same file into a store that
// already holds it changes NOTHING (the export before and after is identical).
func TestIdempotentReimport(t *testing.T) {
	ctx := context.Background()
	doc := fullDocument()
	s := freshStore(t)
	if err := doc.Apply(ctx, s); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	exp, _ := Export(ctx, s)
	before, _ := Marshal(exp, FormatJSON)

	// Re-import the SAME file. As an upsert of identical values it must be a no-op.
	reparsed, err := Parse(before, FormatJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reparsed.Apply(ctx, s); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	exp2, _ := Export(ctx, s)
	after, _ := Marshal(exp2, FormatJSON)

	if !bytes.Equal(before, after) {
		t.Fatalf("re-import changed the model:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestYAMLRoundTripEquivalent proves the YAML state file is structurally
// equivalent to the JSON one: export as YAML, re-import, and the JSON export of
// the reloaded model matches the original JSON export.
func TestYAMLRoundTripEquivalent(t *testing.T) {
	ctx := context.Background()
	doc := fullDocument()
	s := freshStore(t)
	if err := doc.Apply(ctx, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	exp, _ := Export(ctx, s)
	jsonBytes, _ := Marshal(exp, FormatJSON)
	yamlBytes, err := Marshal(exp, FormatYAML)
	if err != nil {
		t.Fatalf("marshal yaml: %v", err)
	}

	reparsed, err := Parse(yamlBytes, FormatYAML)
	if err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	roundJSON := applyExport(t, reparsed)
	if !bytes.Equal(jsonBytes, roundJSON) {
		t.Fatalf("yaml round trip not equivalent to json:\n--- json ---\n%s\n--- via yaml ---\n%s", jsonBytes, roundJSON)
	}
}

// TestEmptyStoreRoundTrips proves an empty model exports and re-imports cleanly
// and stably (no nil/`[]` drift).
func TestEmptyStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	exp, err := Export(ctx, s)
	if err != nil {
		t.Fatalf("export empty: %v", err)
	}
	b1, _ := Marshal(exp, FormatJSON)
	reparsed, _ := Parse(b1, FormatJSON)
	b2 := applyExport(t, reparsed)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("empty round trip not stable:\n%s\n---\n%s", b1, b2)
	}
}

// TestWildcardAccountEntitiesRoundTrip proves the "*" (all-accounts) wildcard —
// which is NOT a real Account — is not silently dropped on export. A super-admin
// group's "*"-stamped grant and a principal's "*" membership are the cross-account
// escape hatch the engine honors; if Export enumerated only real accounts they
// would vanish on export/import, quietly stripping a super-admin's reach.
func TestWildcardAccountEntitiesRoundTrip(t *testing.T) {
	ctx := context.Background()
	doc := &Document{
		Accounts:    []Account{{ID: "acme", Name: "Acme"}},
		ObjectTypes: []ObjectType{{Name: "system", Actions: []string{"aperture.admin"}}},
		Permissions: []Permission{{ID: "admin", ObjectType: "system", Action: "aperture.admin"}},
		Principals:  []Principal{{ID: "root", Kind: "user", Identity: "user:root"}},
		Groups:      []Group{{ID: "supers", Name: "Supers", Members: []string{"root"}}},
		Memberships: []Membership{{Principal: "root", Account: model.AccountWildcard}},
		Grants: []Grant{{
			ID: "g-super", Account: model.AccountWildcard,
			Subject: Subject{Kind: "group", ID: "supers"}, Permission: "admin",
			Object: "**", Effect: "allow",
		}},
	}
	s := freshStore(t)
	if err := doc.Apply(ctx, s); err != nil {
		t.Fatalf("apply: %v", err)
	}
	exp, err := Export(ctx, s)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var gotGrant, gotMember bool
	for _, g := range exp.Grants {
		if g.Account == model.AccountWildcard && g.ID == "g-super" {
			gotGrant = true
		}
	}
	for _, m := range exp.Memberships {
		if m.Account == model.AccountWildcard && m.Principal == "root" {
			gotMember = true
		}
	}
	if !gotGrant {
		t.Error("wildcard-account grant was dropped on export")
	}
	if !gotMember {
		t.Error("wildcard-account membership was dropped on export")
	}
}

// TestInvalidRuleRejected proves a state file carrying a structurally broken rule
// AST is rejected with the rules engine's coded error at apply.
func TestInvalidRuleRejected(t *testing.T) {
	ctx := context.Background()
	doc := &Document{
		Rules: []Rule{{
			Name: "broken",
			// A comparison node missing its right operand — invalid per rules.Node.
			AST: json.RawMessage(`{"type":"compare","op":"eq","left":{"type":"var","name":"object.x"}}`),
		}},
	}
	err := doc.Apply(ctx, freshStore(t))
	if err == nil {
		t.Fatalf("expected invalid rule to be rejected")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_INVALID {
		t.Fatalf("code = %s, want APERTURE_RULE_INVALID (err: %v)", got, err)
	}
}

// TestInvalidEntityRejected proves a malformed entity (a permission on an
// undeclared object type) surfaces a coded error at apply.
func TestInvalidEntityRejected(t *testing.T) {
	ctx := context.Background()
	doc := &Document{
		Permissions: []Permission{{ID: "p", ObjectType: "ghost", Action: "read"}},
	}
	err := doc.Apply(ctx, freshStore(t))
	if err == nil {
		t.Fatalf("expected invalid permission to be rejected")
	}
	// seed.Apply wraps a failed entity write as APERTURE_INVALID_INPUT (the
	// import-rejection contract): the underlying object-type-not-found is folded in.
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %s, want APERTURE_INVALID_INPUT (err: %v)", got, err)
	}
}
