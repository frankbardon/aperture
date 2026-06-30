package engine

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// This file is the FOCUSED cross-account isolation suite for E3-S1. It exists to
// prove and lock the non-negotiable, security-critical invariant (FR-14): a
// multi-account principal's grants in one account NEVER apply in another. It
// proves the invariant independently under every grant-subject shape — direct
// principal, role, group, and wildcard grants, plus a scope-strategy grant — and
// pins the deterministic effect of switching the active account. A second group
// of tests covers the opt-in membership-enforcement layer
// (WithMembershipEnforcement): a non-member of the active account is denied at
// the door across Check, Enumerate, and Explain.
//
// The fixtures seed BOTH accounts and memberships as first-class entities so the
// suite exercises the real two-account topology, not a grant-only approximation.

// isoFixture wires an in-memory store with the canonical "document"/"project"
// object types and one permission per verb. It seeds two accounts, acme and
// other, with alice a member of both — the multi-account principal at the heart
// of the invariant.
type isoFixture struct {
	t     *testing.T
	store *memory.Store
}

func newIsoFixture(t *testing.T) *isoFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	f := &isoFixture{t: t, store: store}
	f.must(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read", "write", "delete"}}))
	f.must(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read"}))
	f.must(store.PutPermission(ctx, model.Permission{ID: permWrite, ObjectType: "document", Action: "write"}))
	// Two tenancies; alice is admitted to both, bob only to acme.
	f.account(acctAcme)
	f.account(acctOther)
	f.member("alice", acctAcme)
	f.member("alice", acctOther)
	f.member("bob", acctAcme)
	return f
}

func (f *isoFixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("seed: %v", err)
	}
}

func (f *isoFixture) account(id string) {
	f.t.Helper()
	f.must(f.store.PutAccount(context.Background(), model.Account{ID: id, Name: id}))
}

func (f *isoFixture) member(principalID, accountID string) {
	f.t.Helper()
	f.must(f.store.PutMembership(context.Background(), model.Membership{PrincipalID: principalID, AccountID: accountID}))
}

func (f *isoFixture) principal(id string, roleIDs ...string) {
	f.t.Helper()
	f.must(f.store.PutPrincipal(context.Background(), model.Principal{
		ID: id, Kind: model.PrincipalUser, Identity: "user:" + id, RoleIDs: roleIDs,
	}))
}

func (f *isoFixture) role(id string) {
	f.t.Helper()
	f.must(f.store.PutRole(context.Background(), model.Role{ID: id, Name: id}))
}

func (f *isoFixture) group(id string, members ...string) {
	f.t.Helper()
	f.must(f.store.PutGroup(context.Background(), model.Group{ID: id, Name: id, MemberPrincipalIDs: members}))
}

func (f *isoFixture) grant(id, account string, subj model.Subject, effect model.Effect, permID, object string) {
	f.t.Helper()
	f.must(f.store.PutGrant(context.Background(), model.Grant{
		ID: id, AccountID: account, Subject: subj, PermissionID: permID, Object: object, Effect: effect,
	}))
}

// allowed runs eng.Check and asserts no operational error, returning the verdict.
func allowed(t *testing.T, eng *Engine, account, principal, action, object string) bool {
	t.Helper()
	d, err := eng.Check(context.Background(), Request{Account: account, Principal: principal, Action: action, Object: object})
	if err != nil {
		t.Fatalf("Check(%s,%s,%s,%s): unexpected error: %v", account, principal, action, object, err)
	}
	return d.Allow
}

// --- The core invariant, proven under every grant-subject shape ---
//
// In each case alice holds an ALLOW grant in acme only. The SAME action on the
// SAME object must be allowed in acme and DENIED in other, where alice is equally
// a member but holds no grant. The object identity is shared across accounts to
// rule out any path where the object string alone (not the account scope) decides.

func TestIsolation_GrantNeverCrossesAccounts(t *testing.T) {
	const object = "account:shared/project:atlas/document:42"

	cases := []struct {
		name  string
		setup func(f *isoFixture) // seeds the subject + the acme-only allow grant
	}{
		{
			name: "direct principal grant",
			setup: func(f *isoFixture) {
				f.principal("alice")
				f.grant("g-direct", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, object)
			},
		},
		{
			name: "role grant",
			setup: func(f *isoFixture) {
				f.role("reader")
				f.principal("alice", "reader")
				f.grant("g-role", acctAcme, subjRole("reader"), model.EffectAllow, permRead, object)
			},
		},
		{
			name: "group grant",
			setup: func(f *isoFixture) {
				f.principal("alice")
				f.group("eng", "alice")
				f.grant("g-group", acctAcme, subjGroup("eng"), model.EffectAllow, permRead, object)
			},
		},
		{
			name: "wildcard grant",
			setup: func(f *isoFixture) {
				f.principal("alice")
				f.grant("g-wild", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:shared/project:atlas/**")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIsoFixture(t)
			tc.setup(f)
			eng := New(f.store)

			// acme: the grant applies — allow.
			if !allowed(t, eng, acctAcme, "alice", "read", object) {
				t.Fatalf("[%s] alice's acme grant must allow in acme", tc.name)
			}
			// other: the SAME multi-account principal, SAME action, SAME object —
			// but the grant is stamped to acme, so it must be invisible here.
			if allowed(t, eng, acctOther, "alice", "read", object) {
				t.Fatalf("ISOLATION BREACH [%s]: alice's acme grant leaked into account other", tc.name)
			}
		})
	}
}

// TestIsolation_ScopeStrategyGrantNeverCrossesAccounts proves the invariant holds
// for the E2 scope-strategy path too: an inclusive-strategy grant resolved by the
// scope registry is still account-scoped, so it cannot leak.
func TestIsolation_ScopeStrategyGrantNeverCrossesAccounts(t *testing.T) {
	const object = "account:shared/document:42"
	f := newIsoFixture(t)
	f.principal("alice")
	// An inclusive scope-strategy permission whose id-list contains the object.
	f.must(f.store.PutPermission(context.Background(), model.Permission{
		ID: "perm-inc", ObjectType: "document", Action: "read",
		ScopeStrategy: "inclusive;ids=" + object,
	}))
	f.grant("g-inc", acctAcme, subjPrincipal("alice"), model.EffectAllow, "perm-inc", "account:shared/**")

	eng := New(f.store, WithScopeResolution(scope.DefaultRegistry()))

	if !allowed(t, eng, acctAcme, "alice", "read", object) {
		t.Fatal("inclusive scope grant must allow in acme")
	}
	if allowed(t, eng, acctOther, "alice", "read", object) {
		t.Fatal("ISOLATION BREACH: inclusive scope grant leaked into account other")
	}
}

// TestIsolation_SwitchingActiveAccountIsDeterministic asserts the active account
// is the deciding axis: with grants in BOTH accounts (an allow in acme, a deny in
// other over the same object), flipping only Request.Account flips the verdict,
// repeatably, with no dependence on anything else.
func TestIsolation_SwitchingActiveAccountIsDeterministic(t *testing.T) {
	const object = "account:shared/document:42"
	f := newIsoFixture(t)
	f.principal("alice")
	f.grant("g-allow-acme", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, object)
	f.grant("g-deny-other", acctOther, subjPrincipal("alice"), model.EffectDeny, permRead, object)
	eng := New(f.store)

	// The effective grant set is a pure function of the active account: assert it
	// is stable across repeated switches, ruling out any order/state dependence.
	for i := 0; i < 3; i++ {
		if !allowed(t, eng, acctAcme, "alice", "read", object) {
			t.Fatalf("iter %d: acme must allow (its allow grant is the only visible one)", i)
		}
		if allowed(t, eng, acctOther, "alice", "read", object) {
			t.Fatalf("iter %d: other must deny (its deny grant is the only visible one)", i)
		}
	}

	// Cross-check the effective grant set at the storage seam directly: each
	// account's GrantsForSubjects returns only its own grant.
	subjects := []model.Subject{subjPrincipal("alice")}
	for _, tc := range []struct {
		account, wantGrant string
	}{{acctAcme, "g-allow-acme"}, {acctOther, "g-deny-other"}} {
		got, err := f.store.GrantsForSubjects(context.Background(), tc.account, subjects)
		if err != nil {
			t.Fatalf("GrantsForSubjects(%s): %v", tc.account, err)
		}
		if len(got) != 1 || got[0].ID != tc.wantGrant {
			t.Fatalf("GrantsForSubjects(%s) = %v, want exactly [%s]", tc.account, grantIDs(got), tc.wantGrant)
		}
	}
}

func grantIDs(gs []model.Grant) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

// --- Membership enforcement (opt-in) ---

// TestMembershipEnforcement_NonMemberDeniedDespiteGrant is the defence-in-depth
// case: even if a grant is (mistakenly) stamped to an account the principal was
// never admitted to, enforcement denies before the grant is ever consulted. bob
// is a member of acme only; a stray allow grant for bob in other must not let him
// in there.
func TestMembershipEnforcement_NonMemberDeniedDespiteGrant(t *testing.T) {
	const object = "account:shared/document:42"
	f := newIsoFixture(t)
	f.principal("bob")
	// A grant that WOULD match, stamped to an account bob is not a member of.
	f.grant("g-stray", acctOther, subjPrincipal("bob"), model.EffectAllow, permRead, object)

	enforcing := New(f.store, WithMembershipEnforcement())

	// Without a membership in other, bob is denied even though the grant matches.
	if allowed(t, enforcing, acctOther, "bob", "read", object) {
		t.Fatal("ISOLATION BREACH: non-member bob allowed in account other via a stray grant")
	}
	// Sanity: the same engine WITHOUT enforcement would have honoured the stray
	// grant — proving the deny above is the membership layer, not absent data.
	permissive := New(f.store)
	if !allowed(t, permissive, acctOther, "bob", "read", object) {
		t.Fatal("control failed: stray grant should match when enforcement is off")
	}
}

// TestMembershipEnforcement_MemberAllowed confirms enforcement does not over-deny:
// a member of the active account with a matching grant is still allowed.
func TestMembershipEnforcement_MemberAllowed(t *testing.T) {
	const object = "account:shared/document:42"
	f := newIsoFixture(t)
	f.principal("alice")
	f.grant("g-allow", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, object)
	enforcing := New(f.store, WithMembershipEnforcement())

	if !allowed(t, enforcing, acctAcme, "alice", "read", object) {
		t.Fatal("member alice with a matching acme grant must be allowed under enforcement")
	}
	// And in other, where alice IS a member but holds no grant: clean default-deny.
	if allowed(t, enforcing, acctOther, "alice", "read", object) {
		t.Fatal("alice has no grant in other; must default-deny")
	}
}

// TestMembershipEnforcement_EnumerateAndExplainFailClosed asserts the enforcement
// verdict is uniform across the whole decision API, not just Check: a non-member
// enumerates nothing and explains to a deny that names no grant.
func TestMembershipEnforcement_EnumerateAndExplainFailClosed(t *testing.T) {
	f := newIsoFixture(t)
	f.principal("bob")
	f.grant("g-stray", acctOther, subjPrincipal("bob"), model.EffectAllow, permRead, "account:shared/**")
	enforcing := New(f.store, WithMembershipEnforcement())
	ctx := context.Background()

	ids, err := enforcing.Enumerate(ctx, EnumerateRequest{
		Account: acctOther, Principal: "bob", Action: "read", Pattern: "account:shared/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("non-member must enumerate nothing, got %v", ids)
	}

	tr, err := enforcing.Explain(ctx, Request{
		Account: acctOther, Principal: "bob", Action: "read", Object: "account:shared/document:42",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if tr.Decision.Allow {
		t.Fatal("non-member Explain must be a deny")
	}
	if len(tr.Decision.DecidingGrantIDs) != 0 {
		t.Fatalf("non-member deny names no grant, got %v", tr.Decision.DecidingGrantIDs)
	}
	if len(tr.Considered) != 0 {
		t.Fatalf("non-member deny precedes grant evaluation; considered should be empty, got %d", len(tr.Considered))
	}
}
