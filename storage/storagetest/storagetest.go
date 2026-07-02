// Package storagetest is the shared conformance suite for model.Storage. Both
// backends — storage/memory and storage/sqlite — run Run against a fresh store
// so the two implementations are held to one identical contract: CRUD round
// trips, NOT_FOUND semantics, typed-action validation, account-scoped grant
// queries, and group-membership resolution.
//
// It lives in its own package (imported only from _test.go files) so it never
// becomes part of either backend's production surface.
package storagetest

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// Factory builds a fresh, Setup-completed store for one subtest. The
// implementation is responsible for registering cleanup (t.Cleanup) to close the
// store and release resources.
type Factory func(t *testing.T) model.Storage

// Run executes the full conformance suite against stores produced by newStore.
// Each subtest gets its own store so the cases are independent and order-free.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	t.Run("AccountCRUD", func(t *testing.T) { testAccountCRUD(t, newStore(t)) })
	t.Run("MembershipCRUDAndQueries", func(t *testing.T) { testMembershipCRUDAndQueries(t, newStore(t)) })
	t.Run("ObjectTypeCRUD", func(t *testing.T) { testObjectTypeCRUD(t, newStore(t)) })
	t.Run("PermissionTypedAction", func(t *testing.T) { testPermissionTypedAction(t, newStore(t)) })
	t.Run("PermissionUnknownObjectType", func(t *testing.T) { testPermissionUnknownObjectType(t, newStore(t)) })
	t.Run("PermissionDelegatable", func(t *testing.T) { testPermissionDelegatable(t, newStore(t)) })
	t.Run("PrincipalCRUD", func(t *testing.T) { testPrincipalCRUD(t, newStore(t)) })
	t.Run("RoleCRUD", func(t *testing.T) { testRoleCRUD(t, newStore(t)) })
	t.Run("GroupCRUD", func(t *testing.T) { testGroupCRUD(t, newStore(t)) })
	t.Run("GrantCRUDAndUpsert", func(t *testing.T) { testGrantCRUDAndUpsert(t, newStore(t)) })
	t.Run("GrantValidation", func(t *testing.T) { testGrantValidation(t, newStore(t)) })
	t.Run("ListGrantsAccountScoped", func(t *testing.T) { testListGrantsAccountScoped(t, newStore(t)) })
	t.Run("GrantsForSubjects", func(t *testing.T) { testGrantsForSubjects(t, newStore(t)) })
	t.Run("GrantsForSubjectsWildcardAccount", func(t *testing.T) { testGrantsForSubjectsWildcardAccount(t, newStore(t)) })
	t.Run("GroupsForPrincipal", func(t *testing.T) { testGroupsForPrincipal(t, newStore(t)) })
	t.Run("NotFoundSemantics", func(t *testing.T) { testNotFoundSemantics(t, newStore(t)) })
	t.Run("TimestampsRoundTrip", func(t *testing.T) { testTimestampsRoundTrip(t, newStore(t)) })
	t.Run("AuditAppendAndQuery", func(t *testing.T) { testAuditAppendAndQuery(t, newStore(t)) })
	t.Run("AuditQueryFilters", func(t *testing.T) { testAuditQueryFilters(t, newStore(t)) })
	t.Run("AuditRetentionPrune", func(t *testing.T) { testAuditRetentionPrune(t, newStore(t)) })
	t.Run("TemplateCRUDAndVersions", func(t *testing.T) { testTemplateCRUDAndVersions(t, newStore(t)) })
	t.Run("TemplateValidation", func(t *testing.T) { testTemplateValidation(t, newStore(t)) })
	t.Run("RuleCRUD", func(t *testing.T) { testRuleCRUD(t, newStore(t)) })
	t.Run("RuleValidation", func(t *testing.T) { testRuleValidation(t, newStore(t)) })
	t.Run("AtomicCommit", func(t *testing.T) { testAtomicCommit(t, newStore(t)) })
	t.Run("AtomicRollback", func(t *testing.T) { testAtomicRollback(t, newStore(t)) })
}

func ctx() context.Context { return context.Background() }

func mustCode(t *testing.T, err error, want aerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("error code = %s, want %s (err: %v)", got, want, err)
	}
}

// seedDocumentType creates the canonical "document" object type used across the
// permission and grant cases.
func seedDocumentType(t *testing.T, s model.Storage) {
	t.Helper()
	ot := model.ObjectType{Name: "document", Actions: []string{"read", "write", "delete"}}
	if err := s.PutObjectType(ctx(), ot); err != nil {
		t.Fatalf("seed object type: %v", err)
	}
}

func testAccountCRUD(t *testing.T, s model.Storage) {
	a := model.Account{ID: "acme", Name: "Acme Corp", Description: "the demo tenant"}
	if err := s.PutAccount(ctx(), a); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetAccount(ctx(), "acme")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normAccount(got), normAccount(a)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, a)
	}

	// Upsert replaces the name.
	a.Name = "Acme Incorporated"
	if err := s.PutAccount(ctx(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetAccount(ctx(), "acme")
	if got.Name != "Acme Incorporated" {
		t.Fatalf("upsert did not replace name: %q", got.Name)
	}

	if err := s.PutAccount(ctx(), model.Account{ID: "other", Name: "Other"}); err != nil {
		t.Fatalf("put other: %v", err)
	}
	list, err := s.ListAccounts(ctx())
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v (len %d), err %v", list, len(list), err)
	}

	if err := s.DeleteAccount(ctx(), "acme"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetAccount(ctx(), "acme"); return e }(), aerr.APERTURE_NOT_FOUND)

	// An account with no name is rejected.
	mustCode(t, s.PutAccount(ctx(), model.Account{ID: "noname"}), aerr.APERTURE_INVALID_INPUT)
	// An account with no id is rejected.
	mustCode(t, s.PutAccount(ctx(), model.Account{Name: "noid"}), aerr.APERTURE_INVALID_INPUT)
	// The reserved "*" id (the all-accounts grant wildcard) cannot be a real account.
	mustCode(t, s.PutAccount(ctx(), model.Account{ID: model.AccountWildcard, Name: "star"}), aerr.APERTURE_INVALID_INPUT)
}

func testMembershipCRUDAndQueries(t *testing.T, s model.Storage) {
	put := func(principalID, accountID string) {
		if err := s.PutMembership(ctx(), model.Membership{PrincipalID: principalID, AccountID: accountID}); err != nil {
			t.Fatalf("put membership %s@%s: %v", principalID, accountID, err)
		}
	}
	// alice spans two accounts; bob is only in acme.
	put("alice", "acme")
	put("alice", "other")
	put("bob", "acme")

	// IsMember reflects the edges.
	for _, tc := range []struct {
		principal, account string
		want               bool
	}{
		{"alice", "acme", true},
		{"alice", "other", true},
		{"bob", "acme", true},
		{"bob", "other", false},  // bob was never admitted to other
		{"carol", "acme", false}, // carol has no memberships at all
	} {
		got, err := s.IsMember(ctx(), tc.principal, tc.account)
		if err != nil {
			t.Fatalf("IsMember(%s,%s): %v", tc.principal, tc.account, err)
		}
		if got != tc.want {
			t.Fatalf("IsMember(%s,%s) = %v, want %v", tc.principal, tc.account, got, tc.want)
		}
	}

	// GetMembership returns the edge, or NOT_FOUND for a non-edge.
	if _, err := s.GetMembership(ctx(), "alice", "acme"); err != nil {
		t.Fatalf("get membership: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetMembership(ctx(), "bob", "other"); return e }(), aerr.APERTURE_NOT_FOUND)

	// MembershipsForPrincipal: alice is in two accounts.
	am, err := s.MembershipsForPrincipal(ctx(), "alice")
	if err != nil {
		t.Fatalf("memberships for principal: %v", err)
	}
	if accs := accountSet(am); len(accs) != 2 || !accs["acme"] || !accs["other"] {
		t.Fatalf("alice memberships = %v, want {acme, other}", accs)
	}

	// MembershipsForAccount: acme has two members.
	acme, err := s.MembershipsForAccount(ctx(), "acme")
	if err != nil {
		t.Fatalf("memberships for account: %v", err)
	}
	if ps := principalSet(acme); len(ps) != 2 || !ps["alice"] || !ps["bob"] {
		t.Fatalf("acme members = %v, want {alice, bob}", ps)
	}

	// Deleting one edge leaves the other intact (isolation between edges).
	if err := s.DeleteMembership(ctx(), "alice", "other"); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	if ok, _ := s.IsMember(ctx(), "alice", "other"); ok {
		t.Fatal("deleted membership still reported as member")
	}
	if ok, _ := s.IsMember(ctx(), "alice", "acme"); !ok {
		t.Fatal("deleting alice@other wrongly removed alice@acme")
	}
	mustCode(t, s.DeleteMembership(ctx(), "alice", "other"), aerr.APERTURE_NOT_FOUND)

	// Validation: both endpoints are required.
	mustCode(t, s.PutMembership(ctx(), model.Membership{AccountID: "acme"}), aerr.APERTURE_INVALID_INPUT)
	mustCode(t, s.PutMembership(ctx(), model.Membership{PrincipalID: "alice"}), aerr.APERTURE_INVALID_INPUT)

	// Empty queries return empty, not error.
	none, err := s.MembershipsForPrincipal(ctx(), "nobody")
	if err != nil || len(none) != 0 {
		t.Fatalf("nobody memberships = %v (len %d), err %v", none, len(none), err)
	}
}

func accountSet(ms []model.Membership) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.AccountID] = true
	}
	return out
}

func principalSet(ms []model.Membership) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.PrincipalID] = true
	}
	return out
}

func testObjectTypeCRUD(t *testing.T, s model.Storage) {
	ot := model.ObjectType{Name: "document", Actions: []string{"read", "write"}, Description: "a doc"}
	if err := s.PutObjectType(ctx(), ot); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetObjectType(ctx(), "document")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normObjectType(got), normObjectType(ot)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, ot)
	}

	// Upsert replaces the verb set.
	ot.Actions = []string{"read", "write", "delete"}
	if err := s.PutObjectType(ctx(), ot); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetObjectType(ctx(), "document")
	if !got.HasAction("delete") {
		t.Fatalf("upsert did not replace verb set: %+v", got.Actions)
	}

	list, err := s.ListObjectTypes(ctx())
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (len %d), err %v", list, len(list), err)
	}

	if err := s.DeleteObjectType(ctx(), "document"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetObjectType(ctx(), "document"); return e }(), aerr.APERTURE_NOT_FOUND)

	// Empty verb set is rejected.
	mustCode(t, s.PutObjectType(ctx(), model.ObjectType{Name: "empty"}), aerr.APERTURE_INVALID_INPUT)
}

func testPermissionTypedAction(t *testing.T, s model.Storage) {
	seedDocumentType(t, s)

	// Declared action accepted.
	good := model.Permission{ID: "p-read", ObjectType: "document", Action: "read", ScopeStrategy: "implicit"}
	if err := s.PutPermission(ctx(), good); err != nil {
		t.Fatalf("declared action rejected: %v", err)
	}
	got, err := s.GetPermission(ctx(), "p-read")
	if err != nil {
		t.Fatalf("get permission: %v", err)
	}
	if !reflect.DeepEqual(normPermission(got), normPermission(good)) {
		t.Fatalf("permission round trip mismatch:\n got %+v\nwant %+v", got, good)
	}

	// Undeclared action rejected with the typed-action code.
	bad := model.Permission{ID: "p-publish", ObjectType: "document", Action: "publish"}
	mustCode(t, s.PutPermission(ctx(), bad), aerr.APERTURE_ACTION_UNDECLARED)
	// And it must not have been persisted.
	mustCode(t, func() error { _, e := s.GetPermission(ctx(), "p-publish"); return e }(), aerr.APERTURE_NOT_FOUND)

	if err := s.DeletePermission(ctx(), "p-read"); err != nil {
		t.Fatalf("delete permission: %v", err)
	}
}

// testPermissionDelegatable round-trips the delegatable flag (E3-S2) through the
// backend: it must persist true and default to false, identically on both
// stores.
func testPermissionDelegatable(t *testing.T, s model.Storage) {
	seedDocumentType(t, s)

	// Flag set: must survive the round trip.
	on := model.Permission{ID: "p-deleg", ObjectType: "document", Action: "read", Delegatable: true}
	if err := s.PutPermission(ctx(), on); err != nil {
		t.Fatalf("put delegatable permission: %v", err)
	}
	got, err := s.GetPermission(ctx(), "p-deleg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Delegatable {
		t.Fatalf("delegatable flag not preserved: %+v", got)
	}

	// Flag unset: defaults to false.
	off := model.Permission{ID: "p-plain", ObjectType: "document", Action: "write"}
	if err := s.PutPermission(ctx(), off); err != nil {
		t.Fatalf("put non-delegatable permission: %v", err)
	}
	got, err = s.GetPermission(ctx(), "p-plain")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Delegatable {
		t.Fatalf("delegatable defaulted to true: %+v", got)
	}
}

func testPermissionUnknownObjectType(t *testing.T, s model.Storage) {
	// Referencing an object type that does not exist is NOT_FOUND.
	p := model.Permission{ID: "p1", ObjectType: "ghost", Action: "read"}
	mustCode(t, s.PutPermission(ctx(), p), aerr.APERTURE_NOT_FOUND)
}

func testPrincipalCRUD(t *testing.T, s model.Storage) {
	p := model.Principal{
		ID:          "alice",
		Kind:        model.PrincipalUser,
		Identity:    "user:alice",
		DisplayName: "Alice",
		RoleIDs:     []string{"r-admin", "r-editor"},
	}
	if err := s.PutPrincipal(ctx(), p); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetPrincipal(ctx(), "alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normPrincipal(got), normPrincipal(p)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, p)
	}

	// Invalid kind rejected.
	mustCode(t, s.PutPrincipal(ctx(), model.Principal{ID: "x", Kind: "alien", Identity: "user:x"}), aerr.APERTURE_INVALID_INPUT)
	// Malformed identity rejected with identity code.
	mustCode(t, s.PutPrincipal(ctx(), model.Principal{ID: "x", Kind: model.PrincipalUser, Identity: "no-colon"}), aerr.APERTURE_IDENTITY_INVALID)

	list, err := s.ListPrincipals(ctx())
	if err != nil || len(list) != 1 {
		t.Fatalf("list len %d, err %v", len(list), err)
	}
	if err := s.DeletePrincipal(ctx(), "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func testRoleCRUD(t *testing.T, s model.Storage) {
	r := model.Role{ID: "r-admin", Name: "Administrator", Description: "all", PermissionIDs: []string{"p1", "p2"}}
	if err := s.PutRole(ctx(), r); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetRole(ctx(), "r-admin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normRole(got), normRole(r)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, r)
	}
	// Upsert clears the bundle.
	r.PermissionIDs = nil
	if err := s.PutRole(ctx(), r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetRole(ctx(), "r-admin")
	if len(got.PermissionIDs) != 0 {
		t.Fatalf("upsert did not clear bundle: %+v", got.PermissionIDs)
	}
	mustCode(t, s.PutRole(ctx(), model.Role{ID: "x"}), aerr.APERTURE_INVALID_INPUT) // no name
	if err := s.DeleteRole(ctx(), "r-admin"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func testGroupCRUD(t *testing.T, s model.Storage) {
	g := model.Group{ID: "eng", Name: "Engineering", MemberPrincipalIDs: []string{"alice", "bob"}}
	if err := s.PutGroup(ctx(), g); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetGroup(ctx(), "eng")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normGroup(got), normGroup(g)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, g)
	}
	mustCode(t, s.PutGroup(ctx(), model.Group{ID: "x"}), aerr.APERTURE_INVALID_INPUT) // no name
	if err := s.DeleteGroup(ctx(), "eng"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func testGrantCRUDAndUpsert(t *testing.T, s model.Storage) {
	g := model.Grant{
		ID:           "g1",
		AccountID:    "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read",
		Object:       "account:acme/project:atlas/**",
		Effect:       model.EffectAllow,
	}
	if err := s.PutGrant(ctx(), g); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetGrant(ctx(), "g1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(normGrant(got), normGrant(g)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, g)
	}
	// Upsert flips the effect.
	g.Effect = model.EffectDeny
	if err := s.PutGrant(ctx(), g); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetGrant(ctx(), "g1")
	if got.Effect != model.EffectDeny {
		t.Fatalf("upsert did not flip effect: %s", got.Effect)
	}
	if err := s.DeleteGrant(ctx(), "g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetGrant(ctx(), "g1"); return e }(), aerr.APERTURE_NOT_FOUND)
}

func testGrantValidation(t *testing.T, s model.Storage) {
	// Missing account stamp.
	mustCode(t, s.PutGrant(ctx(), model.Grant{
		ID: "g", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "a"},
		PermissionID: "p", Object: "account:acme", Effect: model.EffectAllow,
	}), aerr.APERTURE_INVALID_INPUT)
	// Bad effect.
	mustCode(t, s.PutGrant(ctx(), model.Grant{
		ID: "g", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "a"},
		PermissionID: "p", Object: "account:acme", Effect: "maybe",
	}), aerr.APERTURE_INVALID_INPUT)
	// Malformed object pattern.
	mustCode(t, s.PutGrant(ctx(), model.Grant{
		ID: "g", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "a"},
		PermissionID: "p", Object: "account:acme/", Effect: model.EffectAllow,
	}), aerr.APERTURE_IDENTITY_INVALID)
}

func testListGrantsAccountScoped(t *testing.T, s model.Storage) {
	seed := func(id, account string) {
		g := model.Grant{
			ID: id, AccountID: account,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-read", Object: "account:" + account + "/**", Effect: model.EffectAllow,
		}
		if err := s.PutGrant(ctx(), g); err != nil {
			t.Fatalf("seed grant %s: %v", id, err)
		}
	}
	seed("g-acme-1", "acme")
	seed("g-acme-2", "acme")
	seed("g-other-1", "other")

	acme, err := s.ListGrants(ctx(), "acme")
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(acme) != 2 {
		t.Fatalf("acme grants = %d, want 2", len(acme))
	}
	for _, g := range acme {
		if g.AccountID != "acme" {
			t.Fatalf("cross-account leak: grant %s stamped %s in acme list", g.ID, g.AccountID)
		}
	}
	other, _ := s.ListGrants(ctx(), "other")
	if len(other) != 1 {
		t.Fatalf("other grants = %d, want 1", len(other))
	}
	none, _ := s.ListGrants(ctx(), "ghost")
	if len(none) != 0 {
		t.Fatalf("ghost grants = %d, want 0", len(none))
	}
}

func testGrantsForSubjects(t *testing.T, s model.Storage) {
	put := func(id, account string, sub model.Subject) {
		g := model.Grant{
			ID: id, AccountID: account, Subject: sub,
			PermissionID: "p-read", Object: "account:" + account + "/**", Effect: model.EffectAllow,
		}
		if err := s.PutGrant(ctx(), g); err != nil {
			t.Fatalf("put grant %s: %v", id, err)
		}
	}
	put("g-alice", "acme", model.Subject{Kind: model.SubjectPrincipal, ID: "alice"})
	put("g-eng", "acme", model.Subject{Kind: model.SubjectGroup, ID: "eng"})
	put("g-admin", "acme", model.Subject{Kind: model.SubjectRole, ID: "admin"})
	put("g-bob", "acme", model.Subject{Kind: model.SubjectPrincipal, ID: "bob"})
	// Same subject, different account — must never be returned for acme.
	put("g-alice-other", "other", model.Subject{Kind: model.SubjectPrincipal, ID: "alice"})

	subjects := []model.Subject{
		{Kind: model.SubjectPrincipal, ID: "alice"},
		{Kind: model.SubjectGroup, ID: "eng"},
		{Kind: model.SubjectRole, ID: "admin"},
	}
	got, err := s.GrantsForSubjects(ctx(), "acme", subjects)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, g := range got {
		gotIDs[g.ID] = true
		if g.AccountID != "acme" {
			t.Fatalf("cross-account grant %s (%s) returned", g.ID, g.AccountID)
		}
	}
	for _, want := range []string{"g-alice", "g-eng", "g-admin"} {
		if !gotIDs[want] {
			t.Fatalf("missing grant %s; got %v", want, gotIDs)
		}
	}
	if gotIDs["g-bob"] {
		t.Fatal("returned grant for unrequested subject bob")
	}
	if gotIDs["g-alice-other"] {
		t.Fatal("cross-account isolation breach: g-alice-other returned for acme")
	}

	// Empty subject set yields nothing.
	empty, _ := s.GrantsForSubjects(ctx(), "acme", nil)
	if len(empty) != 0 {
		t.Fatalf("empty subjects returned %d grants", len(empty))
	}
}

// testGrantsForSubjectsWildcardAccount pins the one deliberate hole in account
// isolation: a grant stamped to model.AccountWildcard ("*") is loaded for every
// active account, alongside that account's own grants, while account-specific
// grants stay confined.
func testGrantsForSubjectsWildcardAccount(t *testing.T, s model.Storage) {
	alice := model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}
	put := func(id, account, object string) {
		g := model.Grant{
			ID: id, AccountID: account, Subject: alice,
			PermissionID: "p-read", Object: object, Effect: model.EffectAllow,
		}
		if err := s.PutGrant(ctx(), g); err != nil {
			t.Fatalf("put grant %s: %v", id, err)
		}
	}
	put("g-acme", "acme", "account:acme/**")   // account-specific
	put("g-star", model.AccountWildcard, "**") // spans every account

	subjects := []model.Subject{alice}
	ids := func(account string) map[string]bool {
		got, err := s.GrantsForSubjects(ctx(), account, subjects)
		if err != nil {
			t.Fatalf("query %s: %v", account, err)
		}
		out := map[string]bool{}
		for _, g := range got {
			out[g.ID] = true
		}
		return out
	}

	// In acme: both the account's own grant and the wildcard are returned.
	acme := ids("acme")
	if !acme["g-acme"] || !acme["g-star"] {
		t.Fatalf("acme grants = %v, want g-acme and g-star", acme)
	}
	// In an account with NO grants of its own: only the wildcard is returned, and
	// acme's account-specific grant never leaks.
	fresh := ids("brand-new-account")
	if !fresh["g-star"] {
		t.Fatalf("wildcard grant not applied to fresh account; got %v", fresh)
	}
	if fresh["g-acme"] {
		t.Fatalf("account-specific grant leaked across accounts; got %v", fresh)
	}
}

func testGroupsForPrincipal(t *testing.T, s model.Storage) {
	put := func(id string, members ...string) {
		if err := s.PutGroup(ctx(), model.Group{ID: id, Name: id, MemberPrincipalIDs: members}); err != nil {
			t.Fatalf("put group %s: %v", id, err)
		}
	}
	put("eng", "alice", "bob")
	put("ops", "bob")
	put("sales", "carol")

	got, err := s.GroupsForPrincipal(ctx(), "bob")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.ID] = true
	}
	if !ids["eng"] || !ids["ops"] || ids["sales"] {
		t.Fatalf("bob groups = %v, want {eng, ops}", ids)
	}
	none, _ := s.GroupsForPrincipal(ctx(), "nobody")
	if len(none) != 0 {
		t.Fatalf("nobody groups = %d, want 0", len(none))
	}
}

func testNotFoundSemantics(t *testing.T, s model.Storage) {
	mustCode(t, func() error { _, e := s.GetAccount(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetMembership(ctx(), "x", "y"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetObjectType(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetPermission(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetPrincipal(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetRole(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetGroup(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetGrant(ctx(), "x"); return e }(), aerr.APERTURE_NOT_FOUND)

	mustCode(t, s.DeleteAccount(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteMembership(ctx(), "x", "y"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteObjectType(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeletePermission(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeletePrincipal(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteRole(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteGroup(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteGrant(ctx(), "x"), aerr.APERTURE_NOT_FOUND)
}

func testTimestampsRoundTrip(t *testing.T, s model.Storage) {
	seedDocumentType(t, s)
	created := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)
	updated := created.Add(time.Hour)
	p := model.Permission{
		ID: "p-ts", ObjectType: "document", Action: "read",
		CreatedAt: created, UpdatedAt: updated,
	}
	if err := s.PutPermission(ctx(), p); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetPermission(ctx(), "p-ts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
		t.Fatalf("timestamps not preserved: created %v (want %v), updated %v (want %v)",
			got.CreatedAt, created, got.UpdatedAt, updated)
	}
}

// ---- Audit trail ----

// auditBase is a reference instant the audit cases stamp events relative to, so
// ordering and time-range filters are deterministic across both backends.
var auditBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func mkAudit(id string, offset time.Duration, mut func(*model.AuditEvent)) model.AuditEvent {
	ev := model.AuditEvent{
		ID:        id,
		Timestamp: auditBase.Add(offset),
		EventType: model.AuditMutation,
		Action:    "PutGrant",
		Actor:     "alice",
		Account:   "acme",
		Target:    "grant:g1",
		Outcome:   model.OutcomeSuccess,
		Reason:    "ok",
	}
	if mut != nil {
		mut(&ev)
	}
	return ev
}

func testAuditAppendAndQuery(t *testing.T, s model.Storage) {
	// A round trip preserving every field, including the impersonation linkage
	// (real actor + effective subject + mode) and the details JSON blob.
	ev := mkAudit("a1", 0, func(e *model.AuditEvent) {
		e.EventType = model.AuditDecision
		e.Action = "Check"
		e.Actor = "operator"
		e.EffectiveSubject = "target"
		e.ImpersonationMode = "become"
		e.Outcome = model.OutcomeAllow
		e.Target = "account:acme/document:42"
		e.Details = map[string]any{"deciding": "g7"}
	})
	if err := s.AppendAudit(ctx(), ev); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.QueryAudit(ctx(), model.AuditFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	g := got[0]
	if g.ID != "a1" || g.Actor != "operator" || g.EffectiveSubject != "target" ||
		g.ImpersonationMode != "become" || g.Outcome != model.OutcomeAllow ||
		g.EventType != model.AuditDecision || g.Target != "account:acme/document:42" {
		t.Fatalf("round trip mismatch: %+v", g)
	}
	if !g.Timestamp.Equal(ev.Timestamp) {
		t.Fatalf("timestamp not preserved: got %v want %v", g.Timestamp, ev.Timestamp)
	}
	if g.Details["deciding"] != "g7" {
		t.Fatalf("details not preserved: %+v", g.Details)
	}
}

func testAuditQueryFilters(t *testing.T, s model.Storage) {
	events := []model.AuditEvent{
		mkAudit("e1", 0, func(e *model.AuditEvent) {
			e.Actor = "alice"
			e.Account = "acme"
			e.EventType = model.AuditMutation
			e.Outcome = model.OutcomeSuccess
		}),
		mkAudit("e2", time.Minute, func(e *model.AuditEvent) {
			e.Actor = "bob"
			e.Account = "acme"
			e.EventType = model.AuditDecision
			e.Outcome = model.OutcomeDeny
		}),
		mkAudit("e3", 2*time.Minute, func(e *model.AuditEvent) {
			e.Actor = "alice"
			e.Account = "other"
			e.EventType = model.AuditDecision
			e.Outcome = model.OutcomeAllow
		}),
		mkAudit("e4", 3*time.Minute, func(e *model.AuditEvent) {
			e.Actor = "alice"
			e.Account = "acme"
			e.EventType = model.AuditMutation
			e.Outcome = model.OutcomeFailure
		}),
	}
	for _, ev := range events {
		if err := s.AppendAudit(ctx(), ev); err != nil {
			t.Fatalf("append %s: %v", ev.ID, err)
		}
	}

	// Newest-first ordering across the whole trail.
	all, err := s.QueryAudit(ctx(), model.AuditFilter{})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if ids := auditIDs(all); !reflect.DeepEqual(ids, []string{"e4", "e3", "e2", "e1"}) {
		t.Fatalf("ordering = %v, want newest-first [e4 e3 e2 e1]", ids)
	}

	// Filter by actor.
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{Actor: "alice"})); !sameSet(ids, "e1", "e3", "e4") {
		t.Fatalf("actor filter = %v, want {e1,e3,e4}", ids)
	}
	// Filter by account.
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{Account: "acme"})); !sameSet(ids, "e1", "e2", "e4") {
		t.Fatalf("account filter = %v, want {e1,e2,e4}", ids)
	}
	// Filter by event type.
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{EventType: model.AuditDecision})); !sameSet(ids, "e2", "e3") {
		t.Fatalf("event-type filter = %v, want {e2,e3}", ids)
	}
	// Filter by outcome.
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{Outcome: model.OutcomeFailure})); !sameSet(ids, "e4") {
		t.Fatalf("outcome filter = %v, want {e4}", ids)
	}
	// Time range: [base+1m, base+3m) → e2, e3 (e4 at +3m is excluded by Until).
	rng := mustQuery(t, s, model.AuditFilter{Since: auditBase.Add(time.Minute), Until: auditBase.Add(3 * time.Minute)})
	if ids := auditIDs(rng); !sameSet(ids, "e2", "e3") {
		t.Fatalf("time-range filter = %v, want {e2,e3}", ids)
	}
	// Combined filter + limit.
	combined := mustQuery(t, s, model.AuditFilter{Actor: "alice", Account: "acme", Limit: 1})
	if len(combined) != 1 || combined[0].ID != "e4" {
		t.Fatalf("combined+limit = %v, want [e4]", auditIDs(combined))
	}
}

func testAuditRetentionPrune(t *testing.T, s model.Storage) {
	for i := 0; i < 5; i++ {
		ev := mkAudit(string(rune('a'+i)), time.Duration(i)*time.Minute, nil)
		if err := s.AppendAudit(ctx(), ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Age prune: drop everything strictly older than base+2m (removes a, b).
	removed, err := s.PruneAudit(ctx(), model.RetentionPolicy{Before: auditBase.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("prune by age: %v", err)
	}
	if removed != 2 {
		t.Fatalf("age prune removed %d, want 2", removed)
	}
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{})); !reflect.DeepEqual(ids, []string{"e", "d", "c"}) {
		t.Fatalf("after age prune = %v, want [e d c]", ids)
	}

	// Size prune: keep only the 2 newest (removes c).
	removed, err = s.PruneAudit(ctx(), model.RetentionPolicy{MaxCount: 2})
	if err != nil {
		t.Fatalf("prune by size: %v", err)
	}
	if removed != 1 {
		t.Fatalf("size prune removed %d, want 1", removed)
	}
	if ids := auditIDs(mustQuery(t, s, model.AuditFilter{})); !reflect.DeepEqual(ids, []string{"e", "d"}) {
		t.Fatalf("after size prune = %v, want [e d]", ids)
	}

	// A no-op policy removes nothing.
	removed, _ = s.PruneAudit(ctx(), model.RetentionPolicy{})
	if removed != 0 {
		t.Fatalf("empty policy removed %d, want 0", removed)
	}
}

func mustQuery(t *testing.T, s model.Storage, f model.AuditFilter) []model.AuditEvent {
	t.Helper()
	out, err := s.QueryAudit(ctx(), f)
	if err != nil {
		t.Fatalf("query %+v: %v", f, err)
	}
	return out
}

func auditIDs(evs []model.AuditEvent) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// ---- Template (named, versioned) ----

func sampleTemplate(name string, version int) model.Template {
	return model.Template{
		Name:        name,
		Version:     version,
		Description: "provision a project member",
		Params: []model.TemplateParam{
			{Name: "account", Type: model.ParamSegment},
			{Name: "project", Type: model.ParamSegment},
		},
		Grants: []model.TemplateGrant{
			{
				Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "${account}-member"},
				PermissionID: "p-read",
				Object:       "account:${account}/project:${project}/**",
				Effect:       model.EffectAllow,
			},
		},
	}
}

func testTemplateCRUDAndVersions(t *testing.T, s model.Storage) {
	v1 := sampleTemplate("onboard", 1)
	if err := s.PutTemplate(ctx(), v1); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	got, err := s.GetTemplate(ctx(), "onboard", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if !reflect.DeepEqual(normTemplate(got), normTemplate(v1)) {
		t.Fatalf("v1 round trip mismatch:\n got %+v\nwant %+v", got, v1)
	}

	// A second version under the same name coexists with the first.
	v2 := sampleTemplate("onboard", 2)
	v2.Description = "v2"
	if err := s.PutTemplate(ctx(), v2); err != nil {
		t.Fatalf("put v2: %v", err)
	}

	// Latest selection (version <= 0) returns the highest version.
	latest, err := s.GetTemplate(ctx(), "onboard", 0)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.Version != 2 {
		t.Fatalf("latest version = %d, want 2", latest.Version)
	}

	// Explicit version still resolves the older one.
	old, err := s.GetTemplate(ctx(), "onboard", 1)
	if err != nil || old.Version != 1 {
		t.Fatalf("get v1 explicit = %+v, err %v", old, err)
	}

	// List returns both versions ordered by (name, version).
	list, err := s.ListTemplates(ctx())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("list = %+v, want [v1, v2]", list)
	}

	// Delete one specific version: the other survives, latest is now v1.
	if err := s.DeleteTemplate(ctx(), "onboard", 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	latest, err = s.GetTemplate(ctx(), "onboard", 0)
	if err != nil || latest.Version != 1 {
		t.Fatalf("after delete v2, latest = %+v err %v, want v1", latest, err)
	}

	// Upsert replaces a version in place.
	v1b := sampleTemplate("onboard", 1)
	v1b.Description = "edited"
	if err := s.PutTemplate(ctx(), v1b); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	got, _ = s.GetTemplate(ctx(), "onboard", 1)
	if got.Description != "edited" {
		t.Fatalf("upsert did not replace description: %q", got.Description)
	}

	// Delete-all-versions removes the name entirely.
	if err := s.PutTemplate(ctx(), sampleTemplate("onboard", 3)); err != nil {
		t.Fatalf("put v3: %v", err)
	}
	if err := s.DeleteTemplate(ctx(), "onboard", 0); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetTemplate(ctx(), "onboard", 0); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetTemplate(ctx(), "onboard", 1); return e }(), aerr.APERTURE_NOT_FOUND)

	// NOT_FOUND semantics for unknown name/version and deletes.
	mustCode(t, func() error { _, e := s.GetTemplate(ctx(), "ghost", 0); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetTemplate(ctx(), "ghost", 7); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteTemplate(ctx(), "ghost", 0), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteTemplate(ctx(), "ghost", 7), aerr.APERTURE_NOT_FOUND)
}

func testTemplateValidation(t *testing.T, s model.Storage) {
	// No grants → TEMPLATE_INVALID, and nothing persisted.
	bad := model.Template{Name: "bad", Version: 1}
	mustCode(t, s.PutTemplate(ctx(), bad), aerr.APERTURE_TEMPLATE_INVALID)
	mustCode(t, func() error { _, e := s.GetTemplate(ctx(), "bad", 1); return e }(), aerr.APERTURE_NOT_FOUND)

	// A grant referencing an undeclared parameter → TEMPLATE_INVALID.
	undeclared := model.Template{
		Name: "undeclared", Version: 1,
		Grants: []model.TemplateGrant{{
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "u"},
			PermissionID: "p", Object: "account:${missing}/**", Effect: model.EffectAllow,
		}},
	}
	mustCode(t, s.PutTemplate(ctx(), undeclared), aerr.APERTURE_TEMPLATE_INVALID)

	// Version below 1 → TEMPLATE_INVALID.
	mustCode(t, s.PutTemplate(ctx(), sampleTemplate("zero", 0)), aerr.APERTURE_TEMPLATE_INVALID)
}

// ---- Rule (named) ----

func sampleRule(name string) model.Rule {
	return model.Rule{
		Name:        name,
		Description: "select classified documents",
		AST: json.RawMessage(
			`{"type":"compare","op":"eq","left":{"type":"var","name":"object.classification"},` +
				`"right":{"type":"literal","value":"public"}}`),
	}
}

func testRuleCRUD(t *testing.T, s model.Storage) {
	r := sampleRule("public-only")
	if err := s.PutRule(ctx(), r); err != nil {
		t.Fatalf("put rule: %v", err)
	}
	got, err := s.GetRule(ctx(), "public-only")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if got.Name != r.Name || got.Description != r.Description {
		t.Fatalf("rule round trip mismatch: got %+v want %+v", got, r)
	}
	if !bytes.Equal(normalizeJSON(t, got.AST), normalizeJSON(t, r.AST)) {
		t.Fatalf("rule AST round trip mismatch:\n got %s\nwant %s", got.AST, r.AST)
	}

	// Upsert replaces in place.
	r2 := sampleRule("public-only")
	r2.Description = "edited"
	if err := s.PutRule(ctx(), r2); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	got, _ = s.GetRule(ctx(), "public-only")
	if got.Description != "edited" {
		t.Fatalf("upsert did not replace description: %q", got.Description)
	}

	// A second rule coexists; List is ordered by name.
	if err := s.PutRule(ctx(), sampleRule("alpha")); err != nil {
		t.Fatalf("put second rule: %v", err)
	}
	list, err := s.ListRules(ctx())
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "public-only" {
		t.Fatalf("list = %+v, want [alpha, public-only]", list)
	}

	// Delete removes the rule; a second delete is NOT_FOUND.
	if err := s.DeleteRule(ctx(), "public-only"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	mustCode(t, func() error { _, e := s.GetRule(ctx(), "public-only"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, s.DeleteRule(ctx(), "public-only"), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetRule(ctx(), "ghost"); return e }(), aerr.APERTURE_NOT_FOUND)
}

func testRuleValidation(t *testing.T, s model.Storage) {
	// Empty name → RULE_INVALID, nothing persisted.
	mustCode(t, s.PutRule(ctx(), model.Rule{AST: json.RawMessage(`{"type":"var","name":"object.x"}`)}),
		aerr.APERTURE_RULE_INVALID)
	// Missing AST → RULE_INVALID.
	mustCode(t, s.PutRule(ctx(), model.Rule{Name: "no-ast"}), aerr.APERTURE_RULE_INVALID)
	mustCode(t, func() error { _, e := s.GetRule(ctx(), "no-ast"); return e }(), aerr.APERTURE_NOT_FOUND)
	// Non-object AST (array) → RULE_INVALID.
	mustCode(t, s.PutRule(ctx(), model.Rule{Name: "arr", AST: json.RawMessage(`[1,2,3]`)}),
		aerr.APERTURE_RULE_INVALID)
	// Malformed JSON → RULE_INVALID.
	mustCode(t, s.PutRule(ctx(), model.Rule{Name: "bad", AST: json.RawMessage(`{not json`)}),
		aerr.APERTURE_RULE_INVALID)
}

// normalizeJSON re-encodes raw JSON to a canonical byte form so AST comparisons
// ignore insignificant whitespace differences a backend may introduce.
func normalizeJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("normalize json: %v (%s)", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalize marshal: %v", err)
	}
	return out
}

// ---- Transactional apply ----

func testAtomicCommit(t *testing.T, s model.Storage) {
	mkGrant := func(id string) model.Grant {
		return model.Grant{
			ID: id, AccountID: "acme",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
		}
	}
	err := s.Atomic(ctx(), func(tx model.Storage) error {
		if e := tx.PutGrant(ctx(), mkGrant("g1")); e != nil {
			return e
		}
		return tx.PutGrant(ctx(), mkGrant("g2"))
	})
	if err != nil {
		t.Fatalf("atomic commit: %v", err)
	}
	// Both grants are visible on the committed store.
	for _, id := range []string{"g1", "g2"} {
		if _, e := s.GetGrant(ctx(), id); e != nil {
			t.Fatalf("committed grant %s missing: %v", id, e)
		}
	}
	// A write made inside the transaction is visible to reads inside it.
	err = s.Atomic(ctx(), func(tx model.Storage) error {
		if e := tx.PutGrant(ctx(), mkGrant("g3")); e != nil {
			return e
		}
		if _, e := tx.GetGrant(ctx(), "g3"); e != nil {
			t.Fatalf("read-your-write inside tx failed: %v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("atomic commit 2: %v", err)
	}
}

// testAtomicRollback proves a partial failure rolls the WHOLE batch back, leaving
// storage byte-for-byte unchanged — the honest transactional guarantee, run on
// BOTH backends. It covers two failure modes: a write error mid-batch, and an
// explicit error returned by fn after a successful write.
func testAtomicRollback(t *testing.T, s model.Storage) {
	keep := model.Grant{
		ID: "g-keep", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}
	if err := s.PutGrant(ctx(), keep); err != nil {
		t.Fatalf("seed keep grant: %v", err)
	}
	good := func(id string) model.Grant {
		g := keep
		g.ID = id
		return g
	}
	// Mode 1: a mid-batch write fails (an invalid grant). The whole apply rolls back.
	badGrant := model.Grant{ID: "g-bad"} // missing account/subject/effect → invalid
	err := s.Atomic(ctx(), func(tx model.Storage) error {
		if e := tx.PutGrant(ctx(), good("g-a")); e != nil {
			return e
		}
		return tx.PutGrant(ctx(), badGrant) // returns INVALID_INPUT
	})
	if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("want INVALID_INPUT from rolled-back apply, got %v", err)
	}
	// g-a must NOT have persisted (rolled back); g-bad never existed; g-keep stands.
	mustCode(t, func() error { _, e := s.GetGrant(ctx(), "g-a"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetGrant(ctx(), "g-bad"); return e }(), aerr.APERTURE_NOT_FOUND)
	if _, e := s.GetGrant(ctx(), "g-keep"); e != nil {
		t.Fatalf("pre-existing grant lost after rollback: %v", e)
	}

	// Mode 2: fn returns an error AFTER a successful write — still fully rolled back.
	sentinel := aerr.New(aerr.APERTURE_STORAGE, "boom")
	err = s.Atomic(ctx(), func(tx model.Storage) error {
		if e := tx.PutGrant(ctx(), good("g-b")); e != nil {
			return e
		}
		return sentinel
	})
	if aerr.CodeOf(err) != aerr.APERTURE_STORAGE {
		t.Fatalf("want STORAGE sentinel from rolled-back apply, got %v", err)
	}
	mustCode(t, func() error { _, e := s.GetGrant(ctx(), "g-b"); return e }(), aerr.APERTURE_NOT_FOUND)

	// The store is exactly as it began: only g-keep, in account acme.
	all, err := s.ListGrants(ctx(), "acme")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(all) != 1 || all[0].ID != "g-keep" {
		t.Fatalf("storage changed after rollbacks: %+v, want only g-keep", all)
	}
}

// ---- normalization helpers ----
//
// Backends persist timestamps verbatim, but the SQLite backend round-trips them
// through RFC3339Nano text, which canonicalizes the location to UTC. Normalize
// both sides through .UTC().Round(0) so reflect.DeepEqual compares instants, not
// location pointers or monotonic clock readings.

func normTime(t time.Time) time.Time { return t.UTC().Round(0) }

func normAccount(a model.Account) model.Account {
	a.CreatedAt, a.UpdatedAt = normTime(a.CreatedAt), normTime(a.UpdatedAt)
	return a
}

func normObjectType(ot model.ObjectType) model.ObjectType {
	ot.CreatedAt, ot.UpdatedAt = normTime(ot.CreatedAt), normTime(ot.UpdatedAt)
	if len(ot.Actions) == 0 {
		ot.Actions = nil
	}
	return ot
}

func normPermission(p model.Permission) model.Permission {
	p.CreatedAt, p.UpdatedAt = normTime(p.CreatedAt), normTime(p.UpdatedAt)
	return p
}

func normPrincipal(p model.Principal) model.Principal {
	p.CreatedAt, p.UpdatedAt = normTime(p.CreatedAt), normTime(p.UpdatedAt)
	if len(p.RoleIDs) == 0 {
		p.RoleIDs = nil
	}
	return p
}

func normRole(r model.Role) model.Role {
	r.CreatedAt, r.UpdatedAt = normTime(r.CreatedAt), normTime(r.UpdatedAt)
	if len(r.PermissionIDs) == 0 {
		r.PermissionIDs = nil
	}
	return r
}

func normGroup(g model.Group) model.Group {
	g.CreatedAt, g.UpdatedAt = normTime(g.CreatedAt), normTime(g.UpdatedAt)
	if len(g.MemberPrincipalIDs) == 0 {
		g.MemberPrincipalIDs = nil
	}
	return g
}

func normGrant(g model.Grant) model.Grant {
	g.CreatedAt, g.UpdatedAt = normTime(g.CreatedAt), normTime(g.UpdatedAt)
	return g
}

func normTemplate(t model.Template) model.Template {
	t.CreatedAt, t.UpdatedAt = normTime(t.CreatedAt), normTime(t.UpdatedAt)
	if len(t.Params) == 0 {
		t.Params = nil
	}
	if len(t.Grants) == 0 {
		t.Grants = nil
	}
	return t
}
