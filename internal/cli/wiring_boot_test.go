package cli

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// E2-S1: an instance can be wired by the database it opens.
//
// The two halves this file exists to hold apart:
//
//   - wiring rows PRESENT -> the registries are built from the database, and an
//     instance with no seed file at all gets working object providers, field types
//     and attribute slots.
//   - wiring rows EMPTY -> the local seed file's wiring is used exactly as it was
//     before these tables existed. That half is the load-bearing one: every
//     deployment in existence is in it, and there is no flag to opt out of a
//     regression here.
//
// The projection is asserted through the REAL builders (seed's own
// BuildRegistryWithConnections / BuildAttributeRegistryWithConnections), never
// against a second implementation, because building the registries a different
// way is precisely the failure mode this story rules out: two instances that hold
// equivalent-but-not-identical wiring authorize differently and nothing says so.

// bootWiringSeed declares the MODEL the wiring tables hang off — the object types
// a provider entry's foreign key points at — plus one LOCAL wiring section
// (`objects:` for project) so a boot that reads the database can be shown NOT to
// have read the file, and a boot that reads the file can be shown to have.
const bootWiringSeed = `
accounts:
  - {id: acme, name: Acme Corp}
object_types:
  - name: document
    description: The type the shared wiring serves.
    actions: [read]
  - name: project
    description: The type the local file serves inline.
    actions: [read]
objects:
  - id: "account:acme/project:atlas"
    metadata: {tier: gold}
`

// unroutedDSN is a well-formed Postgres DSN that is never dialled. sql.Open is
// lazy and pgx's stdlib driver only PARSES the string, so a registry builds over
// it with no server anywhere — which is how CI runs, since the pipeline has no
// service containers.
const unroutedDSN = "postgres://someone:secret@127.0.0.1:1/nothing?sslmode=disable"

// sharedWiringSet is the set an operator's `aperture wiring push` would have
// written for bootWiringSeed: one connection name, one kind: sql provider over
// it, one field-type declaration, and one attribute slot.
func sharedWiringSet(now time.Time) model.WiringSet {
	return model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main", CreatedAt: now, UpdatedAt: now}},
		Providers: []model.WiringProvider{{
			ObjectType: "document",
			Kind:       "sql",
			Connection: "main",
			GetOne:     "SELECT tier FROM documents WHERE id = $1",
			GetAll:     "SELECT 'document:' || d.id AS id, d.tier FROM documents d",
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		FieldTypes: []model.WiringFieldType{{
			ObjectType:   "document",
			Field:        "reviewed_on",
			DeclaredType: "date",
			CreatedAt:    now,
			UpdatedAt:    now,
		}},
		AttributeProviders: []model.WiringAttributeProvider{{
			Subject:    "user",
			Kind:       "sql",
			Connection: "main",
			GetOne:     "SELECT department FROM users WHERE id = $1",
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
	}
}

// bootStack builds a decision stack the way a real command does: through a
// command whose --store and --seed flags were actually PARSED, because
// buildDecisionStack reads --store to answer loadSeed's own question (does an
// absent --seed mean the embedded demo, or nothing?) and a hand-made
// &ucli.Command{} answers "in-memory" to it.
func bootStack(t *testing.T, storeDSN, seedPath string) decisionStack {
	t.Helper()
	ctx := context.Background()

	store, err := buildStore(ctx, storeDSN, seedPath)
	if err != nil {
		t.Fatalf("buildStore(%q, %q): %v", storeDSN, seedPath, err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var stack decisionStack
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: storeFlags(),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			var err error
			stack, err = buildDecisionStack(ctx, cmd, store, cmd.String("seed"))
			return err
		},
	}
	args := []string{"probe", "--store", storeDSN}
	if seedPath != "" {
		args = append(args, "--seed", seedPath)
	}
	if err := cmd.Run(ctx, args); err != nil {
		t.Fatalf("buildDecisionStack(--store %q --seed %q): %v", storeDSN, seedPath, err)
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

// bootStackError is bootStack for the cases that must REFUSE.
func bootStackError(t *testing.T, storeDSN, seedPath string) error {
	t.Helper()
	ctx := context.Background()

	store, err := buildStore(ctx, storeDSN, seedPath)
	if err != nil {
		t.Fatalf("buildStore(%q, %q): %v", storeDSN, seedPath, err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var buildErr error
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: storeFlags(),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			stack, err := buildDecisionStack(ctx, cmd, store, cmd.String("seed"))
			if err == nil {
				_ = stack.Close()
			}
			buildErr = err
			return nil
		},
	}
	args := []string{"probe", "--store", storeDSN}
	if seedPath != "" {
		args = append(args, "--seed", seedPath)
	}
	if err := cmd.Run(ctx, args); err != nil {
		t.Fatalf("running the probe command: %v", err)
	}
	return buildErr
}

// pushWiring provisions the model from bootWiringSeed and replaces the store's
// wiring with set, then closes the store — the two acts an operator performs
// before the second instance is deployed.
func pushWiring(t *testing.T, storeDSN string, set model.WiringSet) {
	t.Helper()
	ctx := context.Background()

	store, err := buildStore(ctx, storeDSN, writeSeed(t, "boot-wiring.yaml", bootWiringSeed))
	if err != nil {
		t.Fatalf("provisioning the store: %v", err)
	}
	if err := store.ReplaceWiring(ctx, set); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}
}

// TestEmptyWiringTablesLeaveTheLocalSeedInCharge is the backward-compatibility
// half, and it is the one that must never be relaxed: an empty set is an ANSWER
// (model.WiringSet.IsEmpty), not a failure, and every deployment that exists
// today gives it. No flag, no configuration, no behaviour change.
func TestEmptyWiringTablesLeaveTheLocalSeedInCharge(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "unpushed.db")
	seedPath := writeSeed(t, "local.yaml", bootWiringSeed)

	stack := bootStack(t, dsn, seedPath)

	if !stack.registry.Has("project") {
		t.Fatalf("the registry serves %v, and does not serve the local seed's inline "+
			"`objects:` type. With no wiring rows the LOCAL file is the wiring, exactly "+
			"as it was before the tables existed.", stack.registry.Keys())
	}
	if stack.fetcher == nil {
		t.Fatal("the rules metadata fetcher is nil although the local seed declares an object source")
	}
	// Anti-vacuity: "the type is registered" is also true of an empty provider.
	// Read the inline metadata back through the registry the stack actually built.
	id, err := identity.Parse("account:acme/project:atlas")
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	md, err := stack.registry.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("fetching the local seed's inline metadata: %v", err)
	}
	if md["tier"] != "gold" {
		t.Fatalf("inline metadata = %v, want tier=gold", md)
	}
}

// TestTheDatabaseWiresAnInstanceThatHasNoSeedFile is the headline: RBAC's whole
// deployment shape. `--store <dsn>`, no `--seed`, working registries.
func TestTheDatabaseWiresAnInstanceThatHasNoSeedFile(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "pushed.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	// The route. The shared wiring holds the connection NAME and nothing else, so
	// this instance supplies the DSN itself — here through the conventional
	// variable an instance with no Go and no file has left.
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	stack := bootStack(t, dsn, "")

	if !stack.registry.Has("document") {
		t.Fatalf("the registry serves %v, and not the DATABASE-declared object type. "+
			"An instance booted with --store and no --seed must build its object "+
			"providers from the wiring rows.", stack.registry.Keys())
	}
	if stack.registry.Has("project") {
		t.Errorf("the registry serves %v, which includes a type only the LOCAL seed "+
			"declares — and there is no local seed. seedDocument must return an empty "+
			"document for a durable store with no --seed, never the embedded acme fixture.",
			stack.registry.Keys())
	}
	if stack.fetcher == nil {
		t.Error("the rules metadata fetcher is nil although the database declares a provider; " +
			"hasObjectSources must count the projected `providers:` section")
	}
	if !stack.attributes.Has(provider.AttributeSlotUser) {
		t.Fatalf("the attribute registry wires %v, and not the database-declared slot",
			stack.attributes.RegisteredSlots())
	}
}

// TestADurableStoreWithNoSeedIsWiredByNothingAtAll closes the seam the model-state
// fix left open.
//
// loadSeed stopped WRITING the embedded acme fixture to a durable store with no
// --seed — the visible half, because an operator sees rows appear. seedDocument
// went on RETURNING that same fixture, so the instance's object providers, field
// types and inline attribute bags were the demo's. Nothing wrote a row, so
// nothing looked wrong: the instance simply decided against a wiring nobody had
// configured.
func TestADurableStoreWithNoSeedIsWiredByNothingAtAll(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "unwired.db")

	stack := bootStack(t, dsn, "")

	if keys := stack.registry.Keys(); len(keys) != 0 {
		t.Errorf("a durable store booted with no --seed and no wiring rows serves object "+
			"types %v. It must serve none: its model is whatever another process "+
			"provisioned, and the embedded acme fixture describes a different deployment.", keys)
	}
	if stack.fetcher != nil {
		t.Error("the rules metadata fetcher is wired although nothing declares an object source; " +
			"a rule reading object.* must see empty metadata, not the demo's")
	}
	if slots := stack.attributes.RegisteredSlots(); len(slots) != 0 {
		t.Errorf("attribute slots %v are wired although nothing declares a source. The acme "+
			"fixture's inline bags were being served to an operator's own principals.", slots)
	}

	// The in-memory demo is untouched: with no flags at all there is no database
	// to be wrong about, and every getting-started page rests on the fixture.
	//
	// Asserted on the DOCUMENT rather than on the registry, because the embedded
	// example declares no wiring section today — which is why the leak was
	// invisible, and exactly why it must be closed before the fixture grows an
	// `objects:` block and starts serving acme's metadata to somebody's production
	// instance.
	demo, err := seedDocument("", storeMemory)
	if err != nil {
		t.Fatalf("parsing the embedded example: %v", err)
	}
	if len(demo.Accounts) == 0 {
		t.Error("the zero-flag in-memory demo no longer reads the embedded example; it is " +
			"the only model there could be for it, and the getting-started pages, " +
			"seed.ExampleAccount as the default --account and cmd/aperture's end-to-end " +
			"test all rest on it")
	}
	durable, err := seedDocument("", storeSQLite)
	if err != nil {
		t.Fatalf("seedDocument for a durable store: %v", err)
	}
	if len(durable.Accounts) != 0 || len(durable.Providers) != 0 || len(durable.Objects) != 0 ||
		len(durable.Attributes) != 0 || len(durable.AttributeProviders) != 0 ||
		len(durable.FieldTypes) != 0 || len(durable.Connections) != 0 {
		t.Errorf("a durable store with no --seed got a non-empty document (%s); it must get "+
			"nothing at all", durable.Describe())
	}
}

// TestASharedConnectionNameRoutesThroughTheHostsOwnOpener is FR-20 stated as a
// test: seed.WithConnectionOpener stays the ONE way a host supplies a route, and
// it works against a name manifest read from the database exactly as it does
// against one read from a file. Arc's internal/authz/objectprovideropener.go
// needs no change.
func TestASharedConnectionNameRoutesThroughTheHostsOwnOpener(t *testing.T) {
	set := sharedWiringSet(time.Now().UTC())
	doc, err := wiringDocument(set, &seed.Document{})
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	var opened []string
	reg, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(name string, cfg seed.ConnectionSettings) (seed.Pool, error) {
			opened = append(opened, name)
			if cfg.DSN != unroutedDSN {
				t.Errorf("the opener was handed a DSN this instance did not supply")
			}
			return fakePool{}, nil
		}))
	if err != nil {
		t.Fatalf("building the registry over a DB-sourced manifest: %v", err)
	}
	defer func() { _ = conns.Close() }()

	if len(opened) != 1 || opened[0] != "main" {
		t.Fatalf("the opener was called for %v, want exactly [main] — once per DECLARED "+
			"connection, which is the whole point of sharing the pool", opened)
	}
	if !reg.Has("document") {
		t.Fatalf("the registry serves %v, not the manifest's type", reg.Keys())
	}
}

// TestALocalConnectionsEntryIsTheRouteForASharedName is the second of the three
// routes: this instance's own seed file names the variable and sizes the pool for
// a connection the DATABASE declared. The row carries the name; the route is
// local, entirely.
func TestALocalConnectionsEntryIsTheRouteForASharedName(t *testing.T) {
	local := &seed.Document{Connections: map[string]seed.Connection{
		"main": {DSNEnv: "MY_OWN_DATABASE_URL", QueryTimeout: "2s", MaxOpenConns: 3},
	}}
	doc, err := wiringDocument(sharedWiringSet(time.Now().UTC()), local)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	t.Setenv("MY_OWN_DATABASE_URL", unroutedDSN)
	// The conventional variable is deliberately left UNSET: if the local entry
	// were ignored the build would fail naming it, which is what makes this
	// assertion about precedence rather than about two variables that happen to
	// hold the same string.

	var got seed.ConnectionSettings
	_, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(name string, cfg seed.ConnectionSettings) (seed.Pool, error) {
			got = cfg
			return fakePool{}, nil
		}))
	if err != nil {
		t.Fatalf("building over a locally-routed shared name: %v", err)
	}
	defer func() { _ = conns.Close() }()

	if got.DSN != unroutedDSN {
		t.Errorf("the pool was opened against a DSN from somewhere other than the local " +
			"connections: entry's dsn_env")
	}
	if got.QueryTimeout != 2*time.Second {
		t.Errorf("query timeout = %v, want 2s from the local entry — pool tuning is a "+
			"per-instance fact and the shared tables have no column for it", got.QueryTimeout)
	}
	if got.MaxOpenConns != 3 {
		t.Errorf("max open conns = %d, want 3 from the local entry", got.MaxOpenConns)
	}
}

// TestAnUnroutedSharedConnectionNamesTheVariableItWanted keeps the third route
// honest. A name nothing routes is not silently dropped and does not fail on the
// first decision that needed the database: it fails at BUILD, naming the variable
// the operator has to export.
func TestAnUnroutedSharedConnectionNamesTheVariableItWanted(t *testing.T) {
	doc, err := wiringDocument(sharedWiringSet(time.Now().UTC()), nil)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	t.Setenv(connectionDSNEnvVar("main"), "")

	_, _, err = doc.BuildRegistryWithConnections("")
	if err == nil {
		t.Fatal("a shared connection name with no route built a registry anyway")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_CONNECTION {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_SQL_PROVIDER_CONNECTION)
	}
	if want := connectionDSNEnvVar("main"); !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal must name %q so an operator knows what to export; got %q",
			want, err.Error())
	}
}

// TestTwoSharedNamesCannotDeriveOneEnvironmentVariable refuses the ambiguity the
// conventional spelling can create rather than resolving it. "main-db" and
// "main_db" are two connections — two pools, possibly two servers — and routing
// both through one variable would send one of them somewhere nobody asked for.
func TestTwoSharedNamesCannotDeriveOneEnvironmentVariable(t *testing.T) {
	now := time.Now().UTC()
	set := model.WiringSet{Connections: []model.WiringConnection{
		{Name: "main-db", CreatedAt: now, UpdatedAt: now},
		{Name: "main_db", CreatedAt: now, UpdatedAt: now},
	}}

	_, err := wiringDocument(set, nil)
	if err == nil {
		t.Fatal("two names deriving one variable were accepted")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %q, want %q", got, aerr.APERTURE_CONFIG_INVALID)
	}
	for _, want := range []string{"main-db", "main_db", connectionDSNEnvVar("main-db")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got %q", want, err.Error())
		}
	}

	// A LOCAL route for one of them resolves it, because the collision is an
	// artefact of the convention and not of the names.
	local := &seed.Document{Connections: map[string]seed.Connection{
		"main-db": {DSNEnv: "ONE_URL"},
	}}
	if _, err := wiringDocument(set, local); err != nil {
		t.Fatalf("a locally-routed name still collided: %v", err)
	}
}

// TestAnIncompleteStatementSetFailsTheBootLoudly records the decision E1-S3 left
// to this layer. `aperture wiring push` does not check statement-set
// completeness, because the statement contract belongs to the registry builder
// and duplicating it would be the same rule in two places. So a pushed entry with
// an empty get_one is refused HERE, on the boot that reads it, with the builder's
// own APERTURE_CONFIG_INVALID — and NOT buried under APERTURE_BOOT.
func TestAnIncompleteStatementSetFailsTheBootLoudly(t *testing.T) {
	now := time.Now().UTC()
	set := sharedWiringSet(now)
	set.Providers[0].GetOne = ""

	dsn := "file:" + filepath.Join(t.TempDir(), "incomplete.db")
	pushWiring(t, dsn, set)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	err := bootStackError(t, dsn, "")
	if err == nil {
		t.Fatal("a provider entry with no get_one built a registry anyway; an instance " +
			"must not serve decisions on metadata it cannot fetch")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %q, want %q — the builder's refusal must not be re-stamped",
			got, aerr.APERTURE_CONFIG_INVALID)
	}
	if chain := codeChain(err); len(chain) != 1 {
		t.Fatalf("code chain = %v, want exactly one coded error; a same-code re-stamp is "+
			"invisible to CodeOf, so depth is what catches it", chain)
	}
}

// wiringReadFails is a store whose GetWiring refuses. Everything else delegates
// to the embedded interface, so the boot reaches the wiring read exactly as it
// would against a real backend.
type wiringReadFails struct {
	model.Storage
	err error
}

func (w wiringReadFails) GetWiring(context.Context) (model.WiringSet, error) {
	return model.WiringSet{}, w.err
}

// TestTheWiringReadDoesNotBuryTheStoresOwnRefusal is the pass-through guard,
// asserted from both sides.
//
// The likely failure here is APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, because the
// wiring tables are exactly what a database written by an older build does not
// have — and its registry fixup is "create a new database", which an operator
// told only "aperture failed to start" will never reach for. A BARE error still
// becomes APERTURE_BOOT, which is what that code is for.
func TestTheWiringReadDoesNotBuryTheStoresOwnRefusal(t *testing.T) {
	ctx := context.Background()
	base, err := buildStore(ctx, "", "")
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })

	t.Run("a coded refusal passes through", func(t *testing.T) {
		coded := aerr.New(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, "storage: this database predates wiring")
		_, err := readSharedWiring(ctx, wiringReadFails{Storage: base, err: coded})
		if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE {
			t.Fatalf("code = %q, want %q (err: %v)", got, aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, err)
		}
		if chain := codeChain(err); len(chain) != 1 {
			t.Fatalf("code chain = %v, want exactly one coded error", chain)
		}
	})

	t.Run("a bare error becomes APERTURE_BOOT", func(t *testing.T) {
		_, err := readSharedWiring(ctx, wiringReadFails{Storage: base, err: errors.New("connection reset")})
		if got := aerr.CodeOf(err); got != aerr.APERTURE_BOOT {
			t.Fatalf("code = %q, want %q", got, aerr.APERTURE_BOOT)
		}
	})
}

// TestFlatFieldTypeRowsRegroupIntoOneEntryPerObjectType pins the one shape change
// the projection makes. Storage flattens field types one row per (type, field) so
// the table has a primary key; seed's fieldTypeIndex refuses an object type
// declared twice. A one-row-per-entry projection therefore builds a document that
// cannot be built — and only for a type with two declared fields, which is
// exactly the case a single-field fixture would miss.
func TestFlatFieldTypeRowsRegroupIntoOneEntryPerObjectType(t *testing.T) {
	now := time.Now().UTC()
	set := model.WiringSet{FieldTypes: []model.WiringFieldType{
		{ObjectType: "document", Field: "published_at", DeclaredType: "datetime", CreatedAt: now, UpdatedAt: now},
		{ObjectType: "document", Field: "reviewed_on", DeclaredType: "date", CreatedAt: now, UpdatedAt: now},
		{ObjectType: "project", Field: "started_on", DeclaredType: "date", CreatedAt: now, UpdatedAt: now},
	}}

	doc, err := wiringDocument(set, nil)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	if len(doc.FieldTypes) != 2 {
		t.Fatalf("field_types: has %d entries, want one per OBJECT TYPE (2). seed refuses "+
			"a type declared twice, so a row-per-entry projection cannot be built at all.",
			len(doc.FieldTypes))
	}
	byType := map[string]map[string]string{}
	for _, ft := range doc.FieldTypes {
		byType[ft.ObjectType] = ft.Fields
	}
	if got := byType["document"]; len(got) != 2 || got["reviewed_on"] != "date" || got["published_at"] != "datetime" {
		t.Fatalf("document's declared fields = %v, want both, each with its own type", got)
	}

	// And the regrouped document really does build, which is the assertion the
	// count alone only implies.
	if _, err := doc.BuildRegistry(""); err != nil {
		t.Fatalf("the regrouped field_types: section does not build: %v", err)
	}
}

// TestASharedProvidersReferencesSurviveTheProjection keeps the reference rows
// wired. They are stored flat and sorted (a references: map has no order of its
// own), and a projection that dropped them would leave `enumerate --via` reporting
// an unregistered reference on an instance whose peer resolves it.
func TestASharedProvidersReferencesSurviveTheProjection(t *testing.T) {
	now := time.Now().UTC()
	set := sharedWiringSet(now)
	set.Providers[0].References = []model.WiringReference{
		{Field: "current_projects", TargetType: "project"},
	}
	set.Providers = append(set.Providers, model.WiringProvider{
		ObjectType: "project",
		Kind:       "sql",
		Connection: "main",
		GetOne:     "SELECT name FROM projects WHERE id = $1",
		GetAll:     "SELECT 'project:' || p.id AS id, p.name FROM projects p",
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	model.SortWiringProviders(set.Providers)

	doc, err := wiringDocument(set, nil)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	reg, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(string, seed.ConnectionSettings) (seed.Pool, error) {
			return fakePool{}, nil
		}))
	if err != nil {
		t.Fatalf("building a registry with a projected reference: %v", err)
	}
	defer func() { _ = conns.Close() }()

	target, ok := reg.ReferenceTarget("document", "current_projects")
	if !ok || target != "project" {
		t.Fatalf("reference target for document.current_projects = %q (declared: %v), want project",
			target, ok)
	}
}

// fakePool satisfies seed.Pool without a database. Nothing above runs a
// statement — a registry BUILD dials nothing, which is the whole reason
// WithConnectionOpener exists — so the two query methods are never called.
type fakePool struct{}

func (fakePool) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("fakePool: no statement should run during a registry build")
}

func (fakePool) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func (fakePool) Close() error { return nil }
