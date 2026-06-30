package service

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

const acct = "acme"

// docProvider is a minimal ObjectProvider over fixed document identities so the
// facade's Enumerate exercises the real engine -> scope -> provider stack.
type docProvider struct{ ids []identity.Identity }

func (p docProvider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	return provider.Metadata{"id": id.String()}, nil
}
func (p docProvider) List(_ context.Context) ([]provider.Object, error) {
	out := make([]provider.Object, len(p.ids))
	for i, o := range p.ids {
		out[i] = provider.Object{ID: o}
	}
	return out, nil
}
func (p docProvider) Query(_ context.Context, f provider.Filter) ([]provider.Object, error) {
	out := make([]provider.Object, 0, len(p.ids))
	for _, o := range p.ids {
		if f.Pattern != nil && !f.Pattern.Matches(o) {
			continue
		}
		out = append(out, provider.Object{ID: o})
	}
	return out, nil
}

// newSvc builds a scoped service over an in-memory store seeded with one
// document type, one implicit read permission, alice, and the supplied grants.
// docIDs back the provider lister.
func newSvc(t *testing.T, docIDs []string, grants ...model.Grant) *Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-impl", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit}))
	mustPut(t, store.PutPermission(ctx, model.Permission{ID: "p-lit", ObjectType: "document", Action: "read"}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	for _, g := range grants {
		mustPut(t, store.PutGrant(ctx, g))
	}

	ids := make([]identity.Identity, len(docIDs))
	for i, s := range docIDs {
		ids[i] = identity.MustParse(s)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", docProvider{ids: ids})

	eng := engine.New(store, engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}))
	return New(eng)
}

func mustPut(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func allowGrant(id, permID, object string) model.Grant {
	return model.Grant{
		ID: id, AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permID, Object: object, Effect: model.EffectAllow,
	}
}

// --- Check: existing fail-closed contract still holds through the facade ---

func TestService_CheckFailClosed(t *testing.T) {
	svc := newSvc(t, nil, allowGrant("g", "p-lit", "account:acme/document:42"))

	// Allow passes through.
	res, err := svc.Check(context.Background(), Query{Account: acct, Principal: "alice", Action: "read", Object: "account:acme/document:42"})
	if err != nil || !res.Allow {
		t.Fatalf("want allow/no-error, got allow=%v err=%v", res.Allow, err)
	}
	// Unknown principal: fail-closed deny, no error.
	res, err = svc.Check(context.Background(), Query{Account: acct, Principal: "ghost", Action: "read", Object: "account:acme/document:42"})
	if err != nil {
		t.Fatalf("operational failure must fold to a deny, got error %v", err)
	}
	if res.Allow {
		t.Fatalf("unknown principal must fail closed to deny")
	}
	// Malformed object: returned as an error (a usage error, not a deny).
	if _, err := svc.Check(context.Background(), Query{Account: acct, Principal: "alice", Action: "read", Object: "not-valid"}); aerr.CodeOf(err) != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("malformed object should surface APERTURE_IDENTITY_INVALID, got %q", aerr.CodeOf(err))
	}
}

// --- Enumerate through the facade ---

func TestService_Enumerate(t *testing.T) {
	svc := newSvc(t, []string{"account:acme/document:1", "account:acme/document:2"},
		allowGrant("g-all", "p-impl", "account:acme/**"))

	ids, err := svc.Enumerate(context.Background(), EnumerateQuery{Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**"})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 ids, got %v", ids)
	}
}

// --- Explain through the facade ---

func TestService_Explain(t *testing.T) {
	svc := newSvc(t, nil, allowGrant("g", "p-lit", "account:acme/document:42"))

	tr, err := svc.Explain(context.Background(), Query{Account: acct, Principal: "alice", Action: "read", Object: "account:acme/document:42"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("explain should allow, got %s", tr.String())
	}
	if len(tr.Decision.DecidingGrantIDs) != 1 || tr.Decision.DecidingGrantIDs[0] != "g" {
		t.Fatalf("deciding = %v, want [g]", tr.Decision.DecidingGrantIDs)
	}
}

// --- Batch alignment + partial errors ---

func TestService_CheckBatchAlignment(t *testing.T) {
	svc := newSvc(t, nil, allowGrant("g", "p-lit", "account:acme/document:42"))

	qs := []Query{
		{Account: acct, Principal: "alice", Action: "read", Object: "account:acme/document:42"}, // allow
		{Account: acct, Principal: "ghost", Action: "read", Object: "account:acme/document:42"}, // fold to deny (no error)
		{Account: acct, Principal: "alice", Action: "read", Object: "bad object"},               // input error
	}
	out := svc.CheckBatch(context.Background(), qs)
	if len(out) != len(qs) {
		t.Fatalf("length mismatch: %d vs %d", len(out), len(qs))
	}
	if out[0].Err != nil || !out[0].Result.Allow {
		t.Fatalf("item 0 want allow, got allow=%v err=%v", out[0].Result.Allow, out[0].Err)
	}
	// Operational failure folds to a deny Result with no item error (Check policy).
	if out[1].Err != nil || out[1].Result.Allow {
		t.Fatalf("item 1 want folded deny/no-error, got allow=%v err=%v", out[1].Result.Allow, out[1].Err)
	}
	if aerr.CodeOf(out[2].Err) != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("item 2 want APERTURE_IDENTITY_INVALID, got %q", aerr.CodeOf(out[2].Err))
	}
}

func TestService_EnumerateBatchAlignment(t *testing.T) {
	svc := newSvc(t, []string{"account:acme/document:1"}, allowGrant("g-all", "p-impl", "account:acme/**"))

	qs := []EnumerateQuery{
		{Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**"},
		{Account: acct, Principal: "alice", Action: "read", Pattern: ""}, // invalid
	}
	out := svc.EnumerateBatch(context.Background(), qs)
	if len(out) != 2 {
		t.Fatalf("length %d, want 2", len(out))
	}
	if out[0].Err != nil || len(out[0].Result) != 1 {
		t.Fatalf("item 0 want 1 id/no-error, got %v err=%v", out[0].Result, out[0].Err)
	}
	if aerr.CodeOf(out[1].Err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("item 1 want APERTURE_INVALID_INPUT, got %q", aerr.CodeOf(out[1].Err))
	}
}

func TestService_ExplainBatchAlignment(t *testing.T) {
	svc := newSvc(t, nil, allowGrant("g", "p-lit", "account:acme/document:42"))

	qs := []Query{
		{Account: acct, Principal: "alice", Action: "read", Object: "account:acme/document:42"},
		{Account: acct, Principal: "alice", Action: "read", Object: "bad object"},
	}
	out := svc.ExplainBatch(context.Background(), qs)
	if len(out) != 2 {
		t.Fatalf("length %d, want 2", len(out))
	}
	if out[0].Err != nil || !out[0].Result.Decision.Allow {
		t.Fatalf("item 0 want allow trace, got err=%v", out[0].Err)
	}
	if aerr.CodeOf(out[1].Err) != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("item 1 want APERTURE_IDENTITY_INVALID, got %q", aerr.CodeOf(out[1].Err))
	}
}

func TestService_BatchNilYieldsNil(t *testing.T) {
	svc := newSvc(t, nil)
	if svc.CheckBatch(context.Background(), nil) != nil {
		t.Fatal("CheckBatch(nil) should be nil")
	}
	if svc.EnumerateBatch(context.Background(), nil) != nil {
		t.Fatal("EnumerateBatch(nil) should be nil")
	}
	if svc.ExplainBatch(context.Background(), nil) != nil {
		t.Fatal("ExplainBatch(nil) should be nil")
	}
}
