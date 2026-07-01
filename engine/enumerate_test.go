package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// fakeProvider is a host ObjectProvider over a fixed set of document identities,
// used to back the scope ObjectLister so implicit/exclusive Enumerate can list
// "all objects of the type" without a real domain backend.
type fakeProvider struct{ ids []identity.Identity }

func newFakeProvider(t *testing.T, ids ...string) fakeProvider {
	t.Helper()
	out := make([]identity.Identity, len(ids))
	for i, s := range ids {
		id, err := identity.Parse(s)
		if err != nil {
			t.Fatalf("fake provider id %q: %v", s, err)
		}
		out[i] = id
	}
	return fakeProvider{ids: out}
}

func (p fakeProvider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	for _, o := range p.ids {
		if o.String() == id.String() {
			return provider.Metadata{"id": id.String()}, nil
		}
	}
	return nil, aerr.New(aerr.APERTURE_NOT_FOUND, "fake provider: absent object")
}

func (p fakeProvider) List(_ context.Context) ([]provider.Object, error) {
	out := make([]provider.Object, len(p.ids))
	for i, o := range p.ids {
		out[i] = provider.Object{ID: o, Metadata: provider.Metadata{"id": o.String()}}
	}
	return out, nil
}

func (p fakeProvider) Query(_ context.Context, f provider.Filter) ([]provider.Object, error) {
	out := make([]provider.Object, 0, len(p.ids))
	for _, o := range p.ids {
		if f.Pattern != nil && !f.Pattern.Matches(o) {
			continue
		}
		out = append(out, provider.Object{ID: o, Metadata: provider.Metadata{"id": o.String()}})
	}
	return out, nil
}

// enumFixture wires a scoped engine whose ObjectLister is a provider.Registry
// holding one document provider, so the full E2 stack (engine -> scope ->
// provider) is exercised end to end.
type enumFixture struct {
	t     *testing.T
	store *memory.Store
	eng   *Engine
}

func newEnumFixture(t *testing.T, docIDs ...string) *enumFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}); err != nil {
		t.Fatalf("seed object type: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", newFakeProvider(t, docIDs...))

	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}))
	return &enumFixture{t: t, store: store, eng: eng}
}

func (f *enumFixture) perm(id, strategy string) {
	f.t.Helper()
	if err := f.store.PutPermission(context.Background(), model.Permission{
		ID: id, ObjectType: "document", Action: "read", ScopeStrategy: strategy,
	}); err != nil {
		f.t.Fatalf("put permission %s: %v", id, err)
	}
}

func (f *enumFixture) principal(id string) {
	f.t.Helper()
	if err := f.store.PutPrincipal(context.Background(), model.Principal{
		ID: id, Kind: model.PrincipalUser, Identity: "user:" + id,
	}); err != nil {
		f.t.Fatalf("put principal %s: %v", id, err)
	}
}

func (f *enumFixture) grant(id string, effect model.Effect, permID, object string) {
	f.t.Helper()
	if err := f.store.PutGrant(context.Background(), model.Grant{
		ID: id, AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permID, Object: object, Effect: effect,
	}); err != nil {
		f.t.Fatalf("put grant %s: %v", id, err)
	}
}

func (f *enumFixture) enumerate(pattern string) []string {
	f.t.Helper()
	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: pattern,
	})
	if err != nil {
		f.t.Fatalf("Enumerate(%s): unexpected error: %v", pattern, err)
	}
	return ids
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// --- Acceptance: enumerate across the three scope strategies ---

func TestEnumerate_Implicit(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:2", "account:acme/document:3")
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g-all", model.EffectAllow, "p-impl", "account:acme/**")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:1", "account:acme/document:2", "account:acme/document:3"}
	if !sameSet(got, want) {
		t.Fatalf("implicit enumerate = %v, want %v", got, want)
	}
}

func TestEnumerate_InclusiveList(t *testing.T) {
	// Inclusive list path needs no lister; an empty provider proves it.
	f := newEnumFixture(t)
	f.principal("alice")
	f.perm("p-inc", "inclusive;ids=account:acme/document:42,account:acme/document:99")
	f.grant("g-inc", model.EffectAllow, "p-inc", "account:acme/**")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:42", "account:acme/document:99"}
	if !sameSet(got, want) {
		t.Fatalf("inclusive enumerate = %v, want %v", got, want)
	}
}

func TestEnumerate_ExclusiveList(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:7", "account:acme/document:9")
	f.principal("alice")
	f.perm("p-exc", "exclusive;ids=account:acme/document:7")
	f.grant("g-exc", model.EffectAllow, "p-exc", "account:acme/**")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:1", "account:acme/document:9"}
	if !sameSet(got, want) {
		t.Fatalf("exclusive enumerate = %v, want %v (document:7 must be excluded)", got, want)
	}
}

// --- Acceptance: enumerate NEVER returns a denied object ---

func TestEnumerate_DenyOverridesExcludesObject(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:2", "account:acme/document:3")
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.perm("p-inc", "inclusive;ids=account:acme/document:2")
	// Allow every document, but deny document:2 at equal specificity.
	f.grant("allow-all", model.EffectAllow, "p-impl", "account:acme/**")
	f.grant("deny-2", model.EffectDeny, "p-inc", "account:acme/**")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:1", "account:acme/document:3"}
	if !sameSet(got, want) {
		t.Fatalf("enumerate = %v, want %v (denied document:2 must never appear)", got, want)
	}
	for _, id := range got {
		if id == "account:acme/document:2" {
			t.Fatalf("enumerate returned a denied object %q", id)
		}
	}
}

// A broad implicit deny carved out by a more-specific inclusive allow: only the
// carve-out is enumerable.
func TestEnumerate_SpecificAllowCarvesBroadDeny(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:42")
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.perm("p-inc", "inclusive;ids=account:acme/document:42")
	f.grant("deny-all", model.EffectDeny, "p-impl", "account:acme/**")
	f.grant("allow-42", model.EffectAllow, "p-inc", "account:acme/document:*")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:42"}
	if !sameSet(got, want) {
		t.Fatalf("enumerate = %v, want %v (only the carve-out is allowed)", got, want)
	}
}

// The query pattern narrows the result below the grant's scope.
func TestEnumerate_QueryPatternNarrows(t *testing.T) {
	f := newEnumFixture(t,
		"account:acme/document:1",
		"account:acme/project:atlas/document:2",
		"account:acme/project:atlas/document:3",
	)
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g-all", model.EffectAllow, "p-impl", "account:acme/**")

	got := f.enumerate("account:acme/project:atlas/**")
	want := []string{"account:acme/project:atlas/document:2", "account:acme/project:atlas/document:3"}
	if !sameSet(got, want) {
		t.Fatalf("narrowed enumerate = %v, want %v", got, want)
	}
}

// Enumerate is bounded by the request Limit.
func TestEnumerate_RespectsLimit(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:2", "account:acme/document:3")
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g-all", model.EffectAllow, "p-impl", "account:acme/**")

	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**", Limit: 2,
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("limit 2 should bound the result to 2 ids, got %d (%v)", len(ids), ids)
	}
}

// A literal grant enumerates only when its pattern is a concrete identity; a
// wildcard literal grant contributes nothing (it is not concretely enumerable
// without a lister), keeping Enumerate bounded.
func TestEnumerate_LiteralConcreteOnly(t *testing.T) {
	f := newEnumFixture(t)
	f.principal("alice")
	f.perm("p-lit", "") // literal
	f.grant("g-concrete", model.EffectAllow, "p-lit", "account:acme/document:42")
	f.grant("g-wildcard", model.EffectAllow, "p-lit", "account:acme/document:*")

	got := f.enumerate("account:acme/**")
	want := []string{"account:acme/document:42"}
	if !sameSet(got, want) {
		t.Fatalf("literal enumerate = %v, want %v (wildcard literal is not enumerable)", got, want)
	}
}

// An implicit/exclusive grant with no configured lister surfaces the
// unconfigured-lister code rather than silently returning an empty set.
func TestEnumerate_UnconfiguredListerErrors(t *testing.T) {
	store := memory.New()
	ctx := context.Background()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{ID: "p-impl", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-impl", Object: "account:acme/**", Effect: model.EffectAllow,
	}))
	// Scope resolution without a lister.
	eng := New(store, WithScopeResolution(scope.DefaultRegistry()))

	_, err := eng.Enumerate(ctx, EnumerateRequest{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED {
		t.Fatalf("code = %q, want APERTURE_SCOPE_LISTER_UNCONFIGURED", code)
	}
}

func TestEnumerate_MissingFieldsError(t *testing.T) {
	f := newEnumFixture(t)
	cases := map[string]EnumerateRequest{
		"no account":   {Principal: "alice", Action: "read", Pattern: "account:acme/**"},
		"no principal": {Account: acctAcme, Action: "read", Pattern: "account:acme/**"},
		"no action":    {Account: acctAcme, Principal: "alice", Pattern: "account:acme/**"},
		"no pattern":   {Account: acctAcme, Principal: "alice", Action: "read"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.eng.Enumerate(context.Background(), req); aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("%s: code = %q, want APERTURE_INVALID_INPUT", name, aerr.CodeOf(err))
			}
		})
	}
}

func mustSeed(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}
