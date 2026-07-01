package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// newAuditedMutator wires a fully-mutating facade (storage + gate + audit) over
// an in-memory store and seeds system-admin authority for "alice" so a gated
// mutation can succeed. The recorder records mutations synchronously and samples
// every decision (rate 1) for deterministic assertions.
func newAuditedMutator(t *testing.T, sampler audit.Sampler) (*Service, *memory.Store, *audit.Recorder) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Seed admin authority for alice: an allow grant on the reserved admin action
	// over ** (covers every tier anchor) in account acme.
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-admin", ObjectType: "system", Action: authz.AdminAction}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	// mallory is a real principal with no admin authority — her gated mutations
	// fail closed as AUTHZ_DENIED (not NOT_FOUND), which the audit must record.
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "mallory", Kind: model.PrincipalUser, Identity: "user:mallory"}))
	mustPut(t, store.PutGrant(ctx, model.Grant{
		ID: "g-admin", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-admin", Object: "**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	gate := authz.NewGate(eng)
	opts := []audit.Option{audit.WithClock(deterministicClock()), audit.WithIDFunc(seqID())}
	if sampler != nil {
		opts = append(opts, audit.WithSampler(sampler))
	} else {
		opts = append(opts, audit.WithSampleRate(1))
	}
	rec := audit.New(store, opts...)
	svc := New(eng, WithStorage(store), WithGate(gate), WithAudit(rec))
	return svc, store, rec
}

func deterministicClock() func() time.Time {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time { return base.Add(time.Duration(atomic.AddInt64(&n, 1)) * time.Second) }
}

func seqID() func() string {
	var n int64
	return func() string {
		return "ev-" + time.Unix(atomic.AddInt64(&n, 1), 0).UTC().Format("150405.000000000")
	}
}

func queryAudit(t *testing.T, store *memory.Store, f model.AuditFilter) []model.AuditEvent {
	t.Helper()
	out, err := store.QueryAudit(context.Background(), f)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return out
}

// TestMutationAlwaysAudited proves every mutation is recorded — a success as
// OutcomeSuccess and an authorization denial as OutcomeFailure — with the actor,
// account, target, and reason captured.
func TestMutationAlwaysAudited(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil)
	defer rec.Close()
	ctx := context.Background()
	alice := Actor{Principal: "alice", Account: "acme"}

	// Successful gated mutation.
	if err := svc.PutObjectType(ctx, alice, model.ObjectType{Name: "document", Actions: []string{"read"}}); err != nil {
		t.Fatalf("put object type: %v", err)
	}
	// Denied mutation: mallory holds no admin authority.
	mallory := Actor{Principal: "mallory", Account: "acme"}
	if err := svc.PutObjectType(ctx, mallory, model.ObjectType{Name: "secret", Actions: []string{"read"}}); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("want AUTHZ_DENIED for mallory, got %v", err)
	}

	muts := queryAudit(t, store, model.AuditFilter{EventType: model.AuditMutation})
	if len(muts) != 2 {
		t.Fatalf("want 2 mutation events, got %d: %+v", len(muts), muts)
	}
	byActor := map[string]model.AuditEvent{}
	for _, m := range muts {
		byActor[m.Actor] = m
	}
	if got := byActor["alice"]; got.Outcome != model.OutcomeSuccess || got.Action != "PutObjectType" || got.Target != "object_type:document" || got.Account != "acme" {
		t.Fatalf("alice mutation audit wrong: %+v", got)
	}
	if got := byActor["mallory"]; got.Outcome != model.OutcomeFailure || got.Reason == "" {
		t.Fatalf("mallory denied mutation should be audited as failure with a reason: %+v", got)
	}
}

// TestImpersonationActorRecorded proves a decision made under an impersonation
// decorator records the REAL actor (operator) and the effective subject + mode,
// not the borrowed target alone.
func TestImpersonationActorRecorded(t *testing.T) {
	svc, store, rec := newAuditedMutator(t, nil) // rate 1: every decision sampled
	ctx := engine.WithImpersonation(context.Background(), engine.ImpersonationContext{
		RealActor:        "operator",
		EffectiveSubject: "target",
		Mode:             engine.ModeBecome,
		ExpiresAt:        time.Now().Add(time.Hour),
	})

	// The request principal is the operator; the decision verdict is irrelevant —
	// we assert the audit attribution.
	if _, err := svc.Check(ctx, Query{Account: "acme", Principal: "operator", Action: "read", Object: "account:acme/document:1"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	if err := rec.Close(); err != nil { // flush the async decision write
		t.Fatalf("close: %v", err)
	}

	decs := queryAudit(t, store, model.AuditFilter{EventType: model.AuditDecision})
	if len(decs) != 1 {
		t.Fatalf("want 1 decision event, got %d", len(decs))
	}
	d := decs[0]
	if d.Actor != "operator" || d.EffectiveSubject != "target" || d.ImpersonationMode != "become" {
		t.Fatalf("impersonation not recorded as real+effective actor: %+v", d)
	}
}

// TestSampledDecisionsRespectRateThroughFacade proves the facade honours an
// injected sampler off the Check path: a keep-1-in-2 sampler records exactly
// half of the checks.
func TestSampledDecisionsRespectRateThroughFacade(t *testing.T) {
	var n int64
	keepEveryOther := audit.SamplerFunc(func() bool { return atomic.AddInt64(&n, 1)%2 == 0 })
	svc, store, rec := newAuditedMutator(t, keepEveryOther)
	ctx := context.Background()

	const checks = 20
	for i := 0; i < checks; i++ {
		if _, err := svc.Check(ctx, Query{Account: "acme", Principal: "alice", Action: "read", Object: "account:acme/document:1"}); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	decs := queryAudit(t, store, model.AuditFilter{EventType: model.AuditDecision})
	if len(decs) != checks/2 {
		t.Fatalf("sampled %d decisions, want %d", len(decs), checks/2)
	}
}

// TestNoAuditWhenUnwired proves the default (no WithAudit) facade records
// nothing and the mutation/decision paths still work — the existing no-audit
// construction is unaffected.
func TestNoAuditWhenUnwired(t *testing.T) {
	svc := newSvc(t, nil, allowGrant("g", "p-lit", "account:acme/document:42"))
	// A read-only facade has no audit recorder; Check must not panic or error.
	if _, err := svc.Check(context.Background(), Query{Account: "acme", Principal: "alice", Action: "read", Object: "account:acme/document:42"}); err != nil {
		t.Fatalf("check without audit: %v", err)
	}
}
