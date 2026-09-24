package postgres

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// ---- the embedded script and the table list ----

// knownTableFloor is the anti-vacuity floor for the schema scan below: the
// schema has nineteen tables and a scanner that suddenly finds three has
// stopped parsing, not stopped needing to. It mirrors the per-dialect
// tableFloor that internal/schemagate's dialect registry keeps for the same
// reason -- this one guards Setup's expectedTables list, that one guards the
// naming gate, and neither is a substitute for the other.
const knownTableFloor = 19

var createTablePattern = regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS\s+` +
	regexp.QuoteMeta(schemaPlaceholder) + `(apt_[a-z_]+)`)

// tablesInSchemaFile returns every table schema.sql creates. Only a line that
// BEGINS a CREATE TABLE statement matches, so schema.sql's prose — which is more
// than half the file and mentions several table names — cannot contribute.
func tablesInSchemaFile(t *testing.T, script string) []string {
	t.Helper()
	var out []string
	for _, m := range createTablePattern.FindAllStringSubmatch(script, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// TestExpectedTablesMatchTheSchema keeps Setup's run-time table list and the
// schema it verifies in lockstep. expectedTables is what INSPECT looks for and
// what VERIFY insists on, so a table added to schema.sql and not to the list is
// a table Setup creates and then never checks — a half-applied schema that
// reports success.
func TestExpectedTablesMatchTheSchema(t *testing.T) {
	// Read from DISK, not only from the embed, so this fails rather than passes
	// vacuously if the file is moved out from under the //go:embed.
	onDisk, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v (the file this package embeds must be readable here)", err)
	}
	if string(onDisk) != schema {
		t.Fatalf("schema.sql on disk differs from the embedded copy: %d bytes vs %d",
			len(onDisk), len(schema))
	}

	found := tablesInSchemaFile(t, string(onDisk))
	if len(found) < knownTableFloor {
		t.Fatalf("found only %d CREATE TABLE statements in schema.sql (floor %d) — the scan is broken, not the schema",
			len(found), knownTableFloor)
	}

	want := append([]string(nil), expectedTables...)
	sort.Strings(want)
	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Errorf("expectedTables disagrees with schema.sql\n  schema.sql:     %v\n  expectedTables: %v", found, want)
	}
}

// TestQualifySubstitutesEveryPlaceholder proves the seam works in both
// directions: empty leaves the identifiers unqualified for the ambient
// search_path, and a name pins them. It also proves no placeholder survives —
// one left behind reaches the server as schema "apt_schema", which does not
// exist.
func TestQualifySubstitutesEveryPlaceholder(t *testing.T) {
	if !strings.Contains(schema, schemaPlaceholder) {
		t.Fatalf("schema.sql no longer contains %q: the substitution seam is gone", schemaPlaceholder)
	}
	ambient := qualify(schema, "")
	if strings.Contains(ambient, schemaPlaceholder) {
		t.Errorf("the empty qualifier left %q in the script", schemaPlaceholder)
	}
	if !strings.Contains(ambient, "CREATE TABLE IF NOT EXISTS apt_accounts") {
		t.Errorf("the empty qualifier did not produce an unqualified CREATE TABLE")
	}
	pinned := qualify(schema, "aperture.")
	if strings.Contains(pinned, schemaPlaceholder) {
		t.Errorf("a named qualifier left %q in the script", schemaPlaceholder)
	}
	if !strings.Contains(pinned, "CREATE TABLE IF NOT EXISTS aperture.apt_accounts") {
		t.Errorf("a named qualifier did not pin the CREATE TABLE")
	}
}

// TestSetupAdvisoryLockKey recomputes the derivation the constant's doc comment
// claims. The number is the only thing making two processes agree on one lock;
// a hand-edit that changed it would leave two builds mutually unsynchronised
// while both look correct.
func TestSetupAdvisoryLockKey(t *testing.T) {
	sum := sha256.Sum256([]byte("github.com/frankbardon/aperture storage/postgres Setup"))
	want := int64(binary.BigEndian.Uint64(sum[:8]))
	if setupAdvisoryLockKey != want {
		t.Errorf("setupAdvisoryLockKey = %d, derivation gives %d", setupAdvisoryLockKey, want)
	}
}

// ---- the pool ----

// TestOpenConfiguresThePool checks the pool limits Open actually applied. It
// needs no server: sql.Open is lazy, and Stats reports the configured caps
// whether or not a connection was ever made.
func TestOpenConfiguresThePool(t *testing.T) {
	s, err := Open("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := s.pool.Stats().MaxOpenConnections; got != DefaultMaxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want %d", got, DefaultMaxOpenConns)
	}
	// The specific value this backend must never inherit. storage/sqlite caps its
	// pool at one connection because SQLite is a single-writer engine; the same
	// line here would serialize every storage read in the process.
	if s.pool.Stats().MaxOpenConnections == 1 {
		t.Errorf("the Postgres pool is capped at one connection — that is SQLite's single-writer workaround, not a house default")
	}
}

// TestPoolConstantsAreIndependentOfSeed states the boundary as an assertion
// rather than a comment: if someone "unifies" the two sets of constants, this
// test is where the intent is written down. It compares values, not identity,
// because the point is that they are ALLOWED to differ.
func TestPoolConstantsAreIndependentOfSeed(t *testing.T) {
	if DefaultMaxOpenConns <= 0 || DefaultMaxIdleConns <= 0 {
		t.Fatalf("both pool caps must be positive: open=%d idle=%d", DefaultMaxOpenConns, DefaultMaxIdleConns)
	}
	if DefaultMaxIdleConns > DefaultMaxOpenConns {
		t.Errorf("MaxIdleConns (%d) exceeds MaxOpenConns (%d); database/sql would silently reduce it",
			DefaultMaxIdleConns, DefaultMaxOpenConns)
	}
	if DefaultConnMaxIdleTime >= DefaultConnMaxLifetime {
		t.Errorf("ConnMaxIdleTime (%s) must be shorter than ConnMaxLifetime (%s), or it never takes effect",
			DefaultConnMaxIdleTime, DefaultConnMaxLifetime)
	}
}

// ---- error mapping ----

// fakePgError carries a SQLSTATE the way pgconn.PgError does, so the mapping can
// be exercised without a server. The mapping matches the SQLState() interface
// rather than a concrete driver type, which is exactly what makes this possible.
type fakePgError struct{ code string }

func (e fakePgError) Error() string    { return "pq: " + e.code }
func (e fakePgError) SQLState() string { return e.code }

func TestWrapStorageMapsSQLStates(t *testing.T) {
	cases := []struct {
		name  string
		state string
		want  aerr.Code
	}{
		{"foreign key violation", "23503", aerr.APERTURE_STORAGE_CONSTRAINT},
		{"unique violation", "23505", aerr.APERTURE_STORAGE_CONSTRAINT},
		{"not null violation", "23502", aerr.APERTURE_STORAGE_CONSTRAINT},
		{"check violation", "23514", aerr.APERTURE_STORAGE_CONSTRAINT},
		{"restrict violation", "23001", aerr.APERTURE_STORAGE_CONSTRAINT},
		{"duplicate table", "42P07", aerr.APERTURE_STORAGE},
		{"insufficient privilege", "42501", aerr.APERTURE_STORAGE},
		{"syntax error", "42601", aerr.APERTURE_STORAGE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapStorage("op", fakePgError{code: tc.state})
			if aerr.CodeOf(got) != tc.want {
				t.Errorf("SQLSTATE %s mapped to %s, want %s", tc.state, aerr.CodeOf(got), tc.want)
			}
			// The driver error stays reachable, so the exact state is never lost.
			var inner fakePgError
			if !errors.As(got, &inner) || inner.code != tc.state {
				t.Errorf("the driver error was not preserved as the cause of %v", got)
			}
		})
	}
}

// TestWrapStorageOnANonDriverError covers everything that carries no SQLSTATE —
// a dial failure, a context cancellation, sql.ErrNoRows.
func TestWrapStorageOnANonDriverError(t *testing.T) {
	got := wrapStorage("op", sql.ErrConnDone)
	if aerr.CodeOf(got) != aerr.APERTURE_STORAGE {
		t.Errorf("a non-driver error mapped to %s, want APERTURE_STORAGE", aerr.CodeOf(got))
	}
	if sqlStateOf(sql.ErrConnDone) != "" {
		t.Errorf("sqlStateOf invented a SQLSTATE for a non-driver error")
	}
}

// TestApplyErrorNamesTheMissingPrivilege is the offline half of the DDL-privilege
// criterion: 42501 must not reach a caller as a raw driver error, and the
// message must say what the operator has to grant. The live half runs against a
// real server in postgres_live_test.go.
func TestApplyErrorNamesTheMissingPrivilege(t *testing.T) {
	err := applyError(fakePgError{code: "42501"}, "public")
	if aerr.CodeOf(err) != aerr.APERTURE_STORAGE {
		t.Fatalf("42501 mapped to %q, want APERTURE_STORAGE", aerr.CodeOf(err))
	}
	for _, want := range []string{"CREATE", "public"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, err)
		}
	}
}

func TestApplyErrorNamesAMissingSchema(t *testing.T) {
	err := applyError(fakePgError{code: "3F000"}, "aperture")
	if aerr.CodeOf(err) != aerr.APERTURE_STORAGE {
		t.Fatalf("3F000 mapped to %q, want APERTURE_STORAGE", aerr.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the refusal does not say the schema is missing: %s", err)
	}
}

// ---- missingTables ----

func TestMissingTables(t *testing.T) {
	if got := missingTables(nil); len(got) != len(expectedTables) {
		t.Errorf("an empty schema reported %d missing tables, want %d", len(got), len(expectedTables))
	}
	all := make(map[string]bool, len(expectedTables))
	for _, tbl := range expectedTables {
		all[tbl] = true
	}
	if got := missingTables(all); len(got) != 0 {
		t.Errorf("a complete schema reported %v missing", got)
	}
	delete(all, "apt_grants")
	if got := missingTables(all); len(got) != 1 || got[0] != "apt_grants" {
		t.Errorf("missingTables = %v, want [apt_grants]", got)
	}
}
