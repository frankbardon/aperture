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
	"context"
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
	t.Run("PrincipalCRUD", func(t *testing.T) { testPrincipalCRUD(t, newStore(t)) })
	t.Run("RoleCRUD", func(t *testing.T) { testRoleCRUD(t, newStore(t)) })
	t.Run("GroupCRUD", func(t *testing.T) { testGroupCRUD(t, newStore(t)) })
	t.Run("GrantCRUDAndUpsert", func(t *testing.T) { testGrantCRUDAndUpsert(t, newStore(t)) })
	t.Run("GrantValidation", func(t *testing.T) { testGrantValidation(t, newStore(t)) })
	t.Run("ListGrantsAccountScoped", func(t *testing.T) { testListGrantsAccountScoped(t, newStore(t)) })
	t.Run("GrantsForSubjects", func(t *testing.T) { testGrantsForSubjects(t, newStore(t)) })
	t.Run("GroupsForPrincipal", func(t *testing.T) { testGroupsForPrincipal(t, newStore(t)) })
	t.Run("NotFoundSemantics", func(t *testing.T) { testNotFoundSemantics(t, newStore(t)) })
	t.Run("TimestampsRoundTrip", func(t *testing.T) { testTimestampsRoundTrip(t, newStore(t)) })
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
