---
name: sql-provider
description: The SQL-backed ObjectProvider and AttributeProvider — the Querier seam and the two wiring paths (Go sqlprovider.New and a seed document's connections:/kind: sql block), the four casting rules a statement must obey (arrays to_jsonb, day-granular dates ::text, numeric ::float8 or ::text, identity composed in the id column), the un-catchable trap where SELECT tags yields a string that silently fails every membership predicate, the closed driver-value mapping table, what Fetch binds and what List/Query enumerate, filtering in Go rather than SQL, the connection defaults and pool-sharing rule, why BuildRegistry refuses a document declaring connections:, why nothing is pinged at build, the coded errors, the attribute seam (*Attributes and the attribute_providers: block, whose get_one binds a BARE subject id and whose optional get_all selects one), and the gated real-Postgres integration run.
applies_to: [library, cli, http, mcp]
---

# The SQL provider

`sqlprovider` implements `provider.ObjectProvider` over a relational database, so
a host serves its real objects to Aperture from the tables it already has instead
of exporting them to a CSV. It is a drop-in sibling of `csvprovider`: register a
`*Provider` under an object-type in a `provider.Registry` exactly as the CSV
loader is registered, and the Registry's cache, invalidation, and rules wiring
are unchanged.

Everything below is a property of code in `sqlprovider/` (the provider and its
value mapping) and `seed/connection.go` (the declarative wiring). This is a
**host data source**. It is unrelated to Aperture's own persistence in
`storage/` — hand-written SQL over `modernc.org/sqlite` — and shares no
connection handling with it. Provider data is pulled, read-only, and never
persisted as source of truth.

## Read this part first: cast it in the statement

**The statement is the only typing mechanism there is.** There is no per-column
type declaration to write in YAML, the way `csvprovider` writes `:int` in a
header. A column becomes a metadata field of whatever Go type `database/sql`
scanned it into, so the developer's `SELECT` list is where the type is decided.

Four rules, and a developer who ignores them gets silently wrong authorization
answers:

| # | Rule | Write |
|---|---|---|
| 1 | **An array must be cast to JSON.** It is the *only* way a list-valued field arrives as a list. | `to_jsonb(tags) AS tags` |
| 2 | **A day-granular date must be cast to text.** Every `time.Time` becomes the *datetime* form; granularity is not inferable. | `hired_on::text AS hired_on` |
| 3 | **A `numeric` must be cast.** `::float8` if it is a number, `::text` if it is an identifier. | `amount::float8 AS amount` / `sku::text AS sku` |
| 4 | **The identity is composed in the `id` column**, by the developer, in SQL. Aperture supplies no template. | `'brand:' \|\| b.id AS id` |

### The trap this package cannot catch for you

Selecting an array column **without** casting it compiles, runs, and is wrong:

```sql
SELECT tags FROM brands WHERE id = $1        -- WRONG
```

A Postgres `text[]` does not arrive as a list. It arrives as the raw array
literal — the six-character **string** `{a,b}` — which is a perfectly valid
metadata string, indistinguishable from a string a host meant to store. Nothing
in `sqlprovider` can tell the two apart, so nothing here will complain. What
happens instead is that **every membership predicate written against that field
silently matches nothing, forever**, and the rule reads as though it simply never
applies. No error, no note, no log line: a quiet deny.

```sql
SELECT to_jsonb(tags) AS tags FROM brands WHERE id = $1   -- RIGHT
```

> If a list-valued field is matching nothing, check its cast first.

This is measured behaviour, not a guess: both candidate Postgres drivers were
probed against a live server, one `SELECT` per type, printing `reflect.TypeOf` of
the scanned value. **No driver produces a `[]any` for a Postgres array.**

## The `Querier` seam

The dependency is a narrow interface, never a concrete `*sql.DB`:

```go
type Querier interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```

`database/sql`'s two read methods and nothing else. A `*sql.DB` satisfies it, and
so does an `*sql.Conn`, an `*sqlx.DB`, a pgx stdlib handle, a `*sql.Tx`, or a
host's own tracing, retrying, or read-replica wrapper. Aperture therefore owns no
connection lifecycle on the Go path: it does not open, close, ping, pool, or
configure anything. The host builds the handle it wants — driver, pool sizes,
TLS, observability — and hands it over.

That is also why **`sqlprovider` imports no driver**. Its dependencies are
`database/sql` plus `errors`, `identity`, and `provider`. A driver is a host
choice; linking one in from the library package would force every Aperture binary
to carry it. (The *seed* package does link one — see
[The driver, and what it costs](#the-driver-and-what-it-costs).)

Two obligations on an implementation:

- The context passed to either method **already carries this provider's
  timeout**. Honour it rather than starting unbounded work of your own.
- An error already carrying an `APERTURE_*` code **passes through verbatim**. A
  wrapping `Querier` that classifies its own failures keeps that classification;
  Aperture never re-stamps a coded error.

`QueryRowContext` is part of the seam but `Fetch` does not use it: `Fetch` must
be able to *see* a second row in order to reject it, and `QueryRowContext`
discards one.

## Wiring path 1: Go

```go
db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))  // the host's driver, the host's pool
if err != nil {
    return err
}
defer db.Close()

brands, err := sqlprovider.New(db, sqlprovider.Config{
    ObjectType: "brand",
    FetchQuery: `SELECT tier, seats, active, to_jsonb(tags) AS tags FROM brands WHERE id = $1`,
    ListQuery:  `SELECT 'brand:' || b.id AS id, b.tier, b.seats, b.active FROM brands b`,
    IDColumn:   "id",          // optional; DefaultIDColumn
    Timeout:    2 * time.Second, // optional; DefaultTimeout is 5s
})
if err != nil {
    return err
}
reg.MustRegister("brand", brands, provider.WithTTL(30*time.Second))
```

`Config` fields:

| Field | Required | Meaning |
|---|---|---|
| `ObjectType` | yes | the object-type this provider is registered under; every enumerated row's identity is checked against it |
| `FetchQuery` | yes | the "get one" statement, taking **exactly one** placeholder |
| `ListQuery` | yes | the "get all" statement, taking **no** parameters, selecting each row's full identity as `IDColumn` |
| `IDColumn` | no | the `ListQuery` result column holding the identity; empty means `DefaultIDColumn` (`"id"`) |
| `Timeout` | no | bounds **one** statement; zero means `DefaultTimeout` (5s); negative is a configuration error |

Unlike `csvprovider.New` — which defers to first use because the file it names
may not exist yet — `sqlprovider.New` **validates now**. A nil `Querier`, a blank
`ObjectType`/`FetchQuery`/`ListQuery`, or a negative `Timeout` is
`APERTURE_CONFIG_INVALID` at wiring time, because an access-control engine should
not learn about its own misconfiguration from a denied `Check`.

`ListQuery` is required alongside `FetchQuery`, not optional. An
`ObjectProvider` owes `List` and `Query`, and a provider that answered them with
an error would enumerate as "no objects" one layer up — which reads as **no
access**.

## Wiring path 2: YAML, with no Go code at all

A seed document declares its database connections in a top-level `connections:`
block and points `kind: sql` provider entries at them by name:

```yaml
connections:
  main:
    dsn_env: APP_DATABASE_URL     # required — the ONLY way to supply a DSN
    max_open_conns: 8             # optional
    max_idle_conns: 4             # optional
    conn_max_lifetime: 1h         # optional
    query_timeout: 3s             # optional

providers:
  - object_type: brand
    kind: sql
    connection: main
    get_one: SELECT tier, seats, to_jsonb(tags) AS tags FROM brands WHERE id = $1
    get_all: SELECT 'brand:' || b.id AS id, b.tier, b.seats FROM brands b
    ttl: "30s"
```

| Provider key | Becomes | Notes |
|---|---|---|
| `kind: sql` | selects this implementation | the other kind is `csv` |
| `connection` | the pool to read through | required; must name a declared `connections:` entry |
| `get_one` | `Config.FetchQuery` | required |
| `get_all` | `Config.ListQuery` | required |
| `id_column` | `Config.IDColumn` | optional, default `id` |
| `ttl` / `max_size` | the Registry's per-type cache options | as for any provider entry |

The two paths **meet at one point**: `seed.buildSQLProvider` resolves the
connection name to a `Querier` and then calls the *same* `sqlprovider.New` a host
calls. Neither path is a special case of the other, so every rule the constructor
enforces is enforced identically whichever way the provider was declared.

Like `providers:`, `objects:`, and `field_types:`, the `connections:` block is
runtime **wiring, not model state**: `Apply` writes nothing for it, and an export
never reproduces it.

### There is no `dsn:` key, and writing one is an error

A seed file is a committed artifact. A DSN carries a password, and a password in
version control is only ever noticed afterwards — so naming an environment
variable is not the *recommended* spelling here, it is the **only** one.

A literal `dsn:` is refused by `seed.Parse` with
`APERTURE_SQL_PROVIDER_DSN_LITERAL`, **before the document is usable for
anything**: not by `Apply`, not by an export round-trip, not by a tool that only
wanted to read the object types. The refusal names the offending connection; it
never names the value. (The `Connection.DSNLiteral` field exists solely so the
refusal can happen — a key silently dropped would look like it worked.)

Any DSN, and the password inside it, is also **redacted out of driver error
messages** these codes carry. Forbidding a literal `dsn:` to keep a password out
of committed text would be pointless if an error handed it straight back.

### One pool per named connection

The opener is called **once per declared connection**, never once per provider
entry. Three `kind: sql` entries over one database are three providers and
**one** pool — duplicate pools to a single server is exactly the failure this
indirection exists to prevent, and a test counts opener calls to prove it.

Every *declared* connection is opened, not only the referenced ones. A
`connections:` entry is a statement that this deployment has that database, and a
declared-but-unused name is far more likely a typo'd `connection:` on a provider
entry than a deliberate spare.

The **query timeout belongs to the connection**, not to the provider entry: it is
a property of the database being read, and three object-types over one pool that
disagreed about how long the server may take would be three opinions about one
server.

### The defaults

| Key | Default | Zero / negative | Why |
|---|---|---|---|
| `query_timeout` | **5s** (`sqlprovider.DefaultTimeout`) | must be **positive**; non-positive is an error | a fetch runs under `Check`, which owes a p99 under a millisecond. There is no "no timeout" setting: an unbounded statement under `Check` is an unbounded decision. |
| `max_open_conns` | **10** | negative restores `database/sql`'s unlimited | deliberately *not* `database/sql`'s own unlimited default: an unbounded pool lets a burst of `Check`s open connections until the host's server refuses them, and that failure lands on the host's application, not on Aperture. |
| `max_idle_conns` | **5** | zero or negative retains none | a provider's traffic is bursty — a wave of `Check`s, then nothing — so retaining half the pool avoids paying TLS and authentication again on every wave. |
| `conn_max_lifetime` | **30m** | `"0"` means reuse forever | a finite lifetime survives the classic deployment behind a connection proxy or a failover pair, where a connection kept forever eventually points at a server that is no longer the primary. |

`conn_max_lifetime` and `query_timeout` are Go durations (`"30m"`, `"500ms"`). An
unparseable one, a negative `conn_max_lifetime`, or a non-positive
`query_timeout` is `APERTURE_SQL_PROVIDER_CONNECTION` at build.

### `BuildRegistry` refuses a document declaring `connections:`

This is the one place a **valid** seed file fails the simple call, so it is worth
stating plainly:

```go
reg, err := doc.BuildRegistry(dir)                       // APERTURE_SQL_PROVIDER_CONNECTION
                                                          // if the document declares connections:

reg, conns, err := doc.BuildRegistryWithConnections(dir)  // the form that works
if err != nil {
    return err
}
defer conns.Close()
```

A pool has a **lifetime**, and the one-return signature cannot hand one back.
Rather than open pools nothing can close, `BuildRegistry` refuses the document
with a coded error naming `BuildRegistryWithConnections`. Every other document —
`csv` providers, inline `objects:`, no `connections:` block at all — still builds
through the one-return form unchanged.

`*seed.Connections` is the registry's other half. A `*provider.Registry` holds no
resources and needs no shutdown; the pools beneath a SQL-backed entry do, and
`Close` is where their lifetime ends. It is **always non-nil on success** and is
empty (`Close` a no-op) for a document declaring no connections, so a caller
defers unconditionally without asking which kinds the seed happened to use.
`Close` is idempotent, closes every pool even if an earlier one fails, and joins
the failures. `Names()` and `Len()` expose connection names and pool count — never
a DSN. Aperture's own CLI owns and closes them in `decisionStack.Close`.

`seed.WithConnectionOpener` replaces how a resolved `connections:` entry becomes a
live pool. It is what lets a whole registry be built from a YAML fixture, with no
database present — which is also how CI runs, since the pipeline has no service
containers.

### Nothing is pinged at build

`sql.Open` is lazy by design, and Aperture does **not** ping. A wrong host, a
wrong port, a wrong password, or a database that is simply down surfaces on **the
first decision that touches a SQL-backed object-type**, as
`APERTURE_SQL_PROVIDER_QUERY` — not at startup.

That is a deliberate trade. Making registry construction wait on a round-trip
would make every `aperture check` a network operation, including the ones that
never touch a SQL-backed type, and would stop a process from booting while the
host's database is still starting.

What **is** eager is everything Aperture can decide without dialling, because a
connection that only fails under a decision fails as a *denial*:

- the `dsn_env` variable must be set and non-empty (whitespace counts as empty);
- `conn_max_lifetime` and `query_timeout` must parse, and `query_timeout` must be
  positive;
- every provider entry's `connection:` must name a declared connection;
- both statements must be present, and the rest of `Config` must validate.

A connection or provider entry that fails takes the whole build with it, and
every pool opened so far is closed on the way out: a failed build strands
nothing.

## What `Fetch` binds

A fetch statement takes **exactly one** parameter, and Aperture binds the
identity's **terminal segment value** — not the identity string:

```
Fetch("brand:42")               →  QueryContext(stmt, "42")
Fetch("account:acme/brand:42")  →  QueryContext(stmt, "42")
```

That is what lets a developer write `WHERE b.id = $1` against the primary key
they already have, and hit its index. A full Aperture identity is rarely a column
in a host's schema, and a statement that had to reconstruct one per row could not
use an index at all.

**Placeholders are engine-native and passed through untouched.** Aperture never
rewrites `$1` to `?` or the reverse. Write the syntax your database speaks —
`$1` for Postgres, `?` for MySQL or SQLite — and it reaches the driver exactly as
written. There is no dialect layer to be surprised by.

**Parameters are always bound, never interpolated.** This is the SQL-injection
boundary of the whole feature and it is not configurable: an object id originates
outside the process, so a provider that pasted it into a statement would hand
every caller of `Check` arbitrary SQL execution. There is no API here that builds
a statement from a value.

Three outcomes, three different errors, deliberately:

| Rows | Result |
|---|---|
| **zero** | `APERTURE_NOT_FOUND` — the documented `ObjectProvider` contract, so the Registry can tell "absent" from an operational failure. The only one of the three that is a normal answer. |
| **exactly one** | the metadata |
| **more than one** | `APERTURE_SQL_PROVIDER_AMBIGUOUS` naming the identity. The first row is **not** silently taken. |

The ambiguity rule matters: without an `ORDER BY`, which row a database hands
back first is unspecified, so taking one would make an object's metadata — and
every decision computed from it — vary between two identical `Check`s. The usual
cause is a join that fans out; the fix is in the statement, not a `LIMIT 1` that
would only hide it.

`rows.Err()` is consulted **before** reporting no rows: a connection that died
mid-statement also reports "no next row", and calling that `NOT_FOUND` would turn
a broken database into a confident "this object does not exist" — and, one layer
up, into a deny that looks deliberate.

## What `List` and `Query` enumerate

Enumeration runs the **second** statement, `ListQuery`, and binds no parameters.
Every row is one object of this provider's type, and `IDColumn` (default `id`)
carries that object's **identity**, composed by the developer in SQL:

```sql
SELECT 'brand:' || b.id AS id, b.tier, b.seats FROM brands b
```

Aperture supplies no template for it, on purpose. An Aperture identity is
hierarchical and host-shaped — `brand:42` for one deployment,
`account:acme/brand:42` for another — and the prefix is exactly where a host
expresses its tenancy. A template baked into the provider would be a second place
tenancy is decided, disagreeing with the first.

The id column is the **identity, not a metadata field**: it is removed from the
row before the remaining columns become metadata. Every other column maps through
the same table `Fetch` uses.

### Project the same columns in both statements, or the cache stops warming

`FetchQuery` and `ListQuery` are independent, and nothing makes their SELECT lists
agree. That matters beyond tidiness, because `provider.Registry` warms its per-type
metadata cache from an enumeration — and every **read** of that cache is a `Fetch`,
the decision path's authoritative view of an object.

```yaml
get_one: SELECT tier, seats, renews_on FROM brands WHERE id = $1
get_all: SELECT 'brand:' || b.id AS id, b.tier FROM brands b   # NARROWER
```

Warming from that listing would cache a one-field bag under an id whose real bag has
three, for the whole of the type's TTL. A rule then reads `object.seats` as
**absent** — not wrong, absent — and every predicate over an absent field is false:
an inclusive grant **denies**, and an **exclusive** grant stops excluding and
therefore **widens**. Nothing in any verdict, trace or note says why, because a bag
of any shape is a legal bag. A listing **wider** than the fetch statement is the
mirror image: a field `Fetch` would never produce, comparing true until the entry
expires.

So a `*Provider` answers `provider.FetchCompleteLister`, and it **derives** the
answer from what the statements really returned:

```go
func (p *Provider) ListedMetadataMatchesFetch() bool
// false  until both statements have executed once
// true   when ListQuery's columns MINUS the id column == FetchQuery's columns, as a set
// false  otherwise — narrower, wider, or a differently named column either way
```

Columns, not values, is what makes that sound cheaply: `rows.Columns()` is the SELECT
list, identical for every row and unaffected by any row's NULLs, where comparing two
bags would not be — a NULL column omits its field and an omitted field is
indistinguishable from an unprojected one. There is deliberately **no** `Config`
field and no YAML key for it: an operator's unchecked promise about two statements is
exactly what this replaces.

Two things follow for the developer:

- **An unequal pair stays legal and stays correct.** A deliberately narrow display
  projection over a wide table is a reasonable thing to write. It costs a fetch per
  candidate per TTL window instead of one per enumeration, and the cost is silent —
  if one SQL-backed type's enumeration is slower than its sibling's, compare the two
  SELECT lists first.
- **Even an equal pair has one cold enumeration per process per type.** The first
  `List` runs before any `Fetch` has taught the fetch projection, so it cannot warm;
  its candidates each fetch, which is what teaches it, and every later enumeration
  warms as before.

The attribute seam has no equivalent, deliberately: `AttributeRegistry.Enumerate`
never writes the slot's cache at all (E3-S5), because it is an admin listing with no
`Fetch` behind it, where `Registry.List` is a decision-path call whose `Fetch`
follows in the same candidate walk.

### The id column takes text — and `[]byte` means raw text there

The id column accepts a `string`, or a `[]byte` read as **raw text**. That is
deliberately unlike a metadata column, where a `[]byte` is unconditionally JSON:
the id column is not metadata and has no competing JSON reading, and some drivers
hand back a text column as bytes. Anything else — a bare integer key included —
is rejected, because `42` is not an identity.

A row Aperture cannot place is an `APERTURE_SQL_PROVIDER_ROW_IDENTITY` error
naming the row's **position** in the result — never a row silently skipped:

- the result has no id column at all;
- the row's id is NULL, empty, or not textual;
- the id does not parse as an identity;
- the identity's **terminal segment type** is not this provider's object-type.

The last one matters most. A `brand:1` row returned by the statement wired under
the `dataset` object-type must never reach the cache: it would be cached under an
identity this provider's own `Fetch` could never return, so a later `Check` would
read one type's row as another type's object. Skipping such rows would be worse
still — enumeration would come back short, and short enumeration reads as "no
access".

### `Query` filters in Go, never in SQL

`Query` runs the **same** "get all" statement `List` runs and applies
`Filter.Fields` with `provider.MatchFields`, in Go. Predicates are **never**
templated into the developer's statement.

That is a correctness decision, not a convenience one. The `Fields` contract says
comparison is **typed** and never a string rendering — `"5"` does not equal `5`,
and `int64(5)` does equal `float64(5)` — because those are the rules engine's own
comparison semantics, and `Enumerate` must not select an object that `Check` then
denies over the same value. Postgres will happily coerce `'5'` to `5`, so a
predicate rendered into SQL would answer a different question than the rule
evaluated over the same field. Reproducing a collection field's **membership**
rule across `text[]`, `jsonb`, and a delimited string is the same hazard again.
Aperture does the comparison once, in one place, for every provider. See
`metadata-values` for the contract itself.

The cost is honest: the whole object-type is materialised per enumeration, and
the Registry's per-type TTL cache is what absorbs it. A very large table will
hurt; that is a known, accepted trade for not having two comparison semantics.

### Streaming, and what a `Limit` hides

Rows are consumed one at a time. `Filter.Pattern` is applied first, then the
field predicates, then the limit, and the loop **stops reading** once the limit
is reached. Two consequences a developer should know:

- **A `Limit` that truncates before a malformed row is reached will not surface
  that row's error.** The same enumeration with a larger limit — or with none —
  can fail where the bounded one succeeded. A row's error is a property of the
  rows actually read, not of the table.
- A row excluded by `Filter.Pattern` never has its metadata mapped, so a bad
  column on a pattern-excluded row is likewise not reported. Its *identity* still
  is: `rowIdentity` runs on every row scanned.

`rows.Err()` is checked after the loop, unconditionally. A connection that dies
half way through a result set ends the loop exactly as a complete one does, so
skipping that check would report a **truncated enumeration as a successful one**
— and a short enumeration is, one layer up, an access the caller silently does
not have.

## Columns become metadata

Every column the statement returns becomes a metadata field keyed by its **column
name**, so the `SELECT` list is the field list:

```sql
SELECT tier, seats, active FROM brands WHERE id = $1
-- → {"tier": "gold", "seats": int64(5), "active": true}
```

A column therefore needs a name. An unnamed expression (`SELECT count(*)`) or two
columns with the same name (a `SELECT b.*, o.*` over two tables that both have
`name`) is `APERTURE_SQL_PROVIDER_SCAN` naming the column — not a field silently
dropped, and not last-wins, which would make a field's value depend on the
`SELECT` list's order.

A **NULL column omits its field** rather than storing `nil`. That is
`csvprovider`'s absent-vs-zero rule applied to SQL: an absent field never matches
a `Filter.Fields` predicate and lets a rule supply its own default, whereas a
stored `nil` is a value that *compares* — which is how a NULL `end_date` silently
satisfies every "before" rule ever written against it.

### The driver-value mapping table

What a column becomes is decided by the Go type `database/sql` scanned it into,
and that mapping is a **closed table** — an inference would be a value the
expression evaluator silently mis-compares:

| Scanned Go type | Metadata value |
|---|---|
| `nil` (SQL NULL) | the field is **omitted** (absent is not zero) |
| `bool` | the scalar, as-is |
| `int64` | the scalar, as-is |
| `float64` | the scalar, as-is |
| `string` | the scalar, as-is |
| `[]byte` | **JSON-decoded** — this is how arrays and nested objects arrive |
| `time.Time` | `.UTC()`, then the canonical `2006-01-02T15:04:05Z` |
| anything else | `APERTURE_SQL_PROVIDER_SCAN` naming the column, the row's identity, the offending Go type, and the types that *are* mapped |

The table lives in `metadataValue`'s type switch in `sqlprovider/values.go`, is
named in `mappedDriverTypes`, and `TestDriverValueMappingTableMatchesTheTypeSwitch`
parses the file with `go/ast` and fails if the two disagree. Adding a case
without adding it to the table (or the reverse) is a build-red change, on
purpose.

Three consequences a developer has to act on:

- **A `[]byte` IS JSON, unconditionally.** It does not fall back to a string. A
  fallback would let one column change *type* depending on its contents — a
  `numeric` of `1.50` decoding as the number `1.5` while a `uuid` stayed a
  string. A decode failure is a **hard error** naming the column and the row.
  A JSON `null` omits its field, exactly as a SQL NULL does.
- **`bytea` no longer works at all.** It arrives as `[]byte`, is decoded as JSON,
  and fails. Encode it in the statement (`encode(bytes,'base64')`) or leave it
  out of the `SELECT` list — an access-control decision has no business reading a
  blob.
- **A `time.Time` is always a timestamp, never a day.** A database `date` and a
  database `timestamp` are the same Go type, so granularity cannot be inferred;
  the datetime form is the one that loses nothing. Write `col::text` for a day.
  Conversion to UTC happens **first**, because a `timestamptz` comes back in the
  *process's* local zone — measured, in both drivers — so the same row read on two
  hosts would otherwise yield two different strings and, near midnight, two
  different calendar days. The value is then routed through
  `provider.ParseDateValue`, so this loader accepts exactly what every other
  loader accepts.

Numbers inside a decoded JSON column normalise **exactly as the scalar columns
do**: an exact integer that fits `int64` becomes an `int64`, everything else a
`float64`, at every depth. That is what keeps `object.limits.seats ==
object.seats` from being a silent `false` — the evaluator does no numeric
coercion. It is `csvprovider`'s rule, deliberately identical, so a host that
migrates a CSV to a table gets the same decisions.

Every mapped row is then checked against the shared value model
(`provider.ValidateMetadata`), so a shape the expression evaluator cannot handle
fails the fetch as `APERTURE_METADATA_INVALID` instead of surfacing as an
evaluation error on the `Check` hot path.

## The attribute seam: `*Attributes`

The same package serves the **attribute** seam — one slot's directory instead of
one object-type's table — through `sqlprovider.NewAttributes(q, AttributeConfig{…})`,
registered in a `provider.AttributeRegistry` rather than a `provider.Registry`:

```go
users, err := sqlprovider.NewAttributes(db, sqlprovider.AttributeConfig{
	Slot:       provider.AttributeSlotUser,
	FetchQuery: `SELECT department, clearance FROM users WHERE id = $1`,
	ListQuery:  `SELECT u.id AS id, u.department, u.clearance FROM users u`,
})
if err != nil { return err }
attrs.MustRegister(provider.AttributeSlotUser, users, provider.WithTTL(60*time.Second))
```

```yaml
attribute_providers:            # the declarative path — NOT a providers: entry
  - subject: user
    kind: sql
    connection: main            # the SAME pool set object providers read through
    get_one: SELECT department, clearance FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
    ttl: 60s
```

**Everything above is true here word for word**: the `Querier` seam, engine-native
placeholders, bound-never-interpolated parameters, columns becoming metadata, the
driver-value mapping table, the four casting rules, the timeout, the read-only
`Metadata` contract, filtering in Go. `metadataValue` is literally the same
function, so a column read through an object provider and the same column read
through an attribute provider produce the same Go value and therefore the same
decision.

Three things differ, and all three are about what a **key** is.

**1. `FetchQuery` binds the BARE subject id, verbatim.** An object provider binds
the identity's terminal segment value, so `brand:42` and `account:acme/brand:42`
both bind `42`. An attribute key has no segments to strip — it is a principal id
or an account id, an opaque handle into the host's directory — so it is bound
exactly as it arrived: `Fetch(ctx, "alice")` → `QueryContext(stmt, "alice")`.

**2. `ListQuery` selects a BARE id — and this is casting rule 4's counterpart,
with no error attached.**

```sql
SELECT u.id AS id, u.department FROM users u            -- CORRECT
SELECT 'user:' || u.id AS id, u.department FROM users u -- WRONG, and SILENT
```

An identity-shaped key is a perfectly legal opaque string. It enumerates happily,
caches happily, and then matches **no principal id any `Fetch` ever presents**,
because the decision path fetches by the bare id. The slot simply never answers
and nothing anywhere complains. There is no check this package could add — the
key is opaque, so there is nothing to test it against — which is exactly why the
asymmetry is written down in the `sqlprovider/attributes.go` file doc, in
`csvprovider`'s `Attributes`, and on `seed.AttributeProvider.GetAll`. All three
move together.

**3. `ListQuery` is OPTIONAL, where `Config.ListQuery` is required.**
`Config.ListQuery` is required because an `ObjectProvider` that can be fetched
from but not enumerated answers `List` with an error, and an errored enumeration
reads as "no access" one layer up — a denial caused by a wiring gap. Attribute
enumeration never participates in scope resolution (a `*provider.AttributeRegistry`
structurally cannot be a `scope.ObjectLister`), so an empty `ListQuery` yields a
**fetch-only** provider: every decision path works unchanged and only `List` /
`Query` refuse, with `APERTURE_CONFIG_INVALID` naming the statement to declare.
That is a feature — a host can serve the principal currently being decided about
without exposing its whole user table to an admin enumeration.

`AttributeConfig` otherwise mirrors `Config`: `Slot` replaces `ObjectType` (it is
not load-bearing per row — an attribute key is opaque — but it is required, since
it is what a runtime diagnostic names when one of three directories is down),
`IDColumn` defaults to `"id"` and is removed from the row before the rest becomes
the bag, and `Timeout` defaults to `DefaultTimeout`. The timeout matters *more*
here: an attribute bag is read by every rule against every object in a decision,
so an unbounded statement is not one object type answering slowly, it is the whole
decision hanging.

A diagnostic names the **slot**, the **column**, the row's **position**, and the
**key** — never a value, for the reason the object seam never names one. Errors
are the same set (`APERTURE_SQL_PROVIDER_QUERY` / `_SCAN` / `_AMBIGUOUS` /
`_ROW_IDENTITY`), plus `APERTURE_NOT_FOUND` for a key the directory does not know
— which the decision path treats **leniently**, so a subject with no row decides
against the engine's floor bag rather than failing. See
`skills/attribute-providers.md` for the leniency contract and the slot set.

## Timeouts, concurrency, and the read-only contract

Every statement runs under `context.WithTimeout`, `DefaultTimeout` (5s) unless
configured otherwise. The deadline applies **on top of** whatever the caller's
context already carries, so the earlier of the two wins and a caller can always
be stricter than the provider.

A `*Provider`'s **configuration** is immutable after `New` — a `Querier`, two
statements, an id column and a timeout, none of which ever change. The one thing that
does change is the pair of column projections it has observed (above), guarded by the
provider's own mutex and written idempotently: the same statement has the same columns
every time, so the lock is about publication rather than contention, and
`ListedMetadataMatchesFetch` is asked once per enumeration rather than once per row.
A `*Provider` is therefore safe for concurrent use, as the `ObjectProvider` contract
requires. Concurrency beneath it is the `Querier`'s business; a `*sql.DB` is itself
concurrency-safe.

Every returned map is **freshly allocated for that one call**, with fresh nested
containers from the JSON decoder, and no container is shared with another call or
retained by the provider. That is what keeps `provider.Metadata` read-only
*transitively*, which the Registry's by-reference cache depends on.

## The coded errors

| Code | Raised when |
|---|---|
| `APERTURE_SQL_PROVIDER_QUERY` | the statement failed at the driver — connection, permission, syntax, timeout. Wraps the cause; reachable with `errors.Is` / `errors.As`. This is also where a wrong host or password surfaces, on the first decision. |
| `APERTURE_SQL_PROVIDER_AMBIGUOUS` | a fetch statement returned more than one row for one identity |
| `APERTURE_SQL_PROVIDER_SCAN` | a result column has no name, two columns share a name, a `[]byte` is not valid JSON, a JSON number is unrepresentable, a `time.Time` is outside the canonical range, or a driver value's Go type is not in the mapping table |
| `APERTURE_SQL_PROVIDER_ROW_IDENTITY` | an enumerated row's id column is missing, NULL, empty, non-textual, unparseable, or of the wrong object-type |
| `APERTURE_SQL_PROVIDER_DSN_LITERAL` | a seed document's `connections:` entry carries a literal `dsn:` |
| `APERTURE_SQL_PROVIDER_CONNECTION` | a declared connection cannot be resolved into a pool: unset `dsn_env`, an invalid duration, a non-positive `query_timeout`, an undeclared `connection:` name, an opener failure, a close failure — or `BuildRegistry` being called on a document that declares `connections:` |
| `APERTURE_CONFIG_INVALID` | a wiring mistake `New` catches: nil `Querier`, missing `ObjectType`/`FetchQuery`/`ListQuery`, negative `Timeout`, a `kind: sql` entry with no `connection:` |
| `APERTURE_NOT_FOUND` | a fetch matched zero rows — the normal answer, not a failure |
| `APERTURE_METADATA_INVALID` | a mapped row violates the shared value model |

Every diagnostic names the developer's own inputs — the column, the object-type,
the row's position, the identity — and **never a row value**. Metadata is host
data, frequently another account's, and an error is a thing that gets logged.
See the cross-account leak rule in `CLAUDE.md`.

## The driver, and what it costs

`sqlprovider` links no driver. The **seed** package does, in exactly one place:
a blank import of `github.com/jackc/pgx/v5/stdlib` in `seed/connection.go`, used
through `database/sql`. Keeping it in one named file makes "which drivers does
this binary link?" a one-line grep. Postgres only, by decision: one engine means
one dialect to document, one placeholder syntax in the statements a seed carries,
and one driver's value mapping to keep the table above honest about.

**pgx over `lib/pq` is a correctness argument, not a performance one, and it was
made knowingly with the price in hand.** Measured, with the project's own
`CGO_ENABLED=0 -ldflags="-s -w" -trimpath` flags: adding `lib/pq` alone to the
pre-epic binary cost **+96,432 bytes (+0.34%)**; adding `pgx/v5/stdlib` alone
cost **+3,589,088 bytes (+12.5%)**. The epic as landed — driver, provider, and
wiring together — took the stripped binary from **28,621,090 to 32,867,698
bytes**, a delta of **+4,246,608 bytes (+14.8%)**.

Roughly 37× the size for the same job, paid because `lib/pq` hands back `[]byte`
for `numeric` and `uuid`, which the value model cannot tell apart from `jsonb`.
Under the "a `[]byte` is JSON" rule, a `lib/pq` `numeric` of `1.50` would arrive
as the **float `1.5`** while a `uuid` stayed a string — an accidental, silent
type change. In an authorization engine, where `"5" != 5` is load-bearing, a
silent type change is not worth 3.3 MiB. Both drivers are pure Go; neither breaks
`CGO_ENABLED=0`.

If more engines land later, each is a fixed cost on every binary — including the
binaries of hosts that use none of them. That argues for build tags or a separate
module per engine if the family grows past two.

## Testing: the always-on suite and the gated one

`make test` passes with **no database present**. CI runs on ubuntu with no
service containers, so the SQL provider's unit tests run against a hand-rolled
fake `Querier` returning canned `driver.Value`s, and the seed wiring's tests run
against a hand-rolled `database/sql` driver behind `WithConnectionOpener`. That
works because the mapping decisions are about **Go types**, not wire behaviour —
a fake can produce every case in the table.

One test talks to a real Postgres, and it is gated, mirroring the
`APERTURE_BENCH_ASSERT=1` convention the bench NFRs use:

```bash
make bench                                                     # informational
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/     # the hard NFR assertion

APERTURE_PG_INTEGRATION=1 \
APERTURE_PG_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable' \
go test -run TestPostgresIntegration ./seed/                   # the real-Postgres run
```

Ungated it **skips**. Gated with an empty or missing `APERTURE_PG_DSN` it
**fails** rather than skipping — asking for the integration run and silently not
getting one is the outcome a gate must never produce. It creates and drops its
own table, so point it at a scratch database. **Never put a DSN in a file**; pass
it in the environment.

It exists because a fake cannot prove the two things only the real driver can:
that pgx is linked into a `CGO_ENABLED=0` binary and actually connects, and that
a real Postgres result set lands in the value model the way the mapping table
claims.

`TestPostgresIntegration` covers **both** seams: `_SeedFileAloneServesRealObjects`
for `providers:`, and `_AttributeProviderServesASlot` for `attribute_providers:`
— the latter creating a real `users` table, serving a slot from it through the
same pool, and reading a bag back through `Fetch` and `Enumerate`.

## Update-Demand

| Change | Also update |
|---|---|
| The driver-value mapping (`metadataValue`'s type switch or `mappedDriverTypes` in `sqlprovider/values.go`) | the table above, the `sqlprovider` package doc, `skills/metadata-values.md`, and `docs/src/concepts/providers.md` — gated by `TestDriverValueMappingTableMatchesTheTypeSwitch` |
| The casting rules or the un-catchable trap | "Read this part first" above, the package doc, `docs/src/concepts/providers.md`, and the `APERTURE_SQL_PROVIDER_SCAN` fixups in `errors/codes.go` |
| The seed `connections:` schema, `kind: sql` keys, or a default (`DefaultQueryTimeout` / `DefaultMaxOpenConns` / `DefaultMaxIdleConns` / `DefaultConnMaxLifetime` in `seed/connection.go`) | the defaults table above, `docs/src/concepts/seed.md`, and the `Connection` / `Provider` doc comments in `seed/` |
| `BuildRegistry` / `BuildRegistryWithConnections` behaviour, or `Connections`' lifetime | "BuildRegistry refuses…" above, `docs/src/concepts/seed.md`, and the doc comments in `seed/provider.go` |
| A `sqlprovider` or connection error code | `errors/codes.go` (`AllCodes` + `Registry` with a Message and Fixups), the error table above, then `make docs-gen` |
| The `Querier` seam | the interface's doc comment, "The Querier seam" above, and `docs/src/concepts/providers.md` |
| What licenses an enumeration to warm the Registry's cache (`provider.FetchCompleteLister`, `Provider.ListedMetadataMatchesFetch`, or where the two projections are observed) | "Project the same columns in both statements" above, `typeEntry.warmsFromListing` in `provider/registry.go`, the `sqlprovider` package doc ("Warming the Registry's cache"), `docs/src/concepts/providers.md` ("The listing and the fetch must be the same bag"), and "The object registry had the same bug" in `skills/attribute-providers.md` — behaviour by `provider/list_projection_test.go`, `sqlprovider/list_projection_test.go` and `seed/provider_sql_projection_test.go`; the NFR half is `bench.TestCheckNFREnumerateBound` |
| The attribute statement contract (`AttributeConfig`, what `Fetch` binds, the bare id in `ListQuery`, `ListQuery` staying optional) | "The attribute seam" above, the `sqlprovider/attributes.go` file doc, `seed.AttributeProvider.GetOne`/`GetAll`, `skills/attribute-providers.md`, `skills/metadata-values.md`, `docs/src/concepts/providers.md` ("Attribute providers"), and `docs/src/concepts/seed.md` ("External attribute sources") — the bare-id contract has **no gate and cannot have one**, so every restatement of it moves together |
| The gated integration run's env vars | this doc, the `Makefile` comment, and the "Gated, NOT in `make test`" list in `CLAUDE.md` |
