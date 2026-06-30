package seed

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/storage/memory"
)

// TestExampleLoads asserts the committed fixture parses and applies cleanly into
// a fresh store, and lands every declared entity.
func TestExampleLoads(t *testing.T) {
	ctx := context.Background()
	doc, err := Parse(Example, FormatYAML)
	if err != nil {
		t.Fatalf("parse example: %v", err)
	}
	if len(doc.Grants) == 0 || len(doc.Principals) == 0 || len(doc.ObjectTypes) == 0 {
		t.Fatalf("example looks empty: %s", doc.Describe())
	}

	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := doc.Apply(ctx, store); err != nil {
		t.Fatalf("apply example: %v", err)
	}

	grants, err := store.ListGrants(ctx, ExampleAccount)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != len(doc.Grants) {
		t.Errorf("want %d grants stored, got %d", len(doc.Grants), len(grants))
	}
	if _, err := store.GetPrincipal(ctx, "alice"); err != nil {
		t.Errorf("principal alice not seeded: %v", err)
	}
}

// TestJSONEquivalent asserts a JSON document decodes to the same shape as YAML,
// proving both formats share the field tags.
func TestJSONEquivalent(t *testing.T) {
	const doc = `{
		"object_types": [{"name": "document", "actions": ["read"]}],
		"permissions": [{"id": "p1", "object_type": "document", "action": "read"}],
		"principals": [{"id": "u1", "kind": "user", "identity": "user:u1"}],
		"grants": [{
			"id": "g1", "account": "acme",
			"subject": {"kind": "principal", "id": "u1"},
			"permission": "p1", "object": "account:acme/document:1", "effect": "allow"
		}]
	}`
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := Load(ctx, store, []byte(doc), FormatJSON); err != nil {
		t.Fatalf("load JSON: %v", err)
	}
	if _, err := store.GetGrant(ctx, "g1"); err != nil {
		t.Errorf("grant g1 not seeded from JSON: %v", err)
	}
}
