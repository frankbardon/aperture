package schemagate

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// A small SQL parser
//
// Only as much SQL as the dialect schema files speak: comments, string and
// delimited-identifier literals, statements, the shape of CREATE TABLE, and the
// REFERENCES / ON DELETE / ON UPDATE clauses those tables carry. Everything else
// is tokenized and stepped over. It exists so the gate can tell a table name
// from a column name from a word inside a comment — the three positions a text
// scan conflates.
//
// This is a MOVE of the parser that used to live in
// storage/sqlite/schema_naming_test.go, not a rewrite: the tokenizer, the
// statement splitter and the CREATE TABLE reader are the same code, and
// schema_naming_fixtures_test.go is the safety net that says so. What was added
// on the way over is foreign-key reading, because a parser that steps blindly
// over the eleven keys each dialect now declares cannot tell a key it understands
// from one it merely ignored.
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokWord   tokenKind = iota // bare identifier or keyword
	tokQuoted                  // delimited identifier: "x", `x`, [x]
	tokString                  // 'string literal'
	tokPunct                   // ( ) , ; and any other single character
)

// token is one lexical unit. text has any delimiters stripped, so a quoted
// identifier compares equal to its bare spelling — quoting a reserved word is
// the workaround this convention rejects, not an escape from it.
type token struct {
	kind tokenKind
	text string
	line int
}

// isWord reports whether the token is the given bare keyword, case-insensitively.
// A DELIMITED identifier is never a keyword: "table" is a column named table.
func (t token) isWord(kw string) bool {
	return t.kind == tokWord && strings.EqualFold(t.text, kw)
}

// isIdent reports whether the token can name something.
func (t token) isIdent() bool { return t.kind == tokWord || t.kind == tokQuoted }

// isPunct reports whether the token is the given punctuation character.
func (t token) isPunct(p string) bool { return t.kind == tokPunct && t.text == p }

// identName is the token's name with any schema qualifier dropped, so
// main.apt_grants reads as apt_grants — and so does apt_schema.apt_grants, which
// is how EVERY table identifier in the Postgres file is written. The qualifier
// there is a substitution placeholder the loader rewrites at Setup time
// (storage/postgres/postgres.go, schemaPlaceholder); dropping it here is what
// lets one rule read both dialects without either one knowing about the other.
func (t token) identName() string {
	if t.kind != tokWord {
		return t.text
	}
	if i := strings.LastIndex(t.text, "."); i >= 0 {
		return t.text[i+1:]
	}
	return t.text
}

// tableDef is one CREATE TABLE: its name, the columns it DEFINES, and the
// foreign keys it declares. Table constraints (PRIMARY KEY (a, b), CHECK ...)
// are not column definitions and are deliberately absent from columns — the
// identifiers inside them are references to columns already declared above, and
// reporting them again would double every finding. Foreign keys ARE kept,
// separately, for the same reason in reverse: they are the one table constraint
// carrying information no column definition holds.
type tableDef struct {
	name        string
	line        int
	columns     []columnDef
	foreignKeys []foreignKey
}

// columnDef is one column as DECLARED: its name, where it is, and the two halves
// of what follows the name. The halves are kept apart because the parity gate
// (schema_parity_test.go) treats them differently — the physical type goes
// through an explicit per-dialect mapping (SQLite INTEGER means what Postgres
// BIGINT means), while everything else must match verbatim across dialects.
type columnDef struct {
	name string
	line int
	// typeName is the declared physical type, upper-cased, with any
	// parenthesised arguments kept and internal spacing collapsed:
	// TEXT, BIGINT, VARCHAR(255), DOUBLE PRECISION. It is "" when the
	// definition names no type, which SQLite permits and this schema never does.
	typeName string
	// constraints is everything after the type, normalized: keywords
	// upper-cased, literals kept as written, spacing collapsed. An inline
	// REFERENCES clause is EXCISED — it is recorded as a foreignKey and compared
	// as an edge, and leaving it here as well would report one divergence twice
	// and would make a key spelled inline in one dialect and as a table
	// constraint in the other look like a column difference.
	constraints string
}

// foreignKey is one REFERENCES relationship, however it was spelled: as a table
// constraint (FOREIGN KEY (a) REFERENCES t (b) ...) or inline on the column
// (a TEXT REFERENCES t (b) ...). table has any schema qualifier dropped, so the
// two dialects' edges are directly comparable — which is what E5-S2 needs and
// why the edges are recorded here rather than stepped over.
type foreignKey struct {
	columns    []string // the local columns, in declaration order
	table      string   // the referenced table, qualifier dropped
	refColumns []string // the referenced columns; empty when the clause omitted them
	onDelete   string   // "", CASCADE, RESTRICT, NO ACTION, SET NULL, SET DEFAULT
	onUpdate   string   // same set
	line       int      // 1-based line the key starts on
	inline     bool     // declared on the column rather than as a table constraint
}

// constraintStarters are the bare keywords that open a TABLE constraint rather
// than a column definition. Bare only: a DELIMITED "check" is a column.
var constraintStarters = map[string]bool{
	"CONSTRAINT": true,
	"PRIMARY":    true,
	"UNIQUE":     true,
	"CHECK":      true,
	"FOREIGN":    true,
}

// parseCreateTables reads every CREATE TABLE out of a SQL script. Anything that
// is not a CREATE TABLE — CREATE INDEX, pragmas, comments — is stepped over.
// Malformed SQL is an ERROR, never a silent zero result.
func parseCreateTables(src string) ([]tableDef, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	stmts, err := splitStatements(toks)
	if err != nil {
		return nil, err
	}
	var out []tableDef
	for _, stmt := range stmts {
		tbl, ok, err := parseCreateTable(stmt)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, tbl)
		}
	}
	return out, nil
}

// tokenize splits SQL into tokens, discarding comments and whitespace.
func tokenize(src string) ([]token, error) {
	var out []token
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			// Line comment. Each schema.sql's header comment quotes the OLD,
			// reserved names on purpose, to explain the convention; dropping
			// comments here is what makes that safe.
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated /* block comment", line)
			}
			consumed := src[i : i+2+end+2]
			line += strings.Count(consumed, "\n")
			i += len(consumed)
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closer, kind := c, tokQuoted
			switch c {
			case '[':
				closer = ']'
			case '\'':
				kind = tokString
			}
			text, n, err := scanDelimited(src[i:], c, closer)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			out = append(out, token{kind: kind, text: text, line: line})
			line += strings.Count(src[i:i+n], "\n")
			i += n
		case isIdentByte(c):
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			out = append(out, token{kind: tokWord, text: src[i:j], line: line})
			i = j
		default:
			out = append(out, token{kind: tokPunct, text: string(c), line: line})
			i++
		}
	}
	return out, nil
}

// scanDelimited reads one delimited run starting at s[0] == open, returning the
// contents with delimiters stripped and the number of bytes consumed. When the
// delimiters are the same character it is doubled to escape ('it”s"), which is
// how SQL escapes both string literals and quoted identifiers.
func scanDelimited(s string, open, closer byte) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != closer {
			b.WriteByte(s[i])
			continue
		}
		if open == closer && i+1 < len(s) && s[i+1] == closer {
			b.WriteByte(closer)
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, fmt.Errorf("unterminated %c...%c literal", open, closer)
}

// isIdentByte reports whether a byte can appear in a bare identifier or numeric
// literal. '.' is included so a schema-qualified name arrives as one token;
// identName drops the qualifier.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c == '.' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		c >= 0x80
}

// splitStatements cuts a token stream at top-level semicolons. A semicolon
// inside parentheses does not end a statement, and an unterminated or unbalanced
// statement is an error rather than a truncated result.
func splitStatements(toks []token) ([][]token, error) {
	var stmts [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		if t.kind == tokPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return nil, fmt.Errorf("line %d: unbalanced ')'", t.line)
				}
			case ";":
				if depth == 0 {
					if len(cur) > 0 {
						stmts = append(stmts, cur)
						cur = nil
					}
					continue
				}
			}
		}
		cur = append(cur, t)
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses: %d '(' left open at end of input", depth)
	}
	if len(cur) > 0 {
		return nil, fmt.Errorf("line %d: statement is not terminated with ';'", cur[0].line)
	}
	return stmts, nil
}

// parseCreateTable reads one statement as a CREATE TABLE. The bool reports
// whether it was one; a statement that is not (CREATE INDEX, anything else) is
// stepped over without complaint, while a CREATE TABLE the parser cannot read is
// an error.
func parseCreateTable(stmt []token) (tableDef, bool, error) {
	i := 0
	if len(stmt) == 0 || !stmt[i].isWord("CREATE") {
		return tableDef{}, false, nil
	}
	i++
	for i < len(stmt) && (stmt[i].isWord("TEMP") || stmt[i].isWord("TEMPORARY")) {
		i++
	}
	if i >= len(stmt) || !stmt[i].isWord("TABLE") {
		return tableDef{}, false, nil // CREATE INDEX, CREATE VIEW, ...
	}
	i++
	if i+2 < len(stmt) && stmt[i].isWord("IF") && stmt[i+1].isWord("NOT") && stmt[i+2].isWord("EXISTS") {
		i += 3
	}
	if i >= len(stmt) || !stmt[i].isIdent() {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE without a table name", stmt[0].line)
	}
	nameTok := stmt[i]
	i++

	if i >= len(stmt) || !stmt[i].isPunct("(") {
		// CREATE TABLE ... AS SELECT has no column list. Aperture's schemas never
		// use it, and a gate that silently ignored a table it could not read
		// would be a hole, so refuse rather than skip.
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: expected '(' and a column list", nameTok.line, nameTok.identName())
	}
	body, err := parenBody(stmt[i:])
	if err != nil {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: %w", nameTok.line, nameTok.identName(), err)
	}

	tbl := tableDef{name: nameTok.identName(), line: nameTok.line}
	for _, item := range splitTopLevel(body) {
		if len(item) == 0 {
			return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: empty column definition", nameTok.line, tbl.name)
		}
		first := item[0]
		// A table constraint is not a column definition. Only a BARE keyword
		// opens one, so a delimited "check" or "primary" stays a column and is
		// still checked — which is the point: quoting is not an exemption.
		if first.kind == tokWord && constraintStarters[strings.ToUpper(first.text)] {
			fk, ok, err := parseTableForeignKey(item, tbl.name)
			if err != nil {
				return tableDef{}, false, err
			}
			if ok {
				tbl.foreignKeys = append(tbl.foreignKeys, fk)
			}
			continue
		}
		if !first.isIdent() {
			return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: expected a column name, got %q", first.line, tbl.name, first.text)
		}
		fk, start, end, ok, err := parseColumnForeignKey(item, tbl.name)
		if err != nil {
			return tableDef{}, false, err
		}
		rest := item[1:]
		if ok {
			tbl.foreignKeys = append(tbl.foreignKeys, fk)
			// Excise the clause rather than truncating at it: a REFERENCES may
			// sit in the middle of a column definition (`a TEXT REFERENCES t (id)
			// NOT NULL`), and truncating would silently drop the NOT NULL.
			rest = append(append([]token{}, item[1:start]...), item[end:]...)
		}
		typeToks, n := readColumnType(rest)
		tbl.columns = append(tbl.columns, columnDef{
			name:        first.identName(),
			line:        first.line,
			typeName:    renderTokens(typeToks),
			constraints: renderTokens(rest[n:]),
		})
	}
	if len(tbl.columns) == 0 {
		return tableDef{}, false, fmt.Errorf("line %d: CREATE TABLE %s: no column definitions", nameTok.line, tbl.name)
	}
	return tbl, true, nil
}

// parenBody returns the tokens inside the parenthesised group that toks opens.
func parenBody(toks []token) ([]token, error) {
	depth := 0
	for i, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
			if depth == 0 {
				return toks[1:i], nil
			}
		}
	}
	return nil, fmt.Errorf("unclosed '('")
}

// splitTopLevel splits a column list on commas that are not nested inside
// parentheses, so DEFAULT (a, b) and CHECK (x IN (1, 2)) stay whole.
func splitTopLevel(toks []token) [][]token {
	var out [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case t.isPunct(",") && depth == 0:
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 || len(out) > 0 {
		out = append(out, cur)
	}
	return out
}

// ---------------------------------------------------------------------------
// Foreign keys
//
// Both dialect files declare their eleven keys as TABLE constraints, but the
// inline column form is legal SQL in both engines and a schema that reached for
// it must not become a key the gate silently loses. Both forms are read, and
// both land in the same foreignKey shape.
// ---------------------------------------------------------------------------

// parseTableForeignKey reads a table constraint as a foreign key. The bool
// reports whether it was one: PRIMARY KEY, UNIQUE and CHECK are constraints the
// parser is right to step over, and they return (_, false, nil). A FOREIGN KEY
// the parser cannot read is an ERROR — an unreadable key that passed as "not a
// foreign key" is exactly the hole this reader exists to close.
func parseTableForeignKey(item []token, table string) (foreignKey, bool, error) {
	i := 0
	if item[i].isWord("CONSTRAINT") {
		i++
		// A BARE constraint keyword cannot be the constraint's name, so
		// `CONSTRAINT FOREIGN KEY (a) ...` is a typo and not a nameless key the
		// parser should swallow as somebody's oddly-named CHECK. Refusing it
		// here is what stops that shape from quietly leaving the edge set.
		if i >= len(item) || !item[i].isIdent() ||
			(item[i].kind == tokWord && constraintStarters[strings.ToUpper(item[i].text)]) {
			return foreignKey{}, false, fmt.Errorf("line %d: table %s: CONSTRAINT without a name", item[0].line, table)
		}
		i++
	}
	if i >= len(item) || !item[i].isWord("FOREIGN") {
		return foreignKey{}, false, nil // PRIMARY KEY / UNIQUE / CHECK
	}
	i++
	if i >= len(item) || !item[i].isWord("KEY") {
		return foreignKey{}, false, fmt.Errorf("line %d: table %s: FOREIGN is not followed by KEY", lineAt(item, i), table)
	}
	i++
	cols, n, err := parseIdentList(item[i:])
	if err != nil {
		return foreignKey{}, false, fmt.Errorf("line %d: table %s: FOREIGN KEY: %w", lineAt(item, i), table, err)
	}
	i += n

	fk := foreignKey{columns: cols, line: item[0].line}
	i, err = parseReferences(item, i, &fk, table)
	if err != nil {
		return foreignKey{}, false, err
	}
	if i < len(item) {
		return foreignKey{}, false, fmt.Errorf(
			"line %d: table %s: FOREIGN KEY (%s) REFERENCES %s: unexpected %q after the clause.\nTeach the parser this syntax rather than leaving it to step over a key it cannot read",
			item[i].line, table, strings.Join(cols, ", "), fk.table, item[i].text)
	}
	return fk, true, nil
}

// parseColumnForeignKey reads an inline `col TYPE REFERENCES t (c) ON DELETE ...`
// column definition. Trailing tokens are NOT an error here — NOT NULL, DEFAULT
// and the rest of a column definition legitimately follow — but a REFERENCES
// clause the parser cannot read still is.
//
// The two ints are the half-open token span the clause occupies, so the caller
// can lift the edge out of the column definition and keep the two comparable
// things separate: the edge, and the rest of the definition.
func parseColumnForeignKey(item []token, table string) (fk foreignKey, start, end int, ok bool, err error) {
	depth := 0
	for i, t := range item {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.isWord("REFERENCES"):
			fk := foreignKey{columns: []string{item[0].identName()}, line: item[0].line, inline: true}
			end, err := parseReferences(item, i, &fk, table)
			if err != nil {
				return foreignKey{}, 0, 0, false, err
			}
			return fk, i, end, true, nil
		}
	}
	return foreignKey{}, 0, 0, false, nil
}

// typeStoppers are the bare keywords that END a column's physical type and open
// a column constraint. Everything before the first of them is the type, which is
// what lets a multi-word spelling (DOUBLE PRECISION, TIMESTAMP WITH TIME ZONE)
// arrive whole rather than as its first word. Neither dialect schema uses one
// today; the reader covers them so that adopting one is not a silent truncation.
var typeStoppers = map[string]bool{
	"CONSTRAINT": true, "NOT": true, "NULL": true, "PRIMARY": true,
	"UNIQUE": true, "CHECK": true, "DEFAULT": true, "COLLATE": true,
	"REFERENCES": true, "GENERATED": true, "AS": true, "AUTOINCREMENT": true,
	"DEFERRABLE": true, "STORED": true, "IDENTITY": true,
}

// readColumnType returns the leading tokens that spell the column's physical
// type and how many were consumed, including a parenthesised argument list.
func readColumnType(toks []token) ([]token, int) {
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t.kind != tokWord || typeStoppers[strings.ToUpper(t.text)] {
			break
		}
		i++
	}
	if i > 0 && i < len(toks) && toks[i].isPunct("(") {
		depth := 0
		for j := i; j < len(toks); j++ {
			switch {
			case toks[j].isPunct("("):
				depth++
			case toks[j].isPunct(")"):
				depth--
				if depth == 0 {
					return toks[:j+1], j + 1
				}
			}
		}
	}
	return toks[:i], i
}

// renderTokens writes a token run back out in a normalized form: bare words
// upper-cased (SQL keywords are case-insensitive, so `not null` and `NOT NULL`
// are the same declaration), literals kept exactly as written and re-delimited
// so a string can never be confused with an identifier, and spacing collapsed.
// Two dialects that declared a column the same way produce the same string.
func renderTokens(toks []token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 && needsSpaceBefore(toks[i-1], t) {
			b.WriteByte(' ')
		}
		switch t.kind {
		case tokWord:
			b.WriteString(strings.ToUpper(t.text))
		case tokQuoted:
			b.WriteString(`"` + t.text + `"`)
		case tokString:
			b.WriteString("'" + t.text + "'")
		default:
			b.WriteString(t.text)
		}
	}
	return b.String()
}

// needsSpaceBefore keeps the rendered form readable — DEFAULT ” and
// VARCHAR(255) rather than DEFAULT” and VARCHAR ( 255 ). It affects only
// legibility: both sides of every comparison go through the same renderer.
func needsSpaceBefore(prev, cur token) bool {
	if prev.isPunct("(") {
		return false
	}
	if cur.isPunct(")") || cur.isPunct(",") || cur.isPunct("(") {
		return false
	}
	return true
}

// parseReferences reads REFERENCES <table> [(cols)] followed by any number of
// MATCH / ON DELETE / ON UPDATE / DEFERRABLE clauses, in any order — Postgres
// accepts them in any order and SQLite's grammar is the same shape. It returns
// the index of the first token that is not part of the clause.
func parseReferences(item []token, i int, fk *foreignKey, table string) (int, error) {
	if i >= len(item) || !item[i].isWord("REFERENCES") {
		return 0, fmt.Errorf("line %d: table %s: expected REFERENCES after the key columns", lineAt(item, i), table)
	}
	i++
	if i >= len(item) || !item[i].isIdent() {
		return 0, fmt.Errorf("line %d: table %s: REFERENCES without a target table", lineAt(item, i), table)
	}
	// identName drops apt_schema. here, which is the whole reason the Postgres
	// file's edges are comparable with SQLite's.
	fk.table = item[i].identName()
	i++
	if i < len(item) && item[i].isPunct("(") {
		cols, n, err := parseIdentList(item[i:])
		if err != nil {
			return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: %w", lineAt(item, i), table, fk.table, err)
		}
		fk.refColumns = cols
		i += n
	}
	for i < len(item) {
		switch {
		case item[i].isWord("MATCH"):
			i++
			if i >= len(item) || !(item[i].isWord("FULL") || item[i].isWord("PARTIAL") || item[i].isWord("SIMPLE")) {
				return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: MATCH must be FULL, PARTIAL or SIMPLE", lineAt(item, i), table, fk.table)
			}
			i++
		case item[i].isWord("ON"):
			if i+1 >= len(item) {
				return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: ON with nothing after it", lineAt(item, i), table, fk.table)
			}
			var slot *string
			var which string
			switch {
			case item[i+1].isWord("DELETE"):
				slot, which = &fk.onDelete, "DELETE"
			case item[i+1].isWord("UPDATE"):
				slot, which = &fk.onUpdate, "UPDATE"
			default:
				return i, nil // an ON that belongs to something else
			}
			act, n, err := parseReferentialAction(item[i+2:])
			if err != nil {
				return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: ON %s: %w", lineAt(item, i), table, fk.table, which, err)
			}
			if *slot != "" {
				return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: ON %s given twice", lineAt(item, i), table, fk.table, which)
			}
			*slot = act
			i += 2 + n
		case item[i].isWord("NOT") && i+1 < len(item) && item[i+1].isWord("DEFERRABLE"):
			i += 2
		case item[i].isWord("DEFERRABLE"):
			i++
		case item[i].isWord("INITIALLY"):
			i++
			if i >= len(item) || !(item[i].isWord("DEFERRED") || item[i].isWord("IMMEDIATE")) {
				return 0, fmt.Errorf("line %d: table %s: REFERENCES %s: INITIALLY must be DEFERRED or IMMEDIATE", lineAt(item, i), table, fk.table)
			}
			i++
		default:
			return i, nil
		}
	}
	return i, nil
}

// parseReferentialAction reads one ON DELETE / ON UPDATE action, returning its
// canonical spelling and how many tokens it consumed. The five actions are the
// whole of standard SQL's set; anything else is a typo or syntax the parser has
// not been taught, and both are failures rather than a silently empty action.
func parseReferentialAction(toks []token) (string, int, error) {
	if len(toks) == 0 {
		return "", 0, fmt.Errorf("missing referential action")
	}
	switch {
	case toks[0].isWord("CASCADE"):
		return "CASCADE", 1, nil
	case toks[0].isWord("RESTRICT"):
		return "RESTRICT", 1, nil
	case toks[0].isWord("NO") && len(toks) > 1 && toks[1].isWord("ACTION"):
		return "NO ACTION", 2, nil
	case toks[0].isWord("SET") && len(toks) > 1 && toks[1].isWord("NULL"):
		return "SET NULL", 2, nil
	case toks[0].isWord("SET") && len(toks) > 1 && toks[1].isWord("DEFAULT"):
		return "SET DEFAULT", 2, nil
	}
	return "", 0, fmt.Errorf("unknown referential action %q (want CASCADE, RESTRICT, NO ACTION, SET NULL or SET DEFAULT)", toks[0].text)
}

// parseIdentList reads a parenthesised comma-separated identifier list and
// returns the names plus the number of tokens consumed, including both parens.
func parseIdentList(toks []token) ([]string, int, error) {
	if len(toks) == 0 || !toks[0].isPunct("(") {
		return nil, 0, fmt.Errorf("expected '(' and a column list")
	}
	var out []string
	i := 1
	for {
		if i >= len(toks) {
			return nil, 0, fmt.Errorf("unclosed '(' in a column list")
		}
		if !toks[i].isIdent() {
			return nil, 0, fmt.Errorf("expected a column name, got %q", toks[i].text)
		}
		out = append(out, toks[i].identName())
		i++
		if i < len(toks) && toks[i].isPunct(",") {
			i++
			continue
		}
		break
	}
	if i >= len(toks) || !toks[i].isPunct(")") {
		return nil, 0, fmt.Errorf("expected ')' after a column list")
	}
	return out, i + 1, nil
}

// lineAt is the line number to blame when an error is discovered at or past the
// end of an item, so a truncated clause still reports somewhere useful.
func lineAt(item []token, i int) int {
	switch {
	case i < len(item):
		return item[i].line
	case len(item) > 0:
		return item[len(item)-1].line
	default:
		return 0
	}
}
