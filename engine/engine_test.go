package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// fixture wires an in-memory store with a single "document" object type that
// declares the verbs the tests grant against, plus one permission per verb. It
// exposes small helpers so each test states only the grants and membership that
// matter to it.
type fixture struct {
	t     *testing.T
	store *memory.Store
	eng   *Engine
}

const (
	acctAcme  = "acme"
	acctOther = "other"
	permRead  = "perm-read"
	permWrite = "perm-write"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustPut(store.PutObjectType(ctx, model.ObjectType{
		Name:    "document",
		Actions: []string{"read", "write", "delete"},
	}))
	mustPut(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read"}))
	mustPut(store.PutPermission(ctx, model.Permission{ID: permWrite, ObjectType: "document", Action: "write"}))
	return &fixture{t: t, store: store, eng: New(store)}
}

func (f *fixture) principal(id string, roleIDs ...string) {
	f.t.Helper()
	if err := f.store.PutPrincipal(context.Background(), model.Principal{
		ID:       id,
		Kind:     model.PrincipalUser,
		Identity: "user:" + id,
		RoleIDs:  roleIDs,
	}); err != nil {
		f.t.Fatalf("put principal %s: %v", id, err)
	}
}

func (f *fixture) role(id string) {
	f.t.Helper()
	if err := f.store.PutRole(context.Background(), model.Role{ID: id, Name: id}); err != nil {
		f.t.Fatalf("put role %s: %v", id, err)
	}
}

func (f *fixture) group(id string, members ...string) {
	f.t.Helper()
	if err := f.store.PutGroup(context.Background(), model.Group{ID: id, Name: id, MemberPrincipalIDs: members}); err != nil {
		f.t.Fatalf("put group %s: %v", id, err)
	}
}

// grant seeds a grant. account/subject/effect/object/permission are spelled out
// per call so the test reads as a policy statement.
func (f *fixture) grant(id, account string, subj model.Subject, effect model.Effect, permID, object string) {
	f.t.Helper()
	if err := f.store.PutGrant(context.Background(), model.Grant{
		ID:           id,
		AccountID:    account,
		Subject:      subj,
		PermissionID: permID,
		Object:       object,
		Effect:       effect,
	}); err != nil {
		f.t.Fatalf("put grant %s: %v", id, err)
	}
}

func subjPrincipal(id string) model.Subject {
	return model.Subject{Kind: model.SubjectPrincipal, ID: id}
}
func subjRole(id string) model.Subject  { return model.Subject{Kind: model.SubjectRole, ID: id} }
func subjGroup(id string) model.Subject { return model.Subject{Kind: model.SubjectGroup, ID: id} }

func (f *fixture) check(account, principal, action, object string) Decision {
	f.t.Helper()
	d, err := f.eng.Check(context.Background(), Request{
		Account:   account,
		Principal: principal,
		Action:    action,
		Object:    object,
	})
	if err != nil {
		f.t.Fatalf("Check(%s,%s,%s,%s): unexpected error: %v", account, principal, action, object, err)
	}
	return d
}

// --- Acceptance: simple allow / deny ---

func TestCheck_SimpleAllow(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")

	d := f.check(acctAcme, "alice", "read", "document:42")
	if !d.Allow {
		t.Fatalf("want allow, got deny (%s)", d.Reason)
	}
	if len(d.DecidingGrantIDs) != 1 || d.DecidingGrantIDs[0] != "g1" {
		t.Fatalf("deciding grants = %v, want [g1]", d.DecidingGrantIDs)
	}
}

func TestCheck_SimpleDeny(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "document:42")

	d := f.check(acctAcme, "alice", "read", "document:42")
	if d.Allow {
		t.Fatalf("want deny, got allow (%s)", d.Reason)
	}
	if len(d.DecidingGrantIDs) != 1 || d.DecidingGrantIDs[0] != "g1" {
		t.Fatalf("deciding grants = %v, want [g1]", d.DecidingGrantIDs)
	}
}

// --- Acceptance: empty-grants default-deny ---

func TestCheck_DefaultDenyNoGrants(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")

	d := f.check(acctAcme, "alice", "read", "document:42")
	if d.Allow {
		t.Fatalf("want default deny, got allow")
	}
	if len(d.DecidingGrantIDs) != 0 {
		t.Fatalf("default deny should name no deciding grant, got %v", d.DecidingGrantIDs)
	}
}

// An allow on a different action must not satisfy the request.
func TestCheck_ActionMismatchIsDefaultDeny(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectAllow, permWrite, "document:42")

	d := f.check(acctAcme, "alice", "read", "document:42")
	if d.Allow {
		t.Fatalf("write allow must not authorize read, got allow (%s)", d.Reason)
	}
}

// --- Acceptance: wildcard overlaps, equal-specificity deny-wins ---

func TestCheck_EqualSpecificityDenyWins(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	// Two grants over the SAME pattern (equal specificity), opposite effects.
	f.grant("allow", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/**")
	f.grant("deny", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")

	d := f.check(acctAcme, "alice", "read", "account:acme/document:42")
	if d.Allow {
		t.Fatalf("equal-specificity allow+deny must deny, got allow (%s)", d.Reason)
	}
	if len(d.DecidingGrantIDs) != 1 || d.DecidingGrantIDs[0] != "deny" {
		t.Fatalf("deciding grants = %v, want [deny]", d.DecidingGrantIDs)
	}
}

// A specific wildcard allow beats a broader wildcard deny only where it is
// strictly more specific.
func TestCheck_WildcardSpecificAllowBeatsBroaderDeny(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("deny-all", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")
	f.grant("allow-doc", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/document:*")

	d := f.check(acctAcme, "alice", "read", "account:acme/document:42")
	if !d.Allow {
		t.Fatalf("more-specific allow must win over broader deny, got deny (%s)", d.Reason)
	}
}

// --- Acceptance: the carve-out case ---

func TestCheck_CarveOut(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	// deny everything in acme, but carve out the atlas project as allowed.
	f.grant("deny-acme", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")
	f.grant("allow-atlas", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/project:atlas/**")

	// Inside the carve-out: allowed.
	if d := f.check(acctAcme, "alice", "read", "account:acme/project:atlas/document:42"); !d.Allow {
		t.Fatalf("atlas object should be allowed by the carve-out, got deny (%s)", d.Reason)
	}
	// Outside the carve-out, still in acme: denied.
	if d := f.check(acctAcme, "alice", "read", "account:acme/project:beta/document:7"); d.Allow {
		t.Fatalf("non-atlas object should remain denied, got allow (%s)", d.Reason)
	}
}

// --- Acceptance: deterministic regardless of insertion order ---

func TestCheck_OrderIndependent(t *testing.T) {
	// Seed the same policy in both orders; the verdict must match.
	run := func(denyFirst bool) Decision {
		f := newFixture(t)
		f.principal("alice")
		if denyFirst {
			f.grant("deny", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")
			f.grant("allow", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/**")
		} else {
			f.grant("allow", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/**")
			f.grant("deny", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")
		}
		return f.check(acctAcme, "alice", "read", "account:acme/document:1")
	}
	a, b := run(true), run(false)
	if a.Allow != b.Allow {
		t.Fatalf("verdict depends on insertion order: denyFirst=%v allowFirst=%v", a.Allow, b.Allow)
	}
	if a.Allow {
		t.Fatalf("equal-specificity tie must deny regardless of order")
	}
}

// --- Acceptance: account isolation ---

func TestCheck_AccountIsolation(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	// Allow in acme; a deny stamped to a different account must never leak in.
	f.grant("allow-acme", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")
	f.grant("deny-other", acctOther, subjPrincipal("alice"), model.EffectDeny, permRead, "document:42")

	// In the acme account: the other-account deny is invisible, so allow stands.
	if d := f.check(acctAcme, "alice", "read", "document:42"); !d.Allow {
		t.Fatalf("acme allow should stand; other-account deny must not leak (%s)", d.Reason)
	}
	// In the other account: the acme allow is invisible (the other-account deny
	// is the only visible grant), so the result is a deny.
	if d := f.check(acctOther, "alice", "read", "document:42"); d.Allow {
		t.Fatalf("acme allow must not apply in the other account")
	}
	// A third account with no grants at all: default deny.
	if d := f.check("third", "alice", "read", "document:42"); d.Allow {
		t.Fatalf("account with no grants must default-deny")
	}
}

// A grant stamped to the all-accounts wildcard reads documents in EVERY account,
// including accounts that hold no grants of their own — the reported "see
// documents in all accounts with a single grant" case.
func TestCheck_WildcardAccountGrantSpansAllAccounts(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g-star", model.AccountWildcard, subjPrincipal("alice"), model.EffectAllow, permRead, "**")

	for _, account := range []string{acctAcme, acctOther, "brand-new"} {
		d := f.check(account, "alice", "read", "document:42")
		if !d.Allow {
			t.Fatalf("wildcard grant should allow read in %q, got deny (%s)", account, d.Reason)
		}
		if len(d.DecidingGrantIDs) != 1 || d.DecidingGrantIDs[0] != "g-star" {
			t.Fatalf("deciding grants in %q = %v, want [g-star]", account, d.DecidingGrantIDs)
		}
	}

	// A different principal is unaffected — the wildcard widens accounts, not subjects.
	f.principal("bob")
	if d := f.check(acctAcme, "bob", "read", "document:42"); d.Allow {
		t.Fatalf("wildcard grant for alice must not allow bob")
	}
}

// --- Subject-set expansion: roles and groups ---

func TestCheck_RoleExpansion(t *testing.T) {
	f := newFixture(t)
	f.role("editor")
	f.principal("alice", "editor")
	// Grant bound to the role, not the principal directly.
	f.grant("g-role", acctAcme, subjRole("editor"), model.EffectAllow, permWrite, "document:42")

	if d := f.check(acctAcme, "alice", "write", "document:42"); !d.Allow {
		t.Fatalf("role grant should authorize the assigned principal, got deny (%s)", d.Reason)
	}
	// A principal without the role must not benefit.
	f.principal("bob")
	if d := f.check(acctAcme, "bob", "write", "document:42"); d.Allow {
		t.Fatalf("principal without the role must not inherit the role grant")
	}
}

func TestCheck_GroupExpansion(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.group("team", "alice")
	f.grant("g-group", acctAcme, subjGroup("team"), model.EffectAllow, permRead, "document:42")

	if d := f.check(acctAcme, "alice", "read", "document:42"); !d.Allow {
		t.Fatalf("group grant should authorize a member, got deny (%s)", d.Reason)
	}
	// A non-member must not benefit.
	f.principal("carol")
	if d := f.check(acctAcme, "carol", "read", "document:42"); d.Allow {
		t.Fatalf("non-member must not inherit the group grant")
	}
}

// A group deny overrides a direct principal allow at equal specificity.
func TestCheck_GroupDenyOverridesPrincipalAllow(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.group("blocked", "alice")
	f.grant("p-allow", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")
	f.grant("g-deny", acctAcme, subjGroup("blocked"), model.EffectDeny, permRead, "document:42")

	if d := f.check(acctAcme, "alice", "read", "document:42"); d.Allow {
		t.Fatalf("group deny must override the principal allow at equal specificity (%s)", d.Reason)
	}
}

// --- Error paths ---

func TestCheck_UnknownPrincipalErrors(t *testing.T) {
	f := newFixture(t)
	_, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "ghost", Action: "read", Object: "document:42",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("unknown principal: code = %q, want APERTURE_NOT_FOUND", code)
	}
}

func TestCheck_InvalidObjectErrors(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	_, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "not a valid identity",
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("malformed object: code = %q, want APERTURE_IDENTITY_INVALID", code)
	}
}

func TestCheck_MissingFieldsError(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	cases := map[string]Request{
		"no account":   {Principal: "alice", Action: "read", Object: "document:42"},
		"no principal": {Account: acctAcme, Action: "read", Object: "document:42"},
		"no action":    {Account: acctAcme, Principal: "alice", Object: "document:42"},
		"no object":    {Account: acctAcme, Principal: "alice", Action: "read"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.eng.Check(context.Background(), req)
			if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("%s: code = %q, want APERTURE_INVALID_INPUT", name, code)
			}
		})
	}
}

// A grant whose permission has been deleted is inert, not a crash, and yields a
// clean default-deny when it was the only candidate.
func TestCheck_DanglingPermissionIsInert(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")
	if err := f.store.DeletePermission(context.Background(), permRead); err != nil {
		t.Fatalf("delete permission: %v", err)
	}
	d := f.check(acctAcme, "alice", "read", "document:42")
	if d.Allow {
		t.Fatalf("grant with a deleted permission must not authorize, got allow (%s)", d.Reason)
	}
}

// Reason names the deciding grant so Explain (E2-S4) has a seed.
func TestCheck_ReasonNamesDecidingGrant(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g-decider", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")
	d := f.check(acctAcme, "alice", "read", "document:42")
	if want := "g-decider"; !contains(d.Reason, want) {
		t.Fatalf("reason %q should name the deciding grant %q", d.Reason, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
