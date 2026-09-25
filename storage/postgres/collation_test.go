package postgres

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// COLLATE "C" is the one dialect divergence in this backend that is invisible
// until it bites, and until this story it was an argued defence rather than a
// measured one. Two gates hold it now:
//
//   - this file, offline and in `make test`: EVERY ORDER BY over a TEXT column in
//     the statement set carries COLLATE "C", checked by parsing the Go source and
//     the schema rather than by reading;
//   - collation_live_test.go, gated: the ordering the statements produce really is
//     byte order even when the columns' own collation is linguistic.
//
// The hazard, restated: SQLite compares TEXT byte-wise. PostgreSQL compares it in
// the COLUMN's collation, which defaults to the database's — on a stock Linux
// cluster a glibc locale like en_US.UTF-8, which weights punctuation below
// alphanumerics on the first pass. Under that collation 'g_x' sorts BEFORE
// 'g-star' and 'ga' before 'gA'; byte-wise both invert. Aperture's own fixtures
// already contain ids of exactly that shape, so two backends the conformance
// suite asserts are identical would hand back list pages in different orders.
//
// Why a parser and not a grep: an ORDER BY term is a small grammar (an optional
// alias, a column, an optional COLLATE, an optional direction) and the statements
// are assembled from several literals at run time. A grep for `COLLATE` would
// pass a statement that carried one on the wrong term.

// orderByFloor is an anti-vacuity floor. The statement set has this many ORDER BY
// clauses today; if a refactor moves them somewhere this gate cannot see, it must
// fail rather than inspect nothing.
const orderByFloor = 24

// packageSQLFiles parses this package's non-test Go sources — the statement set.
func packageSQLFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve the package directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		_, f := parseGo(t, filepath.Join(dir, name))
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("parsed no non-test Go files in this package; this gate is inspecting nothing")
	}
	return out
}

// schemaColumnTypes reads schema.sql and returns column name -> the set of SQL
// types that name is declared with. Aperture's schema never spells one column
// name as two different types, and this gate asserts that too: an ambiguous name
// would make "does this term need a collation?" unanswerable.
func schemaColumnTypes(t *testing.T) map[string]string {
	t.Helper()
	path, err := filepath.Abs("schema.sql")
	if err != nil {
		t.Fatalf("resolve schema.sql: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read schema.sql (%v). This gate reads the column types out of it; "+
			"if it moved, move the gate with it — do not delete it.", err)
	}

	// Constraint clauses look like column definitions to a line reader.
	notColumns := map[string]bool{
		"primary": true, "foreign": true, "unique": true, "check": true,
		"constraint": true, "create": true, "insert": true, "select": true,
	}

	types := map[string]string{}
	inTable := false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "--"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "CREATE TABLE") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if strings.HasPrefix(line, ")") {
			inTable = false
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(line, ","))
		if len(fields) < 2 || notColumns[strings.ToLower(fields[0])] {
			continue
		}
		name, typ := strings.ToLower(fields[0]), strings.ToUpper(fields[1])
		if typ != "TEXT" && typ != "BIGINT" {
			continue
		}
		if prev, seen := types[name]; seen && prev != typ {
			t.Errorf("schema.sql declares %q as both %s and %s. A column name that means two types "+
				"makes the collation rule unanswerable — every ORDER BY over it would need to know "+
				"which table it came from.", name, prev, typ)
		}
		types[name] = typ
	}
	if len(types) < 20 {
		t.Fatalf("read only %d column declarations out of schema.sql; this gate is passing vacuously",
			len(types))
	}
	return types
}

// orderByTerms extracts the ordering terms of every ORDER BY clause in a SQL
// fragment. A clause ends at LIMIT, OFFSET, a closing paren that leaves the
// fragment's own nesting, a statement terminator, or the end of the fragment —
// which is how the run-time-assembled statements (ListGrantsPage, QueryAudit,
// GrantsForSubjects) are handled without reassembling them.
func orderByTerms(sql string) [][]string {
	var out [][]string
	rest := sql
	for {
		i := strings.Index(rest, "ORDER BY")
		if i < 0 {
			return out
		}
		clause := rest[i+len("ORDER BY"):]
		end := len(clause)
		depth := 0
		for j := 0; j < len(clause); j++ {
			switch clause[j] {
			case '(':
				depth++
				continue
			case ')':
				if depth == 0 {
					end = j
					j = len(clause)
					continue
				}
				depth--
				continue
			case ';':
				if depth == 0 {
					end = j
					j = len(clause)
				}
				continue
			}
			if depth != 0 {
				continue
			}
			for _, kw := range []string{"LIMIT", "OFFSET", "FETCH", "FOR ", "RETURNING"} {
				if strings.HasPrefix(clause[j:], kw) && (j == 0 || isSQLBreak(clause[j-1])) {
					end = j
					j = len(clause)
					break
				}
			}
		}
		var terms []string
		for _, term := range splitTop(clause[:end]) {
			if t := strings.TrimSpace(term); t != "" {
				terms = append(terms, t)
			}
		}
		if len(terms) > 0 {
			out = append(out, terms)
		}
		rest = clause[end:]
	}
}

func isSQLBreak(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == ',' }

// splitTop splits on commas that are not inside parentheses.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// TestEveryTextOrderByCarriesCollateC is the gate. It is what keeps the defence
// from decaying: a statement added later without the collation orders correctly
// on a developer's macOS server, correctly under the conformance suite, and
// differently from storage/sqlite on the customer's glibc cluster.
func TestEveryTextOrderByCarriesCollateC(t *testing.T) {
	types := schemaColumnTypes(t)

	var clauses int
	var problems []string
	for name, f := range packageSQLFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, terms := range orderByTerms(v) {
				clauses++
				for _, term := range terms {
					if msg := checkOrderTerm(term, types); msg != "" {
						problems = append(problems, name+": "+msg)
					}
				}
			}
			return true
		})
	}

	if clauses < orderByFloor {
		t.Fatalf("found %d ORDER BY clauses in the statement set, expected at least %d. "+
			"Either the statements moved or this gate is passing vacuously.", clauses, orderByFloor)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("the text-ordering contract is broken in %d place(s):\n  %s\n\n"+
			"PostgreSQL orders TEXT in the column's collation; SQLite orders it byte-wise. Every "+
			"ORDER BY over a TEXT column must pin COLLATE \"C\" or the two backends return list pages "+
			"in different orders on a server whose collation is linguistic — which CI, having no "+
			"server at all, will never tell you.", len(problems), strings.Join(problems, "\n  "))
	}
}

// checkOrderTerm applies the rule to one ordering term and returns "" when it
// holds.
func checkOrderTerm(term string, types map[string]string) string {
	rest := term
	collated := false
	if i := strings.Index(rest, `COLLATE "C"`); i >= 0 {
		collated = true
		rest = rest[:i] + " " + rest[i+len(`COLLATE "C"`):]
	}
	if strings.Contains(strings.ToUpper(rest), "COLLATE") {
		return strconv.Quote(term) + ` carries a collation other than "C"; byte order is the contract`
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return strconv.Quote(term) + " has no column in it"
	}
	col := fields[0]
	for _, f := range fields[1:] {
		switch strings.ToUpper(f) {
		case "ASC", "DESC", "NULLS", "FIRST", "LAST":
		default:
			return strconv.Quote(term) + " is not a plain column ordering; this gate only understands " +
				"`[alias.]column [COLLATE \"C\"] [ASC|DESC]`, so teach it or simplify the statement"
		}
	}
	if i := strings.LastIndex(col, "."); i >= 0 {
		col = col[i+1:] // strip the table alias
	}
	col = strings.ToLower(strings.Trim(col, `"`))

	typ, known := types[col]
	switch {
	case !known:
		return strconv.Quote(term) + " orders by " + strconv.Quote(col) +
			", which schema.sql does not declare — a typo, or a column this gate has not been taught"
	case typ == "TEXT" && !collated:
		return strconv.Quote(term) + " orders by the TEXT column " + strconv.Quote(col) +
			` without COLLATE "C"`
	case typ != "TEXT" && collated:
		return strconv.Quote(term) + " collates " + strconv.Quote(col) + ", which is " + typ +
			" — a collation on a non-text column is a syntax error waiting for the statement to run"
	}
	return ""
}
