package server_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"net/http/httptest"

	"github.com/twitchtv/twirp"
)

// newAuditTestServer boots the Twirp surface exactly like newTestServer but wires
// the audit recorder (sink = the store, sampling every decision) so the E6-S4
// audit viewer's QueryAudit RPC has a populated trail to read. The recorder's
// background writer is flushed on cleanup.
func newAuditTestServer(t *testing.T) (*httptest.Server, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))

	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "root", Kind: model.PrincipalUser, Identity: "user:root"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "root", AccountID: acct}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-admin", ObjectType: "system", Action: authz.AdminAction}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-root-admin", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "root"},
		PermissionID: "perm-admin", Object: "**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	rec := audit.New(store, audit.WithSampleRate(1))
	t.Cleanup(func() { _ = rec.Close() })
	svc := service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithDelegation(delegation.New(store, eng)),
		service.WithImpersonation(impersonation.New(store, eng)),
		service.WithAudit(rec),
	)
	handler := server.Authenticate(auth.NewDev(), server.New(svc))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, store
}

// TestWhatIfSmokeExplain is the UI proxy for the what-if simulator: it drives the
// exact open RPCs the whatif.js component issues — Check + Explain — for a known
// ALLOW (root over the admin anchor) and a known DENY (alice, no admin), and
// asserts the verdict plus a usable trace (deciding grant on allow, no deciding
// grant on deny) come back, mutating nothing.
func TestWhatIfSmokeExplain(t *testing.T) {
	srv, _ := newAuditTestServer(t)
	c := client(srv)
	ctx := context.Background() // decision RPCs are open, no bearer needed

	// Known allow.
	q := &rpc.CheckRequest{Account: acct, Principal: "root", Action: authz.AdminAction, Object: "system:schema"}
	dec, err := c.Check(ctx, q)
	if err != nil {
		t.Fatalf("Check (allow): %v", err)
	}
	if !dec.Allow {
		t.Fatalf("want allow for root admin, got deny: %s", dec.Reason)
	}
	exp, err := c.Explain(ctx, q)
	if err != nil {
		t.Fatalf("Explain (allow): %v", err)
	}
	var tr engine.Trace
	if err := json.Unmarshal([]byte(exp.TraceJson), &tr); err != nil {
		t.Fatalf("unmarshal allow trace: %v", err)
	}
	if !tr.Decision.Allow || len(tr.Decision.DecidingGrantIDs) == 0 {
		t.Fatalf("allow trace missing verdict/deciding grants: %+v", tr.Decision)
	}
	if len(tr.Considered) == 0 {
		t.Fatal("allow trace considered no grants")
	}

	// Known deny: alice holds no admin authority.
	qd := &rpc.CheckRequest{Account: acct, Principal: "alice", Action: authz.AdminAction, Object: "system:schema"}
	decD, err := c.Check(ctx, qd)
	if err != nil {
		t.Fatalf("Check (deny): %v", err)
	}
	if decD.Allow {
		t.Fatal("want deny for alice admin, got allow")
	}
	expD, err := c.Explain(ctx, qd)
	if err != nil {
		t.Fatalf("Explain (deny): %v", err)
	}
	var trD engine.Trace
	if err := json.Unmarshal([]byte(expD.TraceJson), &trD); err != nil {
		t.Fatalf("unmarshal deny trace: %v", err)
	}
	if trD.Decision.Allow {
		t.Fatalf("deny trace should not allow: %+v", trD.Decision)
	}
}

// TestAuditSmokeQuery is the UI proxy for the audit viewer: it performs a mutation
// as the admin (PutRole), then drives the QueryAudit RPC the audit.js component
// issues and asserts the recorded mutation event appears attributed to the real
// actor. It also confirms the tier gate — a non-admin (alice) is denied the read.
func TestAuditSmokeQuery(t *testing.T) {
	srv, _ := newAuditTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	// A mutation the trail must record (always-on, synchronous).
	role := model.Role{ID: "auditor", Name: "Auditor"}
	if _, err := c.PutRole(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, role)}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}

	// The audit viewer queries mutation events; the PutRole must be present with
	// the real actor.
	resp, err := c.QueryAudit(ctx, &rpc.QueryAuditRequest{
		Actor:     actor,
		EventType: string(model.AuditMutation),
	})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	var found *model.AuditEvent
	for _, s := range resp.EventsJson {
		var ev model.AuditEvent
		if err := json.Unmarshal([]byte(s), &ev); err != nil {
			t.Fatalf("unmarshal audit event: %v", err)
		}
		if ev.Action == "PutRole" {
			found = &ev
			break
		}
	}
	if found == nil {
		t.Fatal("PutRole mutation not found in the audit trail")
	}
	if found.Actor != "root" {
		t.Fatalf("audit event actor = %q, want root", found.Actor)
	}
	if found.EventType != model.AuditMutation || found.Outcome != model.OutcomeSuccess {
		t.Fatalf("audit event mis-typed: %+v", found)
	}

	// Tier gate: a non-admin querying the whole trail is denied.
	aliceCtx := asPrincipal(context.Background(), t, "alice")
	if _, err := c.QueryAudit(aliceCtx, &rpc.QueryAuditRequest{
		Actor:     &rpc.Actor{Principal: "alice", Account: acct},
		EventType: string(model.AuditMutation),
	}); err == nil {
		t.Fatal("non-admin audit read should be denied")
	} else if te, ok := err.(twirp.Error); !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("want twirp PermissionDenied for non-admin audit read, got %v", err)
	}
}

// TestPortabilitySmokeExport is the UI proxy for the export screen: the admin
// exports the model (Export) and the returned document parses into the seed
// document the import diff compares against, carrying the seeded entities.
func TestPortabilitySmokeExport(t *testing.T) {
	srv, _ := newAuditTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")

	resp, err := c.Export(ctx, &rpc.ExportRequest{Actor: &rpc.Actor{Principal: "root", Account: acct}})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	doc, err := seed.Parse([]byte(resp.DocumentJson), seed.FormatJSON)
	if err != nil {
		t.Fatalf("parse exported document: %v", err)
	}
	if len(doc.Principals) == 0 || len(doc.Grants) == 0 {
		t.Fatalf("exported document missing seeded entities: %d principals, %d grants", len(doc.Principals), len(doc.Grants))
	}
}
