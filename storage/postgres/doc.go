// Package postgres is Aperture's PostgreSQL implementation of model.Storage.
//
// It is the peer of storage/sqlite, not a variant of it: both backends satisfy
// the single behavioural contract in storage/storagetest, which carries no
// backend-conditional assertions. Anything this package does differently from
// the SQLite backend is a dialect requirement, and every such difference is
// written down at the point it occurs — in schema.sql's header for the schema,
// and here for the way that schema is applied.
//
// # How this file's schema is applied, and why
//
// schema.sql is a single script of nineteen CREATE TABLE and seven CREATE INDEX
// statements. Setup executes the WHOLE script as ONE ExecContext with NO bind
// arguments. It is not split into statements, and it does not opt into pgx's
// simple-protocol mode.
//
// That decision was made by measurement, not by reasoning, because the received
// wisdom points the other way. pgx's stdlib driver defaults to the extended
// query protocol, and the extended protocol genuinely does refuse multiple
// commands in one message — so "you must split the schema, or force simple
// protocol" is the answer you get from the documentation. It is wrong for this
// call, for a reason that is easy to miss and easy to break.
//
// Measured against PostgreSQL 18.4 with jackc/pgx/v5 v5.10.0 through
// database/sql:
//
//   - The connection reports DefaultQueryExecMode = "cache statement" (the
//     extended protocol) as expected.
//
//   - db.ExecContext(ctx, twoStatements) with no arguments SUCCEEDS anyway, and
//     both tables are created. So does tx.ExecContext.
//
//   - db.ExecContext(ctx, twoStatements, oneArgument) FAILS with SQLSTATE 42601,
//     "cannot insert multiple commands into a prepared statement".
//
//   - db.PrepareContext(ctx, twoStatements) fails identically, before any Exec.
//
// The mechanism is an explicit invariant in pgx, at pgx/v5/conn.go in
// (*Conn).exec: "Always use simple protocol when there are no arguments." The
// exec mode is overridden to QueryExecModeSimpleProtocol whenever the argument
// list is empty, and stdlib's ExecContext forwards straight to (*Conn).Exec
// rather than preparing first. An argument-free Exec therefore travels the
// simple query protocol, which accepts a multi-command string, and the whole
// schema applies in one round trip exactly as it does on SQLite.
//
// Splitting the script was rejected because splitting SQL correctly means
// writing a SQL tokenizer that understands comments, string literals and quoted
// identifiers — schema.sql is more than half prose comments, several of which
// contain semicolons — and a splitter that is subtly wrong applies a subtly
// wrong schema. Forcing simple protocol connection-wide was rejected because it
// would apply to every statement the backend ever runs, not just Setup: it
// disables server-side prepared statements and makes every parameter an
// interpolated literal, which is a real cost paid at every decision for the
// sake of one DDL call at boot.
//
// # The precondition, which must not be broken
//
// The property above holds because the call has NO arguments. That is a
// fragile-looking invariant, so it is stated once, here, and depended on
// deliberately:
//
//   - Setup must pass schema.sql to ExecContext with zero bind arguments.
//     Adding even one flips the entire script onto the extended protocol and
//     Postgres refuses all of it with 42601 — not a partial apply, a total
//     refusal at boot.
//
//   - This is why schema.sql's schema qualifier is a TEXTUAL placeholder
//     substituted into the script before it is executed, and can never become a
//     parameter. It is also why a DDL identifier could not be parameterised in
//     any case: SQL has no bind parameter in an identifier position.
//
//   - Nothing may route the script through Prepare. database/sql only falls
//     back to Prepare for a driver that does not implement driver.ExecerContext;
//     stdlib does implement it, so the direct path is taken. A wrapper that
//     re-prepares would reintroduce the failure.
//
// # What is atomic, and what is not
//
// A multi-command simple query is executed by PostgreSQL inside an implicit
// transaction, and Postgres DDL is transactional. Measured: a script whose
// SECOND statement fails at run time (not parse time) leaves the first
// statement's table absent — the batch rolled back whole. So Setup either
// applies the entire schema or none of it, and a half-created schema is not a
// state this backend can reach. SQLite tolerates that state; Postgres removes
// it.
//
// What this does NOT make safe is two processes calling Setup at once.
// CREATE ... IF NOT EXISTS is not race-free in PostgreSQL: both sessions can
// pass the existence check and the loser fails on a pg_class unique violation
// (42P07 / 23505). Two servers booting against one database is an ordinary
// deployment, so Setup takes a transaction-scoped advisory lock around the
// script. Atomicity and mutual exclusion are different properties; this backend
// needs both.
//
// # Foreign keys
//
// PostgreSQL enforces foreign keys unconditionally. There is no per-connection
// pragma to force, nothing to verify at Setup, and no analogue of sqlite.Open's
// DSN rewriting. The defensive dance in storage/sqlite exists because SQLite
// defaults foreign keys OFF and scopes the setting per connection; porting it
// here would be cargo cult.
//
// The three integrity rules SQL cannot express — the wildcard-bearing
// apt_memberships.account_id and apt_grants.account_id, and the polymorphic
// apt_grants.(subject_kind, subject_id) — are enforced in Go, in both
// directions, exactly as storage/sqlite/integrity.go enforces them. Their
// refusal messages are asserted verbatim by the shared contract suite, naming
// the edge that objected, so this backend reproduces the wording rather than
// merely the error code: a caller must not be able to tell which backend, or
// which mechanism, refused it.
//
// # The schema qualifier
//
// Every table identifier in schema.sql — and in every statement in store.go — is
// written apt_schema.apt_<thing>, where "apt_schema." is a placeholder replaced
// before execution: with "" to use whatever search_path resolves to, or with
// `"<name>".` to pin Aperture's tables into a named schema. The apt_ table
// prefix is what makes the unqualified default safe in a database Aperture
// shares with its host, which is the reason that prefix exists. See schema.sql's
// header for the full rationale, including why index names are deliberately left
// unqualified.
//
// The schema is configured with WithSchema, or from APERTURE_POSTGRES_SCHEMA
// with WithSchemaFromEnv; unset means the ambient search_path and is the
// zero-configuration path. Two properties of that knob are load-bearing enough
// to restate here:
//
//   - Where a statement lands is decided when the statement is BUILT, by
//     substituting the qualifier into it. NOTHING in this package sets
//     search_path on a connection. Per-connection session state is the same
//     footgun as SQLite's foreign_keys pragma — silently correct until the pool
//     hands out a connection that missed it — and this pool holds twenty.
//
//   - A schema name is the ONE piece of configuration this package interpolates
//     into SQL text, because SQL has no bind parameter in an identifier
//     position. It is validated at Open against a strict identifier pattern and
//     quoted at every use; see config.go, which documents why both, and why the
//     two controls are kept independent.
package postgres
