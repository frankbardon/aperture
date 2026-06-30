package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// scopeFixture wires an in-memory store and an engine with scope resolution
// enabled, plus one "document" object type. Permissions are seeded per test with
// the scope strategy they exercise.
type scopeFixture struct {
	t     *testing.T
	store *memory.Store
	eng   *Engine
}

func newScopeFixture(t *testing.T, deps ...ScopeDeps) *scopeFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.PutObjectType(ctx, model.ObjectType{
		Name:    "document",
		Actions: []string{"read", "write"},
	}); err != nil {
		t.Fatalf("seed object type: %v", err)
	}
	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), deps...))
	return &scopeFixture{t: t, store: store, eng: eng}
}

func (f *scopeFixture) perm(id, strategy string) {
	f.t.Helper()
	if err := f.store.PutPermission(context.Background(), model.Permission{
		ID:            id,
		ObjectType:    "document",
		Action:        "read",
		ScopeStrategy: strategy,
	}); err != nil {
		f.t.Fatalf("put permission %s: %v", id, err)
	}
}

func (f *scopeFixture) principal(id string) {
	f.t.Helper()
	if err := f.store.PutPrincipal(context.Background(), model.Principal{
		ID: id, Kind: model.PrincipalUser, Identity: "user:" + id,
	}); err != nil {
		f.t.Fatalf("put principal %s: %v", id, err)
	}
}

func (f *scopeFixture) grant(id string, effect model.Effect, permID, object string) {
	f.t.Helper()
	if err := f.store.PutGrant(context.Background(), model.Grant{
		ID:           id,
		AccountID:    acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permID,
		Object:       object,
		Effect:       effect,
	}); err != nil {
		f.t.Fatalf("put grant %s: %v", id, err)
	}
}

func (f *scopeFixture) check(object string) Decision {
	f.t.Helper()
	d, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: object,
	})
	if err != nil {
		f.t.Fatalf("Check(%s): unexpected error: %v", object, err)
	}
	return d
}

// Scope resolution enabled, but an empty (literal) strategy keeps exact E1
// behaviour: pure pattern match.
func TestScope_LiteralDefaultPreservesE1(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-lit", "") // empty == literal
	f.grant("g1", model.EffectAllow, "p-lit", "account:acme/document:*")

	if d := f.check("account:acme/document:42"); !d.Allow {
		t.Fatalf("literal allow should authorize a matching object (%s)", d.Reason)
	}
	if d := f.check("account:acme/project:x/document:42"); d.Allow {
		t.Fatalf("literal pattern must not match a deeper path")
	}
}

func TestScope_Implicit(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g1", model.EffectAllow, "p-impl", "account:acme/**")

	// Every document under acme is covered.
	if d := f.check("account:acme/document:1"); !d.Allow {
		t.Fatalf("implicit should allow any document under acme (%s)", d.Reason)
	}
	if d := f.check("account:acme/project:atlas/document:9"); !d.Allow {
		t.Fatalf("implicit should allow a nested document (%s)", d.Reason)
	}
	// A non-document terminal is not of the type → default deny.
	if d := f.check("account:acme/project:atlas"); d.Allow {
		t.Fatalf("implicit document scope must not cover a project terminal")
	}
}

func TestScope_InclusiveList(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-inc", "inclusive;ids=account:acme/document:42,account:acme/document:99")
	f.grant("g1", model.EffectAllow, "p-inc", "account:acme/**")

	if d := f.check("account:acme/document:42"); !d.Allow {
		t.Fatalf("inclusive should allow a listed object (%s)", d.Reason)
	}
	if d := f.check("account:acme/document:7"); d.Allow {
		t.Fatalf("inclusive must deny an unlisted object")
	}
}

func TestScope_ExclusiveList(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-exc", "exclusive;ids=account:acme/document:7")
	f.grant("g1", model.EffectAllow, "p-exc", "account:acme/**")

	if d := f.check("account:acme/document:1"); !d.Allow {
		t.Fatalf("exclusive should allow a non-excluded document (%s)", d.Reason)
	}
	if d := f.check("account:acme/document:7"); d.Allow {
		t.Fatalf("exclusive must deny the excluded document")
	}
}

// Deny-overrides interaction: an implicit allow and an inclusive deny over the
// SAME pattern (equal specificity). The deny only applies to its listed member,
// so that member is denied while the rest of the implicit set is allowed.
func TestScope_DenyOverridesAtEqualSpecificity(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.perm("p-inc", "inclusive;ids=account:acme/document:42")
	f.grant("allow-all", model.EffectAllow, "p-impl", "account:acme/**")
	f.grant("deny-42", model.EffectDeny, "p-inc", "account:acme/**")

	// document:42 is in both the implicit allow set and the inclusive deny set,
	// at equal specificity → deny wins.
	d := f.check("account:acme/document:42")
	if d.Allow {
		t.Fatalf("equal-specificity inclusive deny must override the implicit allow (%s)", d.Reason)
	}
	if len(d.DecidingGrantIDs) != 1 || d.DecidingGrantIDs[0] != "deny-42" {
		t.Fatalf("deciding = %v, want [deny-42]", d.DecidingGrantIDs)
	}
	// document:7 is only in the implicit allow set (the deny does not contain it).
	if d := f.check("account:acme/document:7"); !d.Allow {
		t.Fatalf("an object outside the inclusive deny set must remain allowed (%s)", d.Reason)
	}
}

// Specificity tiebreak interaction: a broad implicit deny carved out by a
// strictly more-specific inclusive allow. The carve-out only applies to its
// listed member.
func TestScope_SpecificInclusiveAllowCarvesBroadImplicitDeny(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.perm("p-inc", "inclusive;ids=account:acme/document:42")
	// Broad deny over all acme documents; specific allow over the document:* tier.
	f.grant("deny-acme", model.EffectDeny, "p-impl", "account:acme/**")
	f.grant("allow-42", model.EffectAllow, "p-inc", "account:acme/document:*")

	// document:42: the more-specific inclusive allow wins over the broad deny.
	if d := f.check("account:acme/document:42"); !d.Allow {
		t.Fatalf("more-specific inclusive allow must carve out the broad deny (%s)", d.Reason)
	}
	// document:7: the inclusive allow does not contain it, so the broad deny stands.
	if d := f.check("account:acme/document:7"); d.Allow {
		t.Fatalf("a non-listed object must remain denied by the broad implicit deny")
	}
}

// A custom strategy registered by host code is consulted by the engine.
func TestScope_CustomStrategy(t *testing.T) {
	reg := scope.DefaultRegistry()
	// "even" covers documents whose terminal id is an even single digit.
	reg.MustRegister("even", func(gc scope.GrantContext, deps scope.Deps) (scope.ScopeResolver, error) {
		return evenResolver{gc: gc}, nil
	})
	store := memory.New()
	ctx := context.Background()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustPut(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustPut(store.PutPermission(ctx, model.Permission{ID: "p-even", ObjectType: "document", Action: "read", ScopeStrategy: "even"}))
	mustPut(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(store.PutGrant(ctx, model.Grant{
		ID: "g1", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-even", Object: "account:acme/**", Effect: model.EffectAllow,
	}))
	eng := New(store, WithScopeResolution(reg))

	chk := func(obj string) bool {
		d, err := eng.Check(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: obj})
		if err != nil {
			t.Fatalf("Check(%s): %v", obj, err)
		}
		return d.Allow
	}
	if !chk("account:acme/document:2") {
		t.Errorf("custom even strategy should allow document:2")
	}
	if chk("account:acme/document:3") {
		t.Errorf("custom even strategy should deny document:3")
	}
}

// evenResolver is a trivial custom strategy for the registration test.
type evenResolver struct{ gc scope.GrantContext }

func (r evenResolver) Contains(_ context.Context, object identity.Identity) (bool, error) {
	if !r.gc.Pattern.Matches(object) {
		return false, nil
	}
	segs := object.Segments()
	last := segs[len(segs)-1]
	return len(last.ID) == 1 && (last.ID[0]-'0')%2 == 0, nil
}

func (r evenResolver) Members(context.Context, identity.Pattern) ([]identity.Identity, error) {
	return nil, nil
}

// Unknown strategy: Check surfaces a coded error (a non-decision).
func TestScope_UnknownStrategyErrors(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-bad", "no-such-strategy")
	f.grant("g1", model.EffectAllow, "p-bad", "account:acme/**")

	_, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_UNKNOWN_STRATEGY {
		t.Fatalf("code = %q, want APERTURE_SCOPE_UNKNOWN_STRATEGY", code)
	}
}

// A malformed strategy spec (inclusive with no ids) surfaces APERTURE_SCOPE_INVALID.
func TestScope_MisconfiguredStrategyErrors(t *testing.T) {
	f := newScopeFixture(t)
	f.principal("alice")
	f.perm("p-bad", "inclusive") // no ids, no rule
	f.grant("g1", model.EffectAllow, "p-bad", "account:acme/**")

	_, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("code = %q, want APERTURE_SCOPE_INVALID", code)
	}
}
