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
	"errors"
	"reflect"
	"strconv"
	"strings"
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
	t.Run("ListGrantsPageAllAccounts", func(t *testing.T) { testListGrantsPageAllAccounts(t, newStore(t)) })
	t.Run("ListGrantsPagePagination", func(t *testing.T) { testListGrantsPagePagination(t, newStore(t)) })
	t.Run("ListGrantsPageMaxPageSize", func(t *testing.T) { testListGrantsPageMaxPageSize(t, newStore(t)) })
	t.Run("GrantsForSubjects", func(t *testing.T) { testGrantsForSubjects(t, newStore(t)) })
	t.Run("GrantsForSubjectsWildcardAccount", func(t *testing.T) { testGrantsForSubjectsWildcardAccount(t, newStore(t)) })
	t.Run("GroupsForPrincipal", func(t *testing.T) { testGroupsForPrincipal(t, newStore(t)) })
	t.Run("NotFoundSemantics", func(t *testing.T) { testNotFoundSemantics(t, newStore(t)) })
	t.Run("TimestampsRoundTrip", func(t *testing.T) { testTimestampsRoundTrip(t, newStore(t)) })
	t.Run("TimestampUnsetRoundTrip", func(t *testing.T) { testTimestampUnsetRoundTrip(t, newStore(t)) })
	t.Run("TimestampSubMicrosecondPrecision", func(t *testing.T) { testTimestampSubMicrosecondPrecision(t, newStore(t)) })
	t.Run("TimestampRangeBoundaries", func(t *testing.T) { testTimestampRangeBoundaries(t, newStore(t)) })
	t.Run("TimestampOutOfRangeRefused", func(t *testing.T) { testTimestampOutOfRangeRefused(t, newStore(t)) })
	t.Run("AuditTimestampContract", func(t *testing.T) { testAuditTimestampContract(t, newStore(t)) })
	t.Run("AuditAppendAndQuery", func(t *testing.T) { testAuditAppendAndQuery(t, newStore(t)) })
	t.Run("AuditQueryFilters", func(t *testing.T) { testAuditQueryFilters(t, newStore(t)) })
	t.Run("AuditRetentionPrune", func(t *testing.T) { testAuditRetentionPrune(t, newStore(t)) })
	t.Run("TemplateCRUDAndVersions", func(t *testing.T) { testTemplateCRUDAndVersions(t, newStore(t)) })
	t.Run("TemplateValidation", func(t *testing.T) { testTemplateValidation(t, newStore(t)) })
	t.Run("RuleCRUD", func(t *testing.T) { testRuleCRUD(t, newStore(t)) })
	t.Run("RuleValidation", func(t *testing.T) { testRuleValidation(t, newStore(t)) })
	t.Run("WiringRoundTrip", func(t *testing.T) { testWiringRoundTrip(t, newStore(t)) })
	t.Run("WiringReplaceIsWholesale", func(t *testing.T) { testWiringReplaceIsWholesale(t, newStore(t)) })
	t.Run("WiringDeclaredKeySetIsOptional", func(t *testing.T) { testWiringDeclaredKeySetIsOptional(t, newStore(t)) })
	t.Run("WiringValidation", func(t *testing.T) { testWiringValidation(t, newStore(t)) })
	t.Run("WiringReplaceIsAllOrNothing", func(t *testing.T) { testWiringReplaceIsAllOrNothing(t, newStore(t)) })
	t.Run("WiringNotFoundSemantics", func(t *testing.T) { testWiringNotFoundSemantics(t, newStore(t)) })
	t.Run("AtomicCommit", func(t *testing.T) { testAtomicCommit(t, newStore(t)) })
	t.Run("AtomicRollback", func(t *testing.T) { testAtomicRollback(t, newStore(t)) })

	// Referential integrity. These take the Factory rather than a store: each
	// case needs several independent worlds, one per edge, so that exactly one
	// relationship is left holding each delete.
	t.Run("ReferentialWriteRefusesAnUnknownParent", func(t *testing.T) {
		testReferentialWriteRefusesAnUnknownParent(t, newStore)
	})
	t.Run("ReferentialRefusedWriteIsAllOrNothing", func(t *testing.T) {
		testReferentialRefusedWriteIsAllOrNothing(t, newStore)
	})
	t.Run("ReferentialRestrictRefusesADeleteThatWouldOrphan", func(t *testing.T) {
		testReferentialRestrictRefusesADeleteThatWouldOrphan(t, newStore)
	})
	t.Run("ReferentialCascadeRemovesTheJoinRowsWithTheirOwner", func(t *testing.T) {
		testReferentialCascadeRemovesTheJoinRowsWithTheirOwner(t, newStore)
	})
	t.Run("GrantSubjectMustExistInTheTableItsKindSelects", func(t *testing.T) {
		testGrantSubjectMustExistInTheTableItsKindSelects(t, newStore)
	})
	t.Run("DeletingAGrantSubjectIsRefused", func(t *testing.T) {
		testDeletingAGrantSubjectIsRefused(t, newStore)
	})
	t.Run("AccountReferenceIsARowOrTheWildcard", func(t *testing.T) {
		testAccountReferenceIsARowOrTheWildcard(t, newStore)
	})
	t.Run("DeletingAnAccountWithLiveChildrenIsRefused", func(t *testing.T) {
		testDeletingAnAccountWithLiveChildrenIsRefused(t, newStore)
	})
	t.Run("WildcardRowsDoNotPinARealAccount", func(t *testing.T) {
		testWildcardRowsDoNotPinARealAccount(t, newStore)
	})
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

// mustSingleCodedError asserts a refusal carries EXACTLY ONE Aperture-coded error
// in its chain, on top of whatever code CodeOf reports.
//
// The code alone is not enough. aerr.Wrap re-stamps rather than passing through,
// so a call site that wrapped an already-coded error in the SAME code produces a
// chain two deep that CodeOf cannot tell from a chain one deep — and the next
// edit, which wraps in a DIFFERENT code, silently buries the specific refusal and
// its fixups under a generic one. Depth is what proves the pass-through guard
// (`if aerr.CodeOf(err) != "" { return err }`) is actually there.
func mustSingleCodedError(t *testing.T, what string, err error) {
	t.Helper()
	if depth := codedDepth(err); depth != 1 {
		t.Fatalf("%s: %d Aperture-coded errors in the chain, want exactly 1 — "+
			"aerr.Wrap RE-STAMPS, so a call site that wraps an already-coded error "+
			"replaces the code a caller reads. Write the guard: "+
			"if aerr.CodeOf(err) != \"\" { return err }. (err: %v)", what, depth, err)
	}
}

// codedDepth counts the Aperture-coded errors in a chain.
func codedDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
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

// ---- referential fixtures ----
//
// Aperture's storage is RELATIONAL, and the relationships are real: a membership
// names a principal, a role assignment names a role, a grant names a permission,
// a group member names a principal, a membership and a grant name an account,
// and a grant names a subject — a principal, a role, or a group, whichever its
// subject_kind selects. A backend that enforces those relationships
// refuses a child row whose parent was never written — correctly, because a
// grant citing a permission that does not exist is authority nobody can read or
// revoke.
//
// The cases below used to write children into an empty store, which only ever
// worked because nothing was checking. The helpers here write the parents first.
// They ASSERT NOTHING: they are setup, every case keeps exactly the assertions it
// had, and none of this is conditional on which backend is running — a backend
// that does not enforce the relationships is simply unaffected by the parents
// being there. Enforcement itself is asserted by its own cases, not here.
//
// All of them are upserts, so calling one twice in a store is harmless.

// seedPrincipals writes the principals a membership, group member or grant
// subject in the case below refers to.
func seedPrincipals(t *testing.T, s model.Storage, ids ...string) {
	t.Helper()
	for _, id := range ids {
		p := model.Principal{ID: id, Kind: model.PrincipalUser, Identity: "user:" + id}
		if err := s.PutPrincipal(ctx(), p); err != nil {
			t.Fatalf("seed principal %s: %v", id, err)
		}
	}
}

// seedAccounts writes the accounts a membership or a grant in the case below is
// stamped with. account_id must name a real apt_accounts row OR be exactly
// model.AccountWildcard, so the wildcard is SKIPPED rather than written: "*" is
// a sentinel, not an account, and ValidateAccount refuses to create it (see
// testAccountCRUD). Passing it through here is what lets a case seed a mixed
// list of accounts, wildcard included, in one call.
func seedAccounts(t *testing.T, s model.Storage, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if id == model.AccountWildcard {
			continue
		}
		if err := s.PutAccount(ctx(), model.Account{ID: id, Name: id}); err != nil {
			t.Fatalf("seed account %s: %v", id, err)
		}
	}
}

// seedGroups writes the groups a grant subject in the case below refers to.
func seedGroups(t *testing.T, s model.Storage, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.PutGroup(ctx(), model.Group{ID: id, Name: id}); err != nil {
			t.Fatalf("seed group %s: %v", id, err)
		}
	}
}

// seedRoles writes the roles a principal's role assignments refer to.
func seedRoles(t *testing.T, s model.Storage, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.PutRole(ctx(), model.Role{ID: id, Name: id}); err != nil {
			t.Fatalf("seed role %s: %v", id, err)
		}
	}
}

// seedPermissions writes the permissions a grant or a role's permission bundle
// refers to, along with the object type they hang off — a permission's action
// verb is only legal because its object type declares it, so the two are seeded
// together or neither is.
func seedPermissions(t *testing.T, s model.Storage, ids ...string) {
	t.Helper()
	seedDocumentType(t, s)
	for _, id := range ids {
		p := model.Permission{ID: id, ObjectType: "document", Action: "read"}
		if err := s.PutPermission(ctx(), p); err != nil {
			t.Fatalf("seed permission %s: %v", id, err)
		}
	}
}

// seedStampedEntityReferents writes the parents stampedEntities() refers to. The
// stamped-entity table runs every entity against ONE store in a fixed order, and
// the Membership entry names principal "alice" while the Grant entry names
// permission "p-read" — neither of which the table itself ever creates (the
// Principal entry writes "alice" but runs after Membership, and no entry writes
// "p-read" at all). Seeding them up front is what lets the table stay a flat
// list of independent entries rather than an ordered one.
func seedStampedEntityReferents(t *testing.T, s model.Storage) {
	t.Helper()
	seedDocumentType(t, s)
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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
	seedAccounts(t, s, "acme", "other")
	seedPrincipals(t, s, "alice", "bob")
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
	seedRoles(t, s, "r-admin", "r-editor")
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
	seedPermissions(t, s, "p1", "p2")
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
	seedPrincipals(t, s, "alice", "bob")
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
	seedAccounts(t, s, "acme")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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
	seedAccounts(t, s, "acme", "other")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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

// seedGrant puts one minimal valid grant under the given id/account for the
// pagination tests.
func seedGrant(t *testing.T, s model.Storage, id, account string) {
	t.Helper()
	g := model.Grant{
		ID: id, AccountID: account,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}
	if err := s.PutGrant(ctx(), g); err != nil {
		t.Fatalf("seed grant %s: %v", id, err)
	}
}

// testListGrantsPageAllAccounts pins the all-accounts scope: passing
// model.AllAccounts ("") returns grants across every account — including the
// wildcard "*" rows inline — while a concrete account id stays account-scoped.
func testListGrantsPageAllAccounts(t *testing.T, s model.Storage) {
	seedAccounts(t, s, "acme", "other")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
	seedGrant(t, s, "g-acme-1", "acme")
	seedGrant(t, s, "g-acme-2", "acme")
	seedGrant(t, s, "g-other-1", "other")
	seedGrant(t, s, "g-star-1", model.AccountWildcard) // "*" is an ordinary row

	all, total, err := s.ListGrantsPage(ctx(), model.AllAccounts, 0, 100)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 4 {
		t.Fatalf("all-accounts total = %d, want 4", total)
	}
	if len(all) != 4 {
		t.Fatalf("all-accounts page len = %d, want 4", len(all))
	}
	// Wildcard row must be present inline, not filtered out.
	sawStar := false
	for _, g := range all {
		if g.AccountID == model.AccountWildcard {
			sawStar = true
		}
	}
	if !sawStar {
		t.Fatal("wildcard '*' grant missing from all-accounts listing")
	}
	// Deterministic ordering: by account then id. "*" sorts before "acme".
	wantOrder := []string{"g-star-1", "g-acme-1", "g-acme-2", "g-other-1"}
	for i, g := range all {
		if g.ID != wantOrder[i] {
			t.Fatalf("all[%d] = %s, want %s (order = %v)", i, g.ID, wantOrder[i], grantIDs(all))
		}
	}

	// A concrete account id stays scoped to that account only.
	acme, acmeTotal, err := s.ListGrantsPage(ctx(), "acme", 0, 100)
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if acmeTotal != 2 || len(acme) != 2 {
		t.Fatalf("acme total/len = %d/%d, want 2/2", acmeTotal, len(acme))
	}
	for _, g := range acme {
		if g.AccountID != "acme" {
			t.Fatalf("cross-account leak: %s stamped %s in acme page", g.ID, g.AccountID)
		}
	}
}

// testListGrantsPagePagination walks fixed-size pages across the all-accounts
// listing and asserts the total is the pre-pagination count, pages tile the full
// ordered set without gaps or overlap, and an offset past the end is empty.
func testListGrantsPagePagination(t *testing.T, s model.Storage) {
	const n = 7
	seedAccounts(t, s, "acme")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
	for i := 0; i < n; i++ {
		// Zero-padded ids so lexical id order is stable and predictable.
		seedGrant(t, s, "g-"+itoa2(i), "acme")
	}

	var seen []string
	for offset := 0; offset < n; offset += 3 {
		page, total, err := s.ListGrantsPage(ctx(), model.AllAccounts, offset, 3)
		if err != nil {
			t.Fatalf("page offset %d: %v", offset, err)
		}
		if total != n {
			t.Fatalf("total at offset %d = %d, want %d", offset, total, n)
		}
		seen = append(seen, grantIDs(page)...)
	}
	if len(seen) != n {
		t.Fatalf("paged through %d grants, want %d (%v)", len(seen), n, seen)
	}
	// No duplicates across pages.
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("grant %s returned on more than one page", id)
		}
		uniq[id] = true
	}

	// Offset past the end yields an empty page with the total still intact.
	empty, total, err := s.ListGrantsPage(ctx(), model.AllAccounts, 999, 3)
	if err != nil {
		t.Fatalf("page past end: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("page past end len = %d, want 0", len(empty))
	}
	if total != n {
		t.Fatalf("total past end = %d, want %d", total, n)
	}
}

// testListGrantsPageMaxPageSize asserts an over-cap limit is clamped to
// model.MaxGrantPageSize while total still reports the full match count, and a
// non-positive limit falls back to the default page size.
func testListGrantsPageMaxPageSize(t *testing.T, s model.Storage) {
	total := model.MaxGrantPageSize + 5
	seedAccounts(t, s, "acme")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
	for i := 0; i < total; i++ {
		seedGrant(t, s, "g-"+itoa2(i), "acme")
	}
	page, got, err := s.ListGrantsPage(ctx(), model.AllAccounts, 0, total) // ask for more than the cap
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got != total {
		t.Fatalf("total = %d, want %d", got, total)
	}
	if len(page) != model.MaxGrantPageSize {
		t.Fatalf("page len = %d, want clamp to %d", len(page), model.MaxGrantPageSize)
	}

	// Non-positive limit falls back to the default page size.
	deflt, _, err := s.ListGrantsPage(ctx(), model.AllAccounts, 0, 0)
	if err != nil {
		t.Fatalf("default-limit list: %v", err)
	}
	if len(deflt) != model.DefaultGrantPageSize {
		t.Fatalf("default page len = %d, want %d", len(deflt), model.DefaultGrantPageSize)
	}
}

// grantIDs projects a grant slice to its ids for order/paging assertions.
func grantIDs(gs []model.Grant) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

// itoa2 renders n as a fixed 4-digit zero-padded string so seeded grant ids sort
// lexically in numeric order (g-0000, g-0001, ...).
func itoa2(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
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
	seedAccounts(t, s, "acme", "other")
	seedPrincipals(t, s, "alice", "bob")
	seedGroups(t, s, "eng")
	seedRoles(t, s, "admin")
	seedPermissions(t, s, "p-read")
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
	// The wildcard row needs no account: "*" is a sentinel, not an apt_accounts
	// row, and seedAccounts passes it through untouched.
	seedAccounts(t, s, "acme", model.AccountWildcard)
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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
	seedPrincipals(t, s, "alice", "bob", "carol")
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

// ---- The stored-instant contract ----
//
// Every backend stores an instant as a signed 64-bit count of NANOSECONDS since
// the Unix epoch, UTC. Three properties follow, and the cases below prove all
// three on EVERY entity that carries a timestamp, for every backend:
//
//  1. The zero time.Time is "unset" and round-trips as the zero time.Time — not
//     as the Unix epoch, and not by borrowing its sibling column.
//  2. Precision is exact to the nanosecond. This is the load-bearing property:
//     it is why the storage layer stores an integer count of nanoseconds instead
//     of a per-dialect timestamp type (Postgres TIMESTAMPTZ is microsecond
//     resolution and would silently drop the last three digits).
//  3. The representable window is a closed contract. Both endpoints round-trip;
//     one nanosecond outside either endpoint is refused with
//     APERTURE_INVALID_INPUT rather than wrapped, clamped, or written as an
//     overflow value.
//
// There is deliberately NO tolerance, rounding, or precision knob anywhere in
// these comparisons. A conformance suite that compares instants approximately
// stops proving the backends agree, which is the only thing it exists to do.
// storage/storagetest/contract_test.go parses this file and enforces that.

// storableMin and storableMax are the endpoints of the int64-nanosecond window.
//
// They are written out as literals rather than read back from
// storage/storagetime on purpose: the conformance suite states the contract
// independently of the code that implements it. If the suite derived the bounds
// from the implementation, a wrong bound would make the suite agree with itself.
var (
	storableMin = time.Date(1677, 9, 21, 0, 12, 43, 145224192, time.UTC)
	storableMax = time.Date(2262, 4, 11, 23, 47, 16, 854775807, time.UTC)
)

// subMicroCreated and subMicroUpdated carry sub-microsecond detail that a
// microsecond-resolution backend cannot represent. 123456789 truncates to
// 123456000 and 1 truncates to 0, so either loss is visible rather than subtle.
var (
	subMicroCreated = time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.UTC)
	subMicroUpdated = time.Date(2026, 2, 3, 4, 5, 7, 1, time.UTC)
)

// inRangeInstant is an ordinary, uncontroversial instant used as the valid half
// of a pair when the other half is being rejected.
var inRangeInstant = time.Date(2026, 5, 6, 7, 8, 9, 987654321, time.UTC)

// stampedEntity is one entity that carries a CreatedAt/UpdatedAt pair through
// the storage contract: a writer that stamps it and a reader that hands the pair
// back. Every timestamp case walks the whole table, so a backend cannot satisfy
// the contract on some entities and quietly miss others — which is exactly the
// shape the bug takes when a new entity's write path forgets to encode.
type stampedEntity struct {
	name string
	put  func(s model.Storage, created, updated time.Time) error
	get  func(s model.Storage) (created, updated time.Time, err error)
}

// stampedEntities lists every entity in model.Storage with a CreatedAt/UpdatedAt
// pair. Adding a stamped entity to the model means adding it here; the table is
// the suite's definition of "every entity".
//
// Each entity uses a distinct identity and is otherwise minimally valid, so the
// only thing a case can fail on is its timestamps.
func stampedEntities() []stampedEntity {
	return []stampedEntity{
		{
			name: "Account",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutAccount(ctx(), model.Account{
					ID: "acme", Name: "Acme Corp", CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				a, err := s.GetAccount(ctx(), "acme")
				return a.CreatedAt, a.UpdatedAt, err
			},
		},
		{
			name: "Membership",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutMembership(ctx(), model.Membership{
					PrincipalID: "alice", AccountID: "acme", CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				m, err := s.GetMembership(ctx(), "alice", "acme")
				return m.CreatedAt, m.UpdatedAt, err
			},
		},
		{
			name: "ObjectType",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutObjectType(ctx(), model.ObjectType{
					Name: "widget", Actions: []string{"read"}, CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				ot, err := s.GetObjectType(ctx(), "widget")
				return ot.CreatedAt, ot.UpdatedAt, err
			},
		},
		{
			name: "Permission",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutPermission(ctx(), model.Permission{
					ID: "p-stamp", ObjectType: "document", Action: "read",
					CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				p, err := s.GetPermission(ctx(), "p-stamp")
				return p.CreatedAt, p.UpdatedAt, err
			},
		},
		{
			name: "Principal",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutPrincipal(ctx(), model.Principal{
					ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
					CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				p, err := s.GetPrincipal(ctx(), "alice")
				return p.CreatedAt, p.UpdatedAt, err
			},
		},
		{
			name: "Role",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutRole(ctx(), model.Role{
					ID: "r-admin", Name: "Administrator", CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				r, err := s.GetRole(ctx(), "r-admin")
				return r.CreatedAt, r.UpdatedAt, err
			},
		},
		{
			name: "Group",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutGroup(ctx(), model.Group{
					ID: "eng", Name: "Engineering", CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				g, err := s.GetGroup(ctx(), "eng")
				return g.CreatedAt, g.UpdatedAt, err
			},
		},
		{
			name: "Grant",
			put: func(s model.Storage, c, u time.Time) error {
				return s.PutGrant(ctx(), model.Grant{
					ID: "g-stamp", AccountID: "acme",
					Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
					PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
					CreatedAt: c, UpdatedAt: u,
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				g, err := s.GetGrant(ctx(), "g-stamp")
				return g.CreatedAt, g.UpdatedAt, err
			},
		},
		{
			name: "Template",
			put: func(s model.Storage, c, u time.Time) error {
				tpl := sampleTemplate("onboard", 1)
				tpl.CreatedAt, tpl.UpdatedAt = c, u
				return s.PutTemplate(ctx(), tpl)
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				tpl, err := s.GetTemplate(ctx(), "onboard", 1)
				return tpl.CreatedAt, tpl.UpdatedAt, err
			},
		},
		{
			name: "Rule",
			put: func(s model.Storage, c, u time.Time) error {
				r := sampleRule("public-only")
				r.CreatedAt, r.UpdatedAt = c, u
				return s.PutRule(ctx(), r)
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				r, err := s.GetRule(ctx(), "public-only")
				return r.CreatedAt, r.UpdatedAt, err
			},
		},

		// THE FOUR STAMPED WIRING ENTITIES. There are four and not five:
		// apt_wiring_provider_references carries no timestamps, for the reason the
		// other owned child tables carry none — its history is its provider entry's
		// and the CASCADE edge means it cannot outlive it. Do not add a fifth entry
		// for it.
		//
		// Each put is a whole-set ReplaceWiring carrying exactly this one entity,
		// because that is the ONLY write the wiring tables have: the set is
		// meaningful whole, so there is no per-row upsert to call. That makes each
		// entry's put clear the other three sections, which is harmless here — every
		// case puts and then immediately gets the same entity — and it is the one
		// shape that exercises the real write path rather than a test-only one.
		{
			name: "WiringConnection",
			put: func(s model.Storage, c, u time.Time) error {
				return s.ReplaceWiring(ctx(), model.WiringSet{
					Connections: []model.WiringConnection{
						{Name: "main", CreatedAt: c, UpdatedAt: u},
					},
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				wc, err := s.GetWiringConnection(ctx(), "main")
				return wc.CreatedAt, wc.UpdatedAt, err
			},
		},
		{
			name: "WiringProvider",
			put: func(s model.Storage, c, u time.Time) error {
				// object_type is "document" because apt_wiring_providers.object_type is
				// a real foreign key to apt_object_types(name), and "document" is the
				// type every caller of this table seeds.
				return s.ReplaceWiring(ctx(), model.WiringSet{
					Providers: []model.WiringProvider{{
						ObjectType: "document", Kind: "sql",
						CreatedAt: c, UpdatedAt: u,
					}},
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				wp, err := s.GetWiringProvider(ctx(), "document")
				return wp.CreatedAt, wp.UpdatedAt, err
			},
		},
		{
			name: "WiringFieldType",
			put: func(s model.Storage, c, u time.Time) error {
				return s.ReplaceWiring(ctx(), model.WiringSet{
					FieldTypes: []model.WiringFieldType{{
						ObjectType: "document", Field: "published_at", DeclaredType: "datetime",
						CreatedAt: c, UpdatedAt: u,
					}},
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				ft, err := s.GetWiringFieldType(ctx(), "document", "published_at")
				return ft.CreatedAt, ft.UpdatedAt, err
			},
		},
		{
			name: "WiringAttributeProvider",
			put: func(s model.Storage, c, u time.Time) error {
				return s.ReplaceWiring(ctx(), model.WiringSet{
					AttributeProviders: []model.WiringAttributeProvider{{
						Subject: "user", Kind: "sql",
						CreatedAt: c, UpdatedAt: u,
					}},
				})
			},
			get: func(s model.Storage) (time.Time, time.Time, error) {
				ap, err := s.GetWiringAttributeProvider(ctx(), "user")
				return ap.CreatedAt, ap.UpdatedAt, err
			},
		},
	}
}

// sameInstant asserts that a stored instant came back EXACTLY as it went in.
// The comparison is time.Time.Equal on the instant — no tolerance, no rounding,
// no truncation. A backend whose column cannot hold nanoseconds fails here.
func sameInstant(t *testing.T, field string, got, want time.Time) {
	t.Helper()
	if got.Equal(want) {
		return
	}
	t.Fatalf("%s did not survive the round trip exactly:\n got  %s (nanosecond field %d)\n want %s (nanosecond field %d)\n"+
		"Stored instants are int64 nanoseconds; a microsecond-resolution column "+
		"(Postgres TIMESTAMPTZ, for one) fails exactly here. Fix the backend — "+
		"do not add tolerance to this comparison.",
		field,
		got.UTC().Format(time.RFC3339Nano), got.Nanosecond(),
		want.UTC().Format(time.RFC3339Nano), want.Nanosecond())
}

// testTimestampUnsetRoundTrip proves the zero mapping on every entity: the zero
// time.Time is written as "unset" and read back as the zero time.Time. The
// failure this catches is a NOT NULL column defaulting to 0 and decoding as the
// Unix epoch, which would turn "never stamped" into "stamped in 1970".
func testTimestampUnsetRoundTrip(t *testing.T, s model.Storage) {
	seedStampedEntityReferents(t, s)
	for _, e := range stampedEntities() {
		t.Run(e.name, func(t *testing.T) {
			// Both unset.
			if err := e.put(s, time.Time{}, time.Time{}); err != nil {
				t.Fatalf("put with unset stamps: %v", err)
			}
			created, updated, err := e.get(s)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			for _, f := range []struct {
				name string
				got  time.Time
			}{{"CreatedAt", created}, {"UpdatedAt", updated}} {
				if !f.got.IsZero() {
					t.Fatalf("unset %s read back as %s, want the zero time. "+
						"An unset column must decode to time.Time{}, never to the Unix epoch.",
						f.name, f.got.UTC().Format(time.RFC3339Nano))
				}
			}

			// Half set: an unset column must not borrow its sibling's value, and
			// a set column must not be dragged to zero by the unset one.
			if err := e.put(s, inRangeInstant, time.Time{}); err != nil {
				t.Fatalf("put with one unset stamp: %v", err)
			}
			created, updated, err = e.get(s)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			sameInstant(t, "CreatedAt", created, inRangeInstant)
			if !updated.IsZero() {
				t.Fatalf("unset UpdatedAt read back as %s, want the zero time",
					updated.UTC().Format(time.RFC3339Nano))
			}
		})
	}
}

// testTimestampSubMicrosecondPrecision is the load-bearing case of this suite's
// time contract: sub-microsecond detail survives a round trip EXACTLY, on every
// entity, on every backend. Without it a contributor could swap in a
// microsecond-resolution column and the suite would stay green.
func testTimestampSubMicrosecondPrecision(t *testing.T, s model.Storage) {
	seedStampedEntityReferents(t, s)
	for _, e := range stampedEntities() {
		t.Run(e.name, func(t *testing.T) {
			if err := e.put(s, subMicroCreated, subMicroUpdated); err != nil {
				t.Fatalf("put: %v", err)
			}
			created, updated, err := e.get(s)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			sameInstant(t, "CreatedAt", created, subMicroCreated)
			sameInstant(t, "UpdatedAt", updated, subMicroUpdated)
		})
	}
}

// testTimestampRangeBoundaries proves both endpoints of the representable window
// are storable, not merely "close enough". The endpoints are the two instants a
// clamping or overflowing encoder is most likely to mangle.
func testTimestampRangeBoundaries(t *testing.T, s model.Storage) {
	seedStampedEntityReferents(t, s)
	for _, e := range stampedEntities() {
		t.Run(e.name, func(t *testing.T) {
			if err := e.put(s, storableMin, storableMax); err != nil {
				t.Fatalf("put at the window endpoints: %v", err)
			}
			created, updated, err := e.get(s)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			sameInstant(t, "CreatedAt (window minimum)", created, storableMin)
			sameInstant(t, "UpdatedAt (window maximum)", updated, storableMax)
		})
	}
}

// testTimestampOutOfRangeRefused proves the window is closed on both sides, for
// both stamps, on every entity: one nanosecond past either endpoint is refused
// with APERTURE_INVALID_INPUT and nothing is written. The storable range is a
// property of the storage CONTRACT, not of one dialect's encoding — a backend
// that keeps a time.Time in memory must refuse the same instants a backend that
// encodes to an integer refuses, or the two have silently diverged.
func testTimestampOutOfRangeRefused(t *testing.T, s model.Storage) {
	// Only the object type, NOT the full referent set: this case asserts that a
	// refused write leaves NOTHING behind, so seeding an entity it names — the
	// principal "alice", say — would make that assertion vacuous. Nothing here is
	// ever written successfully, so no parent row is needed; the object type is
	// present only so PutPermission reaches validation instead of stopping at
	// NOT_FOUND on the type.
	seedDocumentType(t, s)
	beforeMin := storableMin.Add(-time.Nanosecond)
	afterMax := storableMax.Add(time.Nanosecond)

	for _, e := range stampedEntities() {
		t.Run(e.name, func(t *testing.T) {
			for _, c := range []struct {
				label            string
				created, updated time.Time
			}{
				{"CreatedAtBeforeWindow", beforeMin, inRangeInstant},
				{"CreatedAtAfterWindow", afterMax, inRangeInstant},
				{"UpdatedAtBeforeWindow", inRangeInstant, beforeMin},
				{"UpdatedAtAfterWindow", inRangeInstant, afterMax},
			} {
				t.Run(c.label, func(t *testing.T) {
					mustCode(t, e.put(s, c.created, c.updated), aerr.APERTURE_INVALID_INPUT)
				})
			}
			// A refused write must leave nothing behind: the rejection happens
			// before any row is touched, never as a partial or clamped write.
			_, _, err := e.get(s)
			mustCode(t, err, aerr.APERTURE_NOT_FOUND)
		})
	}
}

// testAuditTimestampContract holds the audit trail's occurred_at to the same
// contract as the entity stamps. It is a separate case only because an audit
// event carries one instant rather than a CreatedAt/UpdatedAt pair — the rules
// it is held to are identical.
func testAuditTimestampContract(t *testing.T, s model.Storage) {
	events := []struct {
		id   string
		when time.Time
	}{
		{"a-submicro", subMicroCreated},
		{"a-min", storableMin},
		{"a-max", storableMax},
	}
	for _, e := range events {
		when := e.when
		if err := s.AppendAudit(ctx(), mkAudit(e.id, 0, func(ev *model.AuditEvent) {
			ev.Timestamp = when
		})); err != nil {
			t.Fatalf("append %s: %v", e.id, err)
		}
	}
	got := mustQuery(t, s, model.AuditFilter{})
	for _, e := range events {
		ev, ok := findAudit(got, e.id)
		if !ok {
			t.Fatalf("appended event %s is missing from the query result", e.id)
		}
		sameInstant(t, "AuditEvent.Timestamp of "+e.id, ev.Timestamp, e.when)
	}

	// The zero Timestamp is "unset" and reads back as the zero time.
	if err := s.AppendAudit(ctx(), mkAudit("a-unset", 0, func(ev *model.AuditEvent) {
		ev.Timestamp = time.Time{}
	})); err != nil {
		t.Fatalf("append unset timestamp: %v", err)
	}
	ev, ok := findAudit(mustQuery(t, s, model.AuditFilter{}), "a-unset")
	if !ok {
		t.Fatalf("appended event a-unset is missing from the query result")
	}
	if !ev.Timestamp.IsZero() {
		t.Fatalf("unset AuditEvent.Timestamp read back as %s, want the zero time",
			ev.Timestamp.UTC().Format(time.RFC3339Nano))
	}

	// One nanosecond outside either endpoint is refused, on the write path...
	for _, when := range []time.Time{
		storableMin.Add(-time.Nanosecond),
		storableMax.Add(time.Nanosecond),
	} {
		out := when
		mustCode(t, s.AppendAudit(ctx(), mkAudit("a-outside", 0, func(ev *model.AuditEvent) {
			ev.Timestamp = out
		})), aerr.APERTURE_INVALID_INPUT)
	}
	if _, ok := findAudit(mustQuery(t, s, model.AuditFilter{}), "a-outside"); ok {
		t.Fatalf("a refused AppendAudit left an event behind")
	}

	// ...and on the query path, where the same bound is a filter argument.
	mustCode(t, func() error {
		_, err := s.QueryAudit(ctx(), model.AuditFilter{Since: storableMin.Add(-time.Nanosecond)})
		return err
	}(), aerr.APERTURE_INVALID_INPUT)
	mustCode(t, func() error {
		_, err := s.QueryAudit(ctx(), model.AuditFilter{Until: storableMax.Add(time.Nanosecond)})
		return err
	}(), aerr.APERTURE_INVALID_INPUT)
}

// findAudit locates one event by id in a query result.
func findAudit(evs []model.AuditEvent, id string) (model.AuditEvent, bool) {
	for _, ev := range evs {
		if ev.ID == id {
			return ev, true
		}
	}
	return model.AuditEvent{}, false
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
	seedAccounts(t, s, "acme")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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
	seedAccounts(t, s, "acme")
	seedPrincipals(t, s, "alice")
	seedPermissions(t, s, "p-read")
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

// ---- Referential integrity ----
//
// Aperture's storage layer refuses to orphan a row. Fourteen relationship columns
// carry that guarantee; ELEVEN of them are declared foreign keys in
// storage/sqlite/schema.sql (seven ON DELETE RESTRICT, four ON DELETE CASCADE)
// and THREE cannot be, because the value they hold is not always a row
// reference — apt_grants.(subject_kind, subject_id) is polymorphic, and the two
// account_id columns carry the reserved model.AccountWildcard sentinel. Those
// three are enforced in Go instead, on identical terms.
//
// The cases below are the proof that EVERY backend does all fourteen, in BOTH
// directions: a write may not name a parent that does not exist, and a delete is
// either refused (RESTRICT) or takes its children with it (CASCADE). Until they
// existed the conformance suite was green only because its fixtures had been
// made referentially complete — nothing asserted a refusal, so a backend that
// enforced nothing at all passed the whole suite.
//
// Everything here is expressed through model.Storage alone. There is no table
// count, no PRAGMA, no SQL: a CASCADE is observed as "the entity the join row
// pointed at is now deletable, and a re-created owner starts empty", which is
// what a caller can actually see and is therefore what the two backends must
// agree on. The backend-specific mirrors (storage/sqlite/foreign_keys_test.go +
// integrity_test.go, storage/memory/referential_test.go +
// sentinel_columns_test.go) keep their sharper, dialect-aware assertions; these
// are the ones that must hold identically everywhere.
//
// TWO edges name a code other than APERTURE_STORAGE_CONSTRAINT or have no
// expressible violation at all. Both are pinned below rather than papered over,
// because "the backends behave the same" is the contract, not "a constraint
// fires everywhere".

// ON READING ERROR TEXT. The three Go-level checks — and ONLY those three — are
// additionally asserted on by the edge name their refusal carries (mustNameEdge
// below). They are hand-duplicated Go code in every backend with no compiler,
// registry, or generator keeping the wording in step, and the message is the only
// thing that says WHICH edge objected — for the polymorphic subject, which TABLE
// its kind selected. A case that merely saw APERTURE_STORAGE_CONSTRAINT could be
// passing for an entirely different reason. The cost is real and is accepted
// knowingly: these cases are coupled to that wording, and a backend that renders
// it differently is a divergence this suite will report.
//
// The eleven SQL edges are deliberately NOT text-asserted. Their refusal is
// rendered by a driver in one backend and by hand in another, so the wording is
// not, and must not become, part of the contract.

// referentialWorld builds one referentially complete world in a fresh store: an
// object type with a permission, a role bundling that permission, two principals
// (alice, who is grouped and holds the role; bob, who is only an account
// member), a group, two memberships, and a grant. Every RESTRICT edge has at
// least one live child in it, so each case below can clear the edges it is not
// testing and leave exactly one relationship holding the delete.
func referentialWorld(t *testing.T, newStore Factory) model.Storage {
	t.Helper()
	s := newStore(t)
	put := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	put("account", s.PutAccount(ctx(), model.Account{ID: "acme", Name: "Acme"}))
	put("object type", s.PutObjectType(ctx(), model.ObjectType{
		Name: "document", Actions: []string{"read", "write"},
	}))
	put("permission", s.PutPermission(ctx(), model.Permission{
		ID: "p-read", ObjectType: "document", Action: "read",
	}))
	put("role", s.PutRole(ctx(), model.Role{
		ID: "r-admin", Name: "Admin", PermissionIDs: []string{"p-read"},
	}))
	put("principal alice", s.PutPrincipal(ctx(), model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"r-admin"},
	}))
	put("principal bob", s.PutPrincipal(ctx(), model.Principal{
		ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob",
	}))
	put("group", s.PutGroup(ctx(), model.Group{
		ID: "eng", Name: "Engineering", MemberPrincipalIDs: []string{"alice"},
	}))
	put("membership alice", s.PutMembership(ctx(), model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	put("membership bob", s.PutMembership(ctx(), model.Membership{PrincipalID: "bob", AccountID: "acme"}))
	put("grant", s.PutGrant(ctx(), grantCitingPermission("p-read")))
	return s
}

// grantCitingPermission is referentialWorld's grant, parameterized on the
// permission it cites so a case can aim it at one that does not exist.
func grantCitingPermission(permissionID string) model.Grant {
	return model.Grant{
		ID: "g1", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permissionID, Object: "account:acme/**", Effect: model.EffectAllow,
	}
}

// mustConstraint asserts an operation was refused with
// APERTURE_STORAGE_CONSTRAINT specifically, and returns the error so a caller
// can go on to inspect it. The code matters as much as the refusal:
// APERTURE_STORAGE would tell a caller "the backend broke, maybe retry", which
// is the opposite of what happened, and APERTURE_NOT_FOUND would say the row was
// never there.
func mustConstraint(t *testing.T, what string, err error) error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want refusal with %s", what, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("%s: got code %s (%v), want %s", what, got, err, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
	return err
}

// mustNameEdge asserts a Go-level refusal names the edge that objected. See "ON
// READING ERROR TEXT" above for why only those three refusals are read this way.
func mustNameEdge(t *testing.T, what string, err error, edge string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), edge) {
		t.Fatalf("%s: refusal does not name %s, so it may have been refused for another "+
			"reason entirely: %v", what, edge, err)
	}
}

// mustNotExist asserts a read came back APERTURE_NOT_FOUND — the shape of "the
// refused write left nothing behind".
func mustNotExist(t *testing.T, what string, err error) {
	t.Helper()
	if got := aerr.CodeOf(err); got != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("%s: got code %s (%v), want %s — the refused write was applied anyway",
			what, got, err, aerr.APERTURE_NOT_FOUND)
	}
}

// mustExist asserts a read succeeded — the shape of "the refused delete removed
// nothing".
func mustExist(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v — the refused delete was applied anyway", what, err)
	}
}

// testReferentialWriteRefusesAnUnknownParent covers direction one: a child row
// may not name a parent that was never written.
//
// SIX of the eleven SQL edges are reachable this way. The other five are pinned
// here too, as the different answers they are:
//
//   - apt_permissions.object_type answers APERTURE_NOT_FOUND, not
//     APERTURE_STORAGE_CONSTRAINT, in every backend. PutPermission must READ the
//     object type anyway to validate the action verb against its declared set, so
//     the lookup misses before the reference is ever offered to the constraint.
//     That is parity of observable behaviour, which is the thing this suite
//     exists to hold; "always a constraint" would be a different, weaker claim.
//   - apt_principal_roles.principal_id, apt_role_permissions.role_id,
//     apt_group_members.group_id and apt_wiring_provider_references.object_type —
//     the four CASCADE edges — have no write direction to violate AT ALL. Their
//     child rows are only ever written as part of the owner record itself (a
//     principal's RoleIDs, a role's PermissionIDs, a group's MemberPrincipalIDs, a
//     wiring provider entry's References), so a row naming an owner that does not
//     exist is not a value model.Storage can express. Their whole behaviour is the
//     delete direction, and it is in
//     testReferentialCascadeRemovesTheJoinRowsWithTheirOwner.
func testReferentialWriteRefusesAnUnknownParent(t *testing.T, newStore Factory) {
	t.Run("apt_memberships.principal_id", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "membership naming an unknown principal",
			s.PutMembership(ctx(), model.Membership{PrincipalID: "ghost", AccountID: "acme"}))
		_, err := s.GetMembership(ctx(), "ghost", "acme")
		mustNotExist(t, "get the refused membership", err)
	})

	t.Run("apt_group_members.principal_id", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "group naming an unknown member",
			s.PutGroup(ctx(), model.Group{
				ID: "phantoms", Name: "Phantoms", MemberPrincipalIDs: []string{"ghost"},
			}))
		_, err := s.GetGroup(ctx(), "phantoms")
		mustNotExist(t, "get the refused group", err)
	})

	t.Run("apt_principal_roles.role_id", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "principal holding an unknown role",
			s.PutPrincipal(ctx(), model.Principal{
				ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
				RoleIDs: []string{"r-nonexistent"},
			}))
		_, err := s.GetPrincipal(ctx(), "carol")
		mustNotExist(t, "get the refused principal", err)
	})

	t.Run("apt_role_permissions.permission_id", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "role bundling an unknown permission",
			s.PutRole(ctx(), model.Role{
				ID: "r-ghost", Name: "Ghost", PermissionIDs: []string{"p-nonexistent"},
			}))
		_, err := s.GetRole(ctx(), "r-ghost")
		mustNotExist(t, "get the refused role", err)
	})

	t.Run("apt_wiring_providers.object_type", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "wiring serving an unknown object type",
			s.ReplaceWiring(ctx(), model.WiringSet{
				Providers: []model.WiringProvider{{ObjectType: "ghosttype", Kind: "sql"}},
			}))
		_, err := s.GetWiringProvider(ctx(), "ghosttype")
		mustNotExist(t, "get the refused wiring provider", err)
	})

	t.Run("apt_grants.permission_id", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "grant citing an unknown permission",
			s.PutGrant(ctx(), model.Grant{
				ID: "g-ghost-permission", AccountID: "acme",
				Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
				PermissionID: "no-such-permission", Object: "account:acme/**", Effect: model.EffectAllow,
			}))
		_, err := s.GetGrant(ctx(), "g-ghost-permission")
		mustNotExist(t, "get the refused grant", err)
	})

	// The documented exception, asserted as what it is.
	t.Run("apt_permissions.object_type answers NOT_FOUND, identically everywhere", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		err := s.PutPermission(ctx(), model.Permission{
			ID: "p-x", ObjectType: "no-such-type", Action: "read",
		})
		mustCode(t, err, aerr.APERTURE_NOT_FOUND)
		_, err = s.GetPermission(ctx(), "p-x")
		mustNotExist(t, "get the refused permission", err)
	})
}

// testReferentialRefusedWriteIsAllOrNothing pins the ordering requirement behind
// every refusal above: the check runs before anything is written, so a list whose
// LAST entry is bad does not leave the earlier entries — or the entity itself —
// behind. A SQL backend gets this from the transaction its upsert runs in; a
// backend without transactions has to get it by checking first, and that is a
// property an ordinary-looking edit can quietly lose.
func testReferentialRefusedWriteIsAllOrNothing(t *testing.T, newStore Factory) {
	t.Run("a new principal whose second role is a ghost", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "principal with one good and one unknown role",
			s.PutPrincipal(ctx(), model.Principal{
				ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
				RoleIDs: []string{"r-admin", "r-ghost"},
			}))
		_, err := s.GetPrincipal(ctx(), "carol")
		mustNotExist(t, "get the partially written principal", err)
	})

	t.Run("re-saving an existing entity with a bad reference leaves it untouched", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "re-save alice holding an unknown role",
			s.PutPrincipal(ctx(), model.Principal{
				ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
				DisplayName: "Alice Renamed", RoleIDs: []string{"r-ghost"},
			}))
		got, err := s.GetPrincipal(ctx(), "alice")
		if err != nil {
			t.Fatalf("get alice: %v", err)
		}
		if got.DisplayName != "" || len(got.RoleIDs) != 1 || got.RoleIDs[0] != "r-admin" {
			t.Fatalf("the refused upsert partially applied: %+v", got)
		}
	})

	t.Run("a group whose second member is a ghost", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "group with one good and one unknown member",
			s.PutGroup(ctx(), model.Group{
				ID: "eng", Name: "Engineering Renamed",
				MemberPrincipalIDs: []string{"alice", "ghost"},
			}))
		got, err := s.GetGroup(ctx(), "eng")
		if err != nil {
			t.Fatalf("get group: %v", err)
		}
		if got.Name != "Engineering" || len(got.MemberPrincipalIDs) != 1 {
			t.Fatalf("the refused group upsert partially applied: %+v", got)
		}
	})

	t.Run("a refused batch rolls the whole Atomic back", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "an atomic batch citing an unknown permission",
			s.Atomic(ctx(), func(tx model.Storage) error {
				if err := tx.PutPrincipal(ctx(), model.Principal{
					ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
				}); err != nil {
					return err
				}
				return tx.PutGrant(ctx(), model.Grant{
					ID: "g-carol", AccountID: "acme",
					Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "carol"},
					PermissionID: "p-ghost", Object: "account:acme/**", Effect: model.EffectAllow,
				})
			}))
		_, err := s.GetPrincipal(ctx(), "carol")
		mustNotExist(t, "carol survived the rolled-back batch", err)
		_, err = s.GetGrant(ctx(), "g-carol")
		mustNotExist(t, "the refused grant survived the rolled-back batch", err)
	})
}

// testReferentialRestrictRefusesADeleteThatWouldOrphan covers the seven
// ON DELETE RESTRICT edges. Every one of these deletes SUCCEEDED before the keys
// landed, leaving the child rows pointing at nothing.
//
// Each case first clears the edges it is not testing, so exactly one
// relationship is left to refuse — which means the case fails when THAT edge
// stops being enforced, not merely when some edge does.
func testReferentialRestrictRefusesADeleteThatWouldOrphan(t *testing.T, newStore Factory) {
	t.Run("apt_memberships.principal_id: a principal still in an account", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// bob is a member of acme and nothing else: no group holds him, no grant
		// names him, he holds no role. The membership edge is the only one that can
		// refuse this, which is why he is seeded separately.
		mustConstraint(t, "delete a member principal", s.DeletePrincipal(ctx(), "bob"))
		_, err := s.GetPrincipal(ctx(), "bob")
		mustExist(t, "get bob after the refusal", err)
	})

	t.Run("apt_group_members.principal_id: a principal still in a group", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Clear alice's OTHER pins — her account membership, and the grant that
		// names her as its subject — so the group member row is all that is left.
		if err := s.DeleteMembership(ctx(), "alice", "acme"); err != nil {
			t.Fatalf("free the membership: %v", err)
		}
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		mustConstraint(t, "delete a grouped principal", s.DeletePrincipal(ctx(), "alice"))
		_, err := s.GetPrincipal(ctx(), "alice")
		mustExist(t, "get alice after the refusal", err)
	})

	t.Run("apt_principal_roles.role_id: a role a principal still holds", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Nothing else points at r-admin: the grant names alice, and the role's own
		// permission bundle is the CASCADE side.
		mustConstraint(t, "delete an assigned role", s.DeleteRole(ctx(), "r-admin"))
		_, err := s.GetRole(ctx(), "r-admin")
		mustExist(t, "get the role after the refusal", err)
	})

	t.Run("apt_role_permissions.permission_id: a permission a role still bundles", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Revoke the grant so the role bundle is the only thing citing p-read.
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		mustConstraint(t, "delete a bundled permission", s.DeletePermission(ctx(), "p-read"))
		_, err := s.GetPermission(ctx(), "p-read")
		mustExist(t, "get the permission after the refusal", err)
	})

	t.Run("apt_grants.permission_id: a permission a grant still cites", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Strip the role's bundle so the GRANT is the only thing citing p-read.
		if err := s.PutRole(ctx(), model.Role{ID: "r-admin", Name: "Admin"}); err != nil {
			t.Fatalf("re-save the role without its bundle: %v", err)
		}
		mustConstraint(t, "delete a cited permission", s.DeletePermission(ctx(), "p-read"))
		_, err := s.GetPermission(ctx(), "p-read")
		mustExist(t, "get the permission after the refusal", err)
	})

	t.Run("apt_permissions.object_type: an object type a permission hangs off", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustConstraint(t, "delete a referenced object type", s.DeleteObjectType(ctx(), "document"))
		_, err := s.GetObjectType(ctx(), "document")
		mustExist(t, "get the object type after the refusal", err)
	})

	t.Run("apt_wiring_providers.object_type: an object type wiring still serves", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Push wiring that serves "document", then clear every OTHER thing pinning
		// the type — the grant, the role's bundle, and the permission itself — so the
		// wiring entry is the only edge left to refuse the delete.
		if err := s.ReplaceWiring(ctx(), model.WiringSet{
			Providers: []model.WiringProvider{{ObjectType: "document", Kind: "sql"}},
		}); err != nil {
			t.Fatalf("push the wiring: %v", err)
		}
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		if err := s.PutRole(ctx(), model.Role{ID: "r-admin", Name: "Admin"}); err != nil {
			t.Fatalf("re-save the role without its bundle: %v", err)
		}
		if err := s.DeletePermission(ctx(), "p-read"); err != nil {
			t.Fatalf("free the permission: %v", err)
		}
		mustConstraint(t, "delete an object type wiring still serves",
			s.DeleteObjectType(ctx(), "document"))
		_, err := s.GetObjectType(ctx(), "document")
		mustExist(t, "get the object type after the refusal", err)

		// Removing the wiring frees it. That is what the refusal is FOR: the operator
		// takes the wiring out on purpose, rather than discovering afterwards that a
		// push-and-read-back round trip quietly lost an entry.
		if err := s.ReplaceWiring(ctx(), model.WiringSet{}); err != nil {
			t.Fatalf("clear the wiring: %v", err)
		}
		if err := s.DeleteObjectType(ctx(), "document"); err != nil {
			t.Fatalf("the cleared wiring still pins the object type: %v", err)
		}
	})

	// Anti-conflation: the checks may not turn a row that was never there into a
	// constraint refusal. A missing parent is still NOT_FOUND.
	t.Run("a delete of something absent is still NOT_FOUND", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		mustCode(t, s.DeletePrincipal(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeleteRole(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeletePermission(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeleteObjectType(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeleteGroup(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeleteAccount(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, s.DeleteGrant(ctx(), "ghost"), aerr.APERTURE_NOT_FOUND)
	})
}

// testReferentialCascadeRemovesTheJoinRowsWithTheirOwner covers the four
// ON DELETE CASCADE edges — the ones where an entity owns its own child rows and
// deleting it deletes them.
//
// A cascade is invisible from model.Storage as such: there is no row count to
// read. So each case proves it by its CONSEQUENCE, which is the same thing a
// caller would notice — the entity the join row pointed at becomes deletable,
// which it could not be if a stale row were still lying around for a RESTRICT
// check to find, and a re-created owner starts with an empty list rather than
// inheriting the dead one. Each case first asserts the RESTRICT refusal that
// proves the join row was really there, so a backend that never wrote it cannot
// pass by having nothing to cascade.
func testReferentialCascadeRemovesTheJoinRowsWithTheirOwner(t *testing.T, newStore Factory) {
	t.Run("apt_principal_roles.principal_id: a principal owns its role assignments", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// The join row exists: alice holds r-admin, so the role is undeletable.
		mustConstraint(t, "delete the role alice holds", s.DeleteRole(ctx(), "r-admin"))

		// Free alice's RESTRICT edges, then delete her.
		if err := s.DeleteMembership(ctx(), "alice", "acme"); err != nil {
			t.Fatalf("free the membership: %v", err)
		}
		if err := s.PutGroup(ctx(), model.Group{ID: "eng", Name: "Engineering"}); err != nil {
			t.Fatalf("empty the group: %v", err)
		}
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		if err := s.DeletePrincipal(ctx(), "alice"); err != nil {
			t.Fatalf("delete principal: %v", err)
		}

		// The cascade: her role assignment went with her, so the role is free.
		if err := s.DeleteRole(ctx(), "r-admin"); err != nil {
			t.Fatalf("alice's role assignment outlived her, so the role is still pinned: %v", err)
		}
		// And a principal re-created under the same id does not inherit it.
		if err := s.PutPrincipal(ctx(), model.Principal{
			ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
		}); err != nil {
			t.Fatalf("re-create alice: %v", err)
		}
		got, err := s.GetPrincipal(ctx(), "alice")
		if err != nil {
			t.Fatalf("get the re-created alice: %v", err)
		}
		if len(got.RoleIDs) != 0 {
			t.Fatalf("the re-created alice inherited role assignments %v from the deleted principal", got.RoleIDs)
		}
	})

	t.Run("apt_role_permissions.role_id: a role owns its permission bundle", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Revoke the grant so the bundle is the only thing citing p-read; the
		// refusal then proves the bundle row is really there.
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		mustConstraint(t, "delete the bundled permission", s.DeletePermission(ctx(), "p-read"))

		// Drop alice's assignment so the role itself is deletable.
		if err := s.PutPrincipal(ctx(), model.Principal{
			ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
		}); err != nil {
			t.Fatalf("drop alice's role assignment: %v", err)
		}
		if err := s.DeleteRole(ctx(), "r-admin"); err != nil {
			t.Fatalf("delete role: %v", err)
		}

		// The cascade: the bundle row went with the role.
		if err := s.DeletePermission(ctx(), "p-read"); err != nil {
			t.Fatalf("the role's bundle outlived the role, so the permission is still pinned: %v", err)
		}
		// The permission was the object type's only child, so it is free too.
		if err := s.DeleteObjectType(ctx(), "document"); err != nil {
			t.Fatalf("delete object type: %v", err)
		}
		// A role re-created under the same id starts with an empty bundle.
		if err := s.PutObjectType(ctx(), model.ObjectType{
			Name: "document", Actions: []string{"read", "write"},
		}); err != nil {
			t.Fatalf("re-create the object type: %v", err)
		}
		if err := s.PutRole(ctx(), model.Role{ID: "r-admin", Name: "Admin"}); err != nil {
			t.Fatalf("re-create the role: %v", err)
		}
		got, err := s.GetRole(ctx(), "r-admin")
		if err != nil {
			t.Fatalf("get the re-created role: %v", err)
		}
		if len(got.PermissionIDs) != 0 {
			t.Fatalf("the re-created role inherited permissions %v from the deleted role", got.PermissionIDs)
		}
	})

	t.Run("apt_group_members.group_id: a group owns its member rows", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Free alice's other pins so the group member row is the only one left.
		if err := s.DeleteMembership(ctx(), "alice", "acme"); err != nil {
			t.Fatalf("free the membership: %v", err)
		}
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		mustConstraint(t, "delete a grouped principal", s.DeletePrincipal(ctx(), "alice"))

		// Nothing points AT a group, so this delete is never refused.
		if err := s.DeleteGroup(ctx(), "eng"); err != nil {
			t.Fatalf("delete group: %v", err)
		}

		// The cascade, seen two ways: the membership edge is gone from the read
		// path, and alice is deletable again.
		if gs, err := s.GroupsForPrincipal(ctx(), "alice"); err != nil || len(gs) != 0 {
			t.Fatalf("GroupsForPrincipal(alice) = %v, %v — the member rows outlived the group", gs, err)
		}
		if err := s.DeletePrincipal(ctx(), "alice"); err != nil {
			t.Fatalf("the group's member row outlived the group, so alice is still pinned: %v", err)
		}
	})

	t.Run("apt_wiring_provider_references.object_type: a provider entry owns its reference rows", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		if err := s.PutObjectType(ctx(), model.ObjectType{
			Name: "project", Actions: []string{"read"},
		}); err != nil {
			t.Fatalf("seed the reference target type: %v", err)
		}
		if err := s.ReplaceWiring(ctx(), model.WiringSet{
			Providers: []model.WiringProvider{{
				ObjectType: "document", Kind: "sql",
				References: []model.WiringReference{{Field: "project_id", TargetType: "project"}},
			}},
		}); err != nil {
			t.Fatalf("push the wiring: %v", err)
		}
		// The reference row exists: the read path shows it.
		wp, err := s.GetWiringProvider(ctx(), "document")
		if err != nil {
			t.Fatalf("get the wiring provider: %v", err)
		}
		if len(wp.References) != 1 {
			t.Fatalf("the reference row was never written: %+v", wp.References)
		}

		// Replace the set with one that has no provider for "document" at all. The
		// owner row goes, and with it — through ON DELETE CASCADE in the SQL backends
		// and through holding the references inside the entry in the in-memory one —
		// its reference rows.
		if err := s.ReplaceWiring(ctx(), model.WiringSet{
			Providers: []model.WiringProvider{{ObjectType: "project", Kind: "sql"}},
		}); err != nil {
			t.Fatalf("replace the wiring without the document entry: %v", err)
		}
		mustCode(t, func() error { _, e := s.GetWiringProvider(ctx(), "document"); return e }(),
			aerr.APERTURE_NOT_FOUND)

		// The cascade, seen the only way model.Storage can see it: an entry
		// re-created under the same object type starts with NO references rather than
		// inheriting the dead ones.
		if err := s.ReplaceWiring(ctx(), model.WiringSet{
			Providers: []model.WiringProvider{{ObjectType: "document", Kind: "sql"}},
		}); err != nil {
			t.Fatalf("re-create the document entry: %v", err)
		}
		again, err := s.GetWiringProvider(ctx(), "document")
		if err != nil {
			t.Fatalf("get the re-created entry: %v", err)
		}
		if len(again.References) != 0 {
			t.Fatalf("the re-created provider entry inherited reference rows %+v from the deleted entry",
				again.References)
		}
	})
}

// ---- The three columns that carry no foreign key ----

// testGrantSubjectMustExistInTheTableItsKindSelects is CHECK 2's write half:
// apt_grants.(subject_kind, subject_id) is POLYMORPHIC — the kind picks which of
// apt_principals, apt_roles or apt_groups the id must exist in — and no
// single-table foreign key can express that, in SQLite or in Postgres. Every
// backend therefore dispatches on the kind in Go, and this is where they are held
// to the same dispatch.
func testGrantSubjectMustExistInTheTableItsKindSelects(t *testing.T, newStore Factory) {
	t.Run("an unknown subject is refused, per kind", func(t *testing.T) {
		for _, tc := range []struct {
			kind  model.SubjectKind
			table string
		}{
			{model.SubjectPrincipal, "apt_principals"},
			{model.SubjectRole, "apt_roles"},
			{model.SubjectGroup, "apt_groups"},
		} {
			t.Run(string(tc.kind), func(t *testing.T) {
				s := referentialWorld(t, newStore)
				err := s.PutGrant(ctx(), model.Grant{
					ID: "g-ghost-subject", AccountID: "acme",
					Subject:      model.Subject{Kind: tc.kind, ID: "ghost"},
					PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
				})
				mustConstraint(t, "grant naming an unknown "+string(tc.kind), err)
				mustNameEdge(t, "grant naming an unknown "+string(tc.kind), err, tc.table)
				_, err = s.GetGrant(ctx(), "g-ghost-subject")
				mustNotExist(t, "get the refused grant", err)
			})
		}
	})

	// The dispatch is real, not "does this id exist anywhere?". The world holds a
	// principal "alice", a role "r-admin" and a group "eng"; naming one of them
	// under ANOTHER kind must be refused. A backend that searched every table
	// would accept all three and hand the engine a grant whose subject can never
	// resolve.
	t.Run("an id from the wrong table is refused", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			kind  model.SubjectKind
			id    string
			table string
		}{
			{"a principal id under kind=role", model.SubjectRole, "alice", "apt_roles"},
			{"a role id under kind=group", model.SubjectGroup, "r-admin", "apt_groups"},
			{"a group id under kind=principal", model.SubjectPrincipal, "eng", "apt_principals"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := referentialWorld(t, newStore)
				err := s.PutGrant(ctx(), model.Grant{
					ID: "g-crossed", AccountID: "acme",
					Subject:      model.Subject{Kind: tc.kind, ID: tc.id},
					PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
				})
				mustConstraint(t, tc.name, err)
				mustNameEdge(t, tc.name, err, tc.table)
			})
		}
	})

	// Anti-vacuity for both blocks above: each kind, pointed at a real row of its
	// own table, goes through. Without this a backend that refused every grant
	// would pass them.
	t.Run("every valid subject kind is writable", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		for _, tc := range []struct {
			id  string
			sub model.Subject
		}{
			{"g-p", model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}},
			{"g-r", model.Subject{Kind: model.SubjectRole, ID: "r-admin"}},
			{"g-g", model.Subject{Kind: model.SubjectGroup, ID: "eng"}},
		} {
			if err := s.PutGrant(ctx(), model.Grant{
				ID: tc.id, AccountID: "acme", Subject: tc.sub,
				PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
			}); err != nil {
				t.Fatalf("grant %s (%s %q) refused: %v", tc.id, tc.sub.Kind, tc.sub.ID, err)
			}
		}
	})
}

// testDeletingAGrantSubjectIsRefused is CHECK 2's delete half: a principal, role
// or group may not be deleted while a grant still names it as its subject. This
// is the ON DELETE RESTRICT the polymorphic column could not declare, and without
// it DeletePrincipal leaves grants pointing at nobody — authority no one can read
// or revoke.
func testDeletingAGrantSubjectIsRefused(t *testing.T, newStore Factory) {
	for _, tc := range []struct {
		name  string
		kind  model.SubjectKind
		id    string
		table string
		free  func(t *testing.T, s model.Storage)
		del   func(s model.Storage) error
	}{
		{
			name: "principal", kind: model.SubjectPrincipal, id: "alice", table: "apt_principals",
			free: func(t *testing.T, s model.Storage) {
				t.Helper()
				if err := s.DeleteMembership(ctx(), "alice", "acme"); err != nil {
					t.Fatalf("free the membership: %v", err)
				}
				if err := s.PutGroup(ctx(), model.Group{ID: "eng", Name: "Engineering"}); err != nil {
					t.Fatalf("empty the group: %v", err)
				}
			},
			del: func(s model.Storage) error { return s.DeletePrincipal(ctx(), "alice") },
		},
		{
			name: "role", kind: model.SubjectRole, id: "r-admin", table: "apt_roles",
			free: func(t *testing.T, s model.Storage) {
				t.Helper()
				// alice holds r-admin; drop the assignment so only the grant pins it.
				if err := s.PutPrincipal(ctx(), model.Principal{
					ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
				}); err != nil {
					t.Fatalf("drop alice's role assignment: %v", err)
				}
			},
			del: func(s model.Storage) error { return s.DeleteRole(ctx(), "r-admin") },
		},
		{
			name: "group", kind: model.SubjectGroup, id: "eng", table: "apt_groups",
			free: func(t *testing.T, s model.Storage) {},
			del:  func(s model.Storage) error { return s.DeleteGroup(ctx(), "eng") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := referentialWorld(t, newStore)
			// Re-point the seeded grant at this subject.
			if err := s.PutGrant(ctx(), model.Grant{
				ID: "g1", AccountID: "acme",
				Subject:      model.Subject{Kind: tc.kind, ID: tc.id},
				PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
			}); err != nil {
				t.Fatalf("re-point the grant: %v", err)
			}
			tc.free(t, s)

			what := "delete a " + tc.name + " a grant names"
			err := tc.del(s)
			mustConstraint(t, what, err)
			mustNameEdge(t, what, err, tc.table)

			// The refusal rolled back: repeating it is refused for the same reason,
			// which it could not be if the first attempt had removed the row.
			mustConstraint(t, what+" (again)", tc.del(s))

			// Revoke the grant and the subject is free.
			if err := s.DeleteGrant(ctx(), "g1"); err != nil {
				t.Fatalf("delete grant: %v", err)
			}
			if err := tc.del(s); err != nil {
				t.Fatalf("nothing names the %s any more, yet: %v", tc.name, err)
			}
		})
	}
}

// testAccountReferenceIsARowOrTheWildcard is CHECK 1's write half, on both
// columns that carry it: apt_memberships.account_id and apt_grants.account_id
// must name an apt_accounts row OR be exactly model.AccountWildcard.
//
// The wildcard half is the case a NAIVE implementation fails. The obvious check —
// "does this account exist?", refuse when it does not — refuses "*" too, because
// "*" is deliberately not an account row and cannot be made one. That would
// reject every wildcard grant and every wildcard membership: the cross-account
// super-admin, gone. So the premise is asserted first, and nobody can make this
// pass by seeding a "*" account instead of writing the rule.
func testAccountReferenceIsARowOrTheWildcard(t *testing.T, newStore Factory) {
	t.Run("apt_memberships.account_id refuses an account that does not exist", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		err := s.PutMembership(ctx(), model.Membership{PrincipalID: "alice", AccountID: "ghost-co"})
		mustConstraint(t, "membership stamped with an unknown account", err)
		mustNameEdge(t, "membership stamped with an unknown account", err, "apt_memberships.account_id")
		_, err = s.GetMembership(ctx(), "alice", "ghost-co")
		mustNotExist(t, "get the refused membership", err)
	})

	t.Run("apt_grants.account_id refuses an account that does not exist", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		err := s.PutGrant(ctx(), model.Grant{
			ID: "g-ghost-account", AccountID: "ghost-co",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
		})
		mustConstraint(t, "grant stamped with an unknown account", err)
		mustNameEdge(t, "grant stamped with an unknown account", err, "apt_grants.account_id")
		_, err = s.GetGrant(ctx(), "g-ghost-account")
		mustNotExist(t, "get the refused grant", err)
	})

	t.Run("the wildcard is accepted on both columns", func(t *testing.T) {
		s := referentialWorld(t, newStore)

		// The premise: "*" is NOT an account, and the model refuses to make it one.
		mustCode(t, s.PutAccount(ctx(), model.Account{ID: model.AccountWildcard, Name: "star"}),
			aerr.APERTURE_INVALID_INPUT)
		_, err := s.GetAccount(ctx(), model.AccountWildcard)
		mustNotExist(t, "a "+model.AccountWildcard+" account row exists, so the wildcard check is vacuous", err)

		// The rule: both columns take it anyway.
		if err := s.PutGrant(ctx(), model.Grant{
			ID: "g-star", AccountID: model.AccountWildcard,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
			PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
		}); err != nil {
			t.Fatalf("wildcard-stamped grant refused: %v\n"+
				"a plain existence check on account_id breaks the cross-account grant", err)
		}
		if err := s.PutMembership(ctx(), model.Membership{
			PrincipalID: "bob", AccountID: model.AccountWildcard,
		}); err != nil {
			t.Fatalf("wildcard-stamped membership refused: %v\n"+
				"engine.requireMembership falls back to IsMember(principal, %q) before denying",
				err, model.AccountWildcard)
		}

		// And both still FUNCTION: the wildcard grant is returned for an account it
		// was never stamped with, and the wildcard membership reads back.
		got, err := s.GrantsForSubjects(ctx(), "acme", []model.Subject{{Kind: model.SubjectPrincipal, ID: "alice"}})
		if err != nil {
			t.Fatalf("grants for subjects: %v", err)
		}
		var sawStar bool
		for _, g := range got {
			if g.ID == "g-star" {
				sawStar = true
			}
		}
		if !sawStar {
			t.Fatalf("the wildcard grant did not span into acme: %+v", got)
		}
		if ok, err := s.IsMember(ctx(), "bob", model.AccountWildcard); err != nil || !ok {
			t.Fatalf("IsMember(bob, %q) = %v, %v — the wildcard membership does not function",
				model.AccountWildcard, ok, err)
		}
	})
}

// testDeletingAnAccountWithLiveChildrenIsRefused is CHECK 1's delete half: an
// account may not be deleted while a membership or a grant is still stamped with
// it. That is the ON DELETE RESTRICT those two columns could not declare, and
// without it DeleteAccount silently strands every membership and grant in the
// tenant it just removed.
func testDeletingAnAccountWithLiveChildrenIsRefused(t *testing.T, newStore Factory) {
	t.Run("a membership pins the account", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Revoke the grant so the memberships are the only thing holding acme.
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("free the grant: %v", err)
		}
		err := s.DeleteAccount(ctx(), "acme")
		mustConstraint(t, "delete an account with members", err)
		mustNameEdge(t, "delete an account with members", err, "apt_memberships.account_id")
		_, err = s.GetAccount(ctx(), "acme")
		mustExist(t, "get the account after the refusal", err)
	})

	t.Run("a grant pins the account", func(t *testing.T) {
		s := referentialWorld(t, newStore)
		// Clear the memberships so the grant is the only thing holding acme.
		for _, p := range []string{"alice", "bob"} {
			if err := s.DeleteMembership(ctx(), p, "acme"); err != nil {
				t.Fatalf("free membership %s: %v", p, err)
			}
		}
		err := s.DeleteAccount(ctx(), "acme")
		mustConstraint(t, "delete an account a grant is stamped with", err)
		mustNameEdge(t, "delete an account a grant is stamped with", err, "apt_grants.account_id")
		_, err = s.GetAccount(ctx(), "acme")
		mustExist(t, "get the account after the refusal", err)

		// With the grant gone too, the account is free — the anti-vacuity half.
		if err := s.DeleteGrant(ctx(), "g1"); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		if err := s.DeleteAccount(ctx(), "acme"); err != nil {
			t.Fatalf("nothing references acme any more, yet: %v", err)
		}
	})
}

// testWildcardRowsDoNotPinARealAccount is the delete-side half of the wildcard
// trap. A "*"-stamped membership or grant references NO account, so it must not
// keep a real one alive. A check that treated "*" as an ordinary account id would
// be wrong here in the opposite direction — it would make every account
// undeletable for as long as any wildcard row existed anywhere.
func testWildcardRowsDoNotPinARealAccount(t *testing.T, newStore Factory) {
	s := referentialWorld(t, newStore)

	if err := s.PutGrant(ctx(), model.Grant{
		ID: "g-star", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}); err != nil {
		t.Fatalf("seed the wildcard grant: %v", err)
	}
	if err := s.PutMembership(ctx(), model.Membership{
		PrincipalID: "bob", AccountID: model.AccountWildcard,
	}); err != nil {
		t.Fatalf("seed the wildcard membership: %v", err)
	}

	// Clear only the acme-stamped children.
	for _, p := range []string{"alice", "bob"} {
		if err := s.DeleteMembership(ctx(), p, "acme"); err != nil {
			t.Fatalf("free membership %s: %v", p, err)
		}
	}
	if err := s.DeleteGrant(ctx(), "g1"); err != nil {
		t.Fatalf("free the grant: %v", err)
	}

	if err := s.DeleteAccount(ctx(), "acme"); err != nil {
		t.Fatalf("the wildcard rows pinned a real account they never referenced: %v", err)
	}
	// The wildcard rows themselves survived: they were never acme's children.
	if _, err := s.GetGrant(ctx(), "g-star"); err != nil {
		t.Fatalf("the wildcard grant was removed with an account it never named: %v", err)
	}
	if ok, err := s.IsMember(ctx(), "bob", model.AccountWildcard); err != nil || !ok {
		t.Fatalf("IsMember(bob, %q) = %v, %v — the wildcard membership was removed with acme",
			model.AccountWildcard, ok, err)
	}
}

// ---- normalization helpers ----
//
// Backends persist instants verbatim, but a backend that encodes to int64
// nanoseconds hands the value back in UTC with no monotonic reading, while the
// in-memory backend returns the time.Time it was given. Normalize both sides
// through .UTC().Round(0) so reflect.DeepEqual compares instants, not location
// pointers or monotonic clock readings.
//
// Round(0) strips the monotonic reading. It does NOT truncate, and it must never
// be changed to do so: rounding here would let a backend that loses precision
// pass the whole suite. storage/storagetest/contract_test.go parses this file
// and fails if this function is anything other than what it is below.

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

// ---- Shared wiring (the five wiring tables) ----
//
// Wiring is not model state: the five tables hold where a decision's object
// metadata and attribute bags are read FROM, so a second instance can boot
// against the shared database with no seed file and decide identically. What the
// cases below hold every backend to is that the wiring READS BACK AS PUSHED —
// same values, same order, same declared-versus-not distinction — because the
// read back is what a second instance builds its registries from, and an instance
// that built them from something slightly different would decide differently
// while reporting nothing.
//
// The write surface is one method. ReplaceWiring takes the whole set and is
// all-or-nothing, so there is no per-row upsert for these cases to exercise and
// no partially-pushed state for them to find.

// seedWiringObjectTypes writes the object types a pushed provider entry names.
// apt_wiring_providers.object_type is a real foreign key, so a set naming a type
// the model does not have is refused — these are the parents that make the sample
// set legal.
func seedWiringObjectTypes(t *testing.T, s model.Storage) {
	t.Helper()
	for _, name := range []string{"document", "project"} {
		if err := s.PutObjectType(ctx(), model.ObjectType{
			Name: name, Actions: []string{"read", "write"},
		}); err != nil {
			t.Fatalf("seed object type %s: %v", name, err)
		}
	}
}

// sampleWiringSet is one complete, valid wiring set: two connections, two
// provider entries (one carrying two reference rows), two field-type
// declarations, and two attribute slots — one with a declared key set and one
// without.
//
// Every section is supplied OUT of canonical order on purpose. A read comes back
// sorted (connections by name, providers by object type, a provider's references
// by field, field types by object type then field, attribute slots by subject),
// and that order is what makes a push-and-read-back round trip byte-stable; a
// backend that returned map or insertion order would pass a value comparison and
// fail an operator diffing two instances.
func sampleWiringSet() model.WiringSet {
	return model.WiringSet{
		Connections: []model.WiringConnection{
			{Name: "main", CreatedAt: subMicroCreated, UpdatedAt: subMicroUpdated},
			{Name: "analytics"},
		},
		Providers: []model.WiringProvider{
			{
				ObjectType: "project", Kind: "sql", Connection: "analytics",
				GetOne: "SELECT name FROM projects WHERE id = $1",
				GetAll: "SELECT 'project:' || p.id AS id, p.name FROM projects p",
				TTL:    "0",
			},
			{
				ObjectType: "document", Kind: "sql", Connection: "main",
				GetOne:   "SELECT title, owner FROM documents WHERE id = $1",
				GetAll:   "SELECT 'document:' || d.id AS id, d.title, d.owner FROM documents d",
				IDColumn: "id",
				// A ttl is the duration TEXT the operator wrote, carried verbatim: a
				// read back has to be re-pushable byte for byte, and an integer would
				// round-trip "30s" as 30000000000.
				TTL:     "30s",
				MaxSize: 512,
				References: []model.WiringReference{
					{Field: "project_id", TargetType: "project"},
					{Field: "archived_projects", TargetType: "project"},
				},
				CreatedAt: subMicroCreated, UpdatedAt: subMicroUpdated,
			},
		},
		FieldTypes: []model.WiringFieldType{
			{ObjectType: "document", Field: "review_on", DeclaredType: "date"},
			{
				ObjectType: "document", Field: "published_at", DeclaredType: "datetime",
				CreatedAt: subMicroCreated, UpdatedAt: subMicroUpdated,
			},
		},
		AttributeProviders: []model.WiringAttributeProvider{
			{
				Subject: "user", Kind: "sql", Connection: "main",
				GetOne:   "SELECT department, clearance FROM users WHERE id = $1",
				GetAll:   "SELECT u.id AS id, u.department FROM users u",
				IDColumn: "id", TTL: "5m", MaxSize: 1000,
				DeclaredKeys: model.DeclaredKeys{
					Declared: true, Keys: []string{"department", "clearance"},
				},
				CreatedAt: subMicroCreated, UpdatedAt: subMicroUpdated,
			},
			{
				// No get_all: a FETCH-ONLY slot, which is legal here where an object
				// provider must declare one. And no declared key set at all, which is
				// the state the next case proves is distinct from a declared empty one.
				Subject: "account", Kind: "sql", Connection: "main",
				GetOne: "SELECT plan FROM accounts WHERE id = $1",
			},
		},
	}
}

// sortedSampleWiringSet is sampleWiringSet in the order a read returns it.
func sortedSampleWiringSet() model.WiringSet {
	set := sampleWiringSet()
	set.Sort()
	return set
}

// testWiringRoundTrip is the whole-set contract: what was pushed is what comes
// back, through the boot read AND through each per-section read, in canonical
// order.
func testWiringRoundTrip(t *testing.T, s model.Storage) {
	seedWiringObjectTypes(t, s)

	// A database nothing has been pushed to reports empty rather than failing. It
	// is the state that tells a booting instance to fall back to its seed file, so
	// "empty" and "broken" must not look alike.
	empty, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring from an unpushed store: %v", err)
	}
	if !empty.IsEmpty() {
		t.Fatalf("an unpushed store reported wiring: %+v", empty)
	}

	if err := s.ReplaceWiring(ctx(), sampleWiringSet()); err != nil {
		t.Fatalf("replace wiring: %v", err)
	}

	want := sortedSampleWiringSet()
	got, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	if !reflect.DeepEqual(normWiringSet(got), normWiringSet(want)) {
		t.Fatalf("the wiring set did not read back as pushed:\n got %s\nwant %s",
			showWiringSet(got), showWiringSet(want))
	}
	if got.IsEmpty() {
		t.Fatalf("a pushed wiring set reports IsEmpty")
	}

	// The per-section reads answer with the same rows in the same order. wiring
	// show reads them one section at a time; a boot reads them all at once. The two
	// must not be able to disagree.
	conns, err := s.ListWiringConnections(ctx())
	if err != nil {
		t.Fatalf("list wiring connections: %v", err)
	}
	if !reflect.DeepEqual(normWiringConnections(conns), normWiringConnections(want.Connections)) {
		t.Fatalf("connections:\n got %+v\nwant %+v", conns, want.Connections)
	}
	provs, err := s.ListWiringProviders(ctx())
	if err != nil {
		t.Fatalf("list wiring providers: %v", err)
	}
	if !reflect.DeepEqual(normWiringProviders(provs), normWiringProviders(want.Providers)) {
		t.Fatalf("providers:\n got %+v\nwant %+v", provs, want.Providers)
	}
	fts, err := s.ListWiringFieldTypes(ctx())
	if err != nil {
		t.Fatalf("list wiring field types: %v", err)
	}
	if !reflect.DeepEqual(normWiringFieldTypes(fts), normWiringFieldTypes(want.FieldTypes)) {
		t.Fatalf("field types:\n got %+v\nwant %+v", fts, want.FieldTypes)
	}
	aps, err := s.ListWiringAttributeProviders(ctx())
	if err != nil {
		t.Fatalf("list wiring attribute providers: %v", err)
	}
	if !reflect.DeepEqual(normWiringAttributeProviders(aps), normWiringAttributeProviders(want.AttributeProviders)) {
		t.Fatalf("attribute providers:\n got %+v\nwant %+v", aps, want.AttributeProviders)
	}

	// The per-entity reads answer with the same rows.
	wc, err := s.GetWiringConnection(ctx(), "main")
	if err != nil {
		t.Fatalf("get wiring connection: %v", err)
	}
	if wc.Name != "main" {
		t.Fatalf("get wiring connection returned %+v", wc)
	}
	wp, err := s.GetWiringProvider(ctx(), "document")
	if err != nil {
		t.Fatalf("get wiring provider: %v", err)
	}
	// A references: map has no order of its own, so field order IS the canonical
	// one — and a single-entry read must apply it exactly as the list read does.
	if len(wp.References) != 2 ||
		wp.References[0].Field != "archived_projects" || wp.References[1].Field != "project_id" {
		t.Fatalf("the provider's reference rows are not in field order: %+v", wp.References)
	}
	if wp.TTL != "30s" {
		t.Fatalf("ttl read back as %q, want the duration text %q — a ttl stored as an "+
			"integer round-trips \"30s\" as 30000000000 and stops being re-pushable",
			wp.TTL, "30s")
	}
	ft, err := s.GetWiringFieldType(ctx(), "document", "published_at")
	if err != nil {
		t.Fatalf("get wiring field type: %v", err)
	}
	if ft.DeclaredType != "datetime" {
		t.Fatalf("get wiring field type returned %+v", ft)
	}
	ap, err := s.GetWiringAttributeProvider(ctx(), "user")
	if err != nil {
		t.Fatalf("get wiring attribute provider: %v", err)
	}
	if !ap.DeclaredKeys.Declared || len(ap.DeclaredKeys.Keys) != 2 {
		t.Fatalf("get wiring attribute provider returned declared keys %+v", ap.DeclaredKeys)
	}

	// Re-pushing exactly what was read back changes nothing. That is the property
	// the whole surface exists for: an operator pulls the shared wiring, edits one
	// line, and pushes it again.
	if err := s.ReplaceWiring(ctx(), got); err != nil {
		t.Fatalf("re-push what was read back: %v", err)
	}
	again, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring after the re-push: %v", err)
	}
	if !reflect.DeepEqual(normWiringSet(again), normWiringSet(got)) {
		t.Fatalf("a push of what was just read back changed it:\n got %s\nwant %s",
			showWiringSet(again), showWiringSet(got))
	}
}

// testWiringReplaceIsWholesale pins that ReplaceWiring REPLACES rather than
// merges. A push is the operator's whole intent, so an entry they removed from
// the set has to disappear — the failure this catches is a backend that upserts
// each section and leaves last week's provider entry serving a type nobody
// declares any more.
func testWiringReplaceIsWholesale(t *testing.T, s model.Storage) {
	seedWiringObjectTypes(t, s)
	if err := s.ReplaceWiring(ctx(), sampleWiringSet()); err != nil {
		t.Fatalf("push the full set: %v", err)
	}

	// A narrower set: one connection, one provider with NO references, no field
	// types, no attribute slots.
	narrow := model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
		Providers: []model.WiringProvider{{
			ObjectType: "document", Kind: "sql", Connection: "main",
			GetOne: "SELECT title FROM documents WHERE id = $1",
		}},
	}
	if err := s.ReplaceWiring(ctx(), narrow); err != nil {
		t.Fatalf("push the narrower set: %v", err)
	}

	got, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	if !reflect.DeepEqual(normWiringSet(got), normWiringSet(narrow)) {
		t.Fatalf("the narrower push did not replace the set:\n got %s\nwant %s",
			showWiringSet(got), showWiringSet(narrow))
	}
	// Everything the narrower set dropped is gone, and gone means NOT_FOUND rather
	// than an empty row.
	mustCode(t, func() error { _, e := s.GetWiringConnection(ctx(), "analytics"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetWiringProvider(ctx(), "project"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetWiringFieldType(ctx(), "document", "published_at"); return e }(), aerr.APERTURE_NOT_FOUND)
	mustCode(t, func() error { _, e := s.GetWiringAttributeProvider(ctx(), "user"); return e }(), aerr.APERTURE_NOT_FOUND)
	// The surviving provider entry kept no reference rows from the entry it
	// replaced. That is the CASCADE, observed the only way model.Storage can see
	// it: a re-written owner starts empty rather than inheriting the dead rows.
	wp, err := s.GetWiringProvider(ctx(), "document")
	if err != nil {
		t.Fatalf("get the surviving provider: %v", err)
	}
	if len(wp.References) != 0 {
		t.Fatalf("the re-written provider entry inherited reference rows %+v from the entry it replaced", wp.References)
	}

	// Pushing the zero set clears the wiring entirely — the operator's way of
	// saying "this instance keeps no shared wiring", and the state a fresh
	// database is already in.
	if err := s.ReplaceWiring(ctx(), model.WiringSet{}); err != nil {
		t.Fatalf("push the empty set: %v", err)
	}
	cleared, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring after clearing: %v", err)
	}
	if !cleared.IsEmpty() {
		t.Fatalf("pushing the empty set left wiring behind: %s", showWiringSet(cleared))
	}
	// With the wiring gone, the object type it pinned is deletable again.
	if err := s.DeleteObjectType(ctx(), "project"); err != nil {
		t.Fatalf("the cleared wiring still pins its object type: %v", err)
	}
}

// testWiringDeclaredKeySetIsOptional is the load-bearing case of the
// declared-key-set column: NOT DECLARED and DECLARED EMPTY are different answers
// and both survive a round trip.
//
// The distinction is what a later story enforces against: a slot that never
// declared a key set is opted OUT (it behaves exactly as a slot did before the
// column existed), and one that declared an empty set is opted IN and permits no
// keys at all. Collapsing them turns an opt-in into an opt-out — silently, in the
// direction of less enforcement, with nothing in a verdict to say so.
func testWiringDeclaredKeySetIsOptional(t *testing.T, s model.Storage) {
	set := model.WiringSet{
		AttributeProviders: []model.WiringAttributeProvider{
			{
				Subject: "user", Kind: "sql",
				GetOne:       "SELECT department FROM users WHERE id = $1",
				DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{"department"}},
			},
			{
				// DECLARED EMPTY: opted in, permitting no keys.
				Subject: "machine", Kind: "sql",
				GetOne:       "SELECT fleet FROM machines WHERE id = $1",
				DeclaredKeys: model.DeclaredKeys{Declared: true},
			},
			{
				// NOT DECLARED: opted out.
				Subject: "account", Kind: "sql",
				GetOne: "SELECT plan FROM accounts WHERE id = $1",
			},
		},
	}
	if err := s.ReplaceWiring(ctx(), set); err != nil {
		t.Fatalf("push the attribute slots: %v", err)
	}

	for _, c := range []struct {
		subject  string
		declared bool
		keys     []string
	}{
		{"user", true, []string{"department"}},
		{"machine", true, nil},
		{"account", false, nil},
	} {
		t.Run(c.subject, func(t *testing.T) {
			ap, err := s.GetWiringAttributeProvider(ctx(), c.subject)
			if err != nil {
				t.Fatalf("get slot %s: %v", c.subject, err)
			}
			if ap.DeclaredKeys.Declared != c.declared {
				t.Fatalf("slot %s read back Declared = %v, want %v. "+
					"\"not declared\" and \"declared empty\" are different answers: the first "+
					"opts the slot OUT of key enforcement, the second opts it IN and permits "+
					"no keys. A backend that stores one as the other silently disables the "+
					"enforcement the operator asked for.",
					c.subject, ap.DeclaredKeys.Declared, c.declared)
			}
			if len(ap.DeclaredKeys.Keys) != len(c.keys) {
				t.Fatalf("slot %s read back keys %v, want %v", c.subject, ap.DeclaredKeys.Keys, c.keys)
			}
			for i, k := range c.keys {
				if ap.DeclaredKeys.Keys[i] != k {
					t.Fatalf("slot %s key %d = %q, want %q", c.subject, i, ap.DeclaredKeys.Keys[i], k)
				}
			}
		})
	}

	// And the distinction survives a push of what was read back, which is the
	// round trip an operator actually performs.
	got, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	if err := s.ReplaceWiring(ctx(), got); err != nil {
		t.Fatalf("re-push: %v", err)
	}
	again, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring after the re-push: %v", err)
	}
	if !reflect.DeepEqual(normWiringAttributeProviders(again.AttributeProviders),
		normWiringAttributeProviders(got.AttributeProviders)) {
		t.Fatalf("a round trip changed a declared key set:\n got %+v\nwant %+v",
			again.AttributeProviders, got.AttributeProviders)
	}
}

// testWiringValidation covers the structural refusals, and covers them the way
// that matters: after every one of them the wiring already in the database is
// UNCHANGED. A push is all-or-nothing, so a malformed set must not have removed
// the working wiring on its way to being rejected.
//
// Every case here is APERTURE_INVALID_INPUT and not APERTURE_STORAGE_CONSTRAINT,
// including the duplicate keys. A collision inside one pushed set is a malformed
// push rather than a database failure, and saying so uniformly is what lets a
// backend with no primary keys refuse the same set with the same code.
func testWiringValidation(t *testing.T, s model.Storage) {
	seedWiringObjectTypes(t, s)
	if err := s.ReplaceWiring(ctx(), sampleWiringSet()); err != nil {
		t.Fatalf("push the baseline set: %v", err)
	}
	baseline, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("read the baseline back: %v", err)
	}

	for _, c := range []struct {
		name string
		set  model.WiringSet
	}{
		{"a connection with no name", model.WiringSet{
			Connections: []model.WiringConnection{{Name: ""}},
		}},
		{"the same connection twice", model.WiringSet{
			Connections: []model.WiringConnection{{Name: "main"}, {Name: "main"}},
		}},
		{"a provider with no object type", model.WiringSet{
			Providers: []model.WiringProvider{{Kind: "sql"}},
		}},
		{"a provider with no kind", model.WiringSet{
			Providers: []model.WiringProvider{{ObjectType: "document"}},
		}},
		{"a provider with a negative max_size", model.WiringSet{
			Providers: []model.WiringProvider{{ObjectType: "document", Kind: "sql", MaxSize: -1}},
		}},
		{"a provider for the same object type twice", model.WiringSet{
			Providers: []model.WiringProvider{
				{ObjectType: "document", Kind: "sql"},
				{ObjectType: "document", Kind: "sql"},
			},
		}},
		{"a reference with no field name", model.WiringSet{
			Providers: []model.WiringProvider{{
				ObjectType: "document", Kind: "sql",
				References: []model.WiringReference{{TargetType: "project"}},
			}},
		}},
		{"a reference with no target type", model.WiringSet{
			Providers: []model.WiringProvider{{
				ObjectType: "document", Kind: "sql",
				References: []model.WiringReference{{Field: "project_id"}},
			}},
		}},
		{"the same reference field twice", model.WiringSet{
			Providers: []model.WiringProvider{{
				ObjectType: "document", Kind: "sql",
				References: []model.WiringReference{
					{Field: "project_id", TargetType: "project"},
					{Field: "project_id", TargetType: "project"},
				},
			}},
		}},
		{"a field type with no field name", model.WiringSet{
			FieldTypes: []model.WiringFieldType{{ObjectType: "document", DeclaredType: "date"}},
		}},
		{"a field type with no declared type", model.WiringSet{
			FieldTypes: []model.WiringFieldType{{ObjectType: "document", Field: "review_on"}},
		}},
		{"the same field type twice", model.WiringSet{
			FieldTypes: []model.WiringFieldType{
				{ObjectType: "document", Field: "review_on", DeclaredType: "date"},
				{ObjectType: "document", Field: "review_on", DeclaredType: "datetime"},
			},
		}},
		{"an attribute provider with no subject", model.WiringSet{
			AttributeProviders: []model.WiringAttributeProvider{{Kind: "sql"}},
		}},
		{"an attribute provider with no kind", model.WiringSet{
			AttributeProviders: []model.WiringAttributeProvider{{Subject: "user"}},
		}},
		{"the same attribute slot twice", model.WiringSet{
			AttributeProviders: []model.WiringAttributeProvider{
				{Subject: "user", Kind: "sql"},
				{Subject: "user", Kind: "sql"},
			},
		}},
		{"declared keys with no declaration", model.WiringSet{
			// Keys present with Declared false is a contradiction, and it is REFUSED
			// rather than resolved: guessing would silently pick one of two opposite
			// meanings.
			AttributeProviders: []model.WiringAttributeProvider{{
				Subject: "user", Kind: "sql",
				DeclaredKeys: model.DeclaredKeys{Keys: []string{"department"}},
			}},
		}},
		{"an empty declared key", model.WiringSet{
			AttributeProviders: []model.WiringAttributeProvider{{
				Subject: "user", Kind: "sql",
				DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{""}},
			}},
		}},
		{"the same declared key twice", model.WiringSet{
			AttributeProviders: []model.WiringAttributeProvider{{
				Subject: "user", Kind: "sql",
				DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{"dept", "dept"}},
			}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusal := s.ReplaceWiring(ctx(), c.set)
			mustCode(t, refusal, aerr.APERTURE_INVALID_INPUT)
			mustSingleCodedError(t, "the validation refusal", refusal)
			got, err := s.GetWiring(ctx())
			if err != nil {
				t.Fatalf("get wiring after the refusal: %v", err)
			}
			if !reflect.DeepEqual(normWiringSet(got), normWiringSet(baseline)) {
				t.Fatalf("the refused push changed the stored wiring:\n got %s\nwant %s",
					showWiringSet(got), showWiringSet(baseline))
			}
		})
	}
}

// testWiringReplaceIsAllOrNothing is the refusal that arrives HALF-WAY through
// the write rather than before it: a set whose second provider entry names an
// object type the model does not have. The first entry is perfectly valid, so a
// backend that wrote as it went would leave the wiring replaced by half of the
// new set.
//
// It is a case of its own rather than a row in testWiringValidation because the
// code is different — APERTURE_STORAGE_CONSTRAINT, from
// apt_wiring_providers.object_type, not APERTURE_INVALID_INPUT — and because the
// refusal necessarily happens after the DELETEs a replace begins with.
func testWiringReplaceIsAllOrNothing(t *testing.T, s model.Storage) {
	seedWiringObjectTypes(t, s)
	if err := s.ReplaceWiring(ctx(), sampleWiringSet()); err != nil {
		t.Fatalf("push the baseline set: %v", err)
	}
	baseline, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("read the baseline back: %v", err)
	}

	bad := model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
		Providers: []model.WiringProvider{
			{ObjectType: "document", Kind: "sql", Connection: "main"},
			{ObjectType: "ghost", Kind: "sql", Connection: "main"},
		},
		FieldTypes: []model.WiringFieldType{
			{ObjectType: "document", Field: "review_on", DeclaredType: "date"},
		},
	}
	refusal := mustConstraint(t, "push a set whose second provider serves an unknown object type",
		s.ReplaceWiring(ctx(), bad))
	// APERTURE_STORAGE_CONSTRAINT names an actionable failure — remove the entry or
	// declare the type — and carries its own fixups. Re-wrapping it as
	// APERTURE_STORAGE would bury both, and the SQL backends' transaction helper
	// passes it through only because the guard is written there by hand.
	mustSingleCodedError(t, "the constraint refusal", refusal)

	got, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring after the refusal: %v", err)
	}
	if !reflect.DeepEqual(normWiringSet(got), normWiringSet(baseline)) {
		t.Fatalf("a refused replace left the wiring part-way between the two sets:\n got %s\nwant %s",
			showWiringSet(got), showWiringSet(baseline))
	}

	// The same refusal against an EMPTY store leaves it empty, so the check cannot
	// be passing because the delete happened to be a no-op.
	mustConstraint(t, "push the same set with no baseline", s.ReplaceWiring(ctx(), bad))
	still, err := s.GetWiring(ctx())
	if err != nil {
		t.Fatalf("get wiring after the second refusal: %v", err)
	}
	if !reflect.DeepEqual(normWiringSet(still), normWiringSet(baseline)) {
		t.Fatalf("the second refused replace changed the wiring: %s", showWiringSet(still))
	}
}

// testWiringNotFoundSemantics pins the per-entity reads on both sides of "the row
// is not there": against a store with no wiring at all, and against one whose
// wiring simply does not contain the key asked for. Both are NOT_FOUND — a read
// for an absent slot must not come back as a zero-valued row, which a caller
// building a registry would read as "declared, with nothing in it".
func testWiringNotFoundSemantics(t *testing.T, s model.Storage) {
	check := func(label string) {
		t.Helper()
		mustCode(t, func() error { _, e := s.GetWiringConnection(ctx(), "nope"); return e }(), aerr.APERTURE_NOT_FOUND)
		mustCode(t, func() error { _, e := s.GetWiringProvider(ctx(), "nope"); return e }(), aerr.APERTURE_NOT_FOUND)
		mustCode(t, func() error { _, e := s.GetWiringFieldType(ctx(), "nope", "nope"); return e }(), aerr.APERTURE_NOT_FOUND)
		mustCode(t, func() error { _, e := s.GetWiringAttributeProvider(ctx(), "nope"); return e }(), aerr.APERTURE_NOT_FOUND)
	}
	check("an unpushed store")

	seedWiringObjectTypes(t, s)
	if err := s.ReplaceWiring(ctx(), sampleWiringSet()); err != nil {
		t.Fatalf("push the sample set: %v", err)
	}
	check("a populated store")

	// A key that exists in one section is still absent from another: the field-type
	// read is keyed by the PAIR, so a known object type with an unknown field is
	// NOT_FOUND too.
	mustCode(t, func() error { _, e := s.GetWiringFieldType(ctx(), "document", "nope"); return e }(), aerr.APERTURE_NOT_FOUND)
}

// ---- wiring normalization + rendering helpers ----
//
// These exist for the same reason the norm* helpers above do: reflect.DeepEqual
// compares location pointers and monotonic readings, and distinguishes a nil
// slice from an empty one where the storage contract does not. They normalize
// exactly those two things and NOTHING about the values — in particular nothing
// about DeclaredKeys.Declared, which is a value and not a representation.

func normWiringSet(w model.WiringSet) model.WiringSet {
	return model.WiringSet{
		Connections:        normWiringConnections(w.Connections),
		Providers:          normWiringProviders(w.Providers),
		FieldTypes:         normWiringFieldTypes(w.FieldTypes),
		AttributeProviders: normWiringAttributeProviders(w.AttributeProviders),
	}
}

func normWiringConnections(cs []model.WiringConnection) []model.WiringConnection {
	if len(cs) == 0 {
		return nil
	}
	out := make([]model.WiringConnection, len(cs))
	for i, c := range cs {
		c.CreatedAt, c.UpdatedAt = normTime(c.CreatedAt), normTime(c.UpdatedAt)
		out[i] = c
	}
	return out
}

func normWiringProviders(ps []model.WiringProvider) []model.WiringProvider {
	if len(ps) == 0 {
		return nil
	}
	out := make([]model.WiringProvider, len(ps))
	for i, p := range ps {
		p.CreatedAt, p.UpdatedAt = normTime(p.CreatedAt), normTime(p.UpdatedAt)
		if len(p.References) == 0 {
			p.References = nil
		} else {
			rs := make([]model.WiringReference, len(p.References))
			copy(rs, p.References)
			p.References = rs
		}
		out[i] = p
	}
	return out
}

func normWiringFieldTypes(fs []model.WiringFieldType) []model.WiringFieldType {
	if len(fs) == 0 {
		return nil
	}
	out := make([]model.WiringFieldType, len(fs))
	for i, f := range fs {
		f.CreatedAt, f.UpdatedAt = normTime(f.CreatedAt), normTime(f.UpdatedAt)
		out[i] = f
	}
	return out
}

func normWiringAttributeProviders(as []model.WiringAttributeProvider) []model.WiringAttributeProvider {
	if len(as) == 0 {
		return nil
	}
	out := make([]model.WiringAttributeProvider, len(as))
	for i, a := range as {
		a.CreatedAt, a.UpdatedAt = normTime(a.CreatedAt), normTime(a.UpdatedAt)
		// Declared is preserved verbatim; only the slice's nil-versus-empty
		// representation is normalized, which is the one difference the storage
		// contract does not draw.
		if len(a.DeclaredKeys.Keys) == 0 {
			a.DeclaredKeys.Keys = nil
		} else {
			ks := make([]string, len(a.DeclaredKeys.Keys))
			copy(ks, a.DeclaredKeys.Keys)
			a.DeclaredKeys.Keys = ks
		}
		out[i] = a
	}
	return out
}

// showWiringSet renders a set compactly for a failure message. A %+v of the whole
// struct runs to several screens of SQL, which buries the one field that differs.
func showWiringSet(w model.WiringSet) string {
	var b strings.Builder
	b.WriteString("{connections:[")
	for i, c := range w.Connections {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(c.Name)
	}
	b.WriteString("] providers:[")
	for i, p := range w.Providers {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(p.ObjectType)
		b.WriteString("(ttl=")
		b.WriteString(p.TTL)
		b.WriteString(",max=")
		b.WriteString(strconv.Itoa(p.MaxSize))
		b.WriteString(",refs=")
		for j, r := range p.References {
			if j > 0 {
				b.WriteString("+")
			}
			b.WriteString(r.Field)
			b.WriteString("->")
			b.WriteString(r.TargetType)
		}
		b.WriteString(")")
	}
	b.WriteString("] fieldTypes:[")
	for i, f := range w.FieldTypes {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(f.ObjectType)
		b.WriteString(".")
		b.WriteString(f.Field)
		b.WriteString(":")
		b.WriteString(f.DeclaredType)
	}
	b.WriteString("] attributeProviders:[")
	for i, a := range w.AttributeProviders {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(a.Subject)
		b.WriteString("(keys=")
		if !a.DeclaredKeys.Declared {
			b.WriteString("<not declared>")
		} else {
			b.WriteString("[")
			b.WriteString(strings.Join(a.DeclaredKeys.Keys, ","))
			b.WriteString("]")
		}
		b.WriteString(")")
	}
	b.WriteString("]}")
	return b.String()
}
