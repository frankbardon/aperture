package cli

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"

	_ "modernc.org/sqlite"
)

// E3-S2, the error-propagation half of the CLI sweep.
//
// buildStore is the one place every `aperture` command reaches storage, and
// until this story it re-stamped EVERY startup failure APERTURE_BOOT. That is
// the aerr.Wrap hazard: the wrapper does not pass an existing code through, it
// builds a fresh CodedError with whatever code it is handed, and aerr.CodeOf
// reports the outermost — so the specific refusals E1 and E2 built came out of
// the CLI as "aperture failed to start".
//
// The two codes below are the ones that cost the most when buried, because each
// has a registry fixup that says exactly what to do and neither is guessable
// from the generic one:
//
//   - APERTURE_STORAGE_SCHEMA_INCOMPATIBLE — the database was written by an
//     older build. Aperture ships no migration tool; the fix is a new database,
//     and an operator who is told only "failed to start" will go looking at
//     their environment variables instead.
//   - APERTURE_STORAGE_CONSTRAINT — reachable from Setup (a connection not
//     enforcing foreign keys) and from seeding (a document naming an entity that
//     does not exist). Its six fixups are the map out of every referential
//     refusal in the system.

// codeChain returns every Aperture code in an error chain, outermost first. A
// test that only reads CodeOf cannot tell a preserved code from a re-stamp with
// the same value, and cannot see a cause that a wrapper hid.
func codeChain(err error) []aerr.Code {
	var out []aerr.Code
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		out = append(out, ce.Code)
		err = ce.Inner
	}
	return out
}

// writeLegacyDatabase creates a SQLite file holding an Aperture table in the
// PRE-E1 shape: created_at declared TEXT rather than INTEGER nanoseconds. That
// is signal (1) of storage/sqlite's startup guard, and it is what any database
// an older build created looks like.
func writeLegacyDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE apt_accounts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	return path
}

// TestCLISurfacesTheStorageRefusalCode drives an old-shape database through the
// same buildStore every command calls, and requires the guard's own code to come
// out. Before this story the assertion failed with APERTURE_BOOT.
func TestCLISurfacesTheStorageRefusalCode(t *testing.T) {
	path := writeLegacyDatabase(t)

	_, err := buildStore(context.Background(), "file:"+path, writeSeed(t, "empty.yaml", emptySeed))
	if err == nil {
		t.Fatal("expected a database written by an older build to be refused")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE {
		t.Fatalf("code = %q, want %q — buildStore re-stamped the storage guard's refusal\n(err: %v)",
			got, aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE, err)
	}
	// One coded error, not two with the same value: a wrap that re-applied the
	// SAME code would satisfy CodeOf and still be a re-stamp.
	if chain := codeChain(err); len(chain) != 1 {
		t.Fatalf("code chain = %v, want exactly one coded error (the storage guard's own)", chain)
	}
	// And it must be legible in the text cmd/aperture/main.go prints with %v.
	if !strings.Contains(err.Error(), string(aerr.APERTURE_STORAGE_SCHEMA_INCOMPATIBLE)) {
		t.Fatalf("the printed error must name the code, got %q", err.Error())
	}
}

// TestCLISurfacesTheSeedRefusalCode covers the other coded failure buildStore can
// meet: a seed document whose grant names a principal that does not exist. The
// storage backend refuses it with APERTURE_STORAGE_CONSTRAINT and seed.Apply
// classifies THAT as APERTURE_INVALID_INPUT — a deliberate judgement about its
// own input, and the seed loader's documented contract, so it is the code the
// operator should see.
//
// What must NOT happen is a third stamp on top. Before this story the CLI added
// one, and `aperture serve --seed broken.yaml` reported APERTURE_BOOT for a
// typo in a YAML file.
func TestCLISurfacesTheSeedRefusalCode(t *testing.T) {
	const dangling = `
accounts:
  - {id: acme, name: Acme}
grants:
  - id: g1
    account: acme
    subject: {kind: principal, id: ghost}
    permission: nope
    object: "**"
    effect: allow
`
	dsn := "file:" + filepath.Join(t.TempDir(), "dangling.db")

	_, err := buildStore(context.Background(), dsn, writeSeed(t, "dangling.yaml", dangling))
	if err == nil {
		t.Fatal("expected a seed naming a missing principal to be refused")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want %q (err: %v)", got, aerr.APERTURE_INVALID_INPUT, err)
	}
	// The storage refusal underneath must still be reachable — it is what names
	// the edge and points at the fixups.
	chain := codeChain(err)
	if len(chain) != 2 || chain[1] != aerr.APERTURE_STORAGE_CONSTRAINT {
		t.Fatalf("code chain = %v, want [APERTURE_INVALID_INPUT APERTURE_STORAGE_CONSTRAINT]", chain)
	}
	if !strings.Contains(err.Error(), string(aerr.APERTURE_STORAGE_CONSTRAINT)) {
		t.Fatalf("the printed error must still name the constraint refusal, got %q", err.Error())
	}
}

// TestSeedDocumentDoesNotBuryTheDocumentsOwnRefusal is the same hazard on the
// OTHER read of the same file.
//
// buildStore reads the seed to APPLY it, and seedDocument reads it again as a
// Document, because six of its sections are runtime wiring Apply never writes.
// The second read wrapped unconditionally, so a document carrying a literal
// `dsn:` came back as `[APERTURE_BOOT] cli: parsing the seed file failed:
// [APERTURE_SQL_PROVIDER_DSN_LITERAL] ...` and aerr.CodeOf reported
// APERTURE_BOOT — the fixups an operator got were "check your environment
// variables" and "confirm the backend is reachable" instead of "move it to
// dsn_env: and ROTATE the credential".
//
// Reachable today from `aperture attributes slots`, which parses the document
// before it builds a stack, and from every wiring rebuild in a running `serve`,
// which re-reads the file and hands whatever it gets to the staleness alarm.
//
// Depth is asserted as well as the code, because a same-code re-stamp is
// invisible to CodeOf and the next edit that wraps in a DIFFERENT code is the one
// that buries the refusal again.
func TestSeedDocumentDoesNotBuryTheDocumentsOwnRefusal(t *testing.T) {
	const literalDSN = `
accounts:
  - {id: acme, name: Acme}
connections:
  main:
    dsn: postgres://someone:secret@127.0.0.1:5432/app
`
	path := writeSeed(t, "dsn.yaml", literalDSN)

	_, err := seedDocument(path, storeSQLite)
	if err == nil {
		t.Fatal("a seed document carrying a literal dsn: must be refused")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL {
		t.Fatalf("code = %q, want %q — the document's OWN refusal is the one whose fixups "+
			"say to rotate the credential; APERTURE_BOOT's say to check the environment "+
			"(err: %v)", got, aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL, err)
	}
	if chain := codeChain(err); len(chain) != 1 {
		t.Fatalf("code chain = %v, want exactly one coded error: aerr.Wrap RE-STAMPS, so "+
			"seedDocument must write the pass-through guard "+
			"(if aerr.CodeOf(err) != \"\" { return err }) rather than wrap", chain)
	}
	// The value is never in the message, only the entry. That is seed.Parse's own
	// posture and re-stating it here is what stops a "helpful" edit printing the
	// password the refusal exists to get rotated.
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the refusal printed the credential: %q", err.Error())
	}
}
