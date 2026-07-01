package service

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
)

// newSeededService builds a read+simulate facade over the embedded example model.
func newSeededService(t *testing.T) (*Service, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(engine.New(store), WithStorage(store)), store
}

const (
	simAccount   = seed.ExampleAccount
	simPrincipal = "alice"
	simAction    = "read"
	// nimbus sits outside the atlas-scoped grants, so no grant covers it: a real
	// Check default-denies, and a hypothetical allow is what flips it.
	simUncovered = "account:acme/project:nimbus/document:1"
)

// TestSimulateOverlayChangesDecisionWithoutWriting is the core what-if assertion:
// a hypothetical allow grant flips a decision under Simulate, yet the live model
// is untouched (a real Check still denies, and the stored grant set is unchanged).
func TestSimulateOverlayChangesDecisionWithoutWriting(t *testing.T) {
	ctx := context.Background()
	svc, store := newSeededService(t)

	q := Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered}

	// Baseline: the uncovered object default-denies.
	base, err := svc.Check(ctx, q)
	if err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	if base.Allow {
		t.Fatal("expected baseline default-deny on the uncovered object")
	}

	grantsBefore, _ := store.ListGrants(ctx, simAccount)

	// A hypothetical allow on the uncovered object flips the default-deny to allow.
	ov := Overlay{Grants: []model.Grant{{
		ID:           "what-if-unseal",
		AccountID:    simAccount,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: simPrincipal},
		PermissionID: "perm-doc-read",
		Object:       simUncovered,
		Effect:       model.EffectAllow,
	}}}

	sim, err := svc.Simulate(ctx, ov, q)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if !sim.Allow {
		t.Errorf("expected the hypothetical allow to flip the decision; got deny: %s", sim.Reason)
	}

	// The live model must be untouched: a real Check still denies and the grant set
	// is the same length.
	after, err := svc.Check(ctx, q)
	if err != nil {
		t.Fatalf("post-sim check: %v", err)
	}
	if after.Allow {
		t.Error("simulate leaked into the live model: real Check now allows")
	}
	grantsAfter, _ := store.ListGrants(ctx, simAccount)
	if len(grantsAfter) != len(grantsBefore) {
		t.Errorf("simulate persisted a grant: %d -> %d", len(grantsBefore), len(grantsAfter))
	}
}

// TestSimulateExplainTracesOverlay asserts SimulateExplain returns a trace whose
// deciding grant is the hypothetical one.
func TestSimulateExplainTracesOverlay(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSeededService(t)

	ov := Overlay{Grants: []model.Grant{{
		ID:           "what-if-unseal",
		AccountID:    simAccount,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: simPrincipal},
		PermissionID: "perm-doc-read",
		Object:       simUncovered,
		Effect:       model.EffectAllow,
	}}}
	q := Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered}

	tr, err := svc.SimulateExplain(ctx, ov, q)
	if err != nil {
		t.Fatalf("simulate explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("expected allow under overlay, got: %s", tr.Decision.Reason)
	}
	if !containsGrant(tr, "what-if-unseal") {
		t.Errorf("expected the hypothetical grant among considered grants; trace: %+v", tr.Considered)
	}
}

// TestSimulateReadOnlyFacadeUnimplemented asserts Simulate requires the entity
// surface (a read-only facade with no storage cannot overlay).
func TestSimulateReadOnlyFacadeUnimplemented(t *testing.T) {
	ctx := context.Background()
	svc, store := newSeededService(t)
	bare := New(svc.eng) // no WithStorage
	_ = store

	_, err := bare.Simulate(ctx, Overlay{}, Query{Account: simAccount, Principal: simPrincipal, Action: simAction, Object: simUncovered})
	if err == nil {
		t.Fatal("expected APERTURE_UNIMPLEMENTED from a storage-less facade")
	}
}

func containsGrant(tr engine.Trace, id string) bool {
	for _, ge := range tr.Considered {
		if ge.GrantID == id {
			return true
		}
	}
	return false
}
