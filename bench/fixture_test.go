package bench

import (
	"context"
	"fmt"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

// Fixture scale. These produce a sizable model — thousands of grants across
// several accounts with hundreds of principals — so a single Check resolves a
// non-trivial subject set and candidate set rather than a toy three-grant model.
const (
	numAccounts   = 8
	numRoles      = 60
	numGroups     = 60
	numPrincipals = 480
	// concreteDocs are the concrete (wildcard-free) document grants attached to
	// role0 in every account, so Enumerate has a real, bounded candidate set to
	// materialise (literal coverage cannot enumerate a wildcard grant).
	concreteDocs = 60
)

// model bundles a seeded store with a representative request the benchmarks and
// the NFR test exercise.
type benchModel struct {
	store   *memory.Store
	req     engine.Request
	enumReq engine.EnumerateRequest
}

// buildModel seeds a sizable org -> project -> document authorization model into
// a fresh in-memory store and returns it alongside a representative cached-Check
// request (an allow with several matching candidates and a deny carve-out in the
// same scope, so deny-overrides + specificity actually run).
func buildModel(tb testing.TB) benchModel {
	tb.Helper()
	ctx := context.Background()
	store := memory.New()

	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatalf("seed: %v", err)
		}
	}

	// Object types + permissions (read/write/delete on documents).
	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "document", Actions: []string{"read", "write", "delete", "share"},
	}))
	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "project", Actions: []string{"read", "write", "delete"},
	}))
	const (
		permRead   = "perm-doc-read"
		permWrite  = "perm-doc-write"
		permDelete = "perm-doc-delete"
	)
	must(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read"}))
	must(store.PutPermission(ctx, model.Permission{ID: permWrite, ObjectType: "document", Action: "write"}))
	must(store.PutPermission(ctx, model.Permission{ID: permDelete, ObjectType: "document", Action: "delete"}))

	// Accounts.
	for a := 0; a < numAccounts; a++ {
		must(store.PutAccount(ctx, model.Account{ID: acct(a), Name: acct(a)}))
	}

	// Roles + groups (empty bundles — the engine resolves grants by subject, not
	// by the role's own permission list, so the bundles need only exist).
	for r := 0; r < numRoles; r++ {
		must(store.PutRole(ctx, model.Role{ID: role(r), Name: role(r)}))
	}
	groupMembers := make([][]string, numGroups)

	// Principals: each gets three roles and is enrolled in two groups, so its
	// subject set is {self} U 3 roles U 2 groups = 6 subjects.
	for p := 0; p < numPrincipals; p++ {
		roles := []string{
			role(p % numRoles),
			role((p + 13) % numRoles),
			role((p + 27) % numRoles),
		}
		must(store.PutPrincipal(ctx, model.Principal{
			ID: user(p), Kind: model.PrincipalUser, Identity: "user:" + user(p), RoleIDs: roles,
		}))
		g0, g1 := p%numGroups, (p+7)%numGroups
		groupMembers[g0] = append(groupMembers[g0], user(p))
		if g1 != g0 {
			groupMembers[g1] = append(groupMembers[g1], user(p))
		}
	}
	for g := 0; g < numGroups; g++ {
		must(store.PutGroup(ctx, model.Group{ID: group(g), Name: group(g), MemberPrincipalIDs: groupMembers[g]}))
	}

	// Grants, per account:
	//   - each role gets a wildcard allow-read + allow-write on its own project,
	//     plus a more-specific deny-read carving out one sealed document;
	//   - each group gets a document-scoped allow-read on its project;
	//   - group0 additionally holds an account-wide broad allow-read and a broad
	//     deny-delete, so the hot Check sees several overlapping candidates of
	//     differing specificity.
	for a := 0; a < numAccounts; a++ {
		ac := acct(a)
		base := "account:" + ac // identity-path prefix for this account's objects
		for i := 0; i < numRoles; i++ {
			proj := fmt.Sprintf("project:proj%d", i)
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-read", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/**", Effect: model.EffectAllow,
			}))
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-write", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permWrite, Object: base + "/" + proj + "/**", Effect: model.EffectAllow,
			}))
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role-deny-secret", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/document:secret", Effect: model.EffectDeny,
			}))
		}
		for i := 0; i < numGroups; i++ {
			proj := fmt.Sprintf("project:proj%d", i)
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "group-read", i), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(i)},
				PermissionID: permRead, Object: base + "/" + proj + "/document:*", Effect: model.EffectAllow,
			}))
		}
		// group0 broad grants (overlapping candidates of differing specificity).
		must(store.PutGrant(ctx, model.Grant{
			ID: gid(ac, "group0-broad-read", 0), AccountID: ac,
			Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(0)},
			PermissionID: permRead, Object: base + "/**", Effect: model.EffectAllow,
		}))
		must(store.PutGrant(ctx, model.Grant{
			ID: gid(ac, "group0-broad-deny-delete", 0), AccountID: ac,
			Subject:      model.Subject{Kind: model.SubjectGroup, ID: group(0)},
			PermissionID: permDelete, Object: base + "/project:proj0/**", Effect: model.EffectDeny,
		}))
		// Concrete document grants on role0 so Enumerate has a bounded, real set.
		for d := 0; d < concreteDocs; d++ {
			must(store.PutGrant(ctx, model.Grant{
				ID: gid(ac, "role0-doc", d), AccountID: ac,
				Subject:      model.Subject{Kind: model.SubjectRole, ID: role(0)},
				PermissionID: permRead,
				Object:       fmt.Sprintf("%s/project:proj0/document:doc%d", base, d),
				Effect:       model.EffectAllow,
			}))
		}
	}

	// Representative cached Check: user0 (roles role0/role13/role27, groups
	// group0/group7) reading a document under proj0 in acct0. Matching allow
	// candidates: role0's proj0/** read, group0's proj0/document:* read, and
	// group0's account-wide read — three overlapping allows of differing
	// specificity; the sealed "secret" deny does not cover doc42, so the verdict
	// is ALLOW resolved through the full deny-overrides walk.
	req := engine.Request{
		Account:   acct(0),
		Principal: user(0),
		Action:    "read",
		Object:    "account:" + acct(0) + "/project:proj0/document:doc42",
	}
	enumReq := engine.EnumerateRequest{
		Account:   acct(0),
		Principal: user(0),
		Action:    "read",
		Pattern:   "account:" + acct(0) + "/project:proj0/document:*",
	}
	return benchModel{store: store, req: req, enumReq: enumReq}
}

func acct(i int) string  { return fmt.Sprintf("acct%d", i) }
func role(i int) string  { return fmt.Sprintf("role%d", i) }
func group(i int) string { return fmt.Sprintf("group%d", i) }
func user(i int) string  { return fmt.Sprintf("user%d", i) }
func gid(account, kind string, i int) string {
	return fmt.Sprintf("g-%s-%s-%d", account, kind, i)
}
