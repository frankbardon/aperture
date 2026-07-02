package service

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// readScopeFixture seeds two customer accounts (visa, dish), a platform admin
// (root, "*" admin), a visa account-admin (vmgr), and a plain user (nobody).
func readScopeFixture(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))

	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "account", Actions: []string{authz.AdminAction}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "sysadmin", ObjectType: "system", Action: authz.AdminAction}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "acctadmin", ObjectType: "account", Action: authz.AdminAction}))

	must(t, store.PutAccount(ctx, model.Account{ID: "visa", Name: "VISA"}))
	must(t, store.PutAccount(ctx, model.Account{ID: "dish", Name: "Dish"}))

	for _, p := range []string{"root", "vmgr", "vuser", "duser", "nobody"} {
		must(t, store.PutPrincipal(ctx, model.Principal{ID: p, Kind: model.PrincipalUser, Identity: "user:" + p}))
	}
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "vmgr", AccountID: "visa"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "vuser", AccountID: "visa"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "duser", AccountID: "dish"}))

	// root: platform system-admin (stamped to "*").
	must(t, store.PutGrant(ctx, model.Grant{ID: "g-root", AccountID: model.AccountWildcard,
		Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "root"}, PermissionID: "sysadmin",
		Object: "system:schema", Effect: model.EffectAllow}))
	// vmgr: account-admin of visa only.
	must(t, store.PutGrant(ctx, model.Grant{ID: "g-vmgr", AccountID: "visa",
		Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "vmgr"}, PermissionID: "acctadmin",
		Object: "account:visa/admin:all", Effect: model.EffectAllow}))
	// a data grant in each account so ListGrants has something to scope.
	must(t, store.PutGrant(ctx, model.Grant{ID: "g-visa-data", AccountID: "visa",
		Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "vuser"}, PermissionID: "acctadmin",
		Object: "account:visa/x:1", Effect: model.EffectAllow}))

	eng := engine.New(store)
	return New(eng, WithStorage(store), WithGate(authz.NewGate(eng)))
}

func ids(accs []model.Account) map[string]bool {
	m := map[string]bool{}
	for _, a := range accs {
		m[a.ID] = true
	}
	return m
}

func TestReadScope_SystemAdminSeesAll(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	root := Actor{Principal: "root"}

	accs, err := s.ListAccounts(ctx, root)
	if err != nil || !ids(accs)["visa"] || !ids(accs)["dish"] {
		t.Fatalf("system-admin ListAccounts = %v, err %v; want visa+dish", ids(accs), err)
	}
	ps, err := s.ListPrincipals(ctx, root)
	if err != nil || len(ps) != 5 {
		t.Fatalf("system-admin ListPrincipals = %d, err %v; want 5", len(ps), err)
	}
	if _, err := s.ListGrants(ctx, root, "dish"); err != nil {
		t.Fatalf("system-admin ListGrants(dish): %v", err)
	}
}

func TestReadScope_AccountAdminScopedToOwnAccount(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	vmgr := Actor{Principal: "vmgr"}

	// Accounts: only visa.
	accs, err := s.ListAccounts(ctx, vmgr)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if ids(accs)["dish"] || !ids(accs)["visa"] || len(accs) != 1 {
		t.Fatalf("account-admin ListAccounts = %v; want visa only", ids(accs))
	}

	// Grants: own account ok, other account + "*" denied.
	if _, err := s.ListGrants(ctx, vmgr, "visa"); err != nil {
		t.Fatalf("ListGrants(visa) for its admin: %v", err)
	}
	if _, err := s.ListGrants(ctx, vmgr, "dish"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("ListGrants(dish) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}
	if _, err := s.ListGrants(ctx, vmgr, model.AccountWildcard); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("ListGrants(*) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}

	// Principals: only visa members + self, never dish's user.
	ps, err := s.ListPrincipals(ctx, vmgr)
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range ps {
		seen[p.ID] = true
	}
	if !seen["vmgr"] || !seen["vuser"] || seen["duser"] || seen["root"] {
		t.Fatalf("account-admin ListPrincipals = %v; want visa members + self only", seen)
	}
}

func TestReadScope_MemberSeesOwnAccount(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	// vuser is a plain member of visa (no admin grant) — under the broadened rule
	// it sees visa (and only visa).
	vuser := Actor{Principal: "vuser"}

	accs, err := s.ListAccounts(ctx, vuser)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if !ids(accs)["visa"] || ids(accs)["dish"] || len(accs) != 1 {
		t.Fatalf("member ListAccounts = %v; want visa only", ids(accs))
	}
	if _, err := s.ListGrants(ctx, vuser, "visa"); err != nil {
		t.Fatalf("member ListGrants(visa): %v", err)
	}
	if _, err := s.ListGrants(ctx, vuser, "dish"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("member ListGrants(dish) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}
}

func TestReadScope_NonMemberSeesNothing(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	nobody := Actor{Principal: "nobody"} // no membership, no admin grant

	accs, err := s.ListAccounts(ctx, nobody)
	if err != nil || len(accs) != 0 {
		t.Fatalf("non-member ListAccounts = %d, err %v; want 0", len(accs), err)
	}
	ps, _ := s.ListPrincipals(ctx, nobody)
	if len(ps) != 1 || ps[0].ID != "nobody" {
		t.Fatalf("non-member ListPrincipals = %v; want [nobody]", ps)
	}
	if _, err := s.ListGrants(ctx, nobody, "visa"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-member ListGrants(visa) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}
}

func TestReadScope_NoGateOrNoPrincipalIsUnrestricted(t *testing.T) {
	// No gate wired (local CLI facade): unrestricted even with no principal.
	store := memory.New()
	must(t, store.Setup(context.Background()))
	must(t, store.PutAccount(context.Background(), model.Account{ID: "a", Name: "A"}))
	s := New(engine.New(store), WithStorage(store))
	accs, err := s.ListAccounts(context.Background(), Actor{})
	if err != nil || len(accs) != 1 {
		t.Fatalf("no-gate ListAccounts = %d, err %v; want 1 (unrestricted)", len(accs), err)
	}
}
