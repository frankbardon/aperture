package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	ucli "github.com/urfave/cli/v3"
)

// TestSeedingAnEnforcingStoreSucceeds is the regression guard for the ONE break
// that the rest of the suite structurally cannot catch.
//
// Aperture's SQLite schema carries real foreign keys, so seed.Document.Apply must
// write each entity after everything it references — roles before the principals
// that hold them, permissions before the roles that bundle them, principals
// before the groups and memberships that name them. Get that order wrong and
// seeding fails outright.
//
// Nothing else catches it. Every other test that exercises Apply runs against
// storage/memory, which enforces nothing and therefore accepts any order at all;
// the sqlite package's own tests never call the seed loader. The two halves are
// only brought together HERE, in the wiring that a real `aperture --store
// file:... --seed example.yaml` invocation actually walks: openStore picks the
// SQLite backend, then loadSeed applies the example document through it.
//
// The document is handed over as a FILE rather than left to the empty --seed
// default, because a durable store with no --seed now seeds nothing at all (see
// loadSeed, and TestADurableStoreWithNoSeedSeedsNothing below). The fixture under
// test is the same seed.Example either way; what the explicit path buys is that
// this test keeps exercising Apply against an enforcing backend instead of
// quietly becoming a test that nothing was written.
//
// This test is deliberately end-to-end and deliberately assertion-light. It does
// not check what was seeded — seed/ owns that. It checks that seeding an
// enforcing backend WORKS, which is the property that silently disappears.
func TestSeedingAnEnforcingStoreSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "aperture.db")
	examplePath := writeSeed(t, "example.yaml", string(seed.Example))

	store, err := buildStore(ctx, dsn, examplePath)
	if err != nil {
		t.Fatalf("seeding a SQLite store failed: %v\n\n"+
			"This is almost certainly the ORDER of the loops in seed.Document.Apply: "+
			"an entity was written before something it references, and the database refused it. "+
			"See the dependency order documented above Apply, and the \"Referential integrity\" "+
			"note in storage/sqlite/schema.sql.", err)
	}
	t.Cleanup(func() {
		if c, ok := store.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	// Anti-vacuity: a seed that wrote nothing would also "succeed".
	grants, err := store.ListGrants(ctx, "acme")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) == 0 {
		t.Fatal("the store seeded without error but holds no grants for acme; " +
			"this test would pass against a loader that did nothing")
	}

	// Re-seeding the SAME database exercises the upsert path, where every entity
	// row already exists. That path used to be INSERT OR REPLACE, which deletes
	// the row before re-inserting it and so fires the children's ON DELETE
	// actions — under these foreign keys, re-seeding a principal who is in a
	// group would be refused outright.
	if err := loadSeed(ctx, store, examplePath, storeSQLite); err != nil {
		t.Fatalf("re-seeding an already-seeded store failed: %v\n\n"+
			"An entity upsert is deleting its row instead of updating it in place "+
			"(INSERT OR REPLACE rather than ON CONFLICT DO UPDATE).", err)
	}
	var _ model.Storage = store
}

// TestReseedingAChangedDocumentSucceeds is the E3-S2 half of the same problem.
//
// TestSeedingAnEnforcingStoreSucceeds re-applies the IDENTICAL document, which
// only exercises the upsert of unchanged rows. The case that actually breaks
// under RESTRICT is a re-seed that REPLACES an entity's children: a role's
// permission bundle, a principal's roles, and a group's members are entity
// FIELDS, and every backend implements "put" for them by clearing the join table
// and rewriting it. That clear is a delete, it is invisible from the seed call
// site, and it is fired against tables that sit on the child side of three
// enforced edges.
//
// So this seeds a full model, then re-seeds a document that empties all three
// bundles and renames the entities, and requires it to succeed AND to be
// observable. Against storage/memory it would pass no matter what; the point is
// the enforcing backend.
func TestReseedingAChangedDocumentSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "reseed.db")

	store, err := buildStore(ctx, dsn, writeSeed(t, "before.yaml", delSeed))
	if err != nil {
		t.Fatalf("initial seed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The same ids, with every child bundle emptied and every label changed.
	const after = `
accounts:
  - {id: acme, name: Acme Renamed, description: Same account, new label.}
principals:
  - {id: root, kind: user, identity: "user:root", display_name: Root}
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice Renamed, roles: []}
memberships:
  - {principal: root, account: acme}
  - {principal: alice, account: acme}
object_types:
  - {name: system, description: Same type, new label., actions: [aperture.admin]}
permissions:
  - {id: perm-admin, object_type: system, action: aperture.admin, description: Same permission, new label.}
roles:
  - {id: editor, name: Editor Renamed, description: No permissions any more., permissions: []}
groups:
  - {id: writers, name: Writers Renamed, description: No members any more., members: []}
`
	if err := loadSeed(ctx, store, writeSeed(t, "after.yaml", after), storeSQLite); err != nil {
		t.Fatalf("re-seeding a CHANGED document failed: %v\n\n"+
			"An entity's child bundle (role permissions, principal roles, group members) is "+
			"rewritten by clearing its join table first. That clear is a delete against the "+
			"child side of an enforced edge — see the \"Referential integrity\" note in "+
			"storage/sqlite/schema.sql.", err)
	}

	// Anti-vacuity: a re-seed that silently did nothing would also "succeed".
	role, err := store.GetRole(ctx, "editor")
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if role.Name != "Editor Renamed" || len(role.PermissionIDs) != 0 {
		t.Fatalf("role after re-seed = %+v, want the renamed role with no permissions", role)
	}
	grp, err := store.GetGroup(ctx, "writers")
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if len(grp.MemberPrincipalIDs) != 0 {
		t.Fatalf("group members after re-seed = %v, want none", grp.MemberPrincipalIDs)
	}
	p, err := store.GetPrincipal(ctx, "alice")
	if err != nil {
		t.Fatalf("get principal: %v", err)
	}
	if len(p.RoleIDs) != 0 {
		t.Fatalf("principal roles after re-seed = %v, want none", p.RoleIDs)
	}
}

// TestImportingIntoAnEnforcingStoreSucceeds closes the second half of the same
// blind spot, on the other caller of seed.Document.Apply.
//
// service.Import funnels a whole Document through Apply inside store.Atomic, and
// it is covered on storage/memory only — a backend that enforces nothing and so
// cannot tell a correct write order from a wrong one. SQLite's foreign keys are
// IMMEDIATE, not deferred, so being inside a transaction buys Apply nothing: a
// row written before the row it references is refused at the statement, not at
// COMMIT. This drives `aperture import` end to end over a SQLite store with a
// document that introduces a brand-new dependency chain, so every edge Apply
// orders for is actually exercised.
func TestImportingIntoAnEnforcingStoreSucceeds(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "import.db")

	// A whole new chain: object type -> permission -> role -> principal ->
	// membership + group -> grant. Nothing here exists in delSeed, so each write
	// depends on one earlier in Apply's loop order rather than on a seeded row.
	const state = `
accounts:
  - {id: beta, name: Beta, description: A second tenant.}
object_types:
  - {name: doc, description: A document., actions: [read]}
permissions:
  - {id: perm-read, object_type: doc, action: read, description: Read a document.}
roles:
  - {id: reader, name: Reader, description: Reads documents., permissions: [perm-read]}
principals:
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: [reader]}
memberships:
  - {principal: bob, account: beta}
groups:
  - {id: readers, name: Readers, description: Holds bob., members: [bob]}
grants:
  - id: g-bob
    account: beta
    subject: {kind: group, id: readers}
    permission: perm-read
    object: "doc:*"
    effect: allow
`
	statePath := writeSeed(t, "state.yaml", state)
	out, err := runArgv(t, "import",
		"--seed", writeSeed(t, "boot.yaml", delSeed), "--store", dsn,
		"--principal", "root", "--account", "acme", "--file", statePath)
	if err != nil {
		t.Fatalf("importing a state file into a SQLite store failed: %v\n\noutput: %s\n\n"+
			"This is almost certainly the ORDER of the loops in seed.Document.Apply. "+
			"SQLite's foreign keys are immediate, so the surrounding transaction does not "+
			"defer the check to COMMIT.", err, out)
	}

	// Anti-vacuity: read the far end of the chain back out.
	store, err := buildStore(ctx, dsn, writeSeed(t, "empty.yaml", emptySeed))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	grants, err := store.ListGrants(ctx, "beta")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].ID != "g-bob" {
		t.Fatalf("grants for beta = %+v, want exactly g-bob", grants)
	}
}

// TestADurableStoreWithNoSeedSeedsNothing is the E2-S4 gate, and it asserts BOTH
// halves of one decision from the same file.
//
// loadSeed used to default to the embedded acme fixture whenever --seed was
// empty, whatever --store pointed at. seed.Document.Apply upserts the whole model
// and deliberately sits outside the ManagedEntities posture, so
// `aperture serve --store postgres://prod` with no --seed wrote the demo model
// into production, and two instances sharing one database re-asserted their own
// model over each other on every restart. Nothing refused it and nothing said it
// had happened.
//
// The two halves have to be asserted together because each one stays green when
// the other breaks. Drop the durable skip and the demo tests still pass; drop the
// in-memory fixture and the durable cases here still pass — while every
// getting-started page, seed.ExampleAccount as the default --account, and
// cmd/aperture's end-to-end test would be silently wrong. So:
//
//   - a durable store (SQLite here; Postgres shares the classification through
//     classifyStore, which is the only place the rule is written) with no --seed
//     writes NO model rows, and re-booting over a model somebody else provisioned
//     leaves it exactly as it was; and
//   - an in-memory store with no --seed still loads the fixture.
func TestADurableStoreWithNoSeedSeedsNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("durable, no --seed: nothing is written", func(t *testing.T) {
		dsn := "file:" + filepath.Join(t.TempDir(), "unseeded.db")

		store, err := buildStore(ctx, dsn, "")
		if err != nil {
			t.Fatalf("booting a durable store with no --seed: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })

		assertModelIsEmpty(ctx, t, store, "a durable store booted with no --seed")
	})

	t.Run("durable, no --seed: an existing model survives the boot", func(t *testing.T) {
		// The anti-vacuity half: "no rows" is also true of a store nothing ever
		// wrote to. Provision a model the way an operator would, then boot again
		// with no --seed and require the model to be untouched — in particular NOT
		// overwritten by the acme fixture, which is exactly what the bug did.
		dsn := "file:" + filepath.Join(t.TempDir(), "provisioned.db")

		provisioned, err := buildStore(ctx, dsn, writeSeed(t, "provisioned.yaml", delSeed))
		if err != nil {
			t.Fatalf("provisioning the durable store: %v", err)
		}
		// seed.Export emits every slice in a stable order, so the marshalled
		// document is a byte-comparable snapshot of the whole model. Comparing it
		// is stronger than naming the entities the fixture happens to add today:
		// ANY divergence fails, including one the example document grows later.
		before := exportModel(ctx, t, provisioned)
		if err := provisioned.Close(); err != nil {
			t.Fatalf("close after provisioning: %v", err)
		}

		reboot, err := buildStore(ctx, dsn, "")
		if err != nil {
			t.Fatalf("re-booting the provisioned store with no --seed: %v", err)
		}
		t.Cleanup(func() { _ = reboot.Close() })

		if after := exportModel(ctx, t, reboot); after != before {
			t.Errorf("the model changed across a boot with no --seed.\n\nbefore:\n%s\nafter:\n%s\n"+
				"The embedded acme fixture has been applied over a model the operator "+
				"provisioned. loadSeed must seed nothing when --seed is empty and "+
				"classifyStore reports a durable backend.", before, after)
		}
	})

	t.Run("durable, no --seed: a real command writes nothing either", func(t *testing.T) {
		// Through the real command tree, because buildStore is reached from nine
		// commands and the flag value is what an operator actually types. `check`
		// stands in for `serve`: the two share buildStore verbatim, and `check`
		// exits instead of listening.
		dsn := "file:" + filepath.Join(t.TempDir(), "checked.db")

		// An empty model denies, and a clean deny is a non-zero ExitCoder that
		// urfave/cli would otherwise turn into os.Exit; the no-op ExitErrHandler
		// keeps it inside the test process (the same idiom as runCheckCommand).
		var out bytes.Buffer
		app := NewApp("test")
		app.Writer = &out
		app.ErrWriter = &out
		app.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
		err := app.Run(ctx, []string{"aperture", "check", "--store", dsn,
			"alice", "read", "account:acme/project:atlas/document:42"})
		if !strings.HasPrefix(out.String(), "deny\n") {
			t.Fatalf("check against an unseeded durable store printed %q (err %v), want a deny",
				out.String(), err)
		}

		store, err := openStore(dsn)
		if err != nil {
			t.Fatalf("reopen the store: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		assertModelIsEmpty(ctx, t, store, "`aperture check --store <path>` with no --seed")
	})

	t.Run("in-memory, no --seed: the demo fixture still loads", func(t *testing.T) {
		store, err := buildStore(ctx, "", "")
		if err != nil {
			t.Fatalf("booting the zero-flag demo: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })

		grants, err := store.ListGrants(ctx, seed.ExampleAccount)
		if err != nil {
			t.Fatalf("list grants: %v", err)
		}
		if len(grants) == 0 {
			t.Fatalf("the in-memory store booted with no --seed holds no grants for %q. "+
				"The zero-flag demo is what every getting-started page, the default "+
				"--account (seed.ExampleAccount) and cmd/aperture's end-to-end test "+
				"rest on; the durable skip must not have taken it away.",
				seed.ExampleAccount)
		}
	})
}

// exportModel snapshots the whole model as a stable YAML document, so two boots
// can be compared byte for byte.
func exportModel(ctx context.Context, t *testing.T, store model.Storage) string {
	t.Helper()
	doc, err := seed.Export(ctx, store)
	if err != nil {
		t.Fatalf("export the model: %v", err)
	}
	out, err := seed.Marshal(doc, seed.FormatYAML)
	if err != nil {
		t.Fatalf("marshal the exported model: %v", err)
	}
	return string(out)
}

// assertModelIsEmpty fails unless the store holds no model rows at all.
//
// It reads EVERY unscoped list on model.Storage, plus the account-scoped grant
// list for the fixture's own account, rather than picking one of them: the bug it
// guards wrote the whole document, so a check that looked only at grants would
// pass against a fixture whose accounts, principals, roles and rules had all
// landed.
func assertModelIsEmpty(ctx context.Context, t *testing.T, store model.Storage, what string) {
	t.Helper()

	lists := []struct {
		entity string
		count  func() (int, error)
	}{
		{"accounts", func() (int, error) { v, err := store.ListAccounts(ctx); return len(v), err }},
		{"principals", func() (int, error) { v, err := store.ListPrincipals(ctx); return len(v), err }},
		{"object types", func() (int, error) { v, err := store.ListObjectTypes(ctx); return len(v), err }},
		{"permissions", func() (int, error) { v, err := store.ListPermissions(ctx); return len(v), err }},
		{"roles", func() (int, error) { v, err := store.ListRoles(ctx); return len(v), err }},
		{"groups", func() (int, error) { v, err := store.ListGroups(ctx); return len(v), err }},
		{"templates", func() (int, error) { v, err := store.ListTemplates(ctx); return len(v), err }},
		{"rules", func() (int, error) { v, err := store.ListRules(ctx); return len(v), err }},
		{"grants for " + seed.ExampleAccount, func() (int, error) {
			v, err := store.ListGrants(ctx, seed.ExampleAccount)
			return len(v), err
		}},
	}
	for _, l := range lists {
		n, err := l.count()
		if err != nil {
			t.Fatalf("list %s: %v", l.entity, err)
		}
		if n != 0 {
			t.Errorf("%s holds %d %s, want none. The embedded acme fixture is being "+
				"applied to a database the operator named: loadSeed must seed nothing "+
				"when --seed is empty and classifyStore reports a durable backend.",
				what, n, l.entity)
		}
	}
}
