# Seed & portability

The `seed` package loads a **declarative authorization model** — a single JSON or
YAML document — into a `model.Storage`, and exports one back out. It is both the
human-authored on-ramp behind the `aperture check` / `aperture serve` demo and the
full round-trip state file the model portability endpoints use.

## One document, both directions

A `Document` is a flat list of each entity kind. The field tags cover **both** YAML
and JSON, so either format decodes into the same shape:

```yaml
accounts:      [{ id: acme, name: Acme }]
memberships:   [{ principal: alice, account: acme }]
object_types:  [{ name: document, actions: [read, write] }]
permissions:   [{ id: doc.read, object_type: document, action: read, scope_strategy: implicit }]
principals:    [{ id: alice, kind: user, roles: [reader] }]
roles:         [{ id: reader, name: Reader, permissions: [doc.read] }]
groups:        [ ... ]
grants:        [{ id: g1, account: acme, subject: { kind: principal, id: alice }, permission: doc.read, object: "account:acme/document:*", effect: allow }]
templates:     [ ... ]
rules:         [ ... ]
connections:   { ... }   # named database connections — runtime wiring (see below)
providers:     [ ... ]   # runtime wiring, not model state (see below)
objects:       [ ... ]   # inline object metadata — also wiring, not model state
field_types:   [ ... ]   # declared types for inline metadata fields — also wiring
attributes:    [ ... ]   # inline SUBJECT attributes (principal / account) — also wiring
attribute_providers: [ ... ]  # EXTERNAL sources for those same bags — also wiring
```

`connections:` is a **map** keyed by name, not a list: the name is what a
provider entry's `connection:` refers to, and a map cannot declare one twice.

Every field mirrors its `model` counterpart in declarative form. The `Document`
started as a minimal seed shape and was generalized to the complete model, so **an
export file is a strict superset of a seed file**: a seed that omits
`templates`/`rules`/`providers`/`objects`/`field_types`/`attributes`/`attribute_providers`
loads unchanged, and a full export reloads through the very same path. The field
set is additive-only, so old seeds keep loading.

Rule ASTs are carried as **raw JSON** — exactly the `rules` package's canonical
`Node` serialization — so the file never invents a second rule format; it is the
same shape the node editor reads and writes.

## Loading (import)

```go
// From bytes, explicit format:
err := seed.Load(ctx, store, data, seed.FormatYAML)

// From a file — format inferred from the extension (.json ⇒ JSON, else YAML):
err := seed.LoadFile(ctx, store, "model.yaml")
```

`Parse` decodes the document; `Apply` upserts it into the store in **dependency
order**: accounts, object types, permissions, principals, memberships, roles,
groups, grants, templates, then rules. Each write goes through the storage layer's
own validation — a malformed entity surfaces the *same* coded error a programmatic
`Put` would (e.g. `APERTURE_ACTION_UNDECLARED` for a permission naming an
undeclared action). Rule ASTs are additionally validated against the rules
engine's contract before storing, so an import rejects a structurally broken rule
(`APERTURE_RULE_INVALID`) rather than persisting one the engine could never
compile.

> `Apply` is **not transactional** — a failure may leave a partial model. This is
> acceptable for the seed-and-demo use case. (The mutation API's bulk endpoints use
> `Storage.Atomic` when all-or-nothing is required.)

The YAML path routes through JSON internally (`yaml → generic → json → Document`)
so the raw-JSON rule AST decodes by exactly the same rules the JSON path uses.

### The committed example

`seed.Example` is the embedded `org → project → document` fixture stamped to
account `acme` (`seed.ExampleAccount`). It is what `aperture check` loads when no
`--seed` file is supplied, and it backs the end-to-end test.

## Exporting

`Export(ctx, store)` reads the **complete** model back out into a `Document`, and
`Marshal(doc, format)` renders it to on-disk bytes:

```go
doc, _ := seed.Export(ctx, store)
out, _ := seed.Marshal(doc, seed.FormatJSON)
```

Export captures every source-of-truth entity: accounts, memberships, object types,
permissions, principals, roles, groups, grants, templates, and rule ASTs. Two
properties make a round-trip trustworthy:

- **Byte-stable.** Every slice is emitted in a stable order (sorted by id, name,
  or natural key) and each rule AST is re-serialized to the rules package's
  canonical form, so a re-export of an unchanged model is byte-identical and
  human-diffable.
- **Wildcard edges are preserved.** Memberships and grants stamped to the wildcard
  account `*` (the cross-account super-admin reach) are not among the real
  accounts, so `Export` queries `*` explicitly — omitting it would silently drop a
  super-admin's reach on export/import.

## Inline object metadata

For a small or fixed object set, `objects:` declares metadata **in the seed file
itself**, with no separate CSV beside it. YAML nests natively, so this is the most
direct way to author the arrays and nested objects the
[value model](providers.md#the-metadata-value-model) admits:

```yaml
objects:
  - id: account:acme/brand:1
    metadata:
      tier: gold
      seats: 5
      tags: [premium, launch]
      owner:
        dept: eng
        lead: alice
  - id: account:acme/brand:2      # metadata: may be omitted entirely
  - id: account:acme/app:be
    metadata: { tier: gold }
```

The object-type is **derived from the identity's terminal segment** —
`account:acme/brand:1` is a `brand` — never declared separately, so one fact in
one place cannot disagree with itself. Entries of different types may be
interleaved freely; `BuildRegistry` groups them and registers one in-memory
[`provider.Static`](providers.md#in-memory-objects-providerstatic) per type, with
a TTL of 0 (the data cannot go stale, because nothing can change it). Within a
type, **declaration order is preserved** — it is the order `List` and `Query`
return.

The provider it builds is a full one: `Fetch` (with `APERTURE_NOT_FOUND` for an
undeclared id), `List`, and `Query` honouring `Pattern`, `Limit`, and `Fields` on
the [same contract](providers.md#the-filterfields-contract) every other provider
implements — a collection field matches by membership, everything else by typed
equality.

Values are validated against the shared value model **when the document is
built**, before any `Check` can see them, and numbers are normalised exactly as
every other loader normalises them: an exact integer that fits `int64` becomes an
`int64`, anything else a `float64`. That is what keeps `object.seats == 5` from
answering differently depending on whether the object came from a seed file, a
JSON seed file, or a CSV `:int` column.

A missing or duplicate `id`, `metadata` that is not a mapping, or a value the
value model rejects is `APERTURE_CONFIG_INVALID` naming the object id and the
field (with the inner `APERTURE_METADATA_INVALID` kept in the chain); a malformed
id is `APERTURE_IDENTITY_INVALID`. Ids are deduplicated across the **whole**
section, not per type — the same id declared twice is the same object declared
twice, however it is spelled.

### Declared field types

A CSV header can say `hired_at:date`; a YAML mapping has nowhere to put that, so
`hired_at: 2026-02-30` in an `objects:` entry loads happily as an ordinary string
and only shows up months later as a rule that silently never matches. The
optional `field_types:` section is the missing declaration:

```yaml
field_types:
  - object_type: brand
    fields:
      hired_at: date
      last_seen: datetime

objects:
  - id: account:acme/brand:1
    metadata:
      hired_at: "2026-03-04"
      last_seen: "2026-03-04T12:30:00Z"
```

Each entry names an `object_type` — matched against the identity's terminal
segment, exactly as `providers:` is — and maps field names to a type. The
vocabulary is exactly two words, `date` and `datetime`, **lower-case and exact**:
the CSV loader's column-suffix spelling with the colon removed, so the same two
words mean the same two things in both loaders. `timestamp`, `Date`, and `time`
are `APERTURE_CONFIG_INVALID`, never a silently ignored declaration. Each object
type may be declared at most once.

> **Quote date values in YAML.** YAML resolves an *unquoted* calendar day as a
> `!!timestamp`, and the loader's YAML path normalises through JSON, so
> `hired_at: 2026-03-04` arrives already widened to `2026-03-04T00:00:00Z` and a
> field declared `date` rejects it. The widening happens inside the YAML decoder,
> before the seed package sees the document, so it cannot be undone — the
> rejection names the fix when the instant is exactly midnight. A JSON seed has
> no such trap, and the two formats agree on every quoted value.

Declared values are validated and canonicalised **at `BuildRegistry` time**,
through the same [`provider.ParseDateValue`](providers.md#dates-are-string-scalars-in-two-canonical-forms)
the CSV loader uses — this document never re-implements what a date is. The
**canonical** text is what is stored, so `2026-03-04T01:02:03.456Z` and
`2026-03-04T01:02:03` both become `2026-03-04T01:02:03Z` and two objects naming
one instant hold one string, which is what makes a `Filter.Fields` equality
predicate over the field mean anything. The declared type also **fixes the
granularity**: a `date` field rejects a timestamp and a `datetime` field rejects
a bare day, rather than quietly widening the day to midnight.

Four properties are worth stating because each is the first question a reader
asks:

- **A declared field is not a required field.** The section declares a *type*,
  not a requirement: an object that omits the field is perfectly valid. An
  explicitly **empty** value omits the field rather than storing `""` — the same
  rule an empty CSV cell follows, because an absent date differs meaningfully
  from any date and a zero time would silently satisfy every `before` rule ever
  written against the field.
- **Declaring a type for an object type with no `objects:` entries is legal.**
  The entries may be arriving, or the type may be served by a `providers:` entry.
  It registers nothing on its own; the declaration itself is still validated, so
  a typo fails the build rather than waiting for an entry to expose it.
- **It applies to `objects:` only, never to provider-loaded rows.** A `providers:`
  entry carries its own typing (the CSV `:date` / `:datetime` suffix). One type
  declaration living in two places could disagree with itself, which is exactly
  what this document's derive-the-type-from-the-identity rule avoids elsewhere.
- **It is not a general metadata schema.** No `required:`, no `default:`, no
  `enum:`, no `pattern:`, no `int`/`float`/`bool`, no nested field paths. The
  [value model](providers.md#the-metadata-value-model) already governs shape,
  depth, and size; the one thing it cannot govern is which strings a host means
  as dates, because nothing in a string says so.

A rejection is `APERTURE_CONFIG_INVALID` naming the **object id and the field**
and carrying the `provider.DateReason` for a parse failure (a granularity
mismatch is not a parse failure and carries no `reason`, so `DateReasonOf`
correctly reports none). It **never carries the value** — a date is frequently
personal data, and an error is a thing that gets logged.

Like `providers:` and `objects:`, this is runtime **wiring**: `Apply` writes no
row for it and an export never reproduces it.

### When both sections claim a type

`providers:` and `objects:` can each claim the same object type. **Precedence is
type-level and total:**

> If a `providers:` entry exists for an object type, the declared provider —
> `csv` or `sql` alike — wins and **every** inline `objects:` entry for that type
> is discarded.

There is **no object-level merge, no field-level merge, and no fallback**. An
inline id the provider's source happens to lack is simply not resolvable —
`Fetch` returns `APERTURE_NOT_FOUND` and enumeration never lists it, exactly as
if the entry had never been written. A field only the inline entry declared does
not appear on an object the source *does* carry.

That is deliberate. Field-level merging is the most useful-sounding behaviour and
the most impossible to debug: a rule reading a field the CSV or table silently
did not override is a support ticket nobody can reproduce. Predictability wins.

**This is the default. A collision builds, it does not fail.** Pointing a type at
a CSV or a table while its inline entries are still in the file is an ordinary
migration step, not an authoring fault — a seed that booted yesterday must not
refuse to boot today because someone added a `providers:` row.

The discard is not silent, though. `BuildRegistry` needs no logger to say so,
because the document can be asked directly:

```go
reg, err := doc.BuildRegistry(dir)
if err != nil {
    return err
}
if types := doc.ProviderCollisions(); len(types) > 0 {
    slog.Warn("seed: inline objects discarded, providers: entry wins",
        "object_types", types)
}
```

`ProviderCollisions` returns exactly the object types whose inline entries the
build discarded — sorted, deduplicated, and object **types** only, never object
ids (an id can embed an account, and this value is destined for a log line). It
reads the document alone — no file IO, no registry — so it answers the same
before and after a build, and a host surfaces it however it already surfaces
things. Nothing in `seed` picks a logger for you.

A host that would rather read the overlap as an authoring mistake — a checked-in
seed nobody is mid-migration on — opts into a refusal:

```go
reg, err := doc.BuildRegistry(dir, seed.StrictProviderCollision())
```

That returns `APERTURE_CONFIG_INVALID` naming every colliding object type
(sorted, in both the message and the error context — never the object ids). It is
Go wiring rather than a seed-file key on purpose: the file stays a plain
declaration of what exists, and the choice to make an ambiguous one fatal sits in
code, where a reviewer sees it.

**Validation is independent of precedence.** Every inline entry is checked —
`id`, identity, duplicates, the value model, and its
[declared field types](#declared-field-types) — *before* any type is discarded,
so a malformed declaration fails the load whether or not its type ultimately
loses to a `providers:` entry. Otherwise a document would silently stop being
validated the day someone added a CSV for one of its types. The canonicalised
values are then discarded along with the rest of those entries: a `field_types:`
declaration never reaches the rows a declared provider serves — a SQL provider's
column typing lives in its statement's casts, not here.

Two `providers:` entries for one type remain a duplicate registration,
`APERTURE_PROVIDER_INVALID` — that is a straight contradiction with no winner to
pick, not a precedence question.

## Database-backed providers

A `providers:` entry declares its `kind`. `csv` names a file resolved relative to
the seed file; **`sql`** names two statements run against a **connection**
declared in the document's top-level `connections:` block. Together they make a
database-backed deployment a no-Go-code experience:

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

| Provider key | Meaning |
|---|---|
| `connection` | the `connections:` entry to read through. Required for `kind: sql`; a name with no matching entry is a hard error at build. |
| `get_one` | the "get one" statement, taking **exactly one** placeholder, to which the identity's terminal segment value is bound |
| `get_all` | the "get all" statement, taking **no** parameters and selecting each row's full identity as the id column. Required alongside `get_one` — a provider that could be fetched from but not enumerated would answer `List` with an error, and an errored enumeration reads as "no access". |
| `id_column` | the `get_all` result column holding the identity. Default `id`. |
| `references` | declared object references: **field name → target object-type**. See [Declaring a reference](#declaring-a-reference). Works on any `kind`. |
| `ttl` / `max_size` | the per-type cache options, as for any provider entry |

The statements are the developer's own, and **the `SELECT` list is where a
column's type is decided** — there is no per-column type declaration here the way
a CSV header carries `:int`. Cast arrays with `to_jsonb(...)`, day-granular dates
with `::text`, and a `numeric` with `::float8` or `::text`; compose the identity
in the id column. Getting a cast wrong is not always an error — `SELECT tags`
yields the raw array literal as a **string**, and every membership predicate over
it then silently matches nothing. See
[the SQL provider](providers.md#worked-example-sqlprovider).

**Project the same columns in `get_one` and `get_all`** (the id column aside) unless
you mean not to. The two SELECT lists are what decide whether an enumeration may warm
the registry's per-type metadata cache, and an unequal pair silently gives that up —
correctly, because a bag from a narrower listing would make a rule read
`object.<dropped_field>` as absent, which denies an inclusive grant and stops an
exclusive one excluding. The pairing is compared from the columns the statements
really return, so there is no key here to declare it with. See
[The listing and the fetch must be the same bag](providers.md#the-listing-and-the-fetch-must-be-the-same-bag).

### There is no `dsn:` key

A seed file is a committed artifact, and a DSN carries a password. Naming an
environment variable is therefore not the recommended spelling — it is the only
one. A literal `dsn:` is refused by `Parse` with
`APERTURE_SQL_PROVIDER_DSN_LITERAL`, **before the document is usable for
anything**: not by `Apply`, not by an export round-trip, not by a tool that only
wanted to read the object types. The refusal names the offending connection and
never the value, and any DSN is redacted out of driver messages these errors
carry.

### One pool per named connection, and its defaults

The pool is opened **once per declared connection** and shared by every provider
entry naming it: three `kind: sql` entries over one database are three providers
and one pool. Every *declared* connection is opened, not only the referenced
ones — a declared-but-unused name is far more likely a typo'd `connection:` than
a deliberate spare. The **query timeout belongs to the connection**, since it is a
property of the database being read.

| Key | Default | Notes |
|---|---|---|
| `query_timeout` | `5s` | bounds **one** statement. Must be positive — there is no "no timeout" setting, because an unbounded statement under `Check` is an unbounded decision. |
| `max_open_conns` | `10` | deliberately *not* `database/sql`'s unlimited default; negative restores it. An unbounded pool lets a burst of `Check`s exhaust the host's server. |
| `max_idle_conns` | `5` | zero or negative retains none. Provider traffic is bursty, so keeping half the pool warm avoids re-paying TLS and auth on every wave. |
| `conn_max_lifetime` | `30m` | `"0"` means reuse forever. A finite lifetime is what survives a connection proxy or a failover pair. |

Durations are Go durations (`"30m"`, `"500ms"`). An unparseable one, a negative
`conn_max_lifetime`, or a non-positive `query_timeout` is
`APERTURE_SQL_PROVIDER_CONNECTION` at build.

### Pools have a lifetime, so the build signature changes

A document that declares `connections:` **cannot** be built through the
one-return `BuildRegistry` — a pool has a lifetime that signature cannot hand
back, so rather than open pools nothing can close, it refuses the document with
`APERTURE_SQL_PROVIDER_CONNECTION` naming the call that works:

```go
reg, conns, err := doc.BuildRegistryWithConnections(dir)
if err != nil {
    return err
}
defer conns.Close()
```

This is the one place a *valid* seed file fails the simple call. Every other
document — `csv` providers, inline `objects:`, no `connections:` block — still
builds through `BuildRegistry` unchanged.

`*seed.Connections` is the registry's other half: a `*provider.Registry` holds no
resources, the pools beneath a SQL-backed entry do. It is always non-nil on
success and empty (`Close` a no-op) for a document declaring no connections, so a
caller defers unconditionally without asking which kinds the seed used. `Close`
is idempotent and joins the failures of every pool it closes. Aperture's own CLI
owns and closes them for the duration of a command.

### Nothing is dialled at build

`sql.Open` is lazy and Aperture does **not** ping. A wrong host, port, password,
or a database that is simply down surfaces on the **first decision that touches a
SQL-backed object-type**, as `APERTURE_SQL_PROVIDER_QUERY` — not at startup.
Making registry construction wait on a round-trip would make every
`aperture check` a network operation, including the ones that touch no SQL-backed
type, and would stop a process from booting while the host's database is still
starting.

Everything Aperture *can* decide without dialling is eager, because a connection
that only fails under a decision fails as a **denial**: the `dsn_env` variable
must be set and non-empty, the durations must parse, `query_timeout` must be
positive, every `connection:` must name a declared connection, and both
statements must be present. A failure takes the whole build with it and closes
every pool opened so far, so a failed build strands nothing.

## Declaring a reference

Any `providers:` entry — `csv` or `sql` — may declare **object references**: a
mapping from a metadata **field name** this provider serves to the **target
object-type** its values identify. It is an application-level foreign key with no
database constraint behind it, so the document is where it is stated:

```yaml
providers:
  - object_type: dataset
    kind: sql
    connection: main
    get_one: SELECT d.tier, to_jsonb(d.brand_ids) AS current_brands FROM datasets d WHERE d.id = $1
    get_all: SELECT 'account:acme/dataset:' || d.id AS id,
                    to_jsonb(d.brand_ids) AS current_brands
             FROM datasets d
    references:
      current_brands: brand

  - object_type: brand
    kind: sql
    connection: main
    get_one: SELECT b.region FROM brands b WHERE b.id = $1
    get_all: SELECT 'account:acme/brand:' || b.id AS id, b.region FROM brands b
```

That declaration is what lets an enumeration be restricted to what a holder
object names — `aperture enumerate … --via account:acme/dataset:x.current_brands`,
"which brands belong to dataset x?".

Four rules govern the block, and each closes a door deliberately:

- **It is declared on the HOLDING side only** — the type whose provider actually
  returns the field. There is no inbound spelling on `brand`: `brand` has no
  column listing its datasets, so an inbound declaration would describe a derived
  view with nothing to attach to, and a second referencing field
  (`archived_brands: brand`) would make the unnamed reverse edge ambiguous.
- **It is a closed set of one descriptor kind.** A field maps to a target
  object-type and to **nothing else**. Do **not** add a `type:` key here: the
  loader is the single typing mechanism (a CSV column suffix, a cast in the
  developer's SQL), and a second place to declare a type is a second place for
  the two to disagree.
- **The field's values are full canonical identities** —
  `"account:acme/brand:1"`, composed by the developer where the data is loaded,
  scalar for one and a list for many. Spell them exactly as the object lister
  yields them; a bare `"brand:1"` in an account-scoped deployment passes the
  declaration check and then resolves to nothing.
- **The blocks are applied last, in a second pass**, once every type is
  registered — so a reference may name a target declared further down the file,
  or one served by the `objects:` section. Order in the file never matters.

A target no provider serves is `APERTURE_PROVIDER_REFERENCE_INVALID` at build,
naming the field and the target. A field name **no object happens to carry is not
an error at all**: metadata fields are discovered at fetch, not declared, so it
simply resolves to nothing.

Like every other `providers:` key, `references:` is runtime **wiring, not model
state**: `Apply` writes nothing for it and an export reproduces none of it. See
[Declared references](providers.md#declared-references) for what a declaration
buys and the security semantics of enumerating through one.

## Inline subject attributes

`objects:` says what Aperture knows about the thing being acted on. `attributes:`
says what it knows about the party **asking** — a principal's department, an
account's plan — so a small deployment can make `principal.department` mean
something with no directory, no CSV, and no Go:

```yaml
attributes:
  - subject: user
    id: alice
    metadata:
      department: eng
      clearance: 3
      teams: [platform, infra]
  - subject: machine
    id: ci-runner
    metadata: { department: eng }
  - subject: account
    id: acme
    metadata: { plan: enterprise }
```

`subject:` names one of the three **attribute slots** — `user` or `machine` for a
principal, `account` for the tenant a decision is made in. The set is closed: it
is the parties a decision has. It is spelled `subject:` rather than `kind:`
because `account` is a slot but **not** a principal kind, and a key called `kind:`
would read as `model.PrincipalKind`. An unknown value is
`APERTURE_ATTRIBUTE_SLOT_UNKNOWN` naming the entry and the three legal subjects.

`id:` is the bare attribute **key** — a principal id, or an account id — and
Aperture never parses it. There is no type to derive from it the way an `objects:`
id derives its object-type from its terminal segment: an attribute key is an
opaque handle into the host's directory with no segment structure, which is
exactly why the slot has to be declared. Keys are deduplicated **per slot**, not
across the section: a tenant called `acme` and a service principal called `acme`
are two unrelated subjects, while the same key twice under one subject would let
the last writer silently win.

`metadata:` is the ordinary [value model](providers.md#the-metadata-value-model),
validated at build and with numbers normalised exactly as `objects:` normalises
them — there is one value model in Aperture and an attribute bag is a value in it.
A rejected value keeps the model's own `APERTURE_METADATA_INVALID` (the code whose
fixups name the legal shapes and the two caps), with the offending entry added to
the message; a missing `subject:`/`id:`, a duplicate pair, or a `metadata:` that
is not a mapping is `APERTURE_CONFIG_INVALID`. The account wildcard `"*"` is never
a legal key — it would ask for the attributes of every account at once — and is
refused with `APERTURE_ATTRIBUTE_PROVIDER_INVALID`.

`Document.BuildAttributeRegistry(baseDir)` turns the block into a live
`*provider.AttributeRegistry`, registering one in-memory provider per declared
slot with a TTL of 0. It always returns a usable registry, so a host wires it
unconditionally:

```go
attrs, err := doc.BuildAttributeRegistry(filepath.Dir(seedPath))
if err != nil {
    return err
}
eng := rules.NewEngine(ruleSource, objectFetcher, rules.WithPrincipalResolver(attrs))
```

The kind picks the slot, and **a missing source is not a failed decision**: a
subject with no entry — or a slot with no provider at all — evaluates against the
floor bag (`{id, kind}`), so a rule reading an attribute nobody declared is
deny-safe rather than a non-decision. See
[Wiring a `*provider.AttributeRegistry`](rules.md#wiring-a-providerattributeregistry).

To back a slot with the host's real directory instead of an inline list, declare
it under [`attribute_providers:`](#external-attribute-sources) — and note that
when both sections claim one slot, the external entry wins it outright.

### Why it is not a `metadata:` field on `principals:`

`principals:` and `accounts:` are **model state** — rows `Apply` writes and
`Export` reads back. An attribute bag is not: it belongs to the host's directory,
and Aperture persists none of it (there is no column for it, by design). Hanging
the bag off a model entry would put wiring inside state, and the export would then
be one of two bad things — **lossy**, because it silently dropped the bags it
could not read out of storage, or **untruthful**, because it invented them. Its
own key keeps the line exactly where the other wiring sections already keep it.

## External attribute sources

`attributes:` lists bags inline. `attribute_providers:` points a slot at the
host's **real** directory instead — a CSV export, or the `users` and `accounts`
tables the deployment already has — one entry per slot:

```yaml
connections:
  main:
    dsn_env: APP_DATABASE_URL

attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department, clearance FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
    ttl: 60s
  - subject: machine
    kind: csv
    path: machines.csv
```

`subject:` names the slot exactly as it does on `attributes:` — `user`,
`machine`, or `account` — and each slot may be declared **at most once**.
`kind:` is the implementation, `csv` or `sql`; the two words are why the slot is
spelled `subject:` here rather than `kind:`. Everything Aperture can check
without reading a file or dialling a database is checked **at build**: the
subject must name a slot, the kind must be one this build knows, a `csv` entry
must carry a `path:`, a `sql` entry must carry a `connection:` naming a
**declared** `connections:` entry plus a `get_one:`, and a `ttl:` must parse. A
source that only failed under a decision would fail as a *denial*.

`ttl:` and `max_size:` are **per slot**, not per document: the three slots have
genuinely different change rates and cardinalities, and one number covering all
of them would tune for whichever entry was declared last. `ttl: "0"` never
expires. A slot's TTL is the window a **revoked** clearance keeps authorizing
for — see [`aperture attributes`](../cli/attributes.md), which reads it back and
can close it.

`dsn:` is refused **by name** wherever it appears, here as on a `providers:`
entry: credentials belong to a `connections:` entry's `dsn_env:`.

### `declared_keys:` — the keys a slot guarantees

An entry may declare the attribute keys it guarantees. The key is **optional**, and
omitting it is legal:

```yaml
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department, clearance FROM users WHERE id = $1
    declared_keys: [department, clearance]
```

Declaring opts that slot into **key enforcement**: a rule may then read only the
keys the set names on that slot. Declaring nothing opts out, and a slot with no
declared set behaves exactly as every slot did before the key existed.

That is what makes a local attribute layer safe. A slot holds a shared layer and a
local one, and the shared layer wins every key both serve, so the keys a local layer
adds on top are unreachable from any rule the deployment can validate — **inert**,
rather than a second answer to a deployment-wide grant. The declared set therefore
lives on the shared entry and nowhere else: a local layer able to narrow or widen it
would be one machine changing which keys a deployment-wide rule may name.

The set is a **plain list of names, with no per-key type information** — the simplest
form that round-trips, and the right one, because the [metadata value
model](providers.md) already governs shape and `field_types:` already governs the
declared date types. A second typing mechanism would be a second place for two
declarations about one key to disagree.

**There are three states, not two:**

| Written | State | Effect |
|---|---|---|
| `declared_keys:` absent (or `null`) | not declared | the slot is opted **out** of key enforcement |
| `declared_keys: []` | declared empty | the slot is opted **in** and permits **no** key |
| `declared_keys: [a, b]` | declared | permits `a` and `b` |

The middle row is the one a plain list of strings would lose, since nil is what both
an absent and an empty list decode to. So the distinction is carried explicitly at
every layer it crosses — the YAML field is a pointer, the stored row carries a
`Declared` bit of its own, and `aperture wiring show` prints all three as words
(`(not declared)`, `(declared empty)`, or the names). Collapsing declared-empty into
not-declared would silently **un-enforce** a slot.

Names are trimmed; an empty or repeated name is refused with
`APERTURE_CONFIG_INVALID` naming the slot and the key. The declaration order is
preserved, so `aperture wiring pull` reproduces the author's
list rather than a sorted paraphrase, and push → pull → push is a fixed point for a
slot that declares a set.

### The bare-id contract

An attribute key is a **bare** principal id or account id — an opaque handle into
the host's directory that Aperture never parses. An object id is a segmented
identity. The two statements therefore differ, in both directions:

```sql
-- providers:            an OBJECT provider selects the FULL IDENTITY
get_all: SELECT 'user:' || u.id AS id, u.department FROM users u   -- WRONG here
-- attribute_providers:  an ATTRIBUTE provider selects a BARE ID
get_all: SELECT u.id AS id, u.department FROM users u              -- CORRECT
```

`get_one:` differs the same way. An object provider binds the identity's
**terminal segment value** (`brand:42` and `account:acme/brand:42` both bind
`42`); an attribute provider binds the **bare subject id verbatim**, because
there is nothing to strip. The same applies to a `csv` entry's `id` column:
`alice`, not `user:alice`.

**Nothing can catch a mistake here.** An identity-shaped key is a perfectly legal
opaque string: it enumerates happily, caches happily, and then matches no
principal id any fetch ever presents, because the decision path fetches by the
bare id. The slot simply never answers, and nothing anywhere complains. There is
no check any package could add — the key is opaque, so there is nothing to test
it against — which is exactly why the asymmetry is written down here, on
`seed.AttributeProvider.GetAll`, and in both loaders' package docs. It is also
why `attribute_providers:` is a separate top-level key rather than a variant of
`providers:`: sharing one struct would make copying a statement between them a
silent fault.

### `get_all:` is optional, where an object provider's is required

A `providers:` entry must declare both statements, because an object provider
that can be fetched from but not enumerated answers `List` with an error, and an
errored enumeration reads as "no access" one layer up — a denial caused by a
wiring gap.

That reason does not apply to an attribute slot: attribute enumeration **never**
participates in scope resolution (see [Attribute
providers](providers.md#attribute-providers)). Omitting `get_all:` yields a
**fetch-only** slot — every decision path works unchanged, and only the
administrative listing refuses, with a coded error naming the statement to
declare. That is a feature: a host can let Aperture read the attributes of the
principal currently being decided about **without** exposing its whole user
table to an admin enumeration.

### Building it, and the shared pool set

```go
reg, conns, err := doc.BuildRegistryWithConnections(dir)   // object providers
if err != nil {
    return err
}
defer conns.Close()
attrs, err := doc.BuildAttributeRegistryWithConnections(dir, conns)
```

`BuildAttributeRegistryWithConnections` **takes** a `*Connections` rather than
opening one, and that is the point: `connections:` is the document's **single**
pool set, and the object registry and the attribute registry read through the
same pools. An entry point that opened its own would double every deployment's
connections and hand nothing back to close them. The plain
`BuildAttributeRegistry(baseDir)` passes no pools — correct for a document whose
attribute sources are all `csv` or inline — so a `kind: sql` entry fails there
with `APERTURE_SQL_PROVIDER_CONNECTION` naming the form to call, rather than
lazily on the first decision that needed the database.

Slots are filled in slot order (`user`, `machine`, `account`), not file order, so
a document with two bad slots always fails on the same one.

### Precedence: the external source wins, entirely

When both sections declare the same slot, the `attribute_providers:` entry
**wins and every inline `attributes:` entry for that slot is discarded
entirely**. There is no per-subject merge and no fallback: an inline id the
external source happens to lack is simply not resolvable, exactly as if the entry
had never been written. It is the [`providers:` / `objects:`
rule](#when-both-sections-claim-a-type) at slot granularity, and for the same
reason — field-level merging is the most useful-sounding behaviour and the most
impossible to debug, because a rule reading a department the directory silently
did not override is a support ticket nobody can reproduce.

The discard is **not silent**. `Document.AttributeCollisions()` reports the
affected slots and the caller surfaces them (`aperture` prints a warning). Only
slot **names** are reported, never keys, so the warning cannot leak a directory's
contents. `Document.AttributeSlotSources()` reports where each slot's bags come
from — `"csv"`, `"sql"`, or `"inline"` — so a surface that displays the wiring
reads the precedence rule instead of re-deriving it and eventually disagreeing
with it.

## What is *not* in the file

Two things are deliberately excluded from the model state file:

- **Live host domain-object metadata** — that is the [provider](providers.md)
  cache: derived, disposable, never source of truth. Because `Export` reads storage
  back, and a provider produces no model rows, it is never reproduced.
- **Live subject attributes** — a principal's or an account's bag is the host
  directory's, for the same reason and with the same consequence: `Apply` writes
  no row for `attributes:` or `attribute_providers:` and an export reproduces
  none of either.
- **Runtime *wiring*** — the `connections:`, `providers:`, `objects:`,
  `field_types:`, `attributes:` and `attribute_providers:` sections are runtime
  wiring, not model state. `Apply` never writes any of them to storage; instead
  `Document.BuildRegistry(baseDir)` — or `BuildRegistryWithConnections` when the
  document declares `connections:` — turns the first four into a live
  `*provider.Registry`, and `Document.BuildAttributeRegistry(baseDir)` — or
  `BuildAttributeRegistryWithConnections` when an attribute source is
  `kind: sql` — turns the last two into a live `*provider.AttributeRegistry`.
  **The seed file is the source
  of truth for them**, exactly as auth config is — and an export reproduces none
  of them. A declared provider names an `object_type`, a `kind` (`csv` or `sql`),
  optional cache `ttl`/`max_size`, and then either a `path` (for `csv`, resolved
  relative to the seed file) or a `connection` plus `get_one` / `get_all`
  statements (for `sql` — see
  [Database-backed providers](#database-backed-providers)), plus an optional
  `references:` block (see [Declaring a reference](#declaring-a-reference)). A
  malformed entry is
  `APERTURE_CONFIG_INVALID` / `APERTURE_PROVIDER_INVALID`, and a broken
  connection declaration is `APERTURE_SQL_PROVIDER_CONNECTION` or
  `APERTURE_SQL_PROVIDER_DSN_LITERAL`. When `providers:` and `objects:` claim one
  type, `providers:` wins the type outright — see
  [When both sections claim a type](#when-both-sections-claim-a-type).

## Related

- [The RBAC model](model.md) — the entities the document mirrors.
- [Rules engine](rules.md) — the canonical AST a rule's `ast` field carries.
- [Providers](providers.md) — the registry `providers:` wiring builds, and the
  cache that is never exported.
- [Storage](storage.md) — the `Storage` backend `Apply` writes through and `Export`
  reads back.
- [Portability CLI](../cli/portability.md) — the command surface over import/export.
