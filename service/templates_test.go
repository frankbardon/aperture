package service

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// seedTemplate writes a template straight to the store (bypassing the gate) so a
// test can set up provisioning fixtures without first minting admin authority.
func seedTemplate(t *testing.T, store *memory.Store, tmpl model.Template) {
	t.Helper()
	if err := store.PutTemplate(context.Background(), tmpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}
}

func memberTemplate(version int) model.Template {
	return model.Template{
		Name:    "member",
		Version: version,
		Params: []model.TemplateParam{
			{Name: "account", Type: model.ParamSegment},
			{Name: "project", Type: model.ParamSegment},
			{Name: "who", Type: model.ParamSegment},
		},
		Grants: []model.TemplateGrant{
			{
				Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "${who}"},
				PermissionID: "p-read",
				Object:       "account:${account}/project:${project}/**",
				Effect:       model.EffectAllow,
			},
			{
				Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "${who}"},
				PermissionID: "p-write",
				Object:       "account:${account}/project:${project}/document:*",
				Effect:       model.EffectAllow,
			},
		},
	}
}

func memberApply() model.TemplateApplication {
	return model.TemplateApplication{
		Name:    "member",
		Account: "acme",
		Params:  map[string]string{"account": "acme", "project": "atlas", "who": "alice"},
	}
}

// TestApplyTemplate_TransactionalAndAudited proves an apply expands a template
// into concrete grants, applies them, and records exactly ONE audit event (not
// one per grant) carrying the template name+version and the resolved parameters.
func TestApplyTemplate_TransactionalAndAudited(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	seedTemplate(t, store, memberTemplate(1))
	alice := Actor{Principal: "alice", Account: "acme"}

	applied, err := svc.ApplyTemplate(ctx, alice, memberApply())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("want 2 applied grants, got %d", len(applied))
	}
	// Both expanded grants persisted, substituted and account-stamped.
	for _, g := range applied {
		got, err := store.GetGrant(ctx, g.ID)
		if err != nil {
			t.Fatalf("expanded grant %s not persisted: %v", g.ID, err)
		}
		if got.AccountID != "acme" || got.Subject.ID != "alice" {
			t.Fatalf("expanded grant not stamped/substituted: %+v", got)
		}
	}

	// Exactly ONE apply audit event, not one per expanded grant.
	muts := queryAudit(t, store, model.AuditFilter{EventType: model.AuditMutation})
	applies := 0
	for _, m := range muts {
		if m.Action == "ApplyTemplate" {
			applies++
			if m.Outcome != model.OutcomeSuccess || m.Target != "template:member:v1" {
				t.Fatalf("apply audit wrong: %+v", m)
			}
			if m.Details["template"] != "member" {
				t.Fatalf("apply audit missing template detail: %+v", m.Details)
			}
			if _, ok := m.Details["grants"]; !ok {
				t.Fatalf("apply audit missing grant ids: %+v", m.Details)
			}
		}
		// No per-grant PutGrant events should be emitted by the apply.
		if m.Action == "PutGrant" {
			t.Fatalf("apply emitted a per-grant PutGrant audit event: %+v", m)
		}
	}
	if applies != 1 {
		t.Fatalf("want exactly 1 ApplyTemplate event, got %d", applies)
	}
}

// TestApplyTemplate_VersionSelection proves apply picks the latest version by
// default and an explicit version when asked.
func TestApplyTemplate_VersionSelection(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	v1 := memberTemplate(1)
	v1.Grants = v1.Grants[:1] // v1 expands to ONE grant
	v2 := memberTemplate(2)   // v2 expands to TWO grants
	seedTemplate(t, store, v1)
	seedTemplate(t, store, v2)
	alice := Actor{Principal: "alice", Account: "acme"}

	// Latest (version 0) → v2 → two grants.
	latest, err := svc.ApplyTemplate(ctx, alice, memberApply())
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest apply = %d grants, err %v, want 2", len(latest), err)
	}

	// Explicit version 1 → one grant.
	app := memberApply()
	app.Version = 1
	app.GrantIDPrefix = "v1run"
	one, err := svc.ApplyTemplate(ctx, alice, app)
	if err != nil || len(one) != 1 {
		t.Fatalf("v1 apply = %d grants, err %v, want 1", len(one), err)
	}
}

// TestApplyTemplate_BadParamWritesNothing proves a parameter failure is caught
// before any grant is written (the expansion is all-or-nothing) and is audited
// as a failure.
func TestApplyTemplate_BadParamWritesNothing(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	seedTemplate(t, store, memberTemplate(1))
	alice := Actor{Principal: "alice", Account: "acme"}

	app := memberApply()
	delete(app.Params, "who") // missing required parameter
	_, err := svc.ApplyTemplate(ctx, alice, app)
	if aerr.CodeOf(err) != aerr.APERTURE_TEMPLATE_PARAM {
		t.Fatalf("want TEMPLATE_PARAM, got %v", err)
	}
	// No grants were written.
	grants, _ := store.ListGrants(ctx, "acme")
	for _, g := range grants {
		if g.PermissionID == "p-read" || g.PermissionID == "p-write" {
			t.Fatalf("a grant was written despite the param failure: %+v", g)
		}
	}
	// The failed apply is audited.
	muts := queryAudit(t, store, model.AuditFilter{EventType: model.AuditMutation})
	found := false
	for _, m := range muts {
		if m.Action == "ApplyTemplate" {
			found = true
			if m.Outcome != model.OutcomeFailure {
				t.Fatalf("failed apply not recorded as failure: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("failed apply was not audited")
	}
}

// TestApplyTemplate_Gated proves a non-admin cannot apply (account-tier gate) and
// nothing is written.
func TestApplyTemplate_Gated(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	seedTemplate(t, store, memberTemplate(1))
	mallory := Actor{Principal: "mallory", Account: "acme"}

	_, err := svc.ApplyTemplate(ctx, mallory, memberApply())
	if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("want AUTHZ_DENIED for non-admin apply, got %v", err)
	}
	grants, _ := store.ListGrants(ctx, "acme")
	if len(grants) != 1 { // only the seeded g-admin grant
		t.Fatalf("denied apply wrote grants: %+v", grants)
	}
}

// TestPutTemplate_SystemTierGated proves template definition needs system-admin.
func TestPutTemplate_SystemTierGated(t *testing.T) {
	svc, _, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	mallory := Actor{Principal: "mallory", Account: "acme"}
	if err := svc.PutTemplate(ctx, mallory, memberTemplate(1)); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("want AUTHZ_DENIED, got %v", err)
	}
	// alice (system-admin via **) succeeds.
	alice := Actor{Principal: "alice", Account: "acme"}
	if err := svc.PutTemplate(ctx, alice, memberTemplate(1)); err != nil {
		t.Fatalf("admin put template: %v", err)
	}
}

// TestBulkPutGrants_RoundTripAndRollback proves the bulk endpoint applies many
// grants atomically and rolls the WHOLE batch back on a mid-batch failure.
func TestBulkPutGrants_RoundTripAndRollback(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	mk := func(id string, effect model.Effect) model.Grant {
		return model.Grant{
			ID: id, AccountID: "acme",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-read", Object: "account:acme/**", Effect: effect,
		}
	}

	// Round trip: three valid grants all land.
	if err := svc.BulkPutGrants(ctx, alice, []model.Grant{
		mk("b1", model.EffectAllow), mk("b2", model.EffectAllow), mk("b3", model.EffectAllow),
	}); err != nil {
		t.Fatalf("bulk put: %v", err)
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		if _, err := store.GetGrant(ctx, id); err != nil {
			t.Fatalf("bulk grant %s missing: %v", id, err)
		}
	}

	// Rollback: a mid-batch invalid grant (bad effect) fails the whole call.
	err := svc.BulkPutGrants(ctx, alice, []model.Grant{
		mk("c1", model.EffectAllow),
		mk("c2", "maybe"), // invalid effect, but stamped to acme so it passes the gate
	})
	if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("want INVALID_INPUT from rolled-back bulk, got %v", err)
	}
	if _, e := store.GetGrant(ctx, "c1"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("c1 should have rolled back, got %v", e)
	}

	// Exactly one bulk audit event per call (two calls = two events).
	muts := queryAudit(t, store, model.AuditFilter{EventType: model.AuditMutation})
	bulk := 0
	for _, m := range muts {
		if m.Action == "BulkPutGrants" {
			bulk++
		}
	}
	if bulk != 2 {
		t.Fatalf("want 2 BulkPutGrants events, got %d", bulk)
	}
}

// TestBulkDeleteGrants_RoundTripAndRollback proves bulk revoke removes many
// grants atomically and rolls back when any id is unknown.
func TestBulkDeleteGrants_RoundTripAndRollback(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	for _, id := range []string{"d1", "d2", "d3"} {
		if err := store.PutGrant(ctx, model.Grant{
			ID: id, AccountID: "acme",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p", Object: "account:acme/**", Effect: model.EffectAllow,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Round trip: delete two; the third survives.
	if err := svc.BulkDeleteGrants(ctx, alice, []string{"d1", "d2"}); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if _, e := store.GetGrant(ctx, "d3"); e != nil {
		t.Fatalf("d3 wrongly removed: %v", e)
	}
	if _, e := store.GetGrant(ctx, "d1"); aerr.CodeOf(e) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("d1 not deleted: %v", e)
	}

	// Rollback: an unknown id fails the whole call; d3 must survive.
	err := svc.BulkDeleteGrants(ctx, alice, []string{"d3", "ghost"})
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("want NOT_FOUND from rolled-back bulk delete, got %v", err)
	}
	if _, e := store.GetGrant(ctx, "d3"); e != nil {
		t.Fatalf("d3 should have survived the rolled-back delete: %v", e)
	}
}

// TestProvisioningUnwired proves the read-only facade reports UNIMPLEMENTED for
// the new write surface rather than panicking.
func TestProvisioningUnwired(t *testing.T) {
	svc := newSvc(t, nil)
	ctx := context.Background()
	a := Actor{Principal: "alice", Account: "acme"}
	if aerr.CodeOf(svc.PutTemplate(ctx, a, memberTemplate(1))) != aerr.APERTURE_UNIMPLEMENTED {
		t.Fatal("PutTemplate on read-only facade should be UNIMPLEMENTED")
	}
	if _, err := svc.ApplyTemplate(ctx, a, memberApply()); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Fatal("ApplyTemplate on read-only facade should be UNIMPLEMENTED")
	}
	if err := svc.BulkPutGrants(ctx, a, nil); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Fatal("BulkPutGrants on read-only facade should be UNIMPLEMENTED")
	}
}
