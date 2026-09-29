package schemagate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/internal/sqlreserved"
)

// This file is the gate that keeps Aperture's database-identifier convention
// from rotting, for every dialect rather than for one of them. If you are
// reading it because a build turned red, the convention, the reasoning and the
// escape hatches are written out in doc.go, next door.
//
// It used to live at storage/sqlite/schema_naming_test.go and audit a single
// hardcoded file. The Postgres backend made that a hole: a second schema landed
// entirely ungoverned. The parser moved here wholesale (schema_sql_test.go), the
// hardcoded path became the registry below, and the fixtures came with it
// unchanged so the move is provable rather than asserted.

// aptPrefix is the namespace every Aperture table carries, and the escape hatch
// for the handful of columns whose bare name is reserved.
const aptPrefix = "apt_"

// ---------------------------------------------------------------------------
// The dialects
// ---------------------------------------------------------------------------

// dialect is one schema file the gate governs. There is no "primary" dialect
// here on purpose: SQLite is the reference backend, but the convention is not
// SQLite's, and a registry that treated one file as the real one and the rest as
// extras would drift the moment someone edited the extra.
type dialect struct {
	// name is the backend package's name, used to name the subtest.
	name string
	// path is the schema file, repo-relative. Repo-relative rather than
	// package-relative so a failure message can be pasted into an editor from
	// wherever `go test` was run, and so this package needs no knowledge of how
	// deep it sits.
	path string
	// tableFloor is an anti-vacuity tripwire: the schema had this many tables
	// when the gate landed, and a parser that silently stopped understanding the
	// file would report fewer and then pass with nothing to check. Adding a
	// table is free. REMOVING one trips this deliberately — lower the number in
	// the same commit that removes the table, and only then.
	tableFloor int
	// foreignKeyFloor is the same tripwire for the REFERENCES clauses. A parser
	// that stepped over every key would report zero and still pass the naming
	// rule, because the naming rule never looks at a key.
	foreignKeyFloor int
	// qualifier is the schema qualifier every table identifier in the file is
	// written with, or "" when they are bare. Postgres writes
	// `apt_schema.apt_grants`, where `apt_schema.` is a substitution placeholder
	// the loader rewrites at Setup time; the parser drops it, and this field is
	// what proves the file still spells it that way rather than the parser
	// having quietly started reading a different shape.
	qualifier string
	// statementSources are the Go files carrying SQL string literals for this
	// dialect — the second place any rename has to land, named in the failure
	// message because a rename that stops at the schema file produces a store
	// that starts and then cannot read itself.
	statementSources []string
}

// dialects is the registry. Adding a backend means adding a row; leaving one out
// is caught by TestEveryDialectSchemaIsGoverned below, which walks the tree
// instead of trusting this list.
var dialects = []dialect{
	{
		name:            "sqlite",
		path:            "storage/sqlite/schema.sql",
		tableFloor:      19,
		foreignKeyFloor: 11,
		qualifier:       "",
		statementSources: []string{
			"storage/sqlite/sqlite.go",
			"storage/sqlite/integrity.go",
			"storage/sqlite/schema_guard.go",
		},
	},
	{
		name:            "postgres",
		path:            "storage/postgres/schema.sql",
		tableFloor:      19,
		foreignKeyFloor: 11,
		qualifier:       "apt_schema.",
		statementSources: []string{
			"storage/postgres/store.go",
			"storage/postgres/integrity.go",
			"storage/postgres/postgres.go",
		},
	},
}

// dialectGlob is where a dialect schema file lives, relative to the repo root.
// TestEveryDialectSchemaIsGoverned walks it so the registry cannot fall behind
// the tree.
const dialectGlob = "storage/*/schema.sql"

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestSchemaUsesNoReservedIdentifiers is the gate. It runs in `make test`; it is
// not behind an environment variable, because a convention nobody is forced to
// keep is a convention that is already gone. It needs no container and no node,
// only the files on disk.
func TestSchemaUsesNoReservedIdentifiers(t *testing.T) {
	root := repoRoot(t)
	if len(dialects) == 0 {
		t.Fatal("the dialect registry is empty; this gate would pass by checking nothing")
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			tables := readAndParse(t, root, d)
			if vs := checkNaming(tables); len(vs) > 0 {
				t.Error(report(d, vs))
			}
		})
	}
}

// TestEveryDialectSchemaIsGoverned is what makes the claim "every dialect" true
// rather than "every dialect somebody remembered". It walks the tree for schema
// files and insists the registry and the disk agree in BOTH directions: a new
// backend's schema that nobody registered is ungoverned, and a registered path
// that no longer exists is the fail-don't-skip case wearing a different hat.
func TestEveryDialectSchemaIsGoverned(t *testing.T) {
	root := repoRoot(t)

	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dialectGlob)))
	if err != nil {
		t.Fatalf("glob %s: %v", dialectGlob, err)
	}
	onDisk := make(map[string]bool, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatalf("relativize %s: %v", m, err)
		}
		onDisk[filepath.ToSlash(rel)] = true
	}

	registered := make(map[string]bool, len(dialects))
	for _, d := range dialects {
		registered[d.path] = true
	}

	var unregistered, missing []string
	for p := range onDisk {
		if !registered[p] {
			unregistered = append(unregistered, p)
		}
	}
	for p := range registered {
		if !onDisk[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(unregistered)
	sort.Strings(missing)

	if len(unregistered) > 0 {
		t.Errorf("these dialect schema files exist but no dialect in the registry governs them:\n  %s\n\n"+
			"A schema nobody registered is a schema the naming gate never reads. Add a row to\n"+
			"`dialects` in this file (with its own tableFloor and foreignKeyFloor) in the same\n"+
			"commit that adds the backend.", strings.Join(unregistered, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("these registered dialect schema files are not on disk:\n  %s\n\n"+
			"Fail, never skip. If a schema moved, move its `dialects` row with it.", strings.Join(missing, "\n  "))
	}
	if len(onDisk) == 0 {
		t.Fatalf("no schema files matched %s under %s; the gate has nothing to govern", dialectGlob, root)
	}
}

// TestSchemaForeignKeysAreReadable proves the REFERENCES / ON DELETE / ON UPDATE
// half of the parser against the real files, per dialect. It is not the parity
// gate — nothing here compares one dialect to another; that is
// schema_parity_test.go's job — it is the anti-vacuity check for the clause
// reader, which the parity gate depends on: the naming rule never
// looks at a foreign key, so a parser that quietly stepped over all eleven of them
// would keep passing.
func TestSchemaForeignKeysAreReadable(t *testing.T) {
	root := repoRoot(t)
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			tables := readAndParse(t, root, d)

			declared := make(map[string]bool, len(tables))
			for _, tbl := range tables {
				declared[strings.ToLower(tbl.name)] = true
			}

			n := 0
			for _, tbl := range tables {
				for _, fk := range tbl.foreignKeys {
					n++
					where := fmt.Sprintf("%s:%d: %s.(%s) -> %s", d.path, fk.line, tbl.name, strings.Join(fk.columns, ", "), fk.table)
					if len(fk.columns) == 0 || fk.table == "" {
						t.Errorf("%s: the parser read a foreign key with no columns or no target", where)
					}
					// The target must resolve to a table declared in the SAME
					// file. For Postgres that only holds if `apt_schema.` was
					// dropped, which makes this the qualifier's proof as much as
					// the key's.
					if !declared[strings.ToLower(fk.table)] {
						t.Errorf("%s: references a table this schema does not declare.\n"+
							"Either the target is wrong, or the parser failed to strip the %q qualifier and is reading the wrong name.",
							where, d.qualifier)
					}
					if !hasAptPrefix(fk.table) {
						t.Errorf("%s: references a table that is not prefixed %s", where, aptPrefix)
					}
					// Every edge in both files states both actions explicitly.
					// An empty one means the parser read the clause and found
					// nothing, which is the failure this test exists to catch.
					if fk.onDelete == "" {
						t.Errorf("%s: no ON DELETE action was read from the clause", where)
					}
					if fk.onUpdate == "" {
						t.Errorf("%s: no ON UPDATE action was read from the clause", where)
					}
				}
			}
			if n < d.foreignKeyFloor {
				t.Fatalf("read only %d foreign keys out of %s, expected at least %d.\n"+
					"Either a key was removed (lower foreignKeyFloor in the same commit) or the parser stopped reading REFERENCES clauses and this check is checking nothing.",
					n, d.path, d.foreignKeyFloor)
			}
		})
	}
}

// TestSchemaQualifiersAreStillWhatTheRegistrySays pins the shape of each file's
// table identifiers. The parser drops a qualifier silently and by design, so
// without this a file that lost its `apt_schema.` placeholder — or gained one
// nobody expected — would sail through every other check here.
func TestSchemaQualifiersAreStillWhatTheRegistrySays(t *testing.T) {
	root := repoRoot(t)
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			src := readSchema(t, root, d)
			for _, want := range []string{
				"CREATE TABLE IF NOT EXISTS " + d.qualifier + aptPrefix,
			} {
				if !strings.Contains(src, want) {
					t.Errorf("%s contains no %q.\nThe registry records this dialect's qualifier as %q; if the file's spelling changed, change the row.",
						d.path, want, d.qualifier)
				}
			}
			if d.qualifier != "" && !strings.Contains(src, "REFERENCES "+d.qualifier+aptPrefix) {
				t.Errorf("%s writes its REFERENCES targets without the %q qualifier the registry records", d.path, d.qualifier)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reading a dialect file
// ---------------------------------------------------------------------------

// readSchema reads one dialect's schema file from DISK, matching
// rules/editor_js_contract_test.go: a file that has moved must FAIL rather than
// vanish into a skip. Each backend separately proves its //go:embed copy is
// byte-identical to this file (storage/sqlite/schema_embed_test.go,
// storage/postgres/postgres_test.go), which is what keeps this package from
// auditing a file no Store executes without importing either backend.
func readSchema(t *testing.T, root string, d dialect) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(d.path)))
	if err != nil {
		// Fail, never skip. If a schema moved, this gate is not "not
		// applicable" — it is broken, and so is the store that embeds the file.
		t.Fatalf("read %s: %v\n\nThis gate audits the %s schema and cannot run without it.\nIf the file moved, move its row in `dialects` with it.", d.path, err, d.name)
	}
	return string(src)
}

// readAndParse is readSchema plus the parse and the anti-vacuity floor, which
// every per-dialect check needs before it can believe its own result.
func readAndParse(t *testing.T, root string, d dialect) []tableDef {
	t.Helper()
	src := readSchema(t, root, d)
	tables, err := parseCreateTables(src)
	if err != nil {
		t.Fatalf("parse %s: %v\n\nThis gate parses the schema rather than scanning it, so SQL it cannot read is a failure.\nEither the schema uses syntax the parser does not cover — teach it — or the schema is malformed.", d.path, err)
	}
	if len(tables) < d.tableFloor {
		t.Fatalf("parsed only %d CREATE TABLE statements out of %s, expected at least %d.\nEither a table was removed (lower tableFloor in the same commit) or the parser lost track of the file and this gate is checking nothing.", len(tables), d.path, d.tableFloor)
	}
	return tables
}

// repoRoot walks up from the test's working directory to the module root, so the
// registry can hold repo-relative paths and this package can be moved without
// rewriting every one of them. Not finding it is a failure, never a skip.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.Contains(string(b), "module github.com/frankbardon/aperture") {
				return dir
			}
			t.Fatalf("found a go.mod at %s that is not Aperture's; the gate cannot locate the schema files", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding Aperture's go.mod; the gate cannot locate the schema files")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// The rule
// ---------------------------------------------------------------------------

// violation is one identifier that breaks the convention, carrying everything a
// maintainer needs to act: what it is, where it is, and who objects to it.
type violation struct {
	kind  string // "table" or "column"
	name  string // the offending identifier, as written
	table string // the table it belongs to ("" for a table violation)
	line  int    // 1-based line in the schema file
	// hit is the reserved-word match, if any. A table violation can be clean
	// under the reserved lists and still fail, because the apt_ rule for tables
	// is blanket.
	hit    reservedHit
	hasHit bool
}

// reservedHit records WHICH reserved word an identifier collided with and which
// lists reserve it. The word can differ from the identifier when the match came
// through singularization (grants -> GRANT).
type reservedHit struct {
	word       string // the reserved spelling, upper-cased
	sources    sqlreserved.Source
	singular   bool   // true when the match required singularizing the identifier
	matchedAs  string // the candidate that actually matched, as fed to the lookup
	identifier string // the identifier that was checked
}

// checkNaming applies the convention to a parsed schema and returns every
// violation, in source order. It is dialect-independent on purpose: the
// convention is Aperture's, not any engine's, and a rule with a per-dialect
// carve-out is a rule two backends can drift apart under.
func checkNaming(tables []tableDef) []violation {
	var vs []violation
	for _, tbl := range tables {
		if !hasAptPrefix(tbl.name) {
			v := violation{kind: "table", name: tbl.name, line: tbl.line}
			// A non-prefixed table is a violation whether or not it is reserved,
			// but if it IS reserved the reader deserves to know.
			if hit, ok := reservedMatch(tbl.name); ok {
				v.hit, v.hasHit = hit, true
			}
			vs = append(vs, v)
		}
		for _, col := range tbl.columns {
			// apt_ is the fix, so a prefixed column is by construction fine.
			if hasAptPrefix(col.name) {
				continue
			}
			hit, ok := reservedMatch(col.name)
			if !ok {
				continue
			}
			vs = append(vs, violation{
				kind: "column", name: col.name, table: tbl.name, line: col.line,
				hit: hit, hasHit: true,
			})
		}
	}
	return vs
}

// hasAptPrefix reports whether an identifier already carries the namespace. SQL
// identifiers are case-insensitive, so APT_GRANTS counts.
func hasAptPrefix(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), aptPrefix)
}

// reservedMatch reports whether an identifier's WHOLE name is a reserved word,
// applying singular-family matching.
//
// Whole-name matching is what keeps compounds safe: account_id and object_type
// contain a keyword but are not one, and no engine cares. Singular-family
// matching is what catches the plurals a table-shaped schema naturally reaches
// for — `grants` is GRANT wearing an s, and renaming the column to `grants`
// rather than apt_grants would re-open exactly the hole this effort closed.
func reservedMatch(name string) (reservedHit, bool) {
	for i, cand := range singularFamily(name) {
		if src := sqlreserved.Sources(cand); src != 0 {
			return reservedHit{
				word:       strings.ToUpper(cand),
				sources:    src,
				singular:   i > 0,
				matchedAs:  cand,
				identifier: name,
			}, true
		}
	}
	return reservedHit{}, false
}

// singularFamily returns the identifier followed by the singular forms worth
// testing, most-literal first, so a direct hit is reported as a direct hit.
// English pluralization is not a solved problem and this is not an attempt at
// one — it covers the three endings a SQL schema actually produces.
func singularFamily(name string) []string {
	out := []string{name}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "ies") && len(name) > 3:
		out = append(out, name[:len(name)-3]+"y") // identities -> identity
	case strings.HasSuffix(lower, "ses") && len(name) > 3:
		out = append(out, name[:len(name)-2]) // aliases -> alias
	case strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "ss") && len(name) > 1:
		out = append(out, name[:len(name)-1]) // grants -> grant
	}
	return out
}

// ---------------------------------------------------------------------------
// The failure message
// ---------------------------------------------------------------------------

// report renders every violation as ONE failure, so the convention, the
// offending names and the fix arrive together rather than scattered across
// several t.Error blocks. The dialect is carried through because the two halves
// of a rename — the schema file and the Go files holding the SQL string literals
// — are different files per backend, and a message naming the wrong ones is a
// message that sends a maintainer to the wrong place.
func report(d dialect, vs []violation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s breaks Aperture's database-identifier convention (%s):\n\n",
		d.path, plural(len(vs), "problem"))
	for _, v := range vs {
		b.WriteString(v.describe(d))
	}
	if d.qualifier != "" {
		fmt.Fprintf(&b, "Table identifiers in this file are written %s<name>; the qualifier is a\nsubstitution placeholder and is not part of the name the convention governs.\n\n", d.qualifier)
	}
	b.WriteString(conventionFooter)
	return b.String()
}

// describe renders one violation: what, where, who objects, and what to do.
func (v violation) describe(d dialect) string {
	var b strings.Builder
	switch v.kind {
	case "table":
		fmt.Fprintf(&b, "  line %d: table %q is not prefixed %s\n", v.line, v.name, aptPrefix)
		fmt.Fprintf(&b, "      Every table Aperture owns is namespaced. Rename it to %s%s.\n",
			aptPrefix, strings.TrimPrefix(v.name, aptPrefix))
	case "column":
		fmt.Fprintf(&b, "  line %d: column %q on table %s spells a SQL reserved word\n", v.line, v.name, v.table)
	}
	if v.hasHit {
		h := v.hit
		if h.singular {
			fmt.Fprintf(&b, "      Reserved as %s, matched as the singular of %q.\n", h.word, h.identifier)
		} else {
			fmt.Fprintf(&b, "      Reserved as %s.\n", h.word)
		}
		fmt.Fprintf(&b, "      Reserved by: %s.\n", h.sources)
		if h.sources == sqlreserved.SQLServerFuture {
			b.WriteString("      NOTE: that is the SQL Server \"future keywords\" list ALONE — no shipping\n" +
				"      engine and no ISO revision reserves this word today. It is the one case\n" +
				"      where an exception is arguable; make the argument in writing.\n")
		}
	}
	if v.kind == "column" {
		fmt.Fprintf(&b, "      Rename it to %s%s.\n", aptPrefix, v.name)
	}
	fmt.Fprintf(&b, "      Rename it in %s AND in every SQL string literal in\n"+
		"          %s\n"+
		"      — including the table names passed to deleteByID as plain Go string\n"+
		"      arguments, which no grep for `FROM <name>` will find.\n"+
		"      The other dialect declares the same table; if this name exists there too,\n"+
		"      it has to move in the same commit or the two schemas diverge.\n\n",
		d.path, strings.Join(d.statementSources, "\n          "))
	return b.String()
}

// conventionFooter restates the rule and the reasoning under every failure, so
// the message is actionable without opening this file — or any other.
const conventionFooter = `The convention:
  * Every table Aperture owns is named apt_<thing>. Blanket, no exceptions.
  * A column is prefixed apt_ only when its WHOLE name is a reserved word,
    counting the singular of a plural (grants -> GRANT). Compound names
    (account_id, object_type, role_id) never qualify and are left alone.
  * Indexes keep idx_ and track the table: idx_apt_grants_account_subject.
  * Database identifiers only. Go types, struct fields, wire fields and CLI
    flags are unaffected.

Why: Aperture's SQL is hand-written with no ORM and no query builder to quote
identifiers for it, and there is no schema versioning, so a rename is a hard
break for every existing database. That break was paid once, deliberately.
Re-introducing a reserved word would mean paying it again.

Reserved-word membership comes from internal/sqlreserved, a vendored snapshot of
ten keyword lists (see the header of internal/sqlreserved/words.go for the
sources, the fetch date, and the caveats). Do not weaken this gate to make a name
fit; if a name is genuinely defensible, add a narrow exception with the reasoning
written beside it.`

// plural renders "1 problem" / "3 problems" so the header reads like English.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
