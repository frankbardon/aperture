// Package sqlprovider implements provider.ObjectProvider and
// provider.AttributeProvider over a relational database, so a host serves its
// real objects AND its real directory to Aperture from the tables it already has
// instead of exporting them to a CSV. It is a drop-in sibling of csvprovider:
// register a *Provider under an object-type in a provider.Registry exactly as the
// CSV loader is registered, and the Registry's cache, invalidation, and rules
// wiring are unchanged.
//
// This doc describes the OBJECT seam. The attribute seam is *Attributes
// (attributes.go): the same Querier, the same statements-in, the same
// driver-value mapping table and the same value model, serving one attribute
// SLOT instead of one object-type. Everything below is true of both unless the
// Attributes doc says otherwise — and it says otherwise about exactly three
// things, all three concerning what a key is. An attribute fetch binds the BARE
// subject id verbatim rather than an identity's terminal segment ("brand:42"
// binds "42", but "alice" binds "alice"). An attribute "get all" selects a BARE
// id rather than a composed identity — "SELECT u.id AS id", never
// "SELECT 'user:' || u.id AS id", which is the one mistake with NO error
// attached: an identity-shaped key enumerates, caches, and then matches no id
// any Fetch presents, so the slot silently never answers. And
// AttributeConfig.ListQuery is OPTIONAL where Config.ListQuery is required: an
// empty one yields a FETCH-ONLY slot, because attribute enumeration never
// participates in scope resolution, so there is no denial-by-wiring-gap to
// prevent — and a host gains the ability to serve the principal being decided
// about without exposing its whole user table to an admin enumeration.
//
//	db, _ := sql.Open("pgx", os.Getenv("DATABASE_URL"))
//	brands, err := sqlprovider.New(db, sqlprovider.Config{
//		ObjectType: "brand",
//		FetchQuery: `SELECT tier, seats, active FROM brands WHERE id = $1`,
//		ListQuery:  `SELECT 'brand:' || b.id AS id, tier, seats, active FROM brands b`,
//	})
//	if err != nil { return err }
//	reg.MustRegister("brand", brands)
//
// # The Querier seam
//
// The dependency is the narrow Querier interface below, never a concrete
// *sql.DB. A *sql.DB satisfies it, and so does an *sqlx.DB, a pgx stdlib
// handle, a *sql.Conn, a *sql.Tx, or a host's own tracing/retrying wrapper —
// anything with database/sql's two read methods. Aperture therefore owns no
// connection lifecycle here: it does not open, close, ping, pool, or configure
// anything. The host builds the handle it wants, with the driver, pool sizes,
// TLS, and observability it already runs, and hands it over.
//
// This is also why the package imports NO driver. A driver is a host choice;
// linking one in would force every Aperture binary to carry it.
//
// Note that this is a HOST data source. It is unrelated to Aperture's own
// storage (storage/, hand-written SQL on modernc.org/sqlite) and shares no
// connection handling with it: provider data is pulled, read-only, and never
// persisted as source of truth.
//
// # The two wiring paths
//
// There are exactly two ways a *Provider comes to exist, and they MEET at New.
//
// The first is the Go path shown above: the host owns the handle, hands it over
// as a Querier, and Aperture opens nothing. That is the path a library consumer
// wants, and it is the only one this package knows about.
//
// The second is DECLARATIVE, and lives in the seed package: a document's
// top-level connections: block names a database, and a kind: sql provider entry
// names that connection and carries both statements inline, so a deployment
// wires a real database with no Go code at all.
//
//	connections:
//	  main:
//	    dsn_env: APP_DATABASE_URL   # a literal dsn: is REFUSED at parse
//	    query_timeout: 3s           # becomes Config.Timeout, per CONNECTION
//	providers:
//	  - object_type: brand
//	    kind: sql
//	    connection: main
//	    get_one: SELECT tier, seats FROM brands WHERE id = $1   # → FetchQuery
//	    get_all: SELECT 'brand:' || b.id AS id, b.tier FROM brands b  # → ListQuery
//
// Neither path is a special case of the other. seed resolves the connection
// name to a pool and then calls THIS package's New with the same Config a host
// fills in by hand, so every rule the constructor enforces — required
// statements, the id column, the timeout — is enforced identically whichever way
// the provider was declared. Everything documented below is therefore true of
// both.
//
// The one asymmetry is resource ownership, and it is the seed package's, not
// this one's: a declared connection opens ONE pool per NAME, shared by every
// entry referencing it, and a pool has a lifetime. So a document declaring
// connections: must be built with Document.BuildRegistryWithConnections and the
// returned *Connections closed at shutdown; the one-return BuildRegistry refuses
// such a document rather than opening pools nothing can close. That is also the
// only place Aperture links a database driver (pgx, Postgres only) — this
// package still links none. Nothing is pinged at build either way: a wrong host
// or password surfaces on the first decision touching a SQL-backed type, as
// APERTURE_SQL_PROVIDER_QUERY, not at startup.
//
// # What Fetch binds
//
// A fetch statement takes exactly ONE parameter, and Aperture binds the
// identity's TERMINAL SEGMENT VALUE to it — not the identity string. Fetching
// "brand:42" and "account:acme/brand:42" both bind "42":
//
//	Fetch("account:acme/brand:42")  →  QueryContext(stmt, "42")
//
// That is what lets the developer write `WHERE b.id = $1` against the primary
// key they already have, and hit its index. Full Aperture identities are rarely
// a column in a host's schema, and a statement that had to reconstruct one per
// row could not use an index at all.
//
// Placeholders are ENGINE-NATIVE and passed through untouched: Aperture never
// rewrites "$1" to "?" or vice versa. Write the placeholder syntax your database
// speaks — "$1" for Postgres, "?" for MySQL or SQLite — and it reaches the
// driver exactly as written. There is no dialect layer to be surprised by.
//
// Parameters are always BOUND, never interpolated. This is the SQL-injection
// boundary of the whole feature and it is not configurable: an object id
// originates outside the process, so a provider that pasted it into a statement
// would hand every caller of Check arbitrary SQL execution. There is no API here
// that builds a statement from a value.
//
// # What List and Query enumerate
//
// Enumeration runs a SECOND statement, Config.ListQuery — the "get all" — and it
// binds NO parameters. Every row it returns is one object of this provider's
// type, and one of its columns, Config.IDColumn (default "id"), carries that
// object's IDENTITY.
//
// The identity is COMPOSED BY THE DEVELOPER, in SQL, and Aperture supplies no
// template for it:
//
//	SELECT 'brand:' || b.id AS id, b.tier, b.seats FROM brands b
//
// That is deliberate. An Aperture identity is hierarchical and host-shaped —
// "brand:42" for one deployment, "account:acme/brand:42" for another — and the
// prefix is exactly where a host expresses its tenancy. A template baked in here
// would be a second place tenancy is decided, disagreeing with the first. The
// column is a string, so the statement can build whatever the host's identities
// actually look like, and Fetch's `WHERE id = $1` still binds a bare key.
//
// The id column is the identity, not a metadata field: it is removed from the
// row before the remaining columns become metadata. Every other column maps
// exactly as it does for Fetch, through the same table.
//
// A row Aperture cannot place is an APERTURE_SQL_PROVIDER_ROW_IDENTITY error
// naming the row's position in the result, never a row silently skipped:
//
//   - the result has no id column at all,
//   - the row's id is NULL, empty, or not textual,
//   - the id does not parse as an identity,
//   - the identity's TERMINAL SEGMENT TYPE is not this provider's object-type.
//
// The last one is the one that matters most. A "brand:1" row returned by the
// statement wired under the "dataset" object-type must never reach the cache:
// it would be cached under an identity this provider's own Fetch could never
// return, so a later Check would read one type's row as another type's object.
// Skipping such rows instead would be worse still — enumeration would come back
// short, and short enumeration reads as "no access".
//
// # Query filters in Go, never in SQL
//
// Query runs the SAME "get all" statement and applies Filter.Fields with
// provider.MatchFields, in Go. Predicates are NEVER templated into the
// developer's statement.
//
// That is a decision about correctness, not convenience. The Fields contract
// says comparison is TYPED and never a string rendering — "5" does not equal 5,
// and int64(5) does equal float64(5) — because those are the rules engine's own
// comparison semantics, and Enumerate must not select an object that Check then
// denies over the same value. Postgres will happily coerce '5' to 5, so a
// predicate rendered into SQL would answer a different question than the rule
// evaluated over the same field. Reproducing a collection field's MEMBERSHIP
// rule across text[], jsonb, and a delimited string is the same hazard again.
// Aperture therefore does the comparison once, in one place, for every provider.
//
// The cost is honest: the whole object-type is materialised per enumeration, and
// the Registry's per-type TTL cache is what absorbs it. Filter.Pattern and
// Filter.Limit are applied to the rows as they stream — pattern first, then the
// field predicates, then the limit — so a bounded enumeration stops reading
// early. A very large table will hurt; that is a known, accepted trade for not
// having two comparison semantics.
//
// Stopping early has one consequence worth knowing: a LIMIT that truncates
// before a malformed row is reached will NOT surface that row's error. The same
// enumeration with a larger limit — or none — can fail where the bounded one
// succeeded, because a row's error is a property of the rows actually READ, not
// of the table. A row excluded by Filter.Pattern is the same story one step
// earlier: its metadata is never mapped, so a bad column on it goes unreported.
// Its IDENTITY still is — every scanned row's id column is checked.
//
// A mid-iteration failure is checked (rows.Err) after the loop and reported. It
// is never a short but successful result: a truncated enumeration under-reports
// access, and under-reported access denies silently.
//
// # Columns become metadata
//
// Every column the fetch statement returns becomes a metadata field keyed by its
// COLUMN NAME, so the SELECT list is the field list and the developer controls
// it in SQL:
//
//	SELECT tier, seats, active FROM brands WHERE id = $1
//	→ {"tier": "gold", "seats": int64(5), "active": true}
//
// A column therefore needs a name: an unnamed expression (`SELECT count(*)`) or
// two columns with the same name (a `SELECT b.*, o.*` over two tables that both
// have "name") is an APERTURE_SQL_PROVIDER_SCAN error naming the column, not a
// field silently dropped or overwritten by whichever came last.
//
// A NULL column OMITS its field rather than storing nil, which is csvprovider's
// absent-vs-zero rule applied to SQL: an absent field never matches a
// Filter.Fields predicate and a rule can supply its own default, whereas a
// stored nil is a value that compares — and a zero that compares is how a NULL
// end_date silently satisfies every "before" rule ever written against it.
//
// # Driver values become metadata
//
// What a column becomes is decided by the GO TYPE database/sql scans it into,
// and that mapping is a closed table:
//
//	NULL       →  the field is OMITTED (absent is not zero)
//	bool       →  the scalar, as-is
//	int64      →  the scalar, as-is
//	float64    →  the scalar, as-is
//	string     →  the scalar, as-is
//	[]byte     →  JSON-DECODED (this is how arrays and nested objects arrive)
//	time.Time  →  converted to UTC, then the canonical "2006-01-02T15:04:05Z"
//	anything else → APERTURE_SQL_PROVIDER_SCAN naming the column and the type
//
// Every mapped row is then checked against the shared metadata value model
// (provider.ValidateMetadata) before it is returned, so a shape the expression
// evaluator cannot handle fails the fetch instead of surfacing as an evaluation
// error on the Check hot path. See values.go for the rule-by-rule reasoning; the
// three consequences a developer has to act on are these.
//
// CAST IT IN THE STATEMENT. The statement is the only typing mechanism there is.
// There is no per-column type declaration to write in YAML, because two
// spellings for one intent drift apart, and the statement is the one the
// developer is already writing. So:
//
//	to_jsonb(tags) AS tags     an array — the ONLY way one arrives
//	hired_on::text AS hired_on a day-granular date ("2026-03-04")
//	amount::float8 AS amount   a numeric meant as a number
//	sku::text AS sku           a numeric or uuid meant as an identifier
//
// A []byte IS JSON, unconditionally. It does not fall back to a string. That is
// deliberate: a fallback would let one column change TYPE depending on its
// contents. A bytea therefore does not work — encode it in the statement, or
// leave it out, since an access-control decision has no business reading a blob.
// A JSON null omits its field, exactly as a SQL NULL does.
//
// A time.Time is ALWAYS a timestamp, never a day. A database date and a database
// timestamp are the same Go type, so granularity cannot be inferred; the
// datetime form is the one that loses nothing. Write `col::text` for a day.
//
// # The trap this package cannot catch for you
//
// Selecting an array column WITHOUT casting it compiles, runs, and is wrong:
//
//	SELECT tags FROM brands WHERE id = $1        -- WRONG
//
// A Postgres text[] does not arrive as a list. It arrives as the raw array
// literal, the six-character string "{a,b}" — a perfectly valid metadata string,
// indistinguishable from a string a host meant to store. Nothing in this package
// can tell the two apart, so nothing here will complain. What happens instead is
// that every membership predicate written against that field silently matches
// nothing, forever, and the rule reads as though it simply never applies.
//
// The fix is one function call in the SELECT list:
//
//	SELECT to_jsonb(tags) AS tags FROM brands WHERE id = $1   -- RIGHT
//
// If a list-valued field is matching nothing, check its cast first.
//
// # Absent, ambiguous, and broken
//
// The three outcomes a fetch can have are three different errors, deliberately:
//
//   - ZERO rows is APERTURE_NOT_FOUND — the documented ObjectProvider contract,
//     so the Registry can distinguish an absent object from an operational
//     failure. It is the only one of the three that is a normal, expected
//     answer.
//   - MORE THAN ONE row is APERTURE_SQL_PROVIDER_AMBIGUOUS, naming the identity.
//     The first row is NOT silently taken. Without an ORDER BY, which row a
//     database hands back first is unspecified, so taking one would make an
//     object's metadata — and every decision computed from it — vary between two
//     identical Checks. The usual cause is a join that fans out; the fix is in
//     the statement, not in a LIMIT 1 that would only hide it.
//   - A DRIVER or connection failure is APERTURE_SQL_PROVIDER_QUERY with the
//     driver's error wrapped verbatim (reachable with errors.Is / errors.As).
//     An error that already carries an APERTURE_* code — a coded error raised by
//     a host's wrapping Querier — passes through untouched; wrappers never
//     re-stamp a code.
//
// # Timeouts
//
// Every statement runs under context.WithTimeout, DefaultTimeout (5s) unless
// Config.Timeout says otherwise. Fetch sits under Check, which owes a p99 under
// a millisecond, so an unbounded query against a host database is not a slow
// decision — it is a decision that never returns, holding a connection while it
// does not. The deadline applies on top of whatever the caller's own context
// already carries, so the earlier of the two wins and a caller can always be
// stricter than the provider.
//
// # Warming the Registry's cache: the two statements must project the same columns
//
// provider.Registry caches an object's metadata per type, and every READ of that
// cache is a Fetch — the decision path's authoritative view of an object, what a
// rule sees as `object.*`. An enumeration may warm that cache, which is what keeps
// a rule-backed Enumerate from paying a round trip per candidate, but only when a
// listed bag is the bag Fetch would have produced. Two independent statements make
// that a real question rather than a given:
//
//	get_one: SELECT tier, seats, renews_on FROM brands WHERE id = $1
//	get_all: SELECT 'brand:' || b.id AS id, b.tier FROM brands b   -- NARROWER
//
// Warming from that listing would cache a one-field bag under an id whose real bag
// has three, for the whole of the type's TTL. A rule then reads `object.seats` as
// ABSENT — not wrong, absent — and every predicate over an absent field is false:
// an inclusive grant denies, and an EXCLUSIVE grant stops excluding and therefore
// WIDENS. A wider get_all is the mirror image, caching a field Fetch would never
// produce. Neither leaves anything in a verdict, trace or note, because a bag of
// any shape is a legal bag.
//
// So a *Provider answers provider.FetchCompleteLister, and it answers it from what
// it has OBSERVED: the column names of each statement's own result
// (ListedMetadataMatchesFetch). The list statement's columns minus the id column
// must equal the fetch statement's columns, as a set; until both statements have run
// once the answer is false and the Registry simply does not warm. There is no
// Config field and no YAML key for it — an operator's unchecked promise about two
// statements is exactly the thing this is replacing.
//
// Two practical consequences for the developer writing the pair:
//
//   - PROJECT THE SAME COLUMNS IN BOTH, and the cache warms as it always did. It
//     is also the pairing that makes an enumerate-then-decide sequence self
//     consistent, which is worth more than the round trips.
//   - A deliberately narrow get_all — a display projection over a wide table — stays
//     legal and stays correct. It costs one Fetch per candidate per TTL window
//     instead of one per enumeration, and the cost is silent. If a SQL-backed type's
//     enumeration is slower than its sibling's, compare the two SELECT lists first.
//
// # Concurrency and the read-only contract
//
// A *Provider's configuration is immutable after New — a Querier, two statements,
// an id column and a timeout, none of which ever change. The one thing that does
// change is the pair of column projections it has observed (above), which is
// guarded by the Provider's own mutex and written idempotently. So a *Provider is
// safe for concurrent use, as the ObjectProvider contract requires. Concurrency
// beneath it is the Querier's business, and *sql.DB is itself concurrency-safe.
//
// Per the provider.Metadata contract every returned map is freshly allocated for
// that one Fetch, with no container shared with another call or retained by the
// provider, and must be treated as read-only by its holder.
//
// Dependencies stay minimal: sqlprovider imports database/sql from the standard
// library plus errors, identity, and provider — no driver, and no CGO.
package sqlprovider

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// compile-time assertions: a *Provider is a usable ObjectProvider, and it answers
// the FetchCompleteLister question the Registry asks before warming its metadata
// cache from an enumeration.
var (
	_ provider.ObjectProvider      = (*Provider)(nil)
	_ provider.FetchCompleteLister = (*Provider)(nil)
)

// DefaultTimeout bounds one statement when Config.Timeout is zero. Fetch runs
// under Check, so the default is short on purpose: a provider query that has not
// answered in five seconds has already failed the decision it was serving.
const DefaultTimeout = 5 * time.Second

// Querier is the seam this provider depends on: database/sql's two read methods
// and nothing else. It is deliberately narrower than *sql.DB so a host can hand
// over whatever handle it already owns — a *sql.DB or *sql.Conn, an *sqlx.DB, a
// pgx stdlib handle, a *sql.Tx, or its own tracing, retrying, or read-replica
// wrapper — without Aperture taking over connection lifecycle or pooling.
//
// The context passed to either method already carries this provider's timeout;
// an implementation must honour it rather than starting its own unbounded work.
type Querier interface {
	// QueryContext runs query with args bound as parameters and returns its rows.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	// QueryRowContext runs a query expected to return at most one row. Fetch does
	// not use it — it must be able to SEE a second row to reject it, and
	// QueryRowContext discards one — but it is part of the seam so a host can
	// satisfy the interface with a handle it already has and so later single-row
	// reads need no second interface.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DefaultIDColumn is the result column ListQuery must expose the identity in
// when Config.IDColumn is empty. It is "id" because that is what the developer
// writing `SELECT 'brand:' || b.id AS id` already types, and the CSV loader's
// required header column is spelled the same way.
const DefaultIDColumn = "id"

// Config is the developer-supplied wiring for one object-type's provider.
type Config struct {
	// ObjectType is the object-type this provider serves — the same key it is
	// registered under in a provider.Registry.
	//
	// It is required, and it is not decoration: every row the list statement
	// returns is checked against it, so a row whose identity belongs to another
	// type is rejected instead of cached under an identity this provider's own
	// Fetch could never return.
	ObjectType string
	// FetchQuery is the "get one" statement. It takes exactly one placeholder,
	// written in the database's own syntax and passed through untouched, to which
	// Fetch binds the identity's terminal segment value:
	//
	//	SELECT tier, seats FROM brands WHERE id = $1
	//
	// Every column it returns becomes a metadata field keyed by the column name,
	// so the SELECT list is the field list. Required.
	FetchQuery string
	// ListQuery is the "get all" statement behind List and Query. It takes NO
	// parameters, returns one row per object of this type, and must select the
	// object's full identity as IDColumn:
	//
	//	SELECT 'brand:' || b.id AS id, b.tier, b.seats FROM brands b
	//
	// Every other column becomes a metadata field keyed by its column name,
	// exactly as for FetchQuery.
	//
	// It is required. An ObjectProvider owes List and Query, and a provider that
	// answered them with an error would enumerate as "no objects" one layer up —
	// which reads as "no access". A provider that can be fetched from but not
	// enumerated is therefore not a configuration this package offers.
	ListQuery string
	// IDColumn names the ListQuery result column holding each row's identity.
	// Empty means DefaultIDColumn ("id"). The column is removed from the row
	// before the rest becomes metadata: it is the identity, not a field.
	IDColumn string
	// Timeout bounds one statement, applied with context.WithTimeout on top of
	// the caller's context. Zero means DefaultTimeout; negative is a
	// configuration error. There is no "no timeout" setting.
	Timeout time.Duration
}

// Provider is a SQL-backed ObjectProvider for one object-type. Its configuration
// is immutable after New; the only thing that changes over its life is the pair of
// column projections it has observed (see observedProjections), which is guarded
// by its own mutex. It is therefore safe for concurrent use, as the
// ObjectProvider contract requires; the Querier owns whatever pooling happens
// beneath it.
type Provider struct {
	q          Querier
	objectType string
	fetchQuery string
	listQuery  string
	idColumn   string
	timeout    time.Duration

	// The observed column projections of the two statements, which is what
	// ListedMetadataMatchesFetch answers from. They are the ONLY mutable state on
	// a Provider; see "Warming the Registry's cache" in the package doc for why
	// they are observed rather than declared.
	mu        sync.Mutex
	fetchCols []string // sorted; nil until the fetch statement has executed once
	listCols  []string // sorted, id column removed; nil until the list statement has
}

// New returns a Provider that reads objects through q using cfg's statements.
// Register it under the object-type whose instances the statement describes.
//
// Unlike csvprovider.New — which defers to first use because the file it names
// may not exist yet — this validates now: a nil Querier, a blank ObjectType,
// FetchQuery, or ListQuery, or a negative Timeout is a wiring mistake that would
// otherwise first surface as a
// failed decision, and an access-control engine should not learn about its own
// misconfiguration from a denied Check.
func New(q Querier, cfg Config) (*Provider, error) {
	if q == nil {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: nil Querier; pass a *sql.DB or another handle with QueryContext and QueryRowContext")
	}
	objectType := strings.TrimSpace(cfg.ObjectType)
	if objectType == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.ObjectType is required; it is the object-type this provider is registered under, and every enumerated row's identity is checked against it")
	}
	stmt := strings.TrimSpace(cfg.FetchQuery)
	if stmt == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.FetchQuery is required; it is the statement that fetches one object by its terminal segment value")
	}
	listStmt := strings.TrimSpace(cfg.ListQuery)
	if listStmt == "" {
		return nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.ListQuery is required; it is the statement that returns every object of this type, with its identity composed in the id column (SELECT 'brand:' || b.id AS id, ...)")
	}
	idColumn := strings.TrimSpace(cfg.IDColumn)
	if idColumn == "" {
		idColumn = DefaultIDColumn
	}
	if cfg.Timeout < 0 {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"sqlprovider: Config.Timeout cannot be negative; leave it zero for the default",
			map[string]any{"timeout": cfg.Timeout.String(), "default": DefaultTimeout.String()})
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Provider{
		q:          q,
		objectType: objectType,
		fetchQuery: stmt,
		listQuery:  listStmt,
		idColumn:   idColumn,
		timeout:    timeout,
	}, nil
}

// Fetch returns id's metadata by running the configured fetch statement with
// id's TERMINAL SEGMENT VALUE bound as its single parameter ("brand:42" and
// "account:acme/brand:42" both bind "42").
//
// No rows yields APERTURE_NOT_FOUND, so the Registry can distinguish an absent
// object from an operational failure. More than one row yields
// APERTURE_SQL_PROVIDER_AMBIGUOUS naming the identity — the first row is never
// silently taken, because which row that is would be unspecified. A driver
// failure yields APERTURE_SQL_PROVIDER_QUERY wrapping the cause, unless the
// cause already carries an APERTURE_* code, which passes through verbatim.
//
// The returned map is freshly allocated for this call and is read-only to its
// holder, per the provider.Metadata contract.
func (p *Provider) Fetch(ctx context.Context, id identity.Identity) (provider.Metadata, error) {
	key, err := terminalValue(id)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rows, err := p.q.QueryContext(ctx, p.fetchQuery, key)
	if err != nil {
		return nil, queryError(err, id)
	}
	defer rows.Close()

	// The fetch statement's projection, recorded before any row is read: a column
	// list is a property of the STATEMENT, so even a fetch that finds nothing
	// teaches it. This is one half of what ListedMetadataMatchesFetch compares.
	if cols, cerr := rows.Columns(); cerr == nil {
		p.observeFetchColumns(cols)
	}

	if !rows.Next() {
		// rows.Err first: a connection that died mid-statement also reports "no
		// next row", and reporting that as NOT_FOUND would turn a broken database
		// into a confident "this object does not exist" — and, one layer up, into
		// a deny that looks deliberate.
		if err := rows.Err(); err != nil {
			return nil, queryError(err, id)
		}
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"sqlprovider: no object with this id", map[string]any{"id": id.String()})
	}

	md, err := scanRow(rows, id.String())
	if err != nil {
		return nil, err
	}

	// A second row is checked BEFORE the metadata is returned, never after it is
	// used: an ambiguous fetch is a broken statement, not a fetch with a bonus.
	if rows.Next() {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_AMBIGUOUS,
			"sqlprovider: fetch statement returned more than one row for one identity; "+
				"filter on a unique key rather than adding LIMIT 1, which would make the metadata depend on an unspecified row order",
			map[string]any{"id": id.String()})
	}
	if err := rows.Err(); err != nil {
		return nil, queryError(err, id)
	}
	return md, nil
}

// List returns every object of this provider's type, in the order the "get all"
// statement returns them. It is Query with the zero Filter.
//
// Each row's IDColumn is the object's identity, composed by the developer in
// SQL; every other column becomes a metadata field keyed by its column name. A
// row Aperture cannot place — no id column, a NULL/empty/non-textual id, an
// unparseable identity, or an identity whose terminal segment type is not this
// provider's object-type — is an APERTURE_SQL_PROVIDER_ROW_IDENTITY error naming
// the row, never a row silently skipped.
func (p *Provider) List(ctx context.Context) ([]provider.Object, error) {
	return p.enumerate(ctx, provider.Filter{})
}

// Query returns the objects of this provider's type that satisfy filter.
//
// It runs the SAME "get all" statement List runs and applies Filter.Fields with
// provider.MatchFields, in Go. The predicates are never templated into the
// developer's SQL: the Fields contract is typed comparison ("5" != 5) and
// membership for collection fields, which is the rules engine's own semantics,
// and a database's own coercion rules do not reproduce it — an Enumerate that
// selected what a Check then denies is exactly the disagreement that contract
// exists to prevent.
//
// Filter.Pattern and Filter.Limit are honoured here as well as by the Registry,
// so a bounded enumeration stops reading rows early and Query is correct when
// called standalone. Pattern is applied first, then the field predicates, then
// the limit.
func (p *Provider) Query(ctx context.Context, filter provider.Filter) ([]provider.Object, error) {
	return p.enumerate(ctx, filter)
}

// enumerate is the one implementation behind List and Query: run the "get all"
// statement, turn every row into an Object, and keep the ones filter selects.
//
// It streams — the rows are consumed one at a time and never buffered as driver
// values — but it does materialise every selected object, which is the accepted
// cost of filtering in Go rather than in the host's SQL. The Registry's per-type
// TTL cache is what amortises it.
func (p *Provider) enumerate(ctx context.Context, filter provider.Filter) ([]provider.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// No arguments: the "get all" statement takes no parameters, and nothing a
	// caller supplied is ever interpolated into it.
	rows, err := p.q.QueryContext(ctx, p.listQuery)
	if err != nil {
		return nil, p.listError(err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot read the result columns of the list statement for object-type %q", p.objectType)
	}
	// The column shape is a property of the STATEMENT, so it is validated once
	// rather than re-derived per row.
	where := map[string]any{"object_type": p.objectType}
	if err := validateColumns(cols, where); err != nil {
		return nil, err
	}
	idIndex := indexOf(cols, p.idColumn)
	if idIndex < 0 {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: the list statement's result has no id column; select each object's full identity under that name (SELECT 'brand:' || b.id AS id, ...)",
			map[string]any{"object_type": p.objectType, "id_column": p.idColumn, "columns": strings.Join(cols, ", ")})
	}
	// The list statement's METADATA projection — its columns minus the id column,
	// which is the identity and not a field. Recorded only once the projection is
	// known to be well-formed, so a statement that is rejected teaches nothing.
	p.observeListColumns(cols)

	// One scan buffer for the whole result — see scanSlots for why reusing it is
	// safe — and a non-nil result slice, so an empty enumeration is an empty
	// slice rather than a nil that a caller might read as "unset".
	vals, dest := scanSlots(len(cols))
	out := make([]provider.Object, 0, 16)
	row := 0
	for rows.Next() {
		row++
		if err := rows.Scan(dest...); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
				"sqlprovider: cannot scan row %d of the list statement for object-type %q", row, p.objectType)
		}
		id, err := p.rowIdentity(vals[idIndex], row)
		if err != nil {
			return nil, err
		}
		if filter.Pattern != nil && !filter.Pattern.Matches(id) {
			continue
		}
		md, err := rowMetadata(cols, vals, idIndex, id.String())
		if err != nil {
			return nil, err
		}
		if !provider.MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, provider.Object{ID: id, Metadata: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	// rows.Err LAST and unconditionally. A connection that dies half way through
	// a result set ends the loop exactly as a complete one does, so skipping this
	// would report a truncated enumeration as a successful one — and a short
	// enumeration is, one layer up, an access the caller silently does not have.
	if err := rows.Err(); err != nil {
		return nil, p.listError(err)
	}
	return out, nil
}

// ListedMetadataMatchesFetch answers provider.FetchCompleteLister: whether the
// bags List and Query return are the bags this provider's own Fetch would return,
// which is what permits provider.Registry to warm its per-type metadata cache from
// an enumeration instead of fetching every candidate.
//
// It is DERIVED from the two statements' real column projections, never declared.
// There is no Config field for it and no YAML key, because a promise an operator
// types is a promise nothing checks — and this one, broken, changes verdicts (see
// the package doc's "Warming the Registry's cache").
//
// The answer is:
//
//	false  until BOTH statements have executed once — the projections are not
//	       known yet, so nothing may be assumed about them;
//	true   when the list statement's columns MINUS the id column are exactly the
//	       fetch statement's columns, as a set;
//	false  otherwise — a narrower get_all, a wider one, or a differently named
//	       column in either direction.
//
// EQUALITY, not containment, and in both directions on purpose. A narrower listing
// caches a bag with a field missing, and an absent field makes every predicate over
// it false — which denies under an inclusive grant and stops EXCLUDING under an
// exclusive one. A wider listing caches a field Fetch would never produce, so a
// predicate over it is true until the entry expires and false afterwards. Both are
// verdicts computed from something no statement of the host's actually says.
//
// Columns, not values, is what makes this sound cheaply. rows.Columns() is the
// statement's SELECT list, so it is the same for every row and unaffected by any
// row's NULLs — where comparing two bags would not be, since a NULL column omits
// its field and an omitted field is indistinguishable from an unprojected one. The
// one thing it does not prove is that two statements over the same id return the
// same DATA; a get_all whose join produces a different value for a column get_one
// also projects is a host bug about the object's own data, which nothing in this
// package can see.
//
// The practical consequence of the "not known yet" answer is one cold enumeration
// per process per object-type: the first List cannot warm, its candidates each
// fetch (which is what teaches the fetch projection), and every later enumeration
// warms as before. That cost is O(1) in enumerations, not O(1) per candidate, and
// is the reason the answer is re-read per enumeration rather than settled at
// registration.
func (p *Provider) ListedMetadataMatchesFetch() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetchCols == nil || p.listCols == nil {
		return false
	}
	return slices.Equal(p.fetchCols, p.listCols)
}

// observeFetchColumns records the fetch statement's projection, sorted so the
// comparison is a set comparison rather than a SELECT-list-order one.
func (p *Provider) observeFetchColumns(cols []string) {
	sorted := slices.Sorted(slices.Values(cols))
	p.mu.Lock()
	p.fetchCols = sorted
	p.mu.Unlock()
}

// observeListColumns records the list statement's METADATA projection: its columns
// with the id column removed, because that column carries the identity and never
// becomes a field (rowMetadata skips it).
func (p *Provider) observeListColumns(cols []string) {
	fields := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == p.idColumn {
			continue
		}
		fields = append(fields, c)
	}
	slices.Sort(fields)
	p.mu.Lock()
	p.listCols = fields
	p.mu.Unlock()
}

// observedProjections returns what the provider has learned about its two
// statements, for a diagnostic or a test. A nil slice means that statement has not
// run yet. The slices are copies, so a caller cannot reach into the provider's own.
//
// Writes here are idempotent after the first one — the same statement has the same
// columns every time — so the mutex is about publication, not about contention:
// concurrent callers must not observe a half-assigned slice header, and a Fetch
// racing a List must not have its record lost. Nothing on the decision path waits
// on it for longer than a slice copy, and ListedMetadataMatchesFetch is asked once
// per enumeration rather than once per row.
func (p *Provider) observedProjections() (fetch, list []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.fetchCols), slices.Clone(p.listCols)
}

// rowIdentity turns the id column of the row'th row into an identity of this
// provider's object-type.
//
// The id must be TEXT. A []byte is accepted as raw text here — unlike a metadata
// column, where a []byte is JSON — because the id column is not metadata and has
// no JSON reading that could compete: some drivers hand back a string column as
// bytes, and an identity is a string either way. Anything else, a bare integer
// key included, is rejected with the composition it is missing, because
// "42" is not an identity and Aperture supplies no template that would make it
// one.
func (p *Provider) rowIdentity(v any, row int) (identity.Identity, error) {
	// Every diagnostic here names the ROW's position in the result, which is the
	// only handle a developer has on a row whose id is exactly what went wrong.
	base := map[string]any{"object_type": p.objectType, "id_column": p.idColumn, "row": row}
	where := func(extra map[string]any) map[string]any { return mergeContext(base, extra) }

	var raw string
	switch x := v.(type) {
	case string:
		raw = x
	case []byte:
		raw = string(x)
	case nil:
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is NULL; every enumerated row needs an identity, so exclude the row in the statement or give it one",
			where(nil))
	default:
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is not text; compose the full identity in the statement ('brand:' || b.id AS id), casting the key with ::text if it is numeric",
			where(map[string]any{"type": goTypeName(v)}))
	}
	if strings.TrimSpace(raw) == "" {
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column is empty; every enumerated row needs an identity",
			where(nil))
	}

	id, err := identity.Parse(raw)
	if err != nil {
		// The id is the developer's own composition, not host row data, so naming
		// it is actionable rather than a leak.
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's id column does not parse as an object identity; compose it as type:value in the statement ('brand:' || b.id AS id)",
			where(map[string]any{"id": raw, "error": err.Error()}))
	}
	segs := id.Segments()
	if len(segs) == 0 || segs[len(segs)-1].Type != p.objectType {
		// The row belongs to another object-type. It is rejected rather than
		// skipped: caching it would key one type's metadata under an identity
		// this provider's own Fetch could never return, and skipping it would
		// under-report the enumeration, which reads as "no access".
		return identity.Identity{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY,
			"sqlprovider: a row's identity belongs to a different object-type than this provider serves; the terminal segment's type must be the registered object-type",
			where(map[string]any{"id": id.String(), "terminal_type": terminalType(segs)}))
	}
	return id, nil
}

// listError normalises a failure of the "get all" statement. Like queryError it
// passes an already-coded error through verbatim, and names the object-type
// rather than any row value.
func (p *Provider) listError(err error) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_QUERY, err,
		"sqlprovider: list statement failed for object-type %q", p.objectType)
}

// terminalType names the type of an identity's terminal segment, for a
// diagnostic. It renders "" for an identity with no segments.
func terminalType(segs []identity.Segment) string {
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1].Type
}

// indexOf reports where name sits in cols, or -1.
func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

// terminalValue extracts the value Fetch binds: the id part of the identity's
// terminal segment. An empty identity has no terminal segment and so nothing to
// bind; it is rejected here rather than binding "" and letting the database
// answer "no rows", which would report a caller's bug as a missing object.
func terminalValue(id identity.Identity) (string, error) {
	segs := id.Segments()
	if len(segs) == 0 {
		return "", aerr.New(aerr.APERTURE_IDENTITY_INVALID,
			"sqlprovider: cannot fetch an empty identity; it has no terminal segment to bind")
	}
	return segs[len(segs)-1].ID, nil
}

// queryError normalises a database failure. An error already carrying an
// APERTURE_* code passes through verbatim — a host's wrapping Querier may raise
// one, and re-stamping it would bury the classification the host chose —
// while a plain driver error is wrapped as APERTURE_SQL_PROVIDER_QUERY with the
// cause reachable through errors.Is / errors.As.
//
// The message names the identity, which is the caller's own input, and never the
// row values, which are host data belonging to some account.
func queryError(err error, id identity.Identity) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_QUERY, err,
		"sqlprovider: fetch statement failed for %s", id.String())
}

// scanRow turns the row rows is positioned on into a fresh Metadata, keyed by
// result column name.
//
// The destination slice is allocated per call — a Provider retains nothing
// between fetches — which is what keeps the read-only Metadata contract true for
// concurrent callers.
func scanRow(rows *sql.Rows, id string) (provider.Metadata, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot read the result columns for %s", id)
	}

	if err := validateColumns(cols, map[string]any{"id": id}); err != nil {
		return nil, err
	}

	vals, dest := scanSlots(len(cols))
	if err := rows.Scan(dest...); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_SQL_PROVIDER_SCAN, err,
			"sqlprovider: cannot scan the row for %s", id)
	}
	// -1: a fetch result has no identity column to skip — the identity is the
	// caller's own input, not something the row carries.
	return rowMetadata(cols, vals, -1, id)
}

// validateColumns checks the shape of a result's column names, which is a
// property of the STATEMENT and therefore checked once per statement rather than
// once per row. where names whatever the caller can say about the context — a
// fetch's identity, or an enumeration's object-type — and is merged into every
// diagnostic.
func validateColumns(cols []string, where map[string]any) error {
	seen := make(map[string]bool, len(cols))
	for i, name := range cols {
		if name == "" {
			return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
				"sqlprovider: result column has no name; alias every selected expression, because a column name is the metadata field key",
				mergeContext(where, map[string]any{"index": i}))
		}
		if seen[name] {
			// Last-wins would make the field's value depend on the SELECT list's
			// order, which is exactly the kind of invisible dependency that turns
			// a schema change into a changed decision.
			return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_SCAN,
				"sqlprovider: result has two columns with the same name; alias one, because a column name is the metadata field key",
				mergeContext(where, map[string]any{"column": name}))
		}
		seen[name] = true
	}
	return nil
}

// scanSlots allocates one row's scan destinations. The values are plain `any`s
// so database/sql hands back the driver's own Go type, which is what the mapping
// table in values.go switches on.
//
// An enumeration reuses one pair across every row, which is safe precisely
// because nothing downstream retains a scanned value: a scalar is copied by
// assignment, database/sql clones a []byte into a fresh slice, and jsonValue
// decodes into containers it allocates itself.
func scanSlots(n int) (vals []any, dest []any) {
	vals = make([]any, n)
	dest = make([]any, n)
	for i := range vals {
		dest[i] = &vals[i]
	}
	return vals, dest
}

// rowMetadata maps one scanned row into a fresh Metadata keyed by column name,
// skipping the column at index skip (the key column of an enumeration; -1 when
// there is none).
//
// id is the row's key rendered as text — an object identity's String() here, a
// bare subject key on the attribute seam — and it appears only in diagnostics.
//
// The map and every container inside it are allocated for this one row — a
// Provider retains nothing between rows — which is what keeps the transitively
// read-only Metadata contract true for concurrent holders.
func rowMetadata(cols []string, vals []any, skip int, id string) (provider.Metadata, error) {
	md := make(provider.Metadata, len(cols))
	for i, name := range cols {
		if i == skip {
			continue // the identity, not a field
		}
		// values.go owns the driver-value mapping table and builds its own coded
		// errors, so every rejection already names the column and this row's
		// identity.
		v, present, err := metadataValue(vals[i], name, id)
		if err != nil {
			return nil, err
		}
		if !present {
			continue // a NULL — or a JSON null — omits the field
		}
		md[name] = v
	}

	// The shared value model is the single authority on what a metadata value may
	// be; the loader never re-implements those rules. A violation fails the read
	// rather than reaching the expression evaluator.
	if err := provider.ValidateMetadata(md); err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
			"sqlprovider: row for %s rejected by the metadata value model", id)
	}
	return md, nil
}

// mergeContext copies base and overlays extra, so a shared diagnostic helper can
// add its own keys without mutating a map its caller reuses.
func mergeContext(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
