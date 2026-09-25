package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
)

// E2-S2: the database is authoritative; the local file may only ADD.
//
// The four states an instance can be in, and the fifth that has no document in
// it at all:
//
//   - DB only    -> TestTheDatabaseWiresAnInstanceThatHasNoSeedFile (E2-S1).
//   - local only -> TestEmptyWiringTablesLeaveTheLocalSeedInCharge (E2-S1).
//   - DISJOINT   -> both are built. This is Arc's permanent situation and the
//     reason the rule is additive rather than exclusive.
//   - COLLIDING  -> the boot fails, coded, naming the entry and both sections.
//   - Go         -> a host registering onto the registry a stack already built
//     gets the same treatment from the registry itself, because the rule is about
//     the REGISTRY and a Register call passes through no document.
//
// Every collision case asserts the CODE and the NAME, not just that something
// failed: the whole value of refusing rather than resolving is that the operator
// is told which of two configuration sources to go and edit, and a refusal that
// says only "aperture failed to start" would have cost them the fleet-wide push
// they are trying to diagnose.

// localAdditionSeed is the seed file an Arc-shaped instance keeps after its
// wiring has been pushed: the model the wiring hangs off, plus the wiring only
// this machine can describe — a kind: csv provider (a path is machine-local, so
// it can never BE shared wiring), the field types for its own inline objects, and
// one attribute slot the push left alone.
const localAdditionSeed = `
accounts:
  - {id: acme, name: Acme Corp}
object_types:
  - name: document
    description: The type the shared wiring serves.
    actions: [read]
  - name: project
    description: The type this instance serves from its own file.
    actions: [read]
providers:
  - object_type: project
    kind: csv
    path: projects.csv
field_types:
  - object_type: task
    fields:
      due_on: date
attributes:
  - subject: account
    id: acme
    metadata: {plan: enterprise}
`

// writeLocalAddition writes localAdditionSeed and the CSV its providers: entry
// resolves, in one directory, and returns the seed path. The CSV is real because
// a csv provider reads its file at build: a fixture that only looked declared
// would pass this test while an operator's boot failed.
func writeLocalAddition(t *testing.T, body string) string {
	t.Helper()
	seedPath := writeSeed(t, "local-addition.yaml", body)
	csv := "id,tier\nproject:atlas,gold\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(seedPath), "projects.csv"), []byte(csv), 0o600); err != nil {
		t.Fatalf("writing the local csv: %v", err)
	}
	return seedPath
}

// TestTheLocalFileAddsWhatTheDatabaseNeverDeclared is the disjoint case, end to
// end through a real store and the real builders.
//
// It is the case that makes the rule usable at all. Arc's `wave` and `metric`
// providers are Go, and a kind: csv provider is refused at the push
// (APERTURE_WIRING_KIND_UNSHAREABLE) because a filesystem path is machine-local —
// so if a DB-wired boot dropped the local wiring sections, pushing any wiring
// would silently switch off every type the database cannot describe, and the
// instance would go on answering with no error anywhere.
func TestTheLocalFileAddsWhatTheDatabaseNeverDeclared(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "disjoint.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	stack := bootStack(t, dsn, writeLocalAddition(t, localAdditionSeed))

	if !stack.registry.Has("document") {
		t.Errorf("the registry serves %v and not the DATABASE-declared type; the shared "+
			"wiring is authoritative and must still be built", stack.registry.Keys())
	}
	if !stack.registry.Has("project") {
		t.Fatalf("the registry serves %v and not the type only the LOCAL file declares. "+
			"A kind: csv provider cannot be shared wiring at all, so dropping the local "+
			"providers: section makes a push a silent loss of every csv-backed type.",
			stack.registry.Keys())
	}
	// Anti-vacuity: "the type is registered" is also true of an empty provider.
	// The local addition has to actually SERVE, through the registry the stack
	// built, from the file beside the seed.
	id, err := identity.Parse("project:atlas")
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	md, err := stack.registry.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("fetching through the locally-added provider: %v", err)
	}
	if md["tier"] != "gold" {
		t.Errorf("metadata from the locally-added csv provider = %v, want tier=gold", md)
	}
	// The attribute seam layers the same way, on the same boot: the database
	// declares the user slot and the file declares the account slot.
	slots := stack.attributes.RegisteredSlots()
	for _, want := range []provider.AttributeSlot{provider.AttributeSlotUser, provider.AttributeSlotAccount} {
		if !stack.attributes.Has(want) {
			t.Errorf("the attribute registry wires %v, and not %q", slots, want)
		}
	}
}

// TestALocalObjectTypeTheDatabaseAlreadyDeclaresFailsTheBoot is the headline
// refusal, asserted through a real store so the failure is the one an operator
// meets.
//
// Resolving it either way is worse than refusing, and neither resolution is
// visible: the database winning discards a provider somebody checked into this
// instance's file, and the file winning means this instance reads `document`
// metadata from a source its peers cannot see. Both surface as a different
// verdict on an identically-configured-looking instance, never as an error.
func TestALocalObjectTypeTheDatabaseAlreadyDeclaresFailsTheBoot(t *testing.T) {
	body := strings.Replace(localAdditionSeed,
		"  - object_type: project\n    kind: csv\n    path: projects.csv\n",
		"  - object_type: document\n    kind: csv\n    path: projects.csv\n", 1)
	if !strings.Contains(body, "object_type: document\n    kind: csv") {
		t.Fatal("the fixture edit did not apply; the test would assert nothing")
	}

	dsn := "file:" + filepath.Join(t.TempDir(), "collide.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	err := bootStackError(t, dsn, writeLocalAddition(t, body))
	if err == nil {
		t.Fatal("a local providers: entry for a DATABASE-declared object type booted anyway; " +
			"one of the two declarations was silently discarded and nothing said which")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_WIRING_LOCAL_COLLISION {
		t.Fatalf("code = %q, want %q (err: %v)", got, aerr.APERTURE_WIRING_LOCAL_COLLISION, err)
	}
	if chain := codeChain(err); len(chain) != 1 {
		t.Fatalf("code chain = %v, want exactly one coded error; bootError must pass this "+
			"code through, and a same-code re-stamp is invisible to CodeOf so depth is "+
			"what catches it", chain)
	}
	for _, want := range []string{"document", "providers:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the operator knows which entry and which "+
				"section to edit; got %q", want, err.Error())
		}
	}
}

// TestEveryCollidingSectionIsRefusedByName walks the four axes a local
// declaration can collide on. They are asserted together because they are ONE
// rule with four spellings, and an axis nobody wrote a case for is an axis where
// a declaration is silently discarded.
//
// Each case is built on the projection rather than on a store, so the axis under
// test is the only thing that differs; the end-to-end proof that the refusal
// reaches an operator's boot is the test above.
func TestEveryCollidingSectionIsRefusedByName(t *testing.T) {
	now := time.Now().UTC()
	shared := model.WiringSet{
		Providers: []model.WiringProvider{{
			ObjectType: "document", Kind: "sql", Connection: "main",
			GetOne: "SELECT 1", GetAll: "SELECT 1", CreatedAt: now, UpdatedAt: now,
		}},
		FieldTypes: []model.WiringFieldType{{
			ObjectType: "project", Field: "started_on", DeclaredType: "date",
			CreatedAt: now, UpdatedAt: now,
		}},
		AttributeProviders: []model.WiringAttributeProvider{{
			Subject: "user", Kind: "sql", Connection: "main",
			GetOne: "SELECT 1", CreatedAt: now, UpdatedAt: now,
		}},
		Connections: []model.WiringConnection{{Name: "main", CreatedAt: now, UpdatedAt: now}},
	}

	for _, tc := range []struct {
		name  string
		local *seed.Document
		names []string
	}{
		{
			name:  "a providers: entry for a shared object type",
			local: &seed.Document{Providers: []seed.Provider{{ObjectType: "document", Kind: "csv", Path: "d.csv"}}},
			names: []string{"document", "providers:"},
		},
		{
			name: "a field_types: entry for a shared object type",
			local: &seed.Document{FieldTypes: []seed.FieldType{
				{ObjectType: "project", Fields: map[string]string{"started_on": "datetime"}},
			}},
			names: []string{"project", "field_types:"},
		},
		{
			name: "an attribute_providers: entry for a shared slot",
			local: &seed.Document{AttributeProviders: []seed.AttributeProvider{
				{Subject: "user", Kind: "csv", Path: "u.csv"},
			}},
			names: []string{"user", "attribute_providers:"},
		},
		{
			// The inline DATA section, on the axis where a silent discard is
			// worst: the external source wins a slot ENTIRELY, every read of it
			// then returns an empty bag, and an empty bag does not deny — it
			// WIDENS an exclusive grant, with nothing in the verdict saying a bag
			// went missing.
			name: "an inline attributes: entry for a shared slot",
			local: &seed.Document{Attributes: []seed.Attribute{
				{Subject: "user", ID: "alice"},
			}},
			names: []string{"user", "attributes:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wiringDocument(shared, tc.local)
			if err == nil {
				t.Fatal("the colliding declaration was accepted; one of the two sources was " +
					"silently discarded")
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_WIRING_LOCAL_COLLISION {
				t.Fatalf("code = %q, want %q (err: %v)", got, aerr.APERTURE_WIRING_LOCAL_COLLISION, err)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must name %q; got %q", want, err.Error())
				}
			}
		})
	}
}

// TestALocalInlineObjectAgainstASharedProviderReusesTheStrictPosture pins the one
// axis that is NOT refused by wiringDocument.
//
// `objects:` against a shared `providers:` entry is exactly what
// seed.StrictProviderCollision() already refuses, so a DB-wired boot passes that
// option rather than restating the rule — and the default silent discard stays
// the default on the file-only path, where adding a providers: row while inline
// entries are still in one author's file is an ordinary migration step.
func TestALocalInlineObjectAgainstASharedProviderReusesTheStrictPosture(t *testing.T) {
	// bootWiringSeed's inline `objects:` block declares project, and the shared
	// set is given a provider for the same type.
	now := time.Now().UTC()
	set := sharedWiringSet(now)
	set.Providers = append(set.Providers, model.WiringProvider{
		ObjectType: "project", Kind: "sql", Connection: "main",
		GetOne:    "SELECT tier FROM projects WHERE id = $1",
		GetAll:    "SELECT 'project:' || p.id AS id, p.tier FROM projects p",
		CreatedAt: now, UpdatedAt: now,
	})
	model.SortWiringProviders(set.Providers)

	dsn := "file:" + filepath.Join(t.TempDir(), "inline-collide.db")
	pushWiring(t, dsn, set)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	err := bootStackError(t, dsn, writeSeed(t, "inline.yaml", bootWiringSeed))
	if err == nil {
		t.Fatal("an inline objects: entry for a DATABASE-declared type booted anyway. The " +
			"discard is type-level and total, so a push on another host had just switched " +
			"off metadata checked into this instance's seed file, silently.")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %q, want %q — the refusal is seed.StrictProviderCollision()'s own, "+
			"not a second mechanism (err: %v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("the refusal must name the colliding type; got %q", err.Error())
	}

	// And the file-only path is UNCHANGED: with no wiring rows the same overlap
	// is the documented silent discard, reported rather than refused. Every
	// deployment that exists is on this path and there is no flag to opt out of a
	// regression here.
	t.Run("the file-only path keeps the documented discard", func(t *testing.T) {
		unpushed := "file:" + filepath.Join(t.TempDir(), "unpushed.db")
		local := bootWiringSeed + `
providers:
  - object_type: project
    kind: csv
    path: projects.csv
`
		seedPath := writeSeed(t, "file-only.yaml", local)
		csv := "id,tier\naccount:acme/project:atlas,silver\n"
		if err := os.WriteFile(filepath.Join(filepath.Dir(seedPath), "projects.csv"), []byte(csv), 0o600); err != nil {
			t.Fatalf("writing the csv: %v", err)
		}
		stack := bootStack(t, unpushed, seedPath)
		if got := stack.collisions; len(got) != 1 || got[0] != "project" {
			t.Fatalf("collisions = %v, want [project] reported (not refused): the strict "+
				"posture is for the ASSEMBLED document only", got)
		}
	})
}

// TestAGoRegistrationCollidesAtTheRegistry is the acceptance criterion with no
// document in it, and it is why the rule is stated about the REGISTRY.
//
// Arc registers `wave` and `metric` by calling provider.Registry.Register from
// its own apertureScopeDeps, on the registry a decision stack already built —
// after every document has been read and projected. wiringDocument cannot see
// that call and must not try to: a projection that guessed at future Register
// calls would be a second rule, and it would disagree with the registry the
// moment a host registered in a different order.
//
// So the check lives where the registrations meet, in Register's own duplicate
// refusal, and this test asserts both halves from one built stack: the type the
// shared wiring declared is refused, and the type nothing declared is accepted.
func TestAGoRegistrationCollidesAtTheRegistry(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "goreg.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	stack := bootStack(t, dsn, "")

	wave, err := provider.NewStatic([]provider.Object{{
		ID:       mustIdentity(t, "wave:1"),
		Metadata: provider.Metadata{"amplitude": int64(3)},
	}})
	if err != nil {
		t.Fatalf("building the host's Go provider: %v", err)
	}

	// The ADD half: a type the shared wiring never declared registers cleanly, so
	// a Go host can read a pushed wiring at all.
	if err := stack.registry.Register("wave", wave, provider.WithTTL(0)); err != nil {
		t.Fatalf("registering a Go provider for a type the database never declared: %v. "+
			"Arc's catalog providers are hand-written Go that no document can describe; "+
			"refusing them would mean it could never read shared wiring.", err)
	}

	// The COLLISION half: the same call for a DATABASE-declared type is refused,
	// with the object type named. The code differs from a file collision's on
	// purpose — the remedy is a duplicate registration a developer removes, not
	// two configuration sources an operator edits — but the ANSWER is the same,
	// and it comes from the registry rather than from a second rule.
	err = stack.registry.Register("document", wave, provider.WithTTL(0))
	if err == nil {
		t.Fatal("a Go registration for a DATABASE-declared object type was accepted. The " +
			"rule is about the registry, not the source syntax: a host that shadowed a " +
			"shared provider in Go would answer `document` questions from a source its " +
			"peers cannot see.")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_INVALID {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_PROVIDER_INVALID)
	}
	if !strings.Contains(err.Error(), "object type") {
		t.Errorf("the refusal must say what collided; got %q", err.Error())
	}

	// The attribute registry answers the same way for a slot, which matters
	// because a second registration there is "last writer wins" over a whole
	// directory: one deployment's user table quietly shadowing another's.
	bags, err := provider.NewStaticAttributes([]provider.AttributeRecord{
		{ID: "alice", Attributes: provider.Metadata{"department": "eng"}},
	})
	if err != nil {
		t.Fatalf("building the host's attribute provider: %v", err)
	}
	if err := stack.attributes.Register(provider.AttributeSlotUser, bags, provider.WithTTL(0)); err == nil {
		t.Error("a Go registration for a DATABASE-declared attribute slot was accepted")
	} else if got := aerr.CodeOf(err); got != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
		t.Errorf("code = %q, want %q", got, aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID)
	}
	if err := stack.attributes.Register(provider.AttributeSlotMachine, bags, provider.WithTTL(0)); err != nil {
		t.Errorf("registering a Go attribute provider for a slot the database never declared: %v", err)
	}
}

// TestALocalConnectionForALocallyAddedProviderIsCarried closes the gap the
// additive rule opens on the one section it does not apply to.
//
// A locally-added kind: sql provider reads through a connections: entry only this
// instance's file declares. If the projection carried only the entries that route
// a SHARED name, that provider would fail the build for an undeclared connection
// — an addition that cannot be added.
func TestALocalConnectionForALocallyAddedProviderIsCarried(t *testing.T) {
	local := &seed.Document{
		Connections: map[string]seed.Connection{
			"analytics": {DSNEnv: "MY_ANALYTICS_URL"},
		},
		Providers: []seed.Provider{{
			ObjectType: "report",
			Kind:       "sql",
			Connection: "analytics",
			GetOne:     "SELECT title FROM reports WHERE id = $1",
			GetAll:     "SELECT 'report:' || r.id AS id, r.title FROM reports r",
		}},
	}
	doc, err := wiringDocument(sharedWiringSet(time.Now().UTC()), local)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	t.Setenv("MY_ANALYTICS_URL", unroutedDSN)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	opened := map[string]bool{}
	reg, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(name string, _ seed.ConnectionSettings) (seed.Pool, error) {
			opened[name] = true
			return fakePool{}, nil
		}))
	if err != nil {
		t.Fatalf("building with a locally-declared connection: %v", err)
	}
	defer func() { _ = conns.Close() }()

	if !opened["analytics"] || !opened["main"] {
		t.Fatalf("pools opened = %v, want both the shared name and the local one", opened)
	}
	if !reg.Has("report") || !reg.Has("document") {
		t.Fatalf("the registry serves %v, want both the added type and the shared one", reg.Keys())
	}
}

// mustIdentity parses an identity or fails the test.
func mustIdentity(t *testing.T, s string) identity.Identity {
	t.Helper()
	id, err := identity.Parse(s)
	if err != nil {
		t.Fatalf("identity.Parse(%q): %v", s, err)
	}
	return id
}
