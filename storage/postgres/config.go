package postgres

import (
	"fmt"
	"os"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// This file owns the one piece of configuration this backend takes, and the one
// injection surface it introduces.
//
// # Why a schema name is not like other configuration
//
// A schema name CANNOT be a bind parameter. SQL has no parameter in an
// identifier position, and this particular identifier is worse than most: it is
// interpolated into DDL (CREATE SCHEMA, and every CREATE TABLE in schema.sql)
// and into every one of the ~60 statements in store.go. There is no layer below
// this one that will catch a bad value — by the time the name reaches the
// server it IS SQL text. So the value is validated ONCE, at Open, against a
// pattern strict enough that no accepted value can carry syntax:
//
//	\A[A-Za-z_][A-Za-z0-9_]*\z, at most 63 bytes
//
// and everything else is refused with APERTURE_CONFIG_INVALID before the pool is
// even created. A malformed name fails the process at boot, not a query at 3am.
//
// # Validate AND quote, deliberately
//
// Both controls are applied, and they are independent on purpose.
//
// Validation is the security boundary. Quoting is defence in depth: a
// double-quoted identifier cannot terminate except on a literal '"', which the
// validator already refuses and quoteSchemaIdent doubles anyway, so quoting
// alone would still contain a validator that some future edit weakens. Two
// independent controls is the right number for the only injection surface in the
// package.
//
// Quoting is not only belt-and-braces, though — it is also required for
// CORRECTNESS, and that is the argument that actually settled it. PostgreSQL
// folds an UNQUOTED identifier to lower case, but pg_namespace.nspname is
// ordinary text and is compared here literally (ensureSchema and inspectTables
// both do `nspname = $1`). Configure "Aperture" and an unquoted qualifier would
// address schema `aperture` while the catalog lookups asked about `Aperture`:
// Setup would create nineteen tables and then report all nineteen missing. With
// the name quoted, the schema the statements address and the schema the catalog
// lookups name are the same string, for every accepted value.
//
// The consequence is worth stating plainly because it surprises people:
// APERTURE_POSTGRES_SCHEMA is used EXACTLY as written, case included.
// APERTURE_POSTGRES_SCHEMA=Aperture means a schema literally named Aperture, the
// one `CREATE SCHEMA "Aperture"` makes — not the one `CREATE SCHEMA Aperture`
// makes, which is `aperture`. Aperture does not apply SQL's folding rule to an
// environment variable, because an environment variable is not SQL text; it
// applies no rule at all, which is the only behaviour with no second case.
//
// # Why the value is not trimmed
//
// The house convention for an APERTURE_* variable is to trim surrounding
// whitespace (service.ManagedEntitiesFromEnv, seed/connection.go). This one
// deliberately does not: the string that is VALIDATED must be, byte for byte,
// the string that is INTERPOLATED. A normalisation step between the check and
// the use is the shape most identifier-injection bugs take, and it buys nothing
// here — an operator with a stray space gets a boot failure that names the
// whitespace and the byte it sits at, which is a better outcome than a silent
// success on a value they did not quite type.

// EnvSchema is the environment variable naming the PostgreSQL schema Aperture's
// tables live in.
//
// UNSET (or set to the empty string) means "use whatever the connection's
// search_path resolves to" — the zero-configuration path, and the reason every
// table Aperture owns is prefixed apt_. That prefix is what makes an
// unqualified deployment safe in a database shared with a host application, so
// pinning a schema is an operator's choice about tidiness and grants, never a
// requirement for correctness.
const EnvSchema = "APERTURE_POSTGRES_SCHEMA"

// SchemaNamePattern is the documented form of an acceptable schema name. It is
// the doc and the error message; validateSchemaName is the implementation, and
// TestSchemaNameValidatorMatchesItsDocumentedPattern proves the two agree over
// every single byte and the whole rejection corpus, so the pattern cannot become
// a comment that lies.
//
// It is narrower than PostgreSQL's own identifier grammar, which also admits '$'
// and non-ASCII letters. Neither is excluded by accident: '$' buys nothing a
// schema name needs, non-ASCII drags in normalisation questions (which of two
// visually identical names is the schema?), and the cost of the narrower rule is
// that an operator renames a schema once. The rule guarding the only place this
// package turns configuration into SQL text should be the strictest one that
// still covers every name anybody actually uses.
const SchemaNamePattern = `\A[A-Za-z_][A-Za-z0-9_]*\z`

// MaxSchemaNameLength is the longest schema name Aperture accepts, in BYTES.
//
// PostgreSQL's identifier limit is NAMEDATALEN-1 = 63 bytes, and it does not
// refuse a longer one — it TRUNCATES, with a NOTICE the client library discards
// (measured against PostgreSQL 18.4: CREATE SCHEMA with a 74-character name
// creates a 63-character schema and reports success). Truncation is silent
// retargeting, so Aperture refuses instead. The failure it prevents is concrete:
// the qualifier would carry the full name while pg_namespace.nspname carried the
// truncated one, so ensureSchema would create the tables and inspectTables would
// then report every one of them missing.
const MaxSchemaNameLength = 63

// Option configures a Store at Open time. Every Option is applied before the
// connection pool is created, so a rejected one costs nothing to clean up.
type Option func(*settings) error

// settings is the resolved configuration an Option writes into. It is
// unexported: the Options are the API, so a field added here later cannot become
// a breaking change to a struct literal somebody wrote.
type settings struct {
	// validatedSchema is the schema name, verbatim, AFTER validateSchemaName has
	// accepted it — or "" for the ambient search_path. The field is named for
	// its invariant rather than for its contents, because that invariant is the
	// whole security property of this package and
	// TestEveryWriteToTheSettingsSchemaValidatesFirst enforces it by name: an
	// Option that assigns to this field without calling validateSchemaName in
	// the same function is build-red.
	validatedSchema string
}

// WithSchema pins Aperture's tables into the named schema.
//
// The name is validated immediately, so a caller that builds an Option from
// untrusted input learns at Open — the error is APERTURE_CONFIG_INVALID and the
// pool is never created. It is used verbatim and case-sensitively; see this
// file's header for why.
//
// There is no "" spelling for "unset": WithSchema("") is an error, because the
// alternative is a typo'd configuration silently selecting the ambient
// search_path. To use search_path, pass no Option at all.
func WithSchema(name string) Option {
	return func(s *settings) error {
		if err := validateSchemaName("postgres.WithSchema", name); err != nil {
			return err
		}
		s.validatedSchema = name
		return nil
	}
}

// WithSchemaFromEnv reads EnvSchema and pins that schema, or does nothing when
// the variable is unset or empty. It is the Option the CLI passes, and it is the
// only place in this package that reads the environment.
//
// The read happens when Open applies the Option, which is boot: a malformed
// value fails the process there, with the variable named, rather than surfacing
// as a syntax error from the first statement that runs.
func WithSchemaFromEnv() Option {
	return func(s *settings) error {
		raw := os.Getenv(EnvSchema)
		if raw == "" {
			// Unset and empty are the same thing, per house convention: a
			// deployment that exports the variable blank is a deployment that
			// did not configure it.
			return nil
		}
		if err := validateSchemaName(EnvSchema, raw); err != nil {
			return err
		}
		s.validatedSchema = raw
		return nil
	}
}

// ValidateSchemaName reports whether name is a schema name Aperture will accept,
// returning APERTURE_CONFIG_INVALID describing the first thing wrong with it.
//
// It is exported because it is the security boundary of this package and a
// caller validating configuration of its own should be able to reach the same
// rule rather than approximate it. WithSchema and WithSchemaFromEnv both go
// through it; there is no path to a qualifier that does not.
func ValidateSchemaName(name string) error {
	return validateSchemaName("postgres: schema name", name)
}

// validateSchemaName is the implementation, hand-written rather than a regexp.
//
// A loop over bytes has no anchor semantics to get subtly wrong — a Go regexp's
// '$' matches at end of text today but would match before a trailing newline if
// somebody ever added (?m), and "schema name with a trailing newline" is
// precisely the value this function exists to refuse. It also lets the refusal
// name the offending byte and its position, which a pattern match cannot.
//
// source names where the value came from, so an operator is told which knob to
// fix rather than being handed a rule with no address.
func validateSchemaName(source, name string) error {
	switch {
	case name == "":
		return schemaNameError(source, name, "it is empty")
	case len(name) > MaxSchemaNameLength:
		return schemaNameError(source, name, fmt.Sprintf(
			"it is %d bytes long and PostgreSQL truncates identifiers at %d, which would silently "+
				"point Aperture at a different schema than the one configured",
			len(name), MaxSchemaNameLength))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// Legal anywhere, including first.
		case c >= '0' && c <= '9':
			if i == 0 {
				return schemaNameError(source, name, fmt.Sprintf(
					"it starts with the digit %q; a schema name must start with a letter or an underscore", c))
			}
		default:
			return schemaNameError(source, name, fmt.Sprintf(
				"byte %d is %q, which is not a letter, a digit or an underscore", i, string(c)))
		}
	}
	return nil
}

// schemaNameError builds the one refusal this file produces.
//
// The offending value is rendered with %q, never raw. A schema name is not a
// secret, so echoing it is what makes the message useful — but it IS attacker-
// influenced text on its way into a log file, and %q escapes the newlines and
// control characters that would otherwise let it forge log lines.
func schemaNameError(source, name, reason string) error {
	return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
		fmt.Sprintf("postgres: %s is not a usable PostgreSQL schema name: %q — %s. "+
			"A schema name must match %s and be at most %d bytes. Aperture is strict here because a "+
			"schema name cannot be a bind parameter: it is interpolated into SQL text, so the pattern "+
			"is the only thing standing between this setting and the database",
			source, name, reason, SchemaNamePattern, MaxSchemaNameLength),
		map[string]any{
			"source":      source,
			"value":       name,
			"reason":      reason,
			"pattern":     SchemaNamePattern,
			"max_bytes":   MaxSchemaNameLength,
			"unset_means": "use whatever the connection's search_path resolves to",
		})
}

// qualifierFor turns a validated schema name into the qualifier substituted into
// every statement: "" for the ambient search_path, or `"name".` for a pinned
// schema. The TRAILING DOT belongs to the qualifier — schema.sql and store.go
// both write `apt_schema.apt_<thing>` and the placeholder replaced is
// `apt_schema.`, dot included, so an empty qualifier yields a bare identifier
// rather than a leading dot.
func qualifierFor(schema string) string {
	if schema == "" {
		return ""
	}
	return quoteSchemaIdent(schema) + "."
}

// quoteSchemaIdent renders a schema name as a quoted SQL identifier.
//
// The doubling of '"' is unreachable for any value validateSchemaName accepts,
// and that is the point: quoting is written to be correct on its own, so it
// remains a real control if the validator is ever loosened. Deleting the
// ReplaceAll because "the validator already rejects quotes" would couple the two
// controls into one.
func quoteSchemaIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// createSchemaStatement is the DDL Setup issues for a pinned schema that does
// not exist yet. It is a function rather than an inline string so the statement
// this package would actually send can be asserted without a server.
//
// IF NOT EXISTS is belt-and-braces here rather than the primary mechanism:
// ensureSchema only calls this after finding the schema absent, and it does so
// holding the setup advisory lock, so the race this clause covers is one that
// should already be excluded. It stays because "should already be excluded" is
// not the same as "is", and the clause costs nothing.
func createSchemaStatement(name string) string {
	return `CREATE SCHEMA IF NOT EXISTS ` + quoteSchemaIdent(name)
}
