package authz

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

const (
	acctAcme   = "acme"
	acctGlobex = "globex"

	// permAdmin is the one permission carrying the reserved admin action. The two
	// tiers differ only in the OBJECT a grant scopes it to (system:* vs
	// account:<acct>/admin:*), never in a separate permission or a parallel
	// authority system.
	permAdmin = "perm-admin"
)

// fixture wires an in-memory store with an "authority" object type that declares
// the reserved admin verb, the admin permission, two accounts, and a cast of
// principals (all members of acme; globex membership added per-test). The engine
// resolves authority; the gate sits on top of it.
type fixture struct {
	t     *testing.T
	store *memory.Store
	eng   *engine.Engine
	gate  *Gate
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.PutAccount(ctx, model.Account{ID: acctAcme, Name: "Acme"}))
	must(store.PutAccount(ctx, model.Account{ID: acctGlobex, Name: "Globex"}))
	// One object type declaring the admin verb; the permission's object type is a
	// write-time typing concern only — the engine matches the grant's object
	// PATTERN against the request object, decoupled from the permission's type.
	must(store.PutObjectType(ctx, model.ObjectType{
		Name:    "authority",
		Actions: []string{AdminAction},
	}))
	must(store.PutPermission(ctx, model.Permission{ID: permAdmin, ObjectType: "authority", Action: AdminAction}))

	for _, p := range []string{"root", "acmeadmin", "globexadmin", "super", "nobody"} {
		must(store.PutPrincipal(ctx, model.Principal{ID: p, Kind: model.PrincipalUser, Identity: "user:" + p}))
		must(store.PutMembership(ctx, model.Membership{PrincipalID: p, AccountID: acctAcme}))
	}

	eng := engine.New(store)
	return &fixture{t: t, store: store, eng: eng, gate: NewGate(eng)}
}

// grant seeds an admin grant for a principal: an allow on the admin permission
// scoped to object, stamped to account.
func (f *fixture) grant(id, account, principal, object string) {
	f.t.Helper()
	g := model.Grant{
		ID:           id,
		AccountID:    account,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: principal},
		PermissionID: permAdmin,
		Object:       object,
		Effect:       model.EffectAllow,
	}
	if err := f.store.PutGrant(context.Background(), g); err != nil {
		f.t.Fatalf("seed grant %s: %v", id, err)
	}
}

func mustCode(t *testing.T, err error, want aerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("error code = %s, want %s (err: %v)", got, want, err)
	}
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("not a coded error: %v", err)
	}
	r, _ := ce.Context["reason"].(string)
	return r
}

// --- Acceptance: system-admin can edit schema. ---

// TestSystemAdminCanEditSchema: a holder of system:* passes the system tier for
// every schema mutation, and account-tier checks only pass where it ALSO holds
// account-admin authority (it does not, by virtue of the system grant alone).
func TestSystemAdminCanEditSchema(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-root-system", acctAcme, "root", "system:*")

	root := Actor{Principal: "root", Account: acctAcme}

	if err := f.gate.RequireSystemAdmin(ctx, root); err != nil {
		t.Fatalf("system admin denied system tier: %v", err)
	}
	// Every system-tier mutation is permitted.
	for _, m := range []Mutation{
		MutationPutObjectType, MutationPutPermission, MutationPutRole,
		MutationPutProvider, MutationPutTemplate, MutationPutRule,
		MutationPutAccount, MutationPutPrincipal, MutationPutGroup,
	} {
		if err := f.gate.Authorize(ctx, root, m, ""); err != nil {
			t.Fatalf("system admin denied schema mutation %s: %v", m, err)
		}
	}
	// A pure system admin is NOT automatically an account admin (the tiers are
	// distinct authorities); its system:* grant does not cover account:acme/admin:*.
	if err := f.gate.RequireAccountAdmin(ctx, root, acctAcme); err == nil {
		t.Fatalf("pure system admin was wrongly granted account-admin authority")
	} else {
		mustCode(t, err, aerr.APERTURE_AUTHZ_DENIED)
	}
}

// --- Acceptance: account-admin CANNOT edit schema. ---

// TestAccountAdminCannotEditSchema: a holder of account:acme/admin:* passes the
// account tier in acme but is refused every system-tier (schema) mutation.
func TestAccountAdminCannotEditSchema(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-acme-admin", acctAcme, "acmeadmin", "account:acme/admin:*")

	admin := Actor{Principal: "acmeadmin", Account: acctAcme}

	// Holds account-admin in acme.
	if err := f.gate.RequireAccountAdmin(ctx, admin, acctAcme); err != nil {
		t.Fatalf("account admin denied its own account tier: %v", err)
	}
	if err := f.gate.Authorize(ctx, admin, MutationPutGrant, acctAcme); err != nil {
		t.Fatalf("account admin denied an account-tier grant mutation: %v", err)
	}

	// Refused system-tier authority outright.
	err := f.gate.RequireSystemAdmin(ctx, admin)
	mustCode(t, err, aerr.APERTURE_AUTHZ_DENIED)
	if r := reasonOf(t, err); r != "no_system_admin" {
		t.Fatalf("reason = %q, want no_system_admin", r)
	}
	// And refused via the dispatcher for every schema mutation.
	for _, m := range []Mutation{
		MutationPutObjectType, MutationDeletePermission, MutationPutRole,
		MutationPutRule, MutationDeleteAccount,
	} {
		mustCode(t, f.gate.Authorize(ctx, admin, m, ""), aerr.APERTURE_AUTHZ_DENIED)
	}
}

// --- Acceptance: account-admin confined to own account. ---

// TestAccountAdminConfinedToOwnAccount: an admin of acme is refused any mutation
// scoped to globex, even after being admitted as a member of globex — its acme
// authority does not apply there.
func TestAccountAdminConfinedToOwnAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-acme-admin", acctAcme, "acmeadmin", "account:acme/admin:*")

	admin := Actor{Principal: "acmeadmin", Account: acctAcme}

	// Not even a member of globex: confined.
	err := f.gate.RequireAccountAdmin(ctx, admin, acctGlobex)
	mustCode(t, err, aerr.APERTURE_AUTHZ_DENIED)
	if r := reasonOf(t, err); r != "no_account_admin" {
		t.Fatalf("reason = %q, want no_account_admin", r)
	}

	// Admit acmeadmin to globex as a member; authority STILL does not leak — its
	// admin grant is stamped to acme, so globex-scoped resolution never loads it.
	if err := f.store.PutMembership(ctx, model.Membership{PrincipalID: "acmeadmin", AccountID: acctGlobex}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	mustCode(t, f.gate.RequireAccountAdmin(ctx, admin, acctGlobex), aerr.APERTURE_AUTHZ_DENIED)
	mustCode(t, f.gate.Authorize(ctx, admin, MutationPutGrant, acctGlobex), aerr.APERTURE_AUTHZ_DENIED)

	// The legitimate globex admin (grant stamped to globex) IS allowed in globex
	// but NOT in acme — confinement cuts both ways.
	if err := f.store.PutMembership(ctx, model.Membership{PrincipalID: "globexadmin", AccountID: acctGlobex}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	f.grant("g-globex-admin", acctGlobex, "globexadmin", "account:globex/admin:*")
	gAdmin := Actor{Principal: "globexadmin", Account: acctGlobex}
	if err := f.gate.RequireAccountAdmin(ctx, gAdmin, acctGlobex); err != nil {
		t.Fatalf("globex admin denied its own account: %v", err)
	}
	mustCode(t, f.gate.RequireAccountAdmin(ctx, gAdmin, acctAcme), aerr.APERTURE_AUTHZ_DENIED)
}

// --- Acceptance: explain works on admin identities. ---

// TestExplainOnAdminIdentities: the admin authority decision is resolved by the
// ordinary engine, so it produces a full Trace naming the deciding admin grant —
// for both a granted holder and a denied non-holder, at both tiers.
func TestExplainOnAdminIdentities(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-root-system", acctAcme, "root", "system:*")
	f.grant("g-acme-admin", acctAcme, "acmeadmin", "account:acme/admin:*")

	root := Actor{Principal: "root", Account: acctAcme}
	admin := Actor{Principal: "acmeadmin", Account: acctAcme}
	nobody := Actor{Principal: "nobody", Account: acctAcme}

	// System tier: root's trace allows and names the system grant.
	tr, err := f.gate.ExplainSystemAdmin(ctx, root)
	if err != nil {
		t.Fatalf("explain system admin: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("expected system-admin allow, got deny: %s", tr.String())
	}
	if tr.Request.Action != AdminAction || tr.Request.Object != systemAnchor {
		t.Fatalf("trace request not on the admin identity: %+v", tr.Request)
	}
	if !containsID(tr.Decision.DecidingGrantIDs, "g-root-system") {
		t.Fatalf("deciding grants %v do not include the system admin grant", tr.Decision.DecidingGrantIDs)
	}

	// Account tier: acmeadmin's trace allows on the per-account admin identity.
	tr, err = f.gate.ExplainAccountAdmin(ctx, admin, acctAcme)
	if err != nil {
		t.Fatalf("explain account admin: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("expected account-admin allow, got deny: %s", tr.String())
	}
	if tr.Request.Object != accountAnchor(acctAcme) {
		t.Fatalf("trace request not on the account admin identity: %+v", tr.Request)
	}
	if !containsID(tr.Decision.DecidingGrantIDs, "g-acme-admin") {
		t.Fatalf("deciding grants %v do not include the account admin grant", tr.Decision.DecidingGrantIDs)
	}

	// A non-holder gets an explainable DENY (default-deny, no deciding grant), not
	// an error — the same engine path renders the refusal.
	tr, err = f.gate.ExplainSystemAdmin(ctx, nobody)
	if err != nil {
		t.Fatalf("explain non-holder: %v", err)
	}
	if tr.Decision.Allow {
		t.Fatalf("expected non-holder deny, got allow: %s", tr.String())
	}
	if len(tr.Decision.DecidingGrantIDs) != 0 {
		t.Fatalf("default-deny should name no deciding grant, got %v", tr.Decision.DecidingGrantIDs)
	}
}

// TestSuperAdminViaWildcard: a holder of the all-covering ** grant satisfies BOTH
// tiers at once — the in-scheme way to be a system-and-account super admin.
func TestSuperAdminViaWildcard(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-super", acctAcme, "super", "**")

	super := Actor{Principal: "super", Account: acctAcme}
	if err := f.gate.RequireSystemAdmin(ctx, super); err != nil {
		t.Fatalf("** holder denied system tier: %v", err)
	}
	if err := f.gate.RequireAccountAdmin(ctx, super, acctAcme); err != nil {
		t.Fatalf("** holder denied account tier in acme: %v", err)
	}
}

// TestDenyGrantConfersNoAuthority: a DENY grant on the admin namespace confers no
// authority — only an allow can (the gate inherits the engine's deny-overrides).
func TestDenyGrantConfersNoAuthority(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := model.Grant{
		ID:           "g-deny",
		AccountID:    acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "nobody"},
		PermissionID: permAdmin,
		Object:       "system:*",
		Effect:       model.EffectDeny,
	}
	if err := f.store.PutGrant(ctx, g); err != nil {
		t.Fatalf("seed deny grant: %v", err)
	}
	mustCode(t, f.gate.RequireSystemAdmin(ctx, Actor{Principal: "nobody", Account: acctAcme}), aerr.APERTURE_AUTHZ_DENIED)
}

// TestUnknownMutationFailsClosed: a mutation the gate has no policy for is refused
// rather than silently allowed.
func TestUnknownMutationFailsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-root-system", acctAcme, "root", "system:*")
	root := Actor{Principal: "root", Account: acctAcme}
	mustCode(t, f.gate.Authorize(ctx, root, Mutation("teleport"), acctAcme), aerr.APERTURE_INVALID_INPUT)
}

// TestAuthorizeAccountTierRequiresAccount: the dispatcher refuses an account-tier
// mutation with no target account.
func TestAuthorizeAccountTierRequiresAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.grant("g-acme-admin", acctAcme, "acmeadmin", "account:acme/admin:*")
	admin := Actor{Principal: "acmeadmin", Account: acctAcme}
	mustCode(t, f.gate.Authorize(ctx, admin, MutationPutGrant, ""), aerr.APERTURE_INVALID_INPUT)
}

// TestActorValidation: empty actor fields are rejected as invalid input.
func TestActorValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mustCode(t, f.gate.RequireSystemAdmin(ctx, Actor{Account: acctAcme}), aerr.APERTURE_INVALID_INPUT)
	mustCode(t, f.gate.RequireSystemAdmin(ctx, Actor{Principal: "root"}), aerr.APERTURE_INVALID_INPUT)
	mustCode(t, f.gate.RequireAccountAdmin(ctx, Actor{}, acctAcme), aerr.APERTURE_INVALID_INPUT)
	mustCode(t, f.gate.RequireAccountAdmin(ctx, Actor{Principal: "root"}, ""), aerr.APERTURE_INVALID_INPUT)
}

// TestTierOf: the mutation→tier table is the single source of truth and covers
// both tiers; an unknown mutation is reported as unmapped.
func TestTierOf(t *testing.T) {
	cases := []struct {
		m    Mutation
		tier Tier
	}{
		{MutationPutPermission, TierSystem},
		{MutationDeleteRule, TierSystem},
		{MutationPutAccount, TierSystem},
		{MutationPutGrant, TierAccount},
		{MutationBestow, TierAccount},
		{MutationDeleteMembership, TierAccount},
	}
	for _, c := range cases {
		got, ok := TierOf(c.m)
		if !ok {
			t.Fatalf("mutation %s not mapped", c.m)
		}
		if got != c.tier {
			t.Fatalf("TierOf(%s) = %s, want %s", c.m, got, c.tier)
		}
	}
	if _, ok := TierOf(Mutation("nope")); ok {
		t.Fatalf("unknown mutation reported as mapped")
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
