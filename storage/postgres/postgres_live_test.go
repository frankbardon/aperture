package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// The Postgres backend's live tests. They talk to a real server, so they are
// GATED and must stay gated: CI runs with no service containers, and `make test`
// has to pass with no database present.
//
// The gate itself — the environment variables, the skip/fail contract, and the
// proof that both hold — lives in gate_test.go. Every test here reaches it
// through requirePostgres, and gate_test.go fails the build if one does not.
//
// These tests exist because the three central claims of this backend's Setup
// cannot be established by reading documentation. That two concurrent Setups
// both succeed is a claim about lock acquisition, catalog visibility and
// transaction isolation interacting correctly; that a role without CREATE gets a
// coded error is a claim about a SQLSTATE a fake driver would simply be told to
// produce. Only a server can settle either.

// withSearchPath returns dsn with the connection's search_path pinned to name.
// pgx passes any connection-string key it does not recognise to the server as a
// runtime parameter, which is how a test gets its own namespace without a
// configuration knob this story does not own.
func withSearchPath(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", pgDSNEnv, err)
		}
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + name
}

// scratchSchema creates an empty schema for one test and drops it afterwards, so
// a live run leaves no residue in whatever database the operator pointed it at.
// It returns the schema name and a DSN whose search_path resolves to it.
func scratchSchema(t *testing.T, ctx context.Context, dsn string) (string, string) {
	t.Helper()
	name := fmt.Sprintf("aperture_e2e_%d", time.Now().UnixNano())
	admin := adminConn(t, ctx, dsn)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(name)); err != nil {
		t.Fatalf("create schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(name) + ` CASCADE`)
	})
	return name, withSearchPath(t, dsn, name)
}

// reservedSchemaName picks a schema name for one test and registers the DROP,
// but deliberately does NOT create it. It is scratchSchema's counterpart for the
// configured-schema tests, where the whole point is that Setup creates the
// schema itself — a helper that created it first would test nothing.
func reservedSchemaName(t *testing.T, ctx context.Context, dsn string) string {
	t.Helper()
	name := fmt.Sprintf("aperture_cfg_%d", time.Now().UnixNano())
	admin := adminConn(t, ctx, dsn)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(name) + ` CASCADE`)
	})
	if err := ValidateSchemaName(name); err != nil {
		t.Fatalf("the test's own generated name is not one Aperture accepts: %v", err)
	}
	return name
}

// schemaExistsOnServer asks the catalog directly, with the name bound rather
// than interpolated — the test's own lookup must not be able to lie about a
// hostile name the way an interpolating one could.
func schemaExistsOnServer(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("look up schema %q: %v", name, err)
	}
	return exists
}

func adminConn(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pgDSNEnv, err)
	}
	return db
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// tableCount reports how many of Aperture's tables exist in schema name.
func tableCount(t *testing.T, ctx context.Context, db *sql.DB, name string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(ctx, `
SELECT count(*)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1 AND c.relkind IN ('r','p') AND c.relname LIKE 'apt\_%'`, name).Scan(&n)
	if err != nil {
		t.Fatalf("count tables in %s: %v", name, err)
	}
	return n
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestPostgresLive_SetupAppliesTheSchemaAndIsIdempotent is the baseline: every
// table and index lands, and a second Setup against the same database is a
// no-op that still returns nil.
func TestPostgresLive_SetupAppliesTheSchemaAndIsIdempotent(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name, storeDSN := scratchSchema(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Fatalf("schema %s has %d apt_ tables after Setup, want %d", name, got, len(expectedTables))
	}

	// Every index the schema declares landed in the same schema as its table,
	// unqualified index names notwithstanding.
	var indexes int
	if err := admin.QueryRowContext(ctx, `
SELECT count(*)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1 AND c.relkind = 'i' AND c.relname LIKE 'idx\_%'`, name).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes != 7 {
		t.Errorf("schema %s has %d idx_ indexes, want 7", name, indexes)
	}

	// Idempotent: creating nothing is a success, not a refusal.
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %s has %d apt_ tables after the second Setup, want %d", name, got, len(expectedTables))
	}
}

// TestPostgresLive_ConcurrentSetupsAllSucceed is this story's central claim, and
// the reason it could not be settled by reading: CREATE TABLE IF NOT EXISTS is
// not race-free in PostgreSQL, so without the advisory lock several of these
// calls fail on 42P07 (duplicate_table) or 23505 (unique_violation on a pg_class
// index) while looking, in the source, entirely correct.
//
// Each goroutine gets its OWN Store and its own pool, which is what makes them
// genuinely concurrent sessions rather than statements multiplexed over one
// connection — the same shape as several aperture processes booting at once.
func TestPostgresLive_ConcurrentSetupsAllSucceed(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name, storeDSN := scratchSchema(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	const racers = 8
	stores := make([]*Store, racers)
	for i := range stores {
		s, err := Open(storeDSN)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		stores[i] = s
		defer func() { _ = s.Close() }()
		// Force the connection open before the barrier, so the race is over
		// Setup rather than over TCP and authentication.
		if err := s.pool.PingContext(ctx); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = stores[i].Setup(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		// Name the two states explicitly: seeing either one is not "a flake",
		// it is the lock not doing its job.
		for _, state := range []string{"42P07", "23505"} {
			if strings.Contains(err.Error(), state) {
				t.Errorf("concurrent Setup %d lost the CREATE race with SQLSTATE %s — the advisory lock is not excluding: %v",
					i, state, err)
			}
		}
		t.Errorf("concurrent Setup %d failed: %v", i, err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %s has %d apt_ tables after %d concurrent Setups, want %d",
			name, got, racers, len(expectedTables))
	}
}

// TestPostgresLive_SetupWithoutCreatePrivilegeIsCoded proves the DDL-privilege
// criterion against the server, with no role management required: PostgreSQL
// refuses CREATE in pg_catalog with SQLSTATE 42501 for EVERY role, superuser
// included ("permission denied to create ...", detail "System catalog
// modifications are currently disallowed"). Pointing search_path there is the
// one privilege-free way to make a real server produce the exact state an
// under-privileged application role would.
func TestPostgresLive_SetupWithoutCreatePrivilegeIsCoded(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)

	s, err := Open(withSearchPath(t, dsn, "pg_catalog"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.Setup(ctx)
	if err == nil {
		t.Fatalf("Setup into pg_catalog succeeded; it must be refused")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_STORAGE {
		t.Fatalf("Setup returned code %q, want APERTURE_STORAGE — a raw driver error reached the caller", code)
	}
	if !strings.Contains(err.Error(), "CREATE") {
		t.Errorf("the refusal does not tell the operator what to grant: %v", err)
	}
	if !strings.Contains(err.Error(), "pg_catalog") {
		t.Errorf("the refusal does not name the schema it could not create in: %v", err)
	}
}

// TestPostgresLive_SetupRefusesAConnectionWithNoSchema covers the ENSURE-SCHEMA
// step: a search_path naming nothing that exists leaves current_schema() NULL,
// and Postgres would otherwise report it as "no schema has been selected to
// create in" from the first CREATE, several steps later.
func TestPostgresLive_SetupRefusesAConnectionWithNoSchema(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)

	s, err := Open(withSearchPath(t, dsn, "aperture_no_such_schema"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.Setup(ctx)
	if err == nil {
		t.Fatalf("Setup with an unresolvable search_path succeeded")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_STORAGE {
		t.Fatalf("Setup returned code %q, want APERTURE_STORAGE", code)
	}
	if !strings.Contains(err.Error(), "search_path") {
		t.Errorf("the refusal does not name search_path: %v", err)
	}
}

// TestPostgresLive_ConstraintViolationsAreCoded proves the SQLSTATE mapping
// against a real server rather than a fake: the foreign keys schema.sql declares
// really do fire, and 23503 and 23505 really do arrive as
// APERTURE_STORAGE_CONSTRAINT. The statement set that will normally produce
// these is E4-S3's; the writes here are deliberately raw so the mapping is
// tested and nothing else is.
func TestPostgresLive_ConstraintViolationsAreCoded(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	_, storeDSN := scratchSchema(t, ctx, dsn)

	s, err := Open(storeDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// 23503: a membership naming a principal that does not exist.
	_, raw := s.pool.ExecContext(ctx,
		`INSERT INTO apt_memberships (principal_id, account_id) VALUES ('ghost', 'acct')`)
	if raw == nil {
		t.Fatalf("an orphan membership was accepted; the foreign key is not enforcing")
	}
	if state := sqlStateOf(raw); state != "23503" {
		t.Fatalf("orphan membership gave SQLSTATE %q, want 23503", state)
	}
	if code := aerr.CodeOf(wrapStorage("put membership", raw)); code != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Errorf("23503 mapped to %q, want APERTURE_STORAGE_CONSTRAINT", code)
	}

	// 23505: the same account inserted twice.
	if _, err := s.pool.ExecContext(ctx, `INSERT INTO apt_accounts (id, name) VALUES ('a', 'A')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	_, raw = s.pool.ExecContext(ctx, `INSERT INTO apt_accounts (id, name) VALUES ('a', 'A')`)
	if raw == nil {
		t.Fatalf("a duplicate primary key was accepted")
	}
	if state := sqlStateOf(raw); state != "23505" {
		t.Fatalf("duplicate account gave SQLSTATE %q, want 23505", state)
	}
	if code := aerr.CodeOf(wrapStorage("put account", raw)); code != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Errorf("23505 mapped to %q, want APERTURE_STORAGE_CONSTRAINT", code)
	}
}

// ---- the configured schema (E4-S4) ----

// TestPostgresLive_SetupCreatesTheConfiguredSchema is the criterion stated end
// to end: point a store at a schema that does not exist, and Setup makes it and
// fills it.
//
// The connection's search_path points at "public" throughout, and every one of
// Aperture's nineteen tables still lands in the configured schema and NOT in
// public. That is the deterministic-resolution criterion made observable: where
// a statement lands is decided when the statement is built, not by session
// state, so a store can address a schema its connection cannot even see
// unqualified.
func TestPostgresLive_SetupCreatesTheConfiguredSchema(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name := reservedSchemaName(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	if schemaExistsOnServer(t, ctx, admin, name) {
		t.Fatalf("schema %q already exists; this test must start from nothing", name)
	}

	s, err := Open(withSearchPath(t, dsn, "public"), WithSchema(name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.Schema() != name {
		t.Fatalf("Schema() = %q, want %q", s.Schema(), name)
	}

	before := tableCount(t, ctx, admin, "public")
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !schemaExistsOnServer(t, ctx, admin, name) {
		t.Fatalf("Setup did not create schema %q", name)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Fatalf("schema %q has %d apt_ tables after Setup, want %d", name, got, len(expectedTables))
	}
	if got := tableCount(t, ctx, admin, "public"); got != before {
		t.Errorf("public gained %d apt_ tables; the qualifier was not applied and search_path decided instead",
			got-before)
	}

	// Idempotent, and creating the schema a second time is not attempted: a
	// second Setup against a database that is fully set up must be a no-op that
	// returns nil, because that is what lets an already-provisioned deployment
	// run with a role holding no CREATE at all.
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %q has %d apt_ tables after the second Setup, want %d", name, got, len(expectedTables))
	}

	// And the store can actually use what it built, over a connection whose
	// search_path still points at public.
	if err := s.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}); err != nil {
		t.Fatalf("write through the configured schema: %v", err)
	}
	var rows int
	if err := admin.QueryRowContext(ctx,
		`SELECT count(*) FROM `+quoteIdent(name)+`.apt_accounts`).Scan(&rows); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if rows != 1 {
		t.Errorf("the configured schema holds %d accounts, want 1", rows)
	}
}

// recordingExec wraps a real sqlExec and keeps every statement that goes through
// it. It is how the tests below assert what Setup DID and DID NOT send, which no
// amount of after-the-fact catalog inspection can show: "the schema exists" is
// the same observation whether Setup created it or found it.
type recordingExec struct {
	inner sqlExec
	stmts []string
}

func (r *recordingExec) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	r.stmts = append(r.stmts, q)
	return r.inner.ExecContext(ctx, q, args...)
}
func (r *recordingExec) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	r.stmts = append(r.stmts, q)
	return r.inner.QueryContext(ctx, q, args...)
}
func (r *recordingExec) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	r.stmts = append(r.stmts, q)
	return r.inner.QueryRowContext(ctx, q, args...)
}

func (r *recordingExec) indexOf(needle string) int {
	for i, s := range r.stmts {
		if strings.Contains(s, needle) {
			return i
		}
	}
	return -1
}

// TestPostgresLive_CreateSchemaRunsUnderTheLockAndOnlyWhenAbsent proves the two
// halves of the criterion that catalog inspection cannot distinguish.
//
// FIRST, ordering: the CREATE SCHEMA is issued AFTER pg_advisory_xact_lock and
// BEFORE the table script, on the same handle — which is what "under the same
// advisory lock and transaction" means operationally. Without that ordering,
// two processes booting at once can both find the schema absent and the loser
// fails on 42P06, exactly as CREATE TABLE IF NOT EXISTS loses on 42P07.
//
// SECOND, check-first: a second Setup, with the schema now present, issues NO
// CREATE SCHEMA at all. That is not an optimisation. Measured against
// PostgreSQL 18.4: `CREATE SCHEMA IF NOT EXISTS x` from a role without CREATE on
// the DATABASE fails with 42501 EVEN WHEN x ALREADY EXISTS — the IF NOT EXISTS
// clause does not skip the privilege check. Issuing it unconditionally would
// therefore break the deployment Setup's CREATE-IF-ABSENT step exists to
// support: an administrator provisions once, and Aperture then runs with a role
// that can only read and write rows.
func TestPostgresLive_CreateSchemaRunsUnderTheLockAndOnlyWhenAbsent(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name := reservedSchemaName(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	// A transaction-scoped Store runs Setup in the transaction it is handed,
	// which is what lets the statements be observed (and, below, rolled back).
	runSetup := func(t *testing.T, commit bool) *recordingExec {
		t.Helper()
		tx, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		rec := &recordingExec{inner: tx}
		s := &Store{pool: nil, exec: rec, schema: name, qualifier: qualifierFor(name)}
		if err := s.Setup(ctx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("Setup: %v", err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		} else if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		return rec
	}

	first := runSetup(t, true)
	lock := first.indexOf("pg_advisory_xact_lock")
	create := first.indexOf("CREATE SCHEMA")
	tables := first.indexOf("CREATE TABLE")
	switch {
	case lock < 0:
		t.Fatalf("Setup took no advisory lock: %v", first.stmts)
	case create < 0:
		t.Fatalf("Setup issued no CREATE SCHEMA for a schema that did not exist: %v", first.stmts)
	case tables < 0:
		t.Fatalf("Setup applied no schema script: %v", first.stmts)
	case !(lock < create && create < tables):
		t.Errorf("Setup's order was lock=%d createSchema=%d tables=%d; the schema must be created after the "+
			"lock is held and before the tables", lock, create, tables)
	}
	if !schemaExistsOnServer(t, ctx, admin, name) {
		t.Fatalf("the committed Setup did not leave schema %q behind", name)
	}

	second := runSetup(t, true)
	if i := second.indexOf("CREATE SCHEMA"); i >= 0 {
		t.Errorf("a second Setup issued %q; with the schema already present it must issue none, or a role "+
			"without CREATE on the database can never start against a provisioned database", second.stmts[i])
	}
	if i := second.indexOf("CREATE TABLE"); i >= 0 {
		t.Errorf("a second Setup re-applied the schema script (%q); every table was already present", second.stmts[i])
	}
}

// TestPostgresLive_ARolledBackSetupLeavesNoSchemaBehind is the "same
// transaction" half, stated as the thing that must not survive.
//
// Setup's CREATE SCHEMA runs on the transaction it is handed, and PostgreSQL DDL
// is transactional — so a Setup that fails anywhere takes the schema with it. A
// CREATE SCHEMA issued on its own connection, or committed early "so the tables
// have somewhere to go", would leave an empty schema behind after every failed
// boot; over a crash loop that is a database full of debris nobody chose.
func TestPostgresLive_ARolledBackSetupLeavesNoSchemaBehind(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name := reservedSchemaName(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	tx, err := admin.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	s := &Store{pool: nil, exec: tx, schema: name, qualifier: qualifierFor(name)}
	if err := s.Setup(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Setup: %v", err)
	}
	// Visible inside the transaction that made it...
	var insideTx bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, name).Scan(&insideTx); err != nil {
		t.Fatalf("look up the schema inside the transaction: %v", err)
	}
	if !insideTx {
		t.Fatalf("Setup reported success without creating schema %q", name)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// ...and gone once it is undone.
	if schemaExistsOnServer(t, ctx, admin, name) {
		t.Errorf("schema %q survived the rollback: CREATE SCHEMA escaped Setup's transaction", name)
	}
}

// TestPostgresLive_ConcurrentSetupsIntoAMissingSchemaAllSucceed is the race the
// advisory lock has to cover now that Setup creates schemas as well as tables.
// Eight processes booting against a database nobody has provisioned yet is the
// FIRST thing that happens to a new deployment, not a corner case, and
// CREATE SCHEMA loses that race with 42P06 exactly as CREATE TABLE loses it with
// 42P07.
func TestPostgresLive_ConcurrentSetupsIntoAMissingSchemaAllSucceed(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	name := reservedSchemaName(t, ctx, dsn)
	admin := adminConn(t, ctx, dsn)

	const racers = 8
	stores := make([]*Store, racers)
	for i := range stores {
		s, err := Open(withSearchPath(t, dsn, "public"), WithSchema(name))
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		stores[i] = s
		defer func() { _ = s.Close() }()
		if err := s.pool.PingContext(ctx); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}

	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = stores[i].Setup(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			continue
		}
		for _, state := range []string{"42P06", "42P07", "23505"} {
			if strings.Contains(err.Error(), state) {
				t.Errorf("concurrent Setup %d lost the create race with SQLSTATE %s — the advisory lock does not "+
					"cover CREATE SCHEMA: %v", i, state, err)
			}
		}
		t.Errorf("concurrent Setup %d failed: %v", i, err)
	}
	if got := tableCount(t, ctx, admin, name); got != len(expectedTables) {
		t.Errorf("schema %q has %d apt_ tables after %d concurrent Setups, want %d",
			name, got, racers, len(expectedTables))
	}
}

// TestPostgresLive_AHostileSchemaNameNeverReachesTheServer is the proof the unit
// tests cannot give: not "the validator returns an error", but "the payload,
// fed through the real entry points at a real server, changes nothing".
//
// A canary schema holding a canary table is created first. Then every value in
// the rejection corpus is pushed through BOTH configuration paths — the Go
// option and the environment variable — against the live DSN. Each must be
// refused; and if one were ever accepted, Setup runs and the canary is checked
// anyway, so an accepted payload that executed would be caught here rather than
// merely assumed impossible. The corpus includes DROP SCHEMA payloads in both
// the plain and the quote-escaping form.
func TestPostgresLive_AHostileSchemaNameNeverReachesTheServer(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	admin := adminConn(t, ctx, dsn)

	canary := fmt.Sprintf("aperture_canary_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(canary)); err != nil {
		t.Fatalf("create the canary schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(canary) + ` CASCADE`) })
	if _, err := admin.ExecContext(ctx,
		`CREATE TABLE `+quoteIdent(canary)+`.canary (id text primary key)`); err != nil {
		t.Fatalf("create the canary table: %v", err)
	}

	// Payloads aimed at the canary specifically, on top of the shared corpus:
	// a value that is refused in general is less interesting than one built to
	// destroy something this test can watch.
	aimed := []string{
		canary + `"; DROP SCHEMA ` + quoteIdent(canary) + ` CASCADE; --`,
		canary + `; DROP SCHEMA ` + quoteIdent(canary) + ` CASCADE; --`,
		`public"; DROP TABLE ` + quoteIdent(canary) + `.canary; --`,
		canary + `.canary`,
		canary + ` CASCADE`,
	}
	var values []string
	for _, tc := range hostileSchemaNames {
		values = append(values, tc.value)
	}
	values = append(values, aimed...)

	for _, v := range values {
		// The Go option.
		if s, err := Open(dsn, WithSchema(v)); err == nil {
			t.Errorf("Open accepted the schema name %q", v)
			if err := s.Setup(ctx); err != nil {
				t.Logf("(the accepted name failed at Setup: %v)", err)
			}
			_ = s.Close()
		}
		// The environment variable. NUL cannot travel this way — setenv(3)
		// refuses it — so it is covered by the option path alone.
		if strings.ContainsRune(v, 0) {
			continue
		}
		t.Setenv(EnvSchema, v)
		if s, err := Open(dsn, WithSchemaFromEnv()); err == nil && s.Schema() != "" {
			t.Errorf("%s=%q was accepted", EnvSchema, v)
			if err := s.Setup(ctx); err != nil {
				t.Logf("(the accepted name failed at Setup: %v)", err)
			}
			_ = s.Close()
		} else if s != nil {
			_ = s.Close()
		}
	}

	// The canary is untouched: the schema is there, the table is there.
	if !schemaExistsOnServer(t, ctx, admin, canary) {
		t.Fatalf("the canary schema %q is gone: a payload executed", canary)
	}
	var tables int
	if err := admin.QueryRowContext(ctx, `
SELECT count(*)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
 WHERE ns.nspname = $1 AND c.relname = 'canary'`, canary).Scan(&tables); err != nil {
		t.Fatalf("look for the canary table: %v", err)
	}
	if tables != 1 {
		t.Errorf("the canary table is gone: a payload executed")
	}
}

// TestPostgresLive_ConfiguredSchemaSurvivesAReservedWordName is the reason the
// qualifier is QUOTED rather than merely validated. "user" and "order" are legal
// identifiers and legal schema names, and an unquoted qualifier turns every
// statement in the backend into a syntax error against them.
func TestPostgresLive_ConfiguredSchemaSurvivesAReservedWordName(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	admin := adminConn(t, ctx, dsn)

	// "user" is the sharpest case: SELECT ... FROM user.apt_accounts does not
	// parse at all, because USER is a reserved word that is also an expression.
	const name = "user"
	if schemaExistsOnServer(t, ctx, admin, name) {
		t.Skipf("a schema named %q already exists on this server; skipping rather than touching it", name)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(name) + ` CASCADE`) })

	s, err := Open(withSearchPath(t, dsn, "public"), WithSchema(name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup into a reserved-word schema: %v", err)
	}
	if err := s.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}); err != nil {
		t.Fatalf("write into a reserved-word schema: %v", err)
	}
	if _, err := s.GetAccount(ctx, "acme"); err != nil {
		t.Fatalf("read back from a reserved-word schema: %v", err)
	}
}

// TestPostgresLive_ConfiguredSchemaIsCaseExact pins the surprising half of the
// quoting decision, so that it is a decision and not an accident.
//
// PostgreSQL folds an UNQUOTED identifier to lower case; Aperture applies no
// folding at all, because an environment variable is not SQL text. So a
// configured "Aperture_Mixed" is the schema `CREATE SCHEMA "Aperture_Mixed"`
// makes, and the all-lowercase schema of the same spelling is a DIFFERENT one.
// The alternative — folding the configured name — would mean a store silently
// retargeting to a schema the operator did not name.
func TestPostgresLive_ConfiguredSchemaIsCaseExact(t *testing.T) {
	dsn := requirePostgres(t)
	ctx := liveCtx(t)
	admin := adminConn(t, ctx, dsn)

	mixed := fmt.Sprintf("Aperture_Case_%d", time.Now().UnixNano())
	lower := strings.ToLower(mixed)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(mixed) + ` CASCADE`)
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(lower) + ` CASCADE`)
	})

	s, err := Open(withSearchPath(t, dsn, "public"), WithSchema(mixed))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !schemaExistsOnServer(t, ctx, admin, mixed) {
		t.Errorf("Setup did not create the schema as written (%q)", mixed)
	}
	if schemaExistsOnServer(t, ctx, admin, lower) {
		t.Errorf("Setup created the folded name %q as well as, or instead of, %q", lower, mixed)
	}
	if got := tableCount(t, ctx, admin, mixed); got != len(expectedTables) {
		t.Errorf("schema %q has %d apt_ tables, want %d", mixed, got, len(expectedTables))
	}
}
