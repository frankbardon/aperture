package delegation

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

const (
	acctAcme  = "acme"
	acctOther = "other"

	permRead     = "perm-read"     // delegatable read
	permWrite    = "perm-write"    // non-delegatable write
	permDelegate = "perm-delegate" // the may-delegate right
)

// fixture wires an in-memory store with a "document" object type that declares
// read, write, and the reserved delegate verb; the three permissions; the
// delegator alice and grantee bob (both members of acme); and a clock so
// bestowed grants get a deterministic timestamp.
type fixture struct {
	t     *testing.T
	store *memory.Store
	eng   *engine.Engine
	svc   *Service
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
	must(store.PutAccount(ctx, model.Account{ID: acctOther, Name: "Other"}))
	must(store.PutObjectType(ctx, model.ObjectType{
		Name:    "document",
		Actions: []string{"read", "write", DelegateAction},
	}))
	must(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read", Delegatable: true}))
	must(store.PutPermission(ctx, model.Permission{ID: permWrite, ObjectType: "document", Action: "write", Delegatable: false}))
	must(store.PutPermission(ctx, model.Permission{ID: permDelegate, ObjectType: "document", Action: DelegateAction}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctAcme}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: acctAcme}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "carol", AccountID: acctAcme}))

	eng := engine.New(store)
	clock := func() time.Time { return time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) }
	return &fixture{t: t, store: store, eng: eng, svc: New(store, eng, WithClock(clock))}
}

// grant seeds a grant for a principal subject directly into storage (bypassing
// the delegation rule), used to set up the delegator's own authority.
func (f *fixture) grant(id, account, principal, perm, object string, effect model.Effect) {
	f.t.Helper()
	g := model.Grant{
		ID:           id,
		AccountID:    account,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: principal},
		PermissionID: perm,
		Object:       object,
		Effect:       effect,
	}
	if err := f.store.PutGrant(context.Background(), g); err != nil {
		f.t.Fatalf("seed grant %s: %v", id, err)
	}
}

// giveAliceAuthority gives alice the may-delegate right over all of acme plus
// read authority over the atlas subtree — the standard "designated delegator".
func (f *fixture) giveAliceAuthority() {
	f.grant("g-alice-delegate", acctAcme, "alice", permDelegate, "account:acme/**", model.EffectAllow)
	f.grant("g-alice-read", acctAcme, "alice", permRead, "account:acme/project:atlas/**", model.EffectAllow)
}

func (f *fixture) check(account, principal, action, object string) bool {
	f.t.Helper()
	dec, err := f.eng.Check(context.Background(), engine.Request{
		Account: account, Principal: principal, Action: action, Object: object,
	})
	if err != nil {
		f.t.Fatalf("check: %v", err)
	}
	return dec.Allow
}

func readGrant(id, account, object string) model.Grant {
	return model.Grant{
		ID:           id,
		AccountID:    account,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: permRead,
		Object:       object,
		Effect:       model.EffectAllow,
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

// TestBestowValid: a designated delegator bestows a more-specific, delegatable,
// in-scope grant; it persists and the grantee is then allowed by the engine.
func TestBestowValid(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	g := readGrant("g-bob", acctAcme, "account:acme/project:atlas/document:42")
	if err := f.svc.Bestow(ctx, "alice", g); err != nil {
		t.Fatalf("bestow: %v", err)
	}

	// Persisted and timestamped by the service clock.
	got, err := f.store.GetGrant(ctx, "g-bob")
	if err != nil {
		t.Fatalf("get bestowed grant: %v", err)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("bestowed grant not timestamped: %+v", got)
	}

	// Behaves identically to any other grant: bob is now allowed.
	if !f.check(acctAcme, "bob", "read", "account:acme/project:atlas/document:42") {
		t.Fatalf("bestowed grant did not grant access")
	}
}

// TestBestowAccountScoped: the bestowed grant vanishes outside its account.
func TestBestowAccountScoped(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	if err := f.svc.Bestow(ctx, "alice", readGrant("g-bob", acctAcme, "account:acme/project:atlas/document:42")); err != nil {
		t.Fatalf("bestow: %v", err)
	}
	// Same object, different active account: default-deny, the grant does not leak.
	if f.check(acctOther, "bob", "read", "account:acme/project:atlas/document:42") {
		t.Fatalf("bestowed grant leaked into another account")
	}
}

// TestBestowSubsetViolation: a grant outside the delegator's own scope (it holds
// the delegate right, but not read there) is rejected as a non-subset.
func TestBestowSubsetViolation(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	// alice's read covers atlas only; zeta is outside her authority.
	g := readGrant("g-bob", acctAcme, "account:acme/project:zeta/document:9")
	err := f.svc.Bestow(ctx, "alice", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	if r := reasonOf(t, err); r != "not_subset" {
		t.Fatalf("reason = %q, want not_subset", r)
	}
	// Nothing was persisted.
	mustCode(t, func() error { _, e := f.store.GetGrant(ctx, "g-bob"); return e }(), aerr.APERTURE_NOT_FOUND)
}

// TestBestowBroaderObjectRejected: bestowing a BROADER pattern than the
// delegator holds is rejected — the core anti-escalation guarantee.
func TestBestowBroaderObjectRejected(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	// alice holds read over atlas/**; bob is handed all of acme — broader. Reject.
	g := readGrant("g-bob", acctAcme, "account:acme/**")
	err := f.svc.Bestow(ctx, "alice", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	if r := reasonOf(t, err); r != "not_subset" {
		t.Fatalf("reason = %q, want not_subset", r)
	}
}

// TestBestowNonDelegatable: even with full authority, a permission that is not
// flagged delegatable cannot be bestowed.
func TestBestowNonDelegatable(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	// Give alice write authority too, so only the delegatable flag can block it.
	f.grant("g-alice-write", acctAcme, "alice", permWrite, "account:acme/**", model.EffectAllow)
	ctx := context.Background()

	g := model.Grant{
		ID:           "g-bob",
		AccountID:    acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: permWrite,
		Object:       "account:acme/project:atlas/document:42",
		Effect:       model.EffectAllow,
	}
	mustCode(t, f.svc.Bestow(ctx, "alice", g), aerr.APERTURE_DELEGATION_NOT_DELEGATABLE)
}

// TestBestowNoDelegateRight: a principal that holds the underlying permission but
// NOT the may-delegate right cannot bestow. Delegation is itself a permission.
func TestBestowNoDelegateRight(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// carol holds read over atlas but no delegate grant.
	f.grant("g-carol-read", acctAcme, "carol", permRead, "account:acme/project:atlas/**", model.EffectAllow)

	g := readGrant("g-bob", acctAcme, "account:acme/project:atlas/document:42")
	err := f.svc.Bestow(ctx, "carol", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	if r := reasonOf(t, err); r != "no_delegate_right" {
		t.Fatalf("reason = %q, want no_delegate_right", r)
	}
}

// TestBestowCrossAccountNonMember: a delegator may not bestow into an account it
// is not a member of.
func TestBestowCrossAccountNonMember(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	// alice is not a member of "other".
	g := readGrant("g-bob", acctOther, "account:other/document:1")
	err := f.svc.Bestow(ctx, "alice", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	if r := reasonOf(t, err); r != "cross_account" {
		t.Fatalf("reason = %q, want cross_account", r)
	}
}

// TestBestowCrossAccountAuthorityDoesNotLeak: even a delegator who IS a member of
// the target account cannot bestow there using authority held only in another
// account — effective grants are account-scoped.
func TestBestowCrossAccountAuthorityDoesNotLeak(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority() // authority in acme only
	ctx := context.Background()
	// alice is admitted to other, but holds no grants there.
	if err := f.store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctOther}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	g := readGrant("g-bob", acctOther, "account:other/document:1")
	err := f.svc.Bestow(ctx, "alice", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	// Her acme authority does not apply in other, so she has no delegate right here.
	if r := reasonOf(t, err); r != "no_delegate_right" {
		t.Fatalf("reason = %q, want no_delegate_right", r)
	}
}

// TestBestowDenyEffectRejected: only allow grants may be bestowed.
func TestBestowDenyEffectRejected(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	g := readGrant("g-bob", acctAcme, "account:acme/project:atlas/document:42")
	g.Effect = model.EffectDeny
	err := f.svc.Bestow(ctx, "alice", g)
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	if r := reasonOf(t, err); r != "non_allow_effect" {
		t.Fatalf("reason = %q, want non_allow_effect", r)
	}
}

// TestRevoke: a delegator revokes a grant it had bestowed; the grantee loses
// access. Revoking an unknown grant is NOT_FOUND.
func TestRevoke(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()

	g := readGrant("g-bob", acctAcme, "account:acme/project:atlas/document:42")
	if err := f.svc.Bestow(ctx, "alice", g); err != nil {
		t.Fatalf("bestow: %v", err)
	}
	if !f.check(acctAcme, "bob", "read", "account:acme/project:atlas/document:42") {
		t.Fatalf("precondition: bob should be allowed")
	}

	if err := f.svc.Revoke(ctx, "alice", "g-bob"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	mustCode(t, func() error { _, e := f.store.GetGrant(ctx, "g-bob"); return e }(), aerr.APERTURE_NOT_FOUND)
	if f.check(acctAcme, "bob", "read", "account:acme/project:atlas/document:42") {
		t.Fatalf("bob still allowed after revoke")
	}

	mustCode(t, f.svc.Revoke(ctx, "alice", "ghost"), aerr.APERTURE_NOT_FOUND)
}

// TestRevokeOutsideAuthorityRejected: a delegator cannot revoke a grant outside
// its own authority (here, a grant in a scope it does not hold).
func TestRevokeOutsideAuthorityRejected(t *testing.T) {
	f := newFixture(t)
	f.giveAliceAuthority()
	ctx := context.Background()
	// A pre-existing grant in zeta, outside alice's read scope.
	f.grant("g-zeta", acctAcme, "bob", permRead, "account:acme/project:zeta/document:9", model.EffectAllow)

	err := f.svc.Revoke(ctx, "alice", "g-zeta")
	mustCode(t, err, aerr.APERTURE_DELEGATION_DENIED)
	// The grant survives a rejected revoke.
	if _, e := f.store.GetGrant(ctx, "g-zeta"); e != nil {
		t.Fatalf("grant should survive rejected revoke: %v", e)
	}
}
