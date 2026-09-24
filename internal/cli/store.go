package cli

import (
	"context"
	"path/filepath"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/postgres"
	"github.com/frankbardon/aperture/storage/sqlite"
)

// buildStore constructs and initialises a model.Storage for a command, then
// seeds it. This is the manual constructor-DI seam the check and serve commands
// share: choose a backend from --store, run Setup, and load a model from --seed
// (or, for the in-memory demo only, the embedded example when --seed is empty).
//
//   - storeDSN == ""            -> in-memory backend (storage/memory), ideal for the demo.
//   - storeDSN is a postgres URL -> PostgreSQL backend (storage/postgres).
//   - any other storeDSN         -> SQLite backend at the DSN (storage/sqlite).
//   - seedPath != ""            -> load the file, format inferred from its extension.
//   - seedPath == "" and an in-memory store -> load the embedded example fixture (account "acme").
//   - seedPath == "" and a DURABLE store    -> seed nothing at all.
//
// On any failure the partially constructed store is closed and the caller gets a
// coded error: the failure's OWN code when it has one, and APERTURE_BOOT only
// when it does not. See bootError for why that distinction is not cosmetic.
func buildStore(ctx context.Context, storeDSN, seedPath string) (model.Storage, error) {
	kind := classifyStore(storeDSN)
	store, err := openStore(storeDSN)
	if err != nil {
		return nil, err
	}
	if err := store.Setup(ctx); err != nil {
		_ = store.Close()
		return nil, bootError("cli: storage setup failed", err)
	}
	if err := loadSeed(ctx, store, seedPath, kind); err != nil {
		_ = store.Close()
		return nil, bootError("cli: seeding the model failed", err)
	}
	return store, nil
}

// bootError codes a startup failure as APERTURE_BOOT — but only when the failure
// does not already carry a code of its own.
//
// aerr.Wrap does NOT pass an existing code through: it builds a fresh CodedError
// with whatever code it is handed, and aerr.CodeOf reports the OUTERMOST one. So
// wrapping unconditionally re-stamps, and the pass-through is a property of the
// call site, not of the wrapper (the same idiom lives in provider/registry.go,
// storage/sqlite/sqlite.go, and delegation/delegation.go).
//
// Storage startup is exactly where that matters. Setup returns two specific,
// actionable refusals — APERTURE_STORAGE_SCHEMA_INCOMPATIBLE for a database
// written by an older build, and APERTURE_STORAGE_CONSTRAINT for a connection
// that is not enforcing foreign keys — and seeding surfaces the coded rejection
// of whichever entity the document got wrong. Re-stamping all of them
// APERTURE_BOOT replaces that with "aperture failed to start", whose registry
// fixups are "check your environment variables" and "confirm the backend is
// reachable": true of every startup failure there is, and no help with any of
// these. The CLI is the surface an operator meets an unreadable database
// through, so burying the code here is burying it everywhere it would be read.
//
// A bare error still gets APERTURE_BOOT, which is what the code is for: it says
// the process could not start, for a reason no layer below classified.
func bootError(msg string, err error) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_BOOT, msg, err)
}

// postgresDSNSchemes are the DSN prefixes that select the PostgreSQL backend.
// They are libpq's own two URI schemes, and they are the whole rule.
//
// A libpq KEYWORD DSN ("host=... dbname=...") deliberately does NOT select
// Postgres, even though pgx accepts one. The alternative would be sniffing a
// caller's string for something that looks like a keyword pair, and --store's
// other value is a FILE PATH: a path containing '=' would start opening
// databases over the network. A rule an operator can state in one sentence, and
// which cannot be triggered by accident, is worth more here than accepting a
// second spelling of the same connection.
var postgresDSNSchemes = []string{"postgres://", "postgresql://"}

// isPostgresDSN reports whether the DSN names a PostgreSQL server.
func isPostgresDSN(storeDSN string) bool {
	for _, scheme := range postgresDSNSchemes {
		if strings.HasPrefix(storeDSN, scheme) {
			return true
		}
	}
	return false
}

// storeKind names which backend a --store DSN selects. It exists so that "which
// backend is this?" and "is this store durable?" are ONE question with one
// answer: openStore switches on it to build the backend, and loadSeed reads its
// durable() to decide whether an absent --seed means the demo fixture or nothing
// at all.
//
// A second notion of durability — a `storeDSN != ""` test written out again at
// the seeding site — would be a second answer that could drift from the backend
// actually opened, and drifting in the permissive direction means writing the
// demo model into somebody's production database.
type storeKind int

const (
	// storeMemory is the in-memory backend: no DSN was given, nothing survives
	// the process, and it is the zero-flag demo.
	storeMemory storeKind = iota
	// storePostgres is the PostgreSQL backend, selected by a libpq URI scheme.
	storePostgres
	// storeSQLite is the SQLite backend at the DSN's path.
	storeSQLite
)

// classifyStore maps a --store DSN onto the backend it selects. This is the
// whole rule, stated once.
func classifyStore(storeDSN string) storeKind {
	if storeDSN == "" {
		return storeMemory
	}
	if isPostgresDSN(storeDSN) {
		return storePostgres
	}
	return storeSQLite
}

// durable reports whether the backend outlives the process — whether, in other
// words, anything this boot writes to the model is still there on the next one.
func (k storeKind) durable() bool { return k != storeMemory }

// openStore selects the storage backend from the DSN.
//
//   - ""                                -> in-memory (storage/memory)
//   - "postgres://..." / "postgresql://" -> PostgreSQL (storage/postgres)
//   - anything else                      -> SQLite at that path (storage/sqlite)
//
// The Postgres store reads APERTURE_POSTGRES_SCHEMA through
// postgres.WithSchemaFromEnv. Unset means "use whatever the connection's
// search_path resolves to", which is the zero-configuration path and the reason
// every table Aperture owns is prefixed apt_: pinning a schema is an operator's
// choice about tidiness and grants, not a requirement.
//
// The schema is configured by ENVIRONMENT rather than by a flag on purpose. It
// is a property of the deployment, not of an invocation — a --store-schema that
// could differ between `aperture serve` and `aperture grant` would be a way to
// write half a model into the wrong namespace — and eight commands carry
// --store, so a flag would be eight places for the two to disagree. A malformed
// value is refused by postgres.Open with APERTURE_CONFIG_INVALID before any
// connection is made, and bootError passes that code through rather than burying
// it under APERTURE_BOOT.
func openStore(storeDSN string) (model.Storage, error) {
	switch classifyStore(storeDSN) {
	case storeMemory:
		return memory.New(), nil
	case storePostgres:
		store, err := postgres.Open(storeDSN, postgres.WithSchemaFromEnv())
		if err != nil {
			return nil, bootError("cli: open postgres store", err)
		}
		return store, nil
	default:
		store, err := sqlite.Open(storeDSN)
		if err != nil {
			return nil, bootError("cli: open sqlite store", err)
		}
		return store, nil
	}
}

// loadSeed loads the model from the seed file. With no --seed it loads the
// embedded example for an in-memory store and NOTHING for a durable one.
//
// That asymmetry is the whole point of the function, so it is worth stating why.
// seed.Document.Apply upserts the ENTIRE model — accounts, principals, roles,
// groups, grants, rules — and does so deliberately outside the ManagedEntities
// posture. Defaulting to the embedded fixture for every store therefore meant
// that `aperture serve --store postgres://prod` with no --seed wrote the acme
// demo model into production, and that two instances sharing one database
// re-asserted their own model over each other on every restart. Nothing refused
// it and nothing said it had happened.
//
// The in-memory half keeps the demo: with no flags at all there is no database to
// overwrite, the fixture is the only model there could be, and the
// getting-started pages, the end-to-end test, and seed.ExampleAccount as the
// default --account all rest on it. A durable store is the opposite situation —
// the operator named a database because it already holds, or is about to hold,
// something they care about — so an absent --seed is read as "leave the model
// alone" rather than guessed at.
//
// This is a skip, not a refusal, and so carries no error code: a durable store
// with no --seed is the normal way a second instance boots against a database
// another process provisioned. Applying model state stays an explicit act —
// --seed, `aperture import`, or a mutation command — and never an implicit
// consequence of starting up.
func loadSeed(ctx context.Context, store model.Storage, seedPath string, kind storeKind) error {
	if seedPath != "" {
		return seed.LoadFile(ctx, store, seedPath)
	}
	if kind.durable() {
		return nil
	}
	return seed.Load(ctx, store, seed.Example, seed.FormatYAML)
}

// seedDocument parses the seed model (the --seed file, or the embedded example
// when empty) into a Document so a command can read the sections Apply does not
// write to storage — the two object-source wiring sections, `providers:` and
// `objects:`, which BuildRegistry turns into one live registry. It mirrors
// loadSeed's file-vs-embedded choice.
func seedDocument(seedPath string) (*seed.Document, error) {
	if seedPath == "" {
		doc, err := seed.Parse(seed.Example, seed.FormatYAML)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: parsing the embedded seed failed", err)
		}
		return doc, nil
	}
	doc, err := seed.ParseFile(seedPath)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_BOOT, "cli: parsing the seed file failed", err)
	}
	return doc, nil
}

// seedBaseDir is the directory declared provider paths resolve against: the
// seed file's directory, or "" (the process CWD) for the embedded seed.
func seedBaseDir(seedPath string) string {
	if seedPath == "" {
		return ""
	}
	return filepath.Dir(seedPath)
}
