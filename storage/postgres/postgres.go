package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"sort"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"

	// The pgx stdlib driver, registered as "pgx" and used exclusively through
	// database/sql. It is pure Go, so CGO_ENABLED=0 still builds; it is already a
	// module dependency (seed/connection.go blank-imports it for host-provider
	// connections), so this backend adds no dependency of its own. That is the
	// same import in a second place, NOT an import edge: nothing here imports
	// seed, and nothing in seed imports storage.
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schema string

// driverName is the database/sql driver this backend opens. pgx/v5/stdlib
// registers itself under this name in its init.
const driverName = "pgx"

// Pool defaults.
//
// These are Aperture's OWN numbers. seed/connection.go defines four constants of
// the same shape, and they are deliberately not reused here: they were reasoned
// for bursty read traffic against somebody ELSE'S database during Check, whereas
// this pool serves Aperture's own store — decision-path reads, admin writes, and
// Atomic callbacks that hold one connection for the callback's entire duration.
// Borrowing them would also create an import edge from a low-level backend to a
// high-level assembly package (storage -> seed -> sqlprovider), and no edge
// between storage and seed exists in either direction today.
//
// Note what is NOT here: SetMaxOpenConns(1). storage/sqlite sets exactly that,
// and copying it across would be the single worst thing this file could do.
// SQLite's cap is a workaround for a single-writer engine — it prevents
// "database is locked" contention and keeps an in-memory database on one handle.
// Postgres is a multi-process server with per-backend concurrency; a cap of one
// would serialize EVERY storage read in the process behind every other one,
// including the decision path that has to sustain >= 10k checks/sec. The SQLite
// number is a property of SQLite, not a house default.
const (
	// DefaultMaxOpenConns caps the pool. It is deliberately not database/sql's
	// own default, which is unlimited: an unbounded pool answers a burst of
	// Checks by opening connections until the SERVER refuses them, turning a
	// latency spike into an outage for every other client of that database —
	// including Aperture's host. Twenty is chosen against Postgres's own
	// max_connections default of 100: it leaves room for a few Aperture
	// processes plus the operator's psql on a stock cluster, while being wide
	// enough that a long-running Atomic callback (which owns its connection for
	// the whole callback, not just one statement) cannot starve the read path.
	DefaultMaxOpenConns = 20
	// DefaultMaxIdleConns is how many of those stay warm. database/sql's default
	// is 2, which against a 20-connection pool means eighteen connections are
	// torn down and re-established — TCP, TLS and authentication — every time
	// traffic ebbs. Aperture's traffic is exactly that shape: a wave of decisions,
	// then quiet. Retaining half the pool absorbs the wave without pinning the
	// whole of it.
	DefaultMaxIdleConns = 10
	// DefaultConnMaxLifetime retires a connection this long after it was opened,
	// used or not. A finite lifetime is what survives the ordinary deployment: a
	// connection proxy that rotates backends, a failover pair where a connection
	// held forever eventually points at a server that is no longer the primary,
	// or a rolling restart that needs the old backends to drain. Thirty minutes
	// is long enough that reconnection cost is noise and short enough that a
	// failover heals without restarting Aperture.
	DefaultConnMaxLifetime = 30 * time.Minute
	// DefaultConnMaxIdleTime retires a connection that has merely been SITTING
	// this long. It is the companion to DefaultMaxIdleConns and answers a
	// different question: that constant caps how MANY idle connections are kept,
	// this one caps how LONG. Without it, an Aperture that served one burst at
	// boot and nothing since pins ten server backends — each one real memory on
	// the database host — until DefaultConnMaxLifetime finally expires. Five
	// minutes keeps a warm pool across the gaps that actually occur between
	// decisions and releases it across the gaps that do not.
	DefaultConnMaxIdleTime = 5 * time.Minute
)

// setupAdvisoryLockKey is the constant key Setup's transaction-scoped advisory
// lock is taken on. Postgres advisory locks live in ONE cluster-wide bigint key
// space that Aperture shares with its host application, so the key is derived
// rather than picked: it is the first eight bytes of
// SHA-256("github.com/frankbardon/aperture storage/postgres Setup"), read as a
// big-endian int64. A host that picks its own keys by hand will not collide with
// it by accident, and the derivation is reproducible — TestSetupAdvisoryLockKey
// recomputes it, so the number cannot be edited into something else unnoticed.
//
// It must stay CONSTANT across builds and across schema qualifiers. Two
// processes that disagree about the key are two processes with no mutual
// exclusion at all, which is the failure this lock exists to prevent, and a
// key derived from (say) the schema name would silently produce that on a
// deployment mid-way through changing it.
const setupAdvisoryLockKey int64 = -5237656721417215243

// schemaPlaceholder is the literal every table identifier in schema.sql is
// written with: apt_schema.apt_<thing>. It is not a schema Aperture creates —
// it is a substitution seam, replaced before the script is executed. See
// schema.sql's header for why it is spelled as a SQL identifier rather than a
// template delimiter, and the package doc for why it can never become a bind
// parameter.
const schemaPlaceholder = "apt_schema."

// sqlExec is the subset of database/sql that both *sql.DB and *sql.Tx satisfy.
// Setup runs its whole sequence through it so the same code serves the
// transaction this Store begins and, for a transaction-scoped Store, the
// enclosing one.
type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is a PostgreSQL-backed store. Construct one with Open and call Setup
// once before use.
//
// A Store is either ROOT (pool != nil) — owning the connection pool and able to
// begin transactions — or TRANSACTION-SCOPED (pool == nil) — a transient handle
// whose exec is a *sql.Tx. The split mirrors storage/sqlite exactly, because it
// is what lets Atomic flatten a nested transaction into the current one instead
// of taking a second connection out of the pool and deadlocking against itself.
type Store struct {
	pool *sql.DB // the connection pool; nil for a transaction-scoped Store
	exec sqlExec // *sql.DB or *sql.Tx — what statements run against

	// schema is the configured schema name, VERBATIM and unquoted, or "" for
	// the ambient search_path. It is the form compared against
	// pg_namespace.nspname, which is ordinary text — so this field, and not the
	// quoted qualifier, is what ensureSchema and inspectTables bind.
	schema string
	// qualifier is what schemaPlaceholder is replaced with: "" to create in
	// whatever search_path resolves to (the default, and the reason the apt_
	// table prefix exists), or `"<name>".` to pin the tables into a named
	// schema. It is derived from schema by qualifierFor and never set
	// independently, so the two can never name different schemas.
	qualifier string
}

// Open opens a connection pool against dsn and returns a Store. dsn is anything
// pgx accepts — a "postgres://user:pass@host:port/db?sslmode=..." URL or a
// libpq keyword string. Like sql.Open everywhere, this does not connect: the
// first connection is made when Setup (or any later statement) needs one, so a
// database that is unreachable surfaces there rather than here.
//
// Options configure the store; today the only one is the schema Aperture's
// tables live in (WithSchema / WithSchemaFromEnv). With no Option the tables are
// addressed UNQUALIFIED and resolve through the connection's search_path, which
// is safe in a database shared with a host application because every table
// Aperture owns is prefixed apt_.
//
// Options are applied BEFORE the pool is created, on purpose: configuration this
// backend cannot use is refused with APERTURE_CONFIG_INVALID at boot, with
// nothing opened and nothing to close. A schema name in particular can never be
// a bind parameter — it is interpolated into SQL text — so the moment it is
// accepted is the last moment it can be checked. See config.go.
//
// There is no DSN rewriting. storage/sqlite rewrites its DSN to force
// _pragma=foreign_keys(1) because SQLite defaults foreign keys OFF and scopes
// the setting per connection, so a caller-supplied DSN can produce a store whose
// constraints are decoration. PostgreSQL enforces foreign keys unconditionally
// and has no such switch, so there is nothing to force and nothing for Setup to
// verify. Porting the dance would be cargo cult.
//
// Note what Open still does NOT do: set search_path on the connection. Where a
// statement lands is decided when the statement is BUILT, by substituting the
// qualifier into it, never by per-connection session state. Session state is the
// same footgun as SQLite's foreign_keys pragma — silently correct until the pool
// hands out a connection that missed it — and a pool of twenty makes that a
// matter of timing rather than of code.
func Open(dsn string, opts ...Option) (*Store, error) {
	var set settings
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&set); err != nil {
			// Already an APERTURE_CONFIG_INVALID with the offending value and
			// the rule in it. Wrapping would re-stamp the code (aerr.Wrap builds
			// a fresh CodedError with whatever code it is handed) and bury both.
			return nil, err
		}
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "open postgres database", err)
	}
	db.SetMaxOpenConns(DefaultMaxOpenConns)
	db.SetMaxIdleConns(DefaultMaxIdleConns)
	db.SetConnMaxLifetime(DefaultConnMaxLifetime)
	db.SetConnMaxIdleTime(DefaultConnMaxIdleTime)
	return &Store{pool: db, exec: db, schema: set.validatedSchema, qualifier: qualifierFor(set.validatedSchema)}, nil
}

// Schema reports the schema this Store pins its tables into, or "" when it uses
// the connection's search_path. It is the value as configured — unquoted and
// case-exact — which is the form that appears in pg_namespace.nspname.
func (s *Store) Schema() string { return s.schema }

// Close releases the underlying pool. It is a no-op on a transaction-scoped
// Store, which does not own one.
func (s *Store) Close() error {
	if s.pool == nil {
		return nil
	}
	if err := s.pool.Close(); err != nil {
		return aerr.Wrap(aerr.APERTURE_STORAGE, "close postgres database", err)
	}
	return nil
}

// Setup creates the schema. It is idempotent, and it is safe to run from several
// processes at the same moment.
//
// The sequence is LOCK -> ENSURE-SCHEMA -> INSPECT -> CREATE-IF-ABSENT ->
// VERIFY, and all five steps run inside ONE transaction:
//
//  1. LOCK takes a transaction-scoped advisory lock on setupAdvisoryLockKey.
//     This is the step that must not be skipped. Postgres DDL is transactional
//     and a multi-command script is implicitly atomic, so the script alone
//     already gives all-or-nothing application — but atomicity is not mutual
//     exclusion. CREATE TABLE IF NOT EXISTS is NOT race-free in PostgreSQL: two
//     sessions can both pass the existence check and the loser fails on a
//     pg_class unique violation (42P07 / 23505). Two aperture processes booting
//     against one database is an ordinary deployment, not a corner case.
//
//  2. ENSURE-SCHEMA resolves the schema the tables will be created in: it
//     CREATEs the configured one if it is absent, or resolves the ambient
//     search_path and refuses a connection that has none. Being inside the lock
//     and the transaction is not incidental — see ensurePinnedSchema.
//
//  3. INSPECT reads which of Aperture's tables already exist there.
//
//  4. CREATE-IF-ABSENT applies schema.sql only when something is missing. The
//     script is idempotent, so skipping is not merely an optimisation: it means
//     a database that is ALREADY set up can be opened by a role holding no
//     CREATE privilege at all, which is the normal shape of an application role
//     in a shared host database.
//
//  5. VERIFY re-reads the catalog and refuses to return success unless every
//     table Aperture needs is present.
//
// The transaction is READ COMMITTED, explicitly. That is load-bearing, not a
// default being restated, and it was measured rather than assumed. Steps 3 and 5
// read pg_class, which is an ordinary MVCC read: under READ COMMITTED each
// statement takes a fresh snapshot, so INSPECT sees a concurrent peer's
// committed tables and VERIFY sees the ones this very transaction just created.
// Setting the same transaction to REPEATABLE READ instead makes every one of
// those reads use the snapshot taken at step 1 — before the lock was even
// granted — and the observed result is that Setup applies the whole schema and
// then reports all nineteen tables missing, on a completely uncontended
// database. The waiter in a race is worse off still: it inspects a pre-lock
// snapshot, concludes the tables are absent, and runs the CREATE statements the
// lock exists to prevent.
func (s *Store) Setup(ctx context.Context) error {
	if s.pool == nil {
		// Transaction-scoped: there is an enclosing transaction, and beginning a
		// second one would take a second connection and lose the advisory lock's
		// relationship to the work it guards. Run in the caller's transaction.
		return s.setup(ctx, s.exec)
	}
	tx, err := s.pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return wrapStorage("begin setup transaction", err)
	}
	if err := s.setup(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrapStorage("commit setup transaction", err)
	}
	return nil
}

// setup is the five-step sequence, run against one transaction.
func (s *Store) setup(ctx context.Context, exec sqlExec) error {
	// 1. LOCK. Held until this transaction commits or rolls back — there is no
	// unlock call to forget and no way to leak the lock by returning early. The
	// key is a bind argument, which is safe here because this is a single
	// command; it is the multi-command schema script, and only that, which must
	// be executed with zero arguments (see the package doc).
	//
	// pg_advisory_xact_lock BLOCKS until the lock is free. A Setup racing a
	// slower peer waits for it rather than failing, and a ctx deadline is what
	// bounds that wait: database/sql cancels the query and the transaction is
	// rolled back, releasing nothing that was ever taken.
	if _, err := exec.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, setupAdvisoryLockKey); err != nil {
		return wrapStorage("take the setup advisory lock", err)
	}

	// 2. ENSURE-SCHEMA.
	target, err := s.ensureSchema(ctx, exec)
	if err != nil {
		return err
	}

	// 3. INSPECT.
	present, err := inspectTables(ctx, exec, target)
	if err != nil {
		return err
	}

	// 4. CREATE-IF-ABSENT.
	if len(missingTables(present)) > 0 {
		if err := s.applySchema(ctx, exec, target); err != nil {
			return err
		}
	}

	// 5. VERIFY.
	present, err = inspectTables(ctx, exec, target)
	if err != nil {
		return err
	}
	if missing := missingTables(present); len(missing) > 0 {
		return aerr.Newf(aerr.APERTURE_STORAGE,
			"schema setup finished but %d of Aperture's %d tables are still absent from schema %q: %s",
			len(missing), len(expectedTables), target, strings.Join(missing, ", "))
	}
	return nil
}

// ensureSchema resolves the schema Setup will create Aperture's tables in,
// creating it when one was configured and does not exist yet, and refusing a
// connection that has none.
//
// It has two paths, and which one runs is decided entirely by configuration, not
// by anything about the connection.
//
// UNCONFIGURED (s.schema == "", the default and the zero-config path). The
// tables are written unqualified, so they land in current_schema() — the first
// schema on the connection's search_path that actually exists. Resolving that
// name here, once, is what lets INSPECT and VERIFY talk about the same namespace
// the CREATE statements will use, instead of asking search_path a second time
// and possibly getting a different answer. It is also why an unqualified
// deployment is safe at all: an unqualified lookup for one of Aperture's tables
// resolves to current_schema() first, so the table this step names is the table
// every later statement will find. Nothing is created — the schema is the host's
// and Aperture is a guest in it.
//
// current_schema() returns NULL when search_path names nothing that exists — a
// role whose "$user" schema was never created and whose search_path was trimmed
// to it, most often. Postgres would report that as "no schema has been selected
// to create in" from the first CREATE, several steps later; catching it here
// costs one query and names the actual problem.
//
// CONFIGURED (s.schema != ""). search_path is not consulted AT ALL — that is the
// whole point of pinning, and it is why a pinned store works over a connection
// whose search_path points somewhere else entirely. The schema is created if
// absent, and the configured name is returned for the steps below to use.
//
// # Why existence is checked before CREATE SCHEMA IF NOT EXISTS, and not left to it
//
// Measured against PostgreSQL 18.4: `CREATE SCHEMA IF NOT EXISTS x` issued by a
// role without CREATE on the DATABASE fails with 42501, "permission denied for
// database", EVEN WHEN x ALREADY EXISTS. The IF NOT EXISTS clause does not skip
// the privilege check. Issuing it unconditionally would therefore break the
// deployment this backend deliberately supports and Setup's step 4 already
// preserves: an administrator sets the database up once, and Aperture then runs
// with a role that can only read and write rows. A store pinned to a schema
// would demand CREATE on the database forever, for a statement whose entire
// effect is a NOTICE. So the catalog is asked first, and CREATE SCHEMA is issued
// only when the answer is "absent" — the same CREATE-IF-ABSENT shape as the
// tables, for the same reason.
//
// # Why pg_namespace and not to_regnamespace
//
// to_regnamespace() parses its argument as an identifier REFERENCE, which means
// it applies PostgreSQL's identifier FOLDING (measured: with a schema named Foo
// present, to_regnamespace('Foo') is NULL and to_regnamespace('"Foo"') is Foo).
// A lookup that folds and a lookup that does not would be two different questions
// asked about one schema, and inspectTables already compares nspname literally.
// Reading pg_namespace directly makes both comparisons the same rule — plain
// text equality against the configured name — so there is no case in which
// ensureSchema and inspectTables can disagree about which schema they mean.
func (s *Store) ensureSchema(ctx context.Context, exec sqlExec) (string, error) {
	if s.schema != "" {
		return s.schema, s.ensurePinnedSchema(ctx, exec)
	}
	var name sql.NullString
	if err := exec.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&name); err != nil {
		return "", wrapStorage("resolve the target schema", err)
	}
	if !name.Valid || name.String == "" {
		return "", aerr.New(aerr.APERTURE_STORAGE,
			"this connection's search_path resolves to no existing schema, so there is nowhere to "+
				"create Aperture's tables — point search_path at a schema that exists (commonly public), "+
				"or create the one it names")
	}
	return name.String, nil
}

// ensurePinnedSchema creates the configured schema if it is not there already.
//
// It runs against the exec it is handed, which is Setup's transaction — so the
// schema is created under the SAME advisory lock and inside the SAME transaction
// as the tables. Both halves of that matter and they are different properties.
// The lock is what stops two booting processes racing the create: without it
// they can both see the schema absent and the loser fails on 42P06
// (duplicate_schema), exactly as CREATE TABLE IF NOT EXISTS loses on 42P07. The
// transaction is what stops a Setup that fails at table creation from leaving an
// empty schema behind — Postgres DDL is transactional, so the rollback takes the
// CREATE SCHEMA with it and a failed Setup changes nothing at all.
func (s *Store) ensurePinnedSchema(ctx context.Context, exec sqlExec) error {
	var exists bool
	err := exec.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, s.schema).Scan(&exists)
	if err != nil {
		return wrapStorage("look up the configured schema", err)
	}
	if exists {
		return nil
	}
	if _, err := exec.ExecContext(ctx, createSchemaStatement(s.schema)); err != nil {
		return createSchemaError(err, s.schema)
	}
	return nil
}

// createSchemaError gives the two refusals an operator actually meets their own
// words, rather than a bare driver error.
func createSchemaError(err error, name string) error {
	switch sqlStateOf(err) {
	case sqlStateInsufficientPrivilege:
		return aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"create schema %q: the database role does not have CREATE on this database. Either grant it "+
				"(GRANT CREATE ON DATABASE <database> TO <role>), or have an administrator create the "+
				"schema once (CREATE SCHEMA %s) and grant the role CREATE on it — Aperture issues no "+
				"CREATE SCHEMA at all once the schema exists",
			name, quoteSchemaIdent(name))
	case sqlStateReservedName:
		return aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"create schema %q: PostgreSQL reserves the pg_ prefix for system schemas, so no role can "+
				"create this one — set %s to a name that does not start with pg_",
			name, EnvSchema)
	}
	return wrapStorage("create the configured schema", err)
}

// applySchema executes the WHOLE of schema.sql as one ExecContext with ZERO bind
// arguments. That precondition is load-bearing and is documented at length in
// the package doc: pgx routes an argument-free Exec through the simple query
// protocol, which accepts a multi-command string, and ANY argument flips the
// script onto the extended protocol, which refuses all of it with 42601. The
// schema qualifier is therefore a textual substitution, never a parameter.
func (s *Store) applySchema(ctx context.Context, exec sqlExec, target string) error {
	if _, err := exec.ExecContext(ctx, qualify(schema, s.qualifier)); err != nil {
		return applyError(err, target)
	}
	return nil
}

// applyError turns a failure to apply schema.sql into a coded error, giving the
// two failures an operator actually hits their own words.
func applyError(err error, target string) error {
	switch sqlStateOf(err) {
	case sqlStateInsufficientPrivilege:
		return aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"create Aperture's tables in schema %q: the database role does not have CREATE there. "+
				"Either grant it (GRANT CREATE ON SCHEMA %s TO <role>), or have an administrator run "+
				"setup once and start Aperture with a role that only needs to read and write the rows — "+
				"setup creates nothing when every table is already present",
			target, target)
	case sqlStateInvalidSchemaName:
		return aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"create Aperture's tables in schema %q: that schema does not exist", target)
	}
	return wrapStorage("apply schema", err)
}

// qualify substitutes the schema qualifier into the embedded script. It is a
// plain literal replacement over the whole file, which rewrites the placeholder
// inside schema.sql's own comments too — harmless, since comments are not
// executed, and preferable to a substitution clever enough to need to know where
// the comments are.
func qualify(script, qualifier string) string {
	return strings.ReplaceAll(script, schemaPlaceholder, qualifier)
}

// expectedTables is every table schema.sql creates. It is a hand-written list
// because Setup needs it at run time and schema.sql is SQL, not data —
// TestExpectedTablesMatchTheSchema reads the embedded script and diffs the two,
// so a table added to one and not the other is build-red rather than a Setup
// that silently verifies less than it created.
var expectedTables = []string{
	"apt_accounts",
	"apt_audit_log",
	"apt_grants",
	"apt_group_members",
	"apt_groups",
	"apt_memberships",
	"apt_object_types",
	"apt_permissions",
	"apt_principal_roles",
	"apt_principals",
	"apt_role_permissions",
	"apt_roles",
	"apt_rules",
	"apt_templates",
	"apt_wiring_attribute_providers",
	"apt_wiring_connections",
	"apt_wiring_field_types",
	"apt_wiring_provider_references",
	"apt_wiring_providers",
}

// inspectTables returns the set of Aperture's tables that exist in target.
//
// It reads pg_class directly rather than asking to_regclass per table: one round
// trip, and — more importantly — it is scoped to ONE namespace by oid, so a
// table of the same name in some other schema on the search_path cannot be
// mistaken for the one Setup is about to create. relkind is restricted to
// ordinary and partitioned tables so a view or a foreign table wearing an
// Aperture name is reported as absent, which is the honest answer: Setup would
// then try to create the table and Postgres would refuse, rather than Aperture
// booting against something that is not its schema.
func inspectTables(ctx context.Context, exec sqlExec, target string) (map[string]bool, error) {
	const q = `
SELECT c.relname
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1
   AND c.relkind IN ('r', 'p')
   AND c.relname LIKE 'apt\_%'`
	rows, err := exec.QueryContext(ctx, q, target)
	if err != nil {
		return nil, wrapStorage("inspect the existing schema", err)
	}
	defer func() { _ = rows.Close() }()
	present := make(map[string]bool, len(expectedTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, wrapStorage("scan an existing table name", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("inspect the existing schema", err)
	}
	return present, nil
}

// missingTables returns, sorted, the expected tables absent from present.
func missingTables(present map[string]bool) []string {
	var missing []string
	for _, t := range expectedTables {
		if !present[t] {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)
	return missing
}

// ---- error mapping ----

// SQLSTATEs this backend reacts to by name. Every other state falls through to
// APERTURE_STORAGE with the driver error preserved as the cause.
const (
	// sqlStateInsufficientPrivilege (42501) is Postgres refusing an operation the
	// role is not allowed to perform. For this backend that is almost always
	// CREATE on the target schema during Setup.
	sqlStateInsufficientPrivilege = "42501"
	// sqlStateInvalidSchemaName (3F000) is a schema qualifier naming nothing.
	// The unsubstituted schema.sql produces exactly this against a real server —
	// deliberately, so a placeholder that never got replaced fails loudly.
	sqlStateInvalidSchemaName = "3F000"
	// sqlStateReservedName (42939) is Postgres refusing a name it reserves for
	// itself. For this backend that is a configured schema starting with pg_,
	// which is a valid identifier and so passes ValidateSchemaName, but which no
	// role — superuser included — may create.
	sqlStateReservedName = "42939"
	// sqlStateClassIntegrityConstraintViolation ("23") is the CLASS, not a single
	// state: 23503 foreign_key_violation, 23505 unique_violation, 23502
	// not_null_violation, 23514 check_violation, 23001 restrict_violation, and
	// the rest of the family.
	sqlStateClassIntegrityConstraintViolation = "23"
)

// sqlStater is what a driver error carrying a SQLSTATE looks like. pgconn.PgError
// implements it, and matching the interface rather than the concrete type keeps
// this file free of a direct pgconn import — the driver is blank-imported and
// reached only through database/sql, which is the arrangement the rest of the
// repo uses too.
type sqlStater interface{ SQLState() string }

// sqlStateOf returns the SQLSTATE a driver error carries, or "" for an error
// that is not one (a context cancellation, a dial failure, sql.ErrNoRows).
func sqlStateOf(err error) string {
	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState()
	}
	return ""
}

// isConstraintViolation reports whether a driver error is Postgres refusing a
// write because it would break a declared constraint — a foreign key above all,
// but a primary-key, uniqueness, NOT NULL or CHECK violation just as much. All
// of them mean the same thing to a caller: the database rejected the write on
// its own terms, nothing is broken, and no retry will help.
//
// The test is on the SQLSTATE CLASS (the first two characters), which is the
// Postgres analogue of storage/sqlite matching SQLITE_CONSTRAINT's primary
// result code rather than its extended one. The class is the standard's own
// grouping — "integrity constraint violation" — so it catches the whole family
// including states this backend has never seen, and the exact state stays
// available on the wrapped cause for anyone who needs the distinction.
func isConstraintViolation(err error) bool {
	return strings.HasPrefix(sqlStateOf(err), sqlStateClassIntegrityConstraintViolation)
}

// wrapStorage is the single place a raw driver error becomes an Aperture-coded
// one, so the constraint mapping covers every statement this package runs
// without each call site remembering it.
//
// Note what it does NOT do: re-stamp an error that already carries a code.
// Callers that may receive one guard with `if aerr.CodeOf(err) != "" { return
// err }` before wrapping, because aerr.Wrap builds a fresh CodedError with
// whatever code it is handed and aerr.CodeOf reports the OUTERMOST one. Wrapping
// an APERTURE_STORAGE_CONSTRAINT in APERTURE_STORAGE buries both the code and
// the fixups that go with it.
func wrapStorage(op string, err error) error {
	if isConstraintViolation(err) {
		return aerr.Wrap(aerr.APERTURE_STORAGE_CONSTRAINT, op, err)
	}
	return aerr.Wrap(aerr.APERTURE_STORAGE, op, err)
}
