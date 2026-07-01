package service

import (
	"context"
	"encoding/json"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"
)

// portDoc is a small but complete state file used by the portability tests.
func portDoc() *seed.Document {
	return &seed.Document{
		Accounts: []seed.Account{{ID: "acme", Name: "Acme"}},
		ObjectTypes: []seed.ObjectType{
			{Name: "document", Actions: []string{"read", "write"}},
		},
		Permissions: []seed.Permission{
			{ID: "perm-read", ObjectType: "document", Action: "read"},
		},
		Principals: []seed.Principal{
			{ID: "carol", Kind: "user", Identity: "user:carol"},
		},
		Grants: []seed.Grant{
			{
				ID: "g1", Account: "acme",
				Subject:    seed.Subject{Kind: "principal", ID: "carol"},
				Permission: "perm-read", Object: "account:acme/**", Effect: "allow",
			},
		},
		Rules: []seed.Rule{
			{Name: "r1", AST: json.RawMessage(`{"type":"var","name":"object.x"}`)},
		},
	}
}

// TestImportSystemTierGated proves import is refused for a principal without
// system-admin authority, and nothing is written.
func TestImportSystemTierGated(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()

	mallory := Actor{Principal: "mallory", Account: "acme"}
	err := svc.Import(ctx, mallory, portDoc())
	if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("want AUTHZ_DENIED for non-admin import, got %v", err)
	}
	// Nothing from the file landed.
	if _, e := store.GetPrincipal(ctx, "carol"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("denied import must not persist; carol present: %v", e)
	}
}

// TestImportTransactionalRollback proves a state file that fails validation
// midway leaves NO partial state — the whole import rolls back.
func TestImportTransactionalRollback(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	bad := portDoc()
	// Append a structurally broken rule so Apply fails after the good entities.
	bad.Rules = append(bad.Rules, seed.Rule{
		Name: "broken",
		AST:  json.RawMessage(`{"type":"compare","op":"eq","left":{"type":"var","name":"object.x"}}`),
	})

	if err := svc.Import(ctx, alice, bad); err == nil {
		t.Fatalf("expected import of a broken file to fail")
	}
	// The good entities that came before the broken rule must NOT have persisted.
	if _, e := store.GetPrincipal(ctx, "carol"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("rollback failed; carol persisted: %v", e)
	}
	if _, e := store.GetGrant(ctx, "g1"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("rollback failed; g1 persisted: %v", e)
	}
}

// TestExportImportRoundTripThroughFacade proves the admin-gated Export/Import
// pair round-trips: import a file, export it back, re-import, export again, and
// assert the two exports are byte-identical (idempotent + stable).
func TestExportImportRoundTripThroughFacade(t *testing.T) {
	svc, _, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	if err := svc.Import(ctx, alice, portDoc()); err != nil {
		t.Fatalf("import: %v", err)
	}
	exp1, err := svc.Export(ctx, alice)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	b1, err := seed.Marshal(exp1, seed.FormatJSON)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Re-import the exported file; a second export must be byte-identical.
	reparsed, err := seed.Parse(b1, seed.FormatJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := svc.Import(ctx, alice, reparsed); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	exp2, _ := svc.Export(ctx, alice)
	b2, _ := seed.Marshal(exp2, seed.FormatJSON)

	if string(b1) != string(b2) {
		t.Fatalf("round trip through facade not stable:\n--- first ---\n%s\n--- second ---\n%s", b1, b2)
	}
}

// TestExportSystemTierGated proves export is refused for a non-admin principal.
func TestExportSystemTierGated(t *testing.T) {
	svc, _, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()

	mallory := Actor{Principal: "mallory", Account: "acme"}
	if _, err := svc.Export(ctx, mallory); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("want AUTHZ_DENIED for non-admin export, got %v", err)
	}
}
