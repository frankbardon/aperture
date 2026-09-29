package cli

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"

	_ "modernc.org/sqlite"
)

// A DB-wired boot layers an attribute slot exactly as one seed document does.
//
// The two shapes this file holds apart, because only one of them is a collision:
//
//   - a shared `attribute_providers:` row AND a local inline `attributes:` block for
//     the same slot are the slot's TWO LAYERS. The row is the shared layer, the
//     inline bags are the local one, a fetch reads their merge, and the shared layer
//     wins every key both serve.
//   - a shared `attribute_providers:` row AND a LOCAL `attribute_providers:` entry
//     for the same slot are two candidates for ONE layer, and stay refused
//     (TestEveryCollidingSectionIsRefusedByName's "an attribute_providers: entry for
//     a shared slot" case).
//
// The first used to be refused too, which left NO spelling that reached the merge:
// an instance that wanted a field its deployment's shared directory does not carry
// had to abandon the shared directory. That is the mutual exclusivity
// provider.AttributeLayer exists to have reversed, restated as a refusal — worse
// than the discard it replaced, because the instance does not start at all.

// hostUsersDB creates the HOST's own users table — the directory a shared
// `attribute_providers:` row reads through — and returns a pool over it.
//
// It is a real database, on SQLite, because the merge is only observable if the
// shared layer actually ANSWERS: a fake pool that refuses every statement proves
// the two layers were registered and nothing about which one a contested key comes
// out of, which is the entire property under test.
func hostUsersDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open the host database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, department TEXT NOT NULL)`); err != nil {
		t.Fatalf("create the host users table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, department) VALUES ('alice', 'eng')`); err != nil {
		t.Fatalf("populate the host users table: %v", err)
	}
	return db
}

// hostUserFetch is the shared row's get_one. The placeholder is SQLite's because
// the fixture's database is SQLite; sqlprovider binds the bare subject key
// verbatim as the statement's one argument and never rewrites the text.
const hostUserFetch = `SELECT department FROM users WHERE id = ?`

// sharedUserSlot is the wiring an operator pushes for the user slot: one
// connection name and one kind: sql attribute provider over it.
func sharedUserSlot(now time.Time) model.WiringSet {
	return model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main", CreatedAt: now, UpdatedAt: now}},
		AttributeProviders: []model.WiringAttributeProvider{{
			Subject:    "user",
			Kind:       "sql",
			Connection: "main",
			GetOne:     hostUserFetch,
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
	}
}

// TestASharedSlotAndALocalInlineBlockAreTheSlotsTwoLayers is the newly legal case,
// asserted through the REAL builders and a database that really answers.
//
// The inline bag deliberately CONTESTS one key and adds another, because the two
// halves fail in opposite directions. If the local layer could win `department`, a
// file on one machine would change what `principal.department == "eng"` compares
// against on that machine only — the same grant, a different verdict, with nothing
// in a verdict, a trace or a note to say which layer answered. If the shared layer
// were the only one read, `cost_centre` would be absent, every predicate over it
// would be false, and an instance could not add a field its deployment's directory
// does not carry without abandoning the directory.
func TestASharedSlotAndALocalInlineBlockAreTheSlotsTwoLayers(t *testing.T) {
	ctx := context.Background()
	db := hostUsersDB(t)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	local, err := seed.Parse([]byte(`
attributes:
  - subject: user
    id: alice
    metadata: {department: hr, cost_centre: cc-42}
`), seed.FormatYAML)
	if err != nil {
		t.Fatalf("parse the local document: %v", err)
	}

	doc, err := wiringDocument(sharedUserSlot(time.Now().UTC()), local)
	if err != nil {
		t.Fatalf("a shared attribute_providers: row beside a local inline attributes: block "+
			"must LAYER, not refuse: the shared row is the slot's shared layer and the inline "+
			"bags its local one, so nothing is dropped and a contested key reads the same on "+
			"every instance in the fleet. Refusing it leaves no spelling that reaches the "+
			"merge at all: %v", err)
	}

	// Reported, never silent: an operator debugging an unexpected attribute value is
	// told which layer answers a contested key, by slot name and never by key.
	if got, want := doc.AttributeCollisions(), []string{"user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AttributeCollisions() = %v, want %v — the layering is reported at the boot "+
			"(decisionStack.reportCollisions), because which layer answers is exactly what an "+
			"operator cannot see in a verdict", got, want)
	}

	_, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(string, seed.ConnectionSettings) (seed.Pool, error) {
			return db, nil
		}))
	if err != nil {
		t.Fatalf("building the object registry over the projected document: %v", err)
	}
	defer func() { _ = conns.Close() }()

	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("building the attribute registry over the projected document: %v", err)
	}

	if got, want := attrs.Layers(provider.AttributeSlotUser),
		[]provider.AttributeLayer{provider.AttributeLayerShared, provider.AttributeLayerLocal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Layers(user) = %v, want %v — the shared row must land in the SHARED layer "+
			"and the inline block in the LOCAL one, in that precedence order", got, want)
	}

	bag, err := attrs.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	if bag["department"] != "eng" {
		t.Errorf("department = %#v, want \"eng\" from the SHARED layer: a rule is written "+
			"against a DEPLOYMENT, so a local file must not be able to redefine a key the "+
			"shared directory serves (bag: %#v)", bag["department"], bag)
	}
	if bag["cost_centre"] != "cc-42" {
		t.Errorf("cost_centre = %#v, want \"cc-42\" from the LOCAL layer: nothing is dropped, "+
			"and contributing a field the shared directory does not carry is the whole reason "+
			"the second layer exists (bag: %#v)", bag["cost_centre"], bag)
	}
}

// TestALocalAttributeProviderForASharedSlotIsStillRefused is the other half, and
// the line the relaxation above must not cross.
//
// Two `attribute_providers:` entries for one slot — one pushed, one in this
// instance's file — are two candidates for the SAME layer: the registry holds at
// most one provider per (slot, layer), so either resolution silently drops a whole
// directory somebody declared, and "last writer wins" over a directory is how one
// deployment's user table quietly shadows another's.
func TestALocalAttributeProviderForASharedSlotIsStillRefused(t *testing.T) {
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	local := &seed.Document{AttributeProviders: []seed.AttributeProvider{
		{Subject: "user", Kind: "csv", Path: "users.csv"},
	}}
	_, err := wiringDocument(sharedUserSlot(time.Now().UTC()), local)
	mustRefuse(t, "a local attribute_providers: entry for a shared slot", err,
		aerr.APERTURE_WIRING_LOCAL_COLLISION, "user", "attribute_providers:")
}
