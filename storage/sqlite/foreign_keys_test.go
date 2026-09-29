package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// This file holds the proof that Aperture's SQLite backend actually enforces the
// foreign keys schema.sql declares. It is an INTERNAL test (package sqlite) for
// one reason: two of the things worth proving — that a DSN is rewritten before
// the driver ever sees it, and that Setup refuses a connection someone else
// built — are not reachable from outside the package, and proving them through
// the public surface only would leave the interesting failure modes untested.
//
// The hazard being guarded is quiet, which is why the cover is this thorough.
// SQLite enforces foreign keys only while PRAGMA foreign_keys is ON; the pragma
// defaults OFF and is per-connection. A build that forgot it does not fail — it
// runs, accepts every write, and silently orphans rows. There is no symptom
// until someone reads a grant whose permission no longer exists.

// ---------------------------------------------------------------------------
// The DSN can never talk Aperture out of enforcement
// ---------------------------------------------------------------------------

// TestForceForeignKeysRewritesAnyDSN pins the rewrite itself: whatever the caller
// wrote, the result carries exactly one foreign_keys pragma and it is ON, and
// nothing else about the DSN is disturbed.
func TestForceForeignKeysRewritesAnyDSN(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "a bare path gains a query string",
			dsn:  "/var/lib/aperture.db",
			want: "/var/lib/aperture.db?_pragma=foreign_keys(1)",
		},
		{
			name: "an empty DSN becomes the file: form so the parameter stays visible",
			dsn:  "",
			want: "file:?_pragma=foreign_keys(1)",
		},
		{
			name: "existing parameters are preserved, in order",
			dsn:  "file:a.db?_pragma=busy_timeout(5000)&_txlock=immediate",
			want: "file:a.db?_pragma=busy_timeout(5000)&_txlock=immediate&_pragma=foreign_keys(1)",
		},
		{
			name: "a caller asking for OFF is overruled, not appended to",
			dsn:  "file:a.db?_pragma=foreign_keys(0)",
			want: "file:a.db?_pragma=foreign_keys(1)",
		},
		{
			name: "the = spelling is overruled too",
			dsn:  "file:a.db?_pragma=foreign_keys=off",
			want: "file:a.db?_pragma=foreign_keys(1)",
		},
		{
			name: "so is a spaced, upper-cased one",
			dsn:  "file:a.db?_pragma=FOREIGN_KEYS+%3D+FALSE",
			want: "file:a.db?_pragma=foreign_keys(1)",
		},
		{
			name: "several attempts are all removed",
			dsn:  "file:a.db?_pragma=foreign_keys(0)&cache=shared&_pragma=foreign_keys(0)",
			want: "file:a.db?cache=shared&_pragma=foreign_keys(1)",
		},
		{
			name: "an already-correct pragma is not duplicated",
			dsn:  "file:a.db?_pragma=foreign_keys(1)",
			want: "file:a.db?_pragma=foreign_keys(1)",
		},
		{
			name: "a DIFFERENT pragma whose name merely starts the same is left alone",
			dsn:  "file:a.db?_pragma=foreign_key_check",
			want: "file:a.db?_pragma=foreign_key_check&_pragma=foreign_keys(1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forceForeignKeys(tc.dsn)
			if got != tc.want {
				t.Fatalf("forceForeignKeys(%q)\n got  %q\n want %q", tc.dsn, got, tc.want)
			}
			if n := strings.Count(got, "foreign_keys("); n != 1 {
				t.Fatalf("result carries %d foreign_keys() pragmas, want exactly 1: %q", n, got)
			}
		})
	}
}

// TestAHostileDSNStillOpensAnEnforcingStore is the acceptance criterion stated as
// a test: a caller who explicitly disables foreign keys does NOT get an
// unenforced database. It goes all the way to the driver — the assertion is what
// the live connection reports and what it does with a violating write, not what
// the DSN string looks like.
func TestAHostileDSNStillOpensAnEnforcingStore(t *testing.T) {
	// Every spelling of "off" a caller might reach for, on a real file database.
	for _, hostile := range []string{
		"?_pragma=foreign_keys(0)",
		"?_pragma=foreign_keys=off",
		"?_pragma=foreign_keys(0)&_pragma=busy_timeout(5000)",
	} {
		t.Run(hostile, func(t *testing.T) {
			s := openAt(t, "file:"+filepath.Join(t.TempDir(), "aperture.db")+hostile)
			// Setup is part of the proof: it verifies enforcement and would refuse
			// here if the rewrite had not taken.
			if err := s.Setup(t.Context()); err != nil {
				t.Fatalf("setup on a store opened with %q: %v", hostile, err)
			}

			var on int
			if err := s.exec.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
				t.Fatalf("read pragma: %v", err)
			}
			if on != 1 {
				t.Fatalf("PRAGMA foreign_keys = %d on a store opened with %q, want 1. "+
					"A caller-supplied DSN must not be able to switch enforcement off.", on, hostile)
			}
			// And the pragma is not merely reported on — it bites.
			mustConstraint(t, "grant citing a permission that does not exist",
				s.PutGrant(t.Context(), grantCiting("ghost-permission")))
		})
	}
}

// TestSetupRefusesAConnectionThatIsNotEnforcing covers the second half of the
// guarantee. Open rewrites the DSN, but a Store can be handed a connection Open
// never touched, and an unenforced connection is indistinguishable from an
// enforced one until the first orphan appears. So Setup checks, and refuses.
//
// This is the case that makes the guarantee unconditional: either enforcement is
// on, or Setup fails. There is no third outcome where Aperture runs unenforced.
func TestSetupRefusesAConnectionThatIsNotEnforcing(t *testing.T) {
	// Deliberately bypassing Open: sql.Open with the raw DSN, no rewrite.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "aperture.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Sanity: this connection really is unenforced, so the test is not vacuous.
	var on int
	if err := db.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if on != 0 {
		t.Fatalf("PRAGMA foreign_keys = %d on a raw connection, want 0. "+
			"If the driver started defaulting this ON, this test proves nothing and needs rewriting.", on)
	}

	s := &Store{pool: db, exec: db}
	mustConstraint(t, "Setup on an unenforced connection", s.Setup(t.Context()))
}

// ---------------------------------------------------------------------------
// The edges themselves
// ---------------------------------------------------------------------------

// fkEdge is one foreign key as SQLite itself reports it, which is the only
// description of the schema that cannot drift from the schema.
type fkEdge struct {
	table, column     string
	parent, parentCol string
	onDelete          string
	onUpdate          string
}

func (e fkEdge) String() string {
	return e.table + "." + e.column + " -> " + e.parent + "(" + e.parentCol + ")" +
		" ON DELETE " + e.onDelete + " ON UPDATE " + e.onUpdate
}

// wantEdges is the full edge set, written out rather than derived, so a key that
// silently disappears from schema.sql fails here instead of being discovered by
// an orphaned row in production.
//
// TWO planned edges are deliberately absent: apt_memberships.account_id and
// apt_grants.account_id. Both columns carry model.AccountWildcard ("*"), which
// is a reserved sentinel and NOT an apt_accounts row — ValidateAccount refuses to
// create one — so a foreign key there would reject every wildcard grant and every
// wildcard membership. See "The two account_id columns" in schema.sql.
var wantEdges = []fkEdge{
	{"apt_grants", "permission_id", "apt_permissions", "id", "RESTRICT", "RESTRICT"},
	{"apt_group_members", "group_id", "apt_groups", "id", "CASCADE", "RESTRICT"},
	{"apt_group_members", "principal_id", "apt_principals", "id", "RESTRICT", "RESTRICT"},
	{"apt_memberships", "principal_id", "apt_principals", "id", "RESTRICT", "RESTRICT"},
	{"apt_permissions", "object_type", "apt_object_types", "name", "RESTRICT", "RESTRICT"},
	{"apt_principal_roles", "principal_id", "apt_principals", "id", "CASCADE", "RESTRICT"},
	{"apt_principal_roles", "role_id", "apt_roles", "id", "RESTRICT", "RESTRICT"},
	{"apt_role_permissions", "permission_id", "apt_permissions", "id", "RESTRICT", "RESTRICT"},
	{"apt_role_permissions", "role_id", "apt_roles", "id", "CASCADE", "RESTRICT"},
	{"apt_wiring_provider_references", "object_type", "apt_wiring_providers", "object_type", "CASCADE", "RESTRICT"},
	{"apt_wiring_providers", "object_type", "apt_object_types", "name", "RESTRICT", "RESTRICT"},
}

// TestSchemaDeclaresExactlyTheIntendedForeignKeys reads the keys back out of a
// live database with PRAGMA foreign_key_list rather than reading schema.sql, so
// what is checked is what SQLite actually built — the actions included. A
// CASCADE that should have been a RESTRICT is the difference between refusing a
// delete and performing it silently, and it is one word in the source.
func TestSchemaDeclaresExactlyTheIntendedForeignKeys(t *testing.T) {
	s := openAt(t, "file:"+filepath.Join(t.TempDir(), "aperture.db"))
	if err := s.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tables, err := apertureTables(t.Context(), s.exec)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var got []fkEdge
	for _, table := range tables {
		got = append(got, foreignKeyList(t, s.exec, table)...)
	}
	sortEdges(got)

	want := append([]fkEdge(nil), wantEdges...)
	sortEdges(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the schema's foreign keys are not the intended set:\n got:\n%s\n want:\n%s",
			renderEdges(got), renderEdges(want))
	}
	// Anti-vacuity: four CASCADE edges and no more. CASCADE is the action that
	// DELETES data, so an extra one is the expensive direction to get wrong.
	cascades := 0
	for _, e := range got {
		if e.onDelete == "CASCADE" {
			cascades++
		}
	}
	if cascades != 4 {
		t.Fatalf("found %d ON DELETE CASCADE edges, want exactly 4 "+
			"(a child table's owner: principal->roles, role->permissions, group->members, "+
			"provider->references)", cascades)
	}
}

func foreignKeyList(t *testing.T, exec sqlExec, table string) []fkEdge {
	t.Helper()
	rows, err := exec.QueryContext(t.Context(), `PRAGMA foreign_key_list(`+quoteIdent(table)+`)`)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()

	var out []fkEdge
	for rows.Next() {
		var (
			id, seq                         int
			parent, from, to                string
			onUpdate, onDelete, matchClause string
		)
		if err := rows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &matchClause); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		out = append(out, fkEdge{
			table: table, column: from,
			parent: parent, parentCol: to,
			onDelete: onDelete, onUpdate: onUpdate,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	return out
}

func sortEdges(es []fkEdge) {
	sort.Slice(es, func(i, j int) bool { return es[i].String() < es[j].String() })
}

func renderEdges(es []fkEdge) string {
	var b strings.Builder
	for _, e := range es {
		b.WriteString("   " + e.String() + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The edges do what they say
// ---------------------------------------------------------------------------

// TestRestrictRefusesADeleteThatWouldOrphan is the behaviour this whole epic
// exists for. Each of these deletes SUCCEEDED before the keys landed, leaving the
// child rows pointing at nothing.
func TestRestrictRefusesADeleteThatWouldOrphan(t *testing.T) {
	ctx := context.Background()

	t.Run("a permission a grant still cites", func(t *testing.T) {
		s := freshStore(t)
		seedWorld(t, s)
		mustConstraint(t, "delete a cited permission", s.DeletePermission(ctx, "p-read"))
		// And it is still there: a refused delete removes nothing.
		if _, err := s.GetPermission(ctx, "p-read"); err != nil {
			t.Fatalf("permission vanished despite the refusal: %v", err)
		}
	})

	t.Run("an object type a permission hangs off", func(t *testing.T) {
		s := freshStore(t)
		seedWorld(t, s)
		mustConstraint(t, "delete a referenced object type", s.DeleteObjectType(ctx, "document"))
	})

	t.Run("a role a principal still holds", func(t *testing.T) {
		s := freshStore(t)
		seedWorld(t, s)
		mustConstraint(t, "delete an assigned role", s.DeleteRole(ctx, "r-admin"))
	})

	t.Run("a principal still in a group", func(t *testing.T) {
		s := freshStore(t)
		seedWorld(t, s)
		mustConstraint(t, "delete a grouped principal", s.DeletePrincipal(ctx, "alice"))
	})

	t.Run("a principal that is still a member of an account", func(t *testing.T) {
		s := freshStore(t)
		seedWorld(t, s)
		// bob is in acme but in no group, so the membership edge is the only one
		// that can refuse this — which is the point of testing him separately.
		mustConstraint(t, "delete a member principal", s.DeletePrincipal(ctx, "bob"))
	})
}

// TestAWriteCannotNameAParentThatDoesNotExist is the other direction: the keys
// refuse the dangling reference at INSERT, not only at DELETE.
func TestAWriteCannotNameAParentThatDoesNotExist(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	seedWorld(t, s)

	mustConstraint(t, "grant citing an unknown permission",
		s.PutGrant(ctx, grantCiting("no-such-permission")))

	mustConstraint(t, "membership naming an unknown principal",
		s.PutMembership(ctx, model.Membership{PrincipalID: "ghost", AccountID: "acme"}))

	mustConstraint(t, "group naming an unknown member",
		s.PutGroup(ctx, model.Group{ID: "phantoms", Name: "Phantoms", MemberPrincipalIDs: []string{"ghost"}}))

	mustConstraint(t, "principal holding an unknown role",
		s.PutPrincipal(ctx, model.Principal{
			ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
			RoleIDs: []string{"r-nonexistent"},
		}))

	mustConstraint(t, "role bundling an unknown permission",
		s.PutRole(ctx, model.Role{ID: "r-ghost", Name: "Ghost", PermissionIDs: []string{"p-nonexistent"}}))
}

// TestCascadeRemovesAJoinTableWithItsOwner covers the three edges that DO delete.
// The Go code still clears these rows by hand inside the same transaction (that
// removal is E6-S2's, gated on parity across all three backends), so this proves
// the constraint agrees with the hand-written cascade rather than replacing it —
// if the two ever disagreed, the hand cascade would be masking a wrong action.
func TestCascadeRemovesAJoinTableWithItsOwner(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	seedWorld(t, s)

	// A group owns its member rows.
	if err := s.DeleteGroup(ctx, "eng"); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if n := countRows(t, s, "apt_group_members"); n != 0 {
		t.Fatalf("apt_group_members still holds %d rows after its group was deleted", n)
	}
	// alice was only undeletable because of that group membership; with the group
	// gone the RESTRICT edge no longer stands, and her own role rows cascade.
	if err := s.DeletePrincipal(ctx, "alice"); err == nil {
		t.Fatal("expected alice's account membership to still refuse the delete")
	}
	if err := s.DeleteMembership(ctx, "alice", "acme"); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	// g1 names alice as its subject, which pins her too. That edge is not a
	// foreign key (it is polymorphic — see integrity.go) but it RESTRICTs just
	// the same, so the grant goes first.
	if err := s.DeleteGrant(ctx, "g1"); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if err := s.DeletePrincipal(ctx, "alice"); err != nil {
		t.Fatalf("delete principal: %v", err)
	}
	if n := countRows(t, s, "apt_principal_roles"); n != 0 {
		t.Fatalf("apt_principal_roles still holds %d rows after its principal was deleted", n)
	}
}

// TestReSavingAnEntityKeepsItsChildren is the regression guard for the upsert
// rewrite. Every entity table used to be written with INSERT OR REPLACE, which
// DELETES the conflicting row before inserting the new one — firing the
// children's ON DELETE actions on the way. Under these keys that turns an
// ordinary "save the same account again" into either a refusal (RESTRICT) or a
// silent wipe of the child rows (CASCADE). The statements are ON CONFLICT DO
// UPDATE instead; this proves it, on the paths where it would actually bite.
func TestReSavingAnEntityKeepsItsChildren(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	seedWorld(t, s)

	// A principal who is BOTH an account member and a group member: under REPLACE
	// this is refused outright by two separate RESTRICT edges.
	if err := s.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
		DisplayName: "Alice Renamed", RoleIDs: []string{"r-admin"},
	}); err != nil {
		t.Fatalf("re-saving a principal with children: %v", err)
	}
	if got, _ := s.GetPrincipal(ctx, "alice"); got.DisplayName != "Alice Renamed" {
		t.Fatalf("upsert did not update the row: %+v", got)
	}
	if ok, _ := s.IsMember(ctx, "alice", "acme"); !ok {
		t.Fatal("re-saving alice dropped her account membership")
	}
	if gs, _ := s.GroupsForPrincipal(ctx, "alice"); len(gs) != 1 {
		t.Fatalf("re-saving alice dropped her group membership: %+v", gs)
	}

	// An account with memberships and grants stamped to it.
	if err := s.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme Renamed"}); err != nil {
		t.Fatalf("re-saving an account with children: %v", err)
	}
	// A permission cited by a grant and bundled into a role.
	if err := s.PutPermission(ctx, model.Permission{
		ID: "p-read", ObjectType: "document", Action: "read", Description: "renamed",
	}); err != nil {
		t.Fatalf("re-saving a cited permission: %v", err)
	}
	// An object type that has permissions.
	if err := s.PutObjectType(ctx, model.ObjectType{
		Name: "document", Actions: []string{"read", "write"}, Description: "renamed",
	}); err != nil {
		t.Fatalf("re-saving a referenced object type: %v", err)
	}
	// A role held by a principal.
	if err := s.PutRole(ctx, model.Role{ID: "r-admin", Name: "Admin Renamed"}); err != nil {
		t.Fatalf("re-saving an assigned role: %v", err)
	}

	// Nothing was lost along the way.
	if n := countRows(t, s, "apt_grants"); n != 1 {
		t.Fatalf("apt_grants holds %d rows after the upserts, want 1", n)
	}
	if n := countRows(t, s, "apt_memberships"); n != 2 {
		t.Fatalf("apt_memberships holds %d rows after the upserts, want 2", n)
	}
	if n := countRows(t, s, "apt_group_members"); n != 1 {
		t.Fatalf("apt_group_members holds %d rows after the upserts, want 1", n)
	}
}

// TestTheWildcardAccountStaysWritable pins the reason two planned edges are
// absent. A "*"-stamped grant and a "*"-stamped membership are shipped features
// (a cross-account super-admin rides on both), and "*" is not an account row. If
// someone later adds account_id -> apt_accounts(id), this is what tells them what
// it costs. The Go rule that replaced the foreign key — an apt_accounts row OR
// exactly the wildcard — is proved in integrity_test.go.
func TestTheWildcardAccountStaysWritable(t *testing.T) {
	ctx := context.Background()
	s := freshStore(t)
	seedWorld(t, s)

	if err := s.PutGrant(ctx, model.Grant{
		ID: "g-star", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}); err != nil {
		t.Fatalf("wildcard-stamped grant refused: %v\n"+
			"model.AccountWildcard is not an apt_accounts row and cannot be made one "+
			"(ValidateAccount refuses it), so apt_grants.account_id can carry no foreign key.", err)
	}
	if err := s.PutMembership(ctx, model.Membership{
		PrincipalID: "alice", AccountID: model.AccountWildcard,
	}); err != nil {
		t.Fatalf("wildcard-stamped membership refused: %v\n"+
			"engine.requireMembership falls back to IsMember(principal, \"*\"), so this row "+
			"must remain writable and apt_memberships.account_id can carry no foreign key.", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openAt opens a Store through the public Open, so every test goes through the
// same DSN rewriting a caller would.
func openAt(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open %q: %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func freshStore(t *testing.T) *Store {
	t.Helper()
	s := openAt(t, "file:"+filepath.Join(t.TempDir(), "aperture.db"))
	if err := s.Setup(t.Context()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return s
}

// seedWorld writes one referentially complete world: an object type with a
// permission, a role bundling it, two principals (one grouped, one merely a
// member), a group, two memberships, and a grant. Every RESTRICT edge in the
// schema has at least one live child in it.
func seedWorld(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	put := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	put("account", s.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	put("object type", s.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read", "write"}}))
	put("permission", s.PutPermission(ctx, model.Permission{ID: "p-read", ObjectType: "document", Action: "read"}))
	put("role", s.PutRole(ctx, model.Role{ID: "r-admin", Name: "Admin", PermissionIDs: []string{"p-read"}}))
	put("principal alice", s.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice", RoleIDs: []string{"r-admin"},
	}))
	put("principal bob", s.PutPrincipal(ctx, model.Principal{
		ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob",
	}))
	put("group", s.PutGroup(ctx, model.Group{ID: "eng", Name: "Engineering", MemberPrincipalIDs: []string{"alice"}}))
	put("membership alice", s.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	put("membership bob", s.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: "acme"}))
	put("grant", s.PutGrant(ctx, grantCiting("p-read")))
}

func grantCiting(permissionID string) model.Grant {
	return model.Grant{
		ID: "g1", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permissionID, Object: "account:acme/**", Effect: model.EffectAllow,
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.exec.QueryRowContext(t.Context(), `SELECT count(*) FROM `+quoteIdent(table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// mustConstraint asserts an operation was refused with APERTURE_STORAGE_CONSTRAINT
// specifically. The code matters as much as the refusal: APERTURE_STORAGE would
// tell a caller "the backend broke, maybe retry", which is the opposite of what
// happened.
func mustConstraint(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want refusal with %s", what, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("%s: got code %s (%v), want %s", what, got, err, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
}
