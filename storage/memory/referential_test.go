package memory_test

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// This file is the proof that the in-memory backend enforces, by hand, the NINE
// MODEL-STATE referential edges storage/sqlite/schema.sql declares as foreign
// keys — six ON DELETE RESTRICT and three ON DELETE CASCADE — in BOTH directions:
// a write may not name a parent that does not exist, and a delete either is
// refused (RESTRICT) or takes its children with it (CASCADE).
//
// The schema declares ELEVEN edges. The two this file does not cover are the
// shared-WIRING pair (apt_wiring_providers.object_type RESTRICT and
// apt_wiring_provider_references.object_type CASCADE), and they are covered in
// storage/storagetest instead, which by then had gained its own refusal cases and
// holds all three backends to them at once. Adding them here as well would be a
// second, narrower assertion of the same thing.
//
// It is a targeted suite rather than conformance cases on purpose: when it was
// written, storagetest asserted no refusal at all, so a green conformance run
// proved only that nothing BROKE, not that anything was enforced. These tests are
// what said it was.
//
// The mirror image of this file is storage/sqlite/foreign_keys_test.go. The two
// worlds are seeded the same way and assert the same outcomes, because the whole
// point is that a caller cannot tell the backends apart.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// seedWorld writes one referentially complete world: an object type with a
// permission, a role bundling it, two principals (alice grouped, bob merely an
// account member), a group, two memberships and a grant. Every RESTRICT edge has
// at least one live child in it, so each delete below is refused for exactly one
// reason.
func seedWorld(t *testing.T) *memory.Store {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
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
	return s
}

func grantCiting(permissionID string) model.Grant {
	return model.Grant{
		ID: "g1", AccountID: "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: permissionID, Object: "account:acme/**", Effect: model.EffectAllow,
	}
}

// mustConstraint asserts an operation was refused with APERTURE_STORAGE_CONSTRAINT
// specifically — the same code the SQLite driver's constraint error maps to. The
// code matters as much as the refusal: APERTURE_STORAGE would tell a caller "the
// backend broke, maybe retry", which is the opposite of what happened.
func mustConstraint(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want refusal with %s", what, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("%s: got code %s (%v), want %s", what, got, err, aerr.APERTURE_STORAGE_CONSTRAINT)
	}
}

func mustCode(t *testing.T, what string, err error, want aerr.Code) {
	t.Helper()
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("%s: got code %s (%v), want %s", what, got, err, want)
	}
}

// ---------------------------------------------------------------------------
// direction 1: a write may not name a parent that does not exist
// ---------------------------------------------------------------------------

func TestAWriteCannotNameAParentThatDoesNotExist(t *testing.T) {
	ctx := context.Background()

	t.Run("apt_memberships.principal_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "membership naming an unknown principal",
			s.PutMembership(ctx, model.Membership{PrincipalID: "ghost", AccountID: "acme"}))
		if _, err := s.GetMembership(ctx, "ghost", "acme"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused membership was written anyway: %v", err)
		}
	})

	t.Run("apt_group_members.principal_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "group naming an unknown member",
			s.PutGroup(ctx, model.Group{ID: "phantoms", Name: "Phantoms", MemberPrincipalIDs: []string{"ghost"}}))
		if _, err := s.GetGroup(ctx, "phantoms"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused group was written anyway: %v", err)
		}
	})

	t.Run("apt_principal_roles.role_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "principal holding an unknown role",
			s.PutPrincipal(ctx, model.Principal{
				ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
				RoleIDs: []string{"r-nonexistent"},
			}))
		if _, err := s.GetPrincipal(ctx, "carol"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused principal was written anyway: %v", err)
		}
	})

	t.Run("apt_role_permissions.permission_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "role bundling an unknown permission",
			s.PutRole(ctx, model.Role{ID: "r-ghost", Name: "Ghost", PermissionIDs: []string{"p-nonexistent"}}))
		if _, err := s.GetRole(ctx, "r-ghost"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("the refused role was written anyway: %v", err)
		}
	})

	t.Run("apt_grants.permission_id", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "grant citing an unknown permission",
			s.PutGrant(ctx, grantCiting("no-such-permission")))
		if _, err := s.GetGrant(ctx, "g1"); err != nil {
			t.Fatalf("the seeded grant vanished: %v", err)
		}
	})

	// apt_permissions.object_type is the one edge whose WRITE side is NOT_FOUND
	// rather than a constraint, in both backends alike: the object type has to be
	// read anyway to validate the permission's action verb, so the lookup fails
	// first and the reference never gets as far as the key. Pinned here because
	// "the same code as SQLite" is the requirement, not "a constraint everywhere".
	t.Run("apt_permissions.object_type reports NOT_FOUND, as SQLite does", func(t *testing.T) {
		s := seedWorld(t)
		mustCode(t, "permission on an unknown object type",
			s.PutPermission(ctx, model.Permission{ID: "p-x", ObjectType: "no-such-type", Action: "read"}),
			aerr.APERTURE_NOT_FOUND)
	})

	// The three model-state CASCADE edges have no separate write direction to
	// refuse: their join rows are only ever written as part of the owner record
	// itself (a principal's RoleIDs, a role's PermissionIDs, a group's
	// MemberPrincipalIDs), so a row naming a nonexistent OWNER cannot be expressed.
	// Their delete direction is the whole of their behaviour, and it is below. The
	// fourth CASCADE edge, the wiring one, is the same shape for the same reason —
	// see this file's header for where it is covered.
}

// ---------------------------------------------------------------------------
// direction 2a: RESTRICT refuses a delete that would orphan
// ---------------------------------------------------------------------------

func TestRestrictRefusesADeleteThatWouldOrphan(t *testing.T) {
	ctx := context.Background()

	t.Run("apt_memberships.principal_id: a principal still in an account", func(t *testing.T) {
		s := seedWorld(t)
		// bob is in acme but in no group, so the membership edge is the only one
		// that can refuse this — which is why he is seeded separately.
		mustConstraint(t, "delete a member principal", s.DeletePrincipal(ctx, "bob"))
		if _, err := s.GetPrincipal(ctx, "bob"); err != nil {
			t.Fatalf("bob vanished despite the refusal: %v", err)
		}
	})

	t.Run("apt_group_members.principal_id: a principal still in a group", func(t *testing.T) {
		s := seedWorld(t)
		// Clear alice's membership so the group edge is the only one left to object.
		if err := s.DeleteMembership(ctx, "alice", "acme"); err != nil {
			t.Fatalf("delete membership: %v", err)
		}
		mustConstraint(t, "delete a grouped principal", s.DeletePrincipal(ctx, "alice"))
		if _, err := s.GetPrincipal(ctx, "alice"); err != nil {
			t.Fatalf("alice vanished despite the refusal: %v", err)
		}
	})

	t.Run("apt_principal_roles.role_id: a role a principal still holds", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "delete an assigned role", s.DeleteRole(ctx, "r-admin"))
		if _, err := s.GetRole(ctx, "r-admin"); err != nil {
			t.Fatalf("the role vanished despite the refusal: %v", err)
		}
	})

	t.Run("apt_role_permissions.permission_id: a permission a role still bundles", func(t *testing.T) {
		s := seedWorld(t)
		// Remove the grant so the role bundle is the only edge left to object.
		if err := s.DeleteGrant(ctx, "g1"); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		mustConstraint(t, "delete a bundled permission", s.DeletePermission(ctx, "p-read"))
		if _, err := s.GetPermission(ctx, "p-read"); err != nil {
			t.Fatalf("the permission vanished despite the refusal: %v", err)
		}
	})

	t.Run("apt_grants.permission_id: a permission a grant still cites", func(t *testing.T) {
		s := seedWorld(t)
		// Strip the role bundle so the GRANT is the only thing left citing p-read.
		if err := s.PutRole(ctx, model.Role{ID: "r-admin", Name: "Admin"}); err != nil {
			t.Fatalf("re-save role without its bundle: %v", err)
		}
		mustConstraint(t, "delete a cited permission", s.DeletePermission(ctx, "p-read"))
		if _, err := s.GetPermission(ctx, "p-read"); err != nil {
			t.Fatalf("the permission vanished despite the refusal: %v", err)
		}
	})

	t.Run("apt_permissions.object_type: an object type a permission hangs off", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "delete a referenced object type", s.DeleteObjectType(ctx, "document"))
		if _, err := s.GetObjectType(ctx, "document"); err != nil {
			t.Fatalf("the object type vanished despite the refusal: %v", err)
		}
	})

	// A delete of something that is not there is still NOT_FOUND, not a
	// constraint: the checks may not turn a missing row into a different error.
	t.Run("a missing parent is still NOT_FOUND", func(t *testing.T) {
		s := seedWorld(t)
		mustCode(t, "delete an unknown principal", s.DeletePrincipal(ctx, "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, "delete an unknown role", s.DeleteRole(ctx, "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, "delete an unknown permission", s.DeletePermission(ctx, "ghost"), aerr.APERTURE_NOT_FOUND)
		mustCode(t, "delete an unknown object type", s.DeleteObjectType(ctx, "ghost"), aerr.APERTURE_NOT_FOUND)
	})
}

// ---------------------------------------------------------------------------
// direction 2b: CASCADE takes the join rows with the owner
// ---------------------------------------------------------------------------

// TestCascadeRemovesTheJoinRowsWithTheirOwner covers the three edges that DELETE
// rather than refuse. In SQLite those join rows live in their own tables and the
// schema cascades them; here they live inside the owner record, so the proof is
// that NOTHING SURVIVES the owner — the assertion in each case is that the entity
// the join row pointed at becomes deletable, which it could not be if a stale
// assignment were still lying around for the RESTRICT check to find.
func TestCascadeRemovesTheJoinRowsWithTheirOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("apt_principal_roles.principal_id: a principal owns its role assignments", func(t *testing.T) {
		s := seedWorld(t)
		// alice holds r-admin, so the role is undeletable while she is here.
		mustConstraint(t, "delete the role alice holds", s.DeleteRole(ctx, "r-admin"))

		// Clear her RESTRICT edges, then delete her. Her role assignment goes with
		// her — that is the cascade.
		if err := s.DeleteMembership(ctx, "alice", "acme"); err != nil {
			t.Fatalf("delete membership: %v", err)
		}
		if err := s.PutGroup(ctx, model.Group{ID: "eng", Name: "Engineering"}); err != nil {
			t.Fatalf("empty the group: %v", err)
		}
		// g1 names alice as its subject, which pins her too — the polymorphic
		// edge, RESTRICT like the rest.
		if err := s.DeleteGrant(ctx, "g1"); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		if err := s.DeletePrincipal(ctx, "alice"); err != nil {
			t.Fatalf("delete principal: %v", err)
		}
		// The role is now free: no assignment survived alice.
		if err := s.DeleteRole(ctx, "r-admin"); err != nil {
			t.Fatalf("alice's role assignment outlived her, so the role is still pinned: %v", err)
		}
		// And re-creating a principal under the same id starts with no assignments,
		// rather than inheriting the dead one.
		if err := s.PutPrincipal(ctx, model.Principal{
			ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
		}); err != nil {
			t.Fatalf("re-create alice: %v", err)
		}
		got, err := s.GetPrincipal(ctx, "alice")
		if err != nil {
			t.Fatalf("get re-created alice: %v", err)
		}
		if len(got.RoleIDs) != 0 {
			t.Fatalf("re-created alice inherited role assignments %v from the deleted principal", got.RoleIDs)
		}
	})

	t.Run("apt_role_permissions.role_id: a role owns its permission bundle", func(t *testing.T) {
		s := seedWorld(t)
		// p-read is pinned by the role bundle AND the grant; clear the grant, and
		// the bundle is what is left holding it.
		if err := s.DeleteGrant(ctx, "g1"); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		mustConstraint(t, "delete the bundled permission", s.DeletePermission(ctx, "p-read"))

		// Deleting the role takes its bundle rows with it (alice must let go first).
		if err := s.PutPrincipal(ctx, model.Principal{
			ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
		}); err != nil {
			t.Fatalf("drop alice's role assignment: %v", err)
		}
		if err := s.DeleteRole(ctx, "r-admin"); err != nil {
			t.Fatalf("delete role: %v", err)
		}
		if err := s.DeletePermission(ctx, "p-read"); err != nil {
			t.Fatalf("the role's bundle outlived the role, so the permission is still pinned: %v", err)
		}
		// The permission was the object type's only child, so the type is free too.
		if err := s.DeleteObjectType(ctx, "document"); err != nil {
			t.Fatalf("delete object type: %v", err)
		}
	})

	t.Run("apt_group_members.group_id: a group owns its member rows", func(t *testing.T) {
		s := seedWorld(t)
		if err := s.DeleteMembership(ctx, "alice", "acme"); err != nil {
			t.Fatalf("delete membership: %v", err)
		}
		// alice is undeletable only because the group holds her.
		mustConstraint(t, "delete a grouped principal", s.DeletePrincipal(ctx, "alice"))

		// Deleting the group takes its member rows with it. Nothing points at a
		// group, so this delete is never refused.
		if err := s.DeleteGroup(ctx, "eng"); err != nil {
			t.Fatalf("delete group: %v", err)
		}
		if gs, err := s.GroupsForPrincipal(ctx, "alice"); err != nil || len(gs) != 0 {
			t.Fatalf("GroupsForPrincipal(alice) = %v, %v — the member rows outlived the group", gs, err)
		}
		// g1 names alice as its subject and pins her independently of the group.
		if err := s.DeleteGrant(ctx, "g1"); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		// alice survives her group and is now deletable.
		if err := s.DeletePrincipal(ctx, "alice"); err != nil {
			t.Fatalf("the group's member row outlived the group, so alice is still pinned: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// a refused operation leaves nothing behind
// ---------------------------------------------------------------------------

// TestARefusedWriteIsAllOrNothing pins the ordering requirement: every check runs
// before any map is touched, so a list whose LAST entry is bad does not leave the
// earlier ones (or the entity itself) written. In the SQL backends a transaction
// guarantees this; here it is guaranteed by checking first, and that is a
// property a future edit could quietly lose.
func TestARefusedWriteIsAllOrNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("a principal whose second role is a ghost", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "principal with one good and one unknown role",
			s.PutPrincipal(ctx, model.Principal{
				ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
				RoleIDs: []string{"r-admin", "r-ghost"},
			}))
		if _, err := s.GetPrincipal(ctx, "carol"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("carol was written with a partial role list: %v", err)
		}
	})

	t.Run("re-saving an existing entity with a bad reference leaves it untouched", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "re-save alice holding an unknown role",
			s.PutPrincipal(ctx, model.Principal{
				ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
				DisplayName: "Alice Renamed", RoleIDs: []string{"r-ghost"},
			}))
		got, err := s.GetPrincipal(ctx, "alice")
		if err != nil {
			t.Fatalf("get alice: %v", err)
		}
		if got.DisplayName != "" || len(got.RoleIDs) != 1 || got.RoleIDs[0] != "r-admin" {
			t.Fatalf("the refused upsert partially applied: %+v", got)
		}
	})

	t.Run("a group whose second member is a ghost", func(t *testing.T) {
		s := seedWorld(t)
		mustConstraint(t, "group with one good and one unknown member",
			s.PutGroup(ctx, model.Group{
				ID: "eng", Name: "Engineering Renamed", MemberPrincipalIDs: []string{"alice", "ghost"},
			}))
		got, err := s.GetGroup(ctx, "eng")
		if err != nil {
			t.Fatalf("get group: %v", err)
		}
		if got.Name != "Engineering" || len(got.MemberPrincipalIDs) != 1 {
			t.Fatalf("the refused group upsert partially applied: %+v", got)
		}
	})
}

// TestAtomicRollsBackAConstraintRefusal proves the refusal composes with the
// staged-snapshot transaction: a batch whose later step is refused leaves the
// parent store exactly as it was, including the steps that had already succeeded.
func TestAtomicRollsBackAConstraintRefusal(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	err := s.Atomic(ctx, func(tx model.Storage) error {
		if err := tx.PutPrincipal(ctx, model.Principal{
			ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
		}); err != nil {
			return err
		}
		// Refused: no such permission. The whole batch must roll back.
		return tx.PutGrant(ctx, model.Grant{
			ID: "g-carol", AccountID: "acme",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "carol"},
			PermissionID: "p-ghost", Object: "account:acme/**", Effect: model.EffectAllow,
		})
	})
	mustConstraint(t, "an atomic batch citing an unknown permission", err)

	if _, err := s.GetPrincipal(ctx, "carol"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("carol survived the rolled-back batch: %v", err)
	}
	if _, err := s.GetGrant(ctx, "g-carol"); aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("the refused grant survived the rolled-back batch: %v", err)
	}

	// The same batch, referentially complete, commits whole.
	if err := s.Atomic(ctx, func(tx model.Storage) error {
		if err := tx.PutPrincipal(ctx, model.Principal{
			ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol",
		}); err != nil {
			return err
		}
		return tx.PutGrant(ctx, model.Grant{
			ID: "g-carol", AccountID: "acme",
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "carol"},
			PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
		})
	}); err != nil {
		t.Fatalf("a referentially complete batch was refused: %v", err)
	}
	if _, err := s.GetGrant(ctx, "g-carol"); err != nil {
		t.Fatalf("the committed grant is missing: %v", err)
	}
}

// TestTheWildcardAccountStaysWritable pins the reason two planned edges are
// absent from the nine. A "*"-stamped grant and a "*"-stamped membership are
// shipped features (a cross-account super-admin rides on both), and "*" is not an
// account row. The account_id rule that replaced the foreign key, and the
// polymorphic subject rule beside it, live in sentinel_columns_test.go.
func TestTheWildcardAccountStaysWritable(t *testing.T) {
	ctx := context.Background()
	s := seedWorld(t)

	if err := s.PutGrant(ctx, model.Grant{
		ID: "g-star", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "**", Effect: model.EffectAllow,
	}); err != nil {
		t.Fatalf("wildcard-stamped grant refused: %v", err)
	}
	if err := s.PutMembership(ctx, model.Membership{
		PrincipalID: "alice", AccountID: model.AccountWildcard,
	}); err != nil {
		t.Fatalf("wildcard-stamped membership refused: %v", err)
	}
}
