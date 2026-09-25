---
name: storage-schema
description: The contract Aperture's own database schema keeps — the apt_ identifier convention, the one timestamp encoding (int64 Unix nanoseconds, 0 means unset, 1677-2262), the eleven foreign-key edges with their ON DELETE/ON UPDATE actions, the three integrity checks SQL cannot express (the two wildcard-bearing account_id columns and the polymorphic grant subject), the columns that deliberately carry NO foreign key and why touching that decision breaks the audit trail, the five shared-wiring tables and the secret they may never carry, their all-or-nothing ReplaceWiring/GetWiring surface and why DeclaredKeys is a struct rather than a slice, the SQLite/Postgres dialect divergences, the gates that keep the two schema files honest, and the operator facts (schema qualifier, DSN selection, account teardown, seed write order) that are otherwise misdiagnosed as bugs.
applies_to: [library, cli, http]
---

# Aperture's storage schema

This document is about **Aperture's own persistence** — the tables behind
`model.Storage`, in `storage/sqlite/schema.sql` and `storage/postgres/schema.sql`.
It is **not** about `sqlprovider/`, which is a *host* data source Aperture reads
objects from; see `skills/sql-provider.md` for that.

Everything below is a property of code, and most of it is gated. Where it is not,
the section says so.

## The shape, in one paragraph

Three backends implement one interface. `storage/memory` is map-backed and
durable-free; `storage/sqlite` is the reference durable backend on pure-Go
`modernc.org/sqlite`; `storage/postgres` is its full peer on
`jackc/pgx/v5/stdlib` through `database/sql`. Both SQL backends apply a
hand-written, `//go:embed`ed `schema.sql` of **19 tables and 7 indexes** — no
ORM, no sqlc, no query builder, **no migration tool and no schema versioning**.
All three run the single behavioural contract in `storage/storagetest`, which
carries no backend-conditional assertions: backends are proven identical, not
assumed to be.

`Setup` **creates**; it never migrates. A schema change is a hard break — see
"Recreating the database" below.

## The `apt_` identifier convention

Every database identifier Aperture owns follows one rule, enforced per dialect by
`TestSchemaUsesNoReservedIdentifiers` in `internal/schemagate`.

1. **Every table Aperture owns is `apt_<thing>`.** Blanket, no exceptions:
   `apt_accounts`, `apt_grants`, `apt_audit_log`. A prefixed name can never
   collide with a SQL keyword, and in a database Aperture shares with its host it
   is immediately obvious which tables are ours. In Postgres the prefix does
   double duty: it is what makes an *unqualified* `search_path` deployment safe,
   so pinning a dedicated schema stays an operator's choice rather than a
   requirement.
2. **A column is prefixed `apt_` only when its *whole* name spells a reserved
   word**, counting the singular of a plural: `apt_action`, `apt_actions`,
   `apt_identity`, `apt_grants`, `apt_object`. A compound name (`account_id`,
   `object_type`, `role_id`, `occurred_at`) can never be a keyword, so it is left
   bare. Prefixing more than the rule requires is noise, not safety.
3. **Index names keep `idx_` and track the table**:
   `idx_apt_grants_account_subject`.

This is a **database-identifier convention only**. Go types, struct fields, JSON
and wire field names, and CLI flags are untouched — a `grants` local variable and
an `object` parameter are not this gate's business.

**Why it exists.** Aperture writes hand-written SQL with no ORM and no query
builder to quote identifiers for it. A column named `action` or a table named
`grants` is a latent parse error: it happens to work in SQLite, breaks the moment
the statement is ported — which is no longer hypothetical, because the Postgres
backend *is* that port — and forces every future statement to remember the
quotes. The alternative, quoting everywhere, was rejected: it makes every
statement noisier to defend one word.

The rename was paid for once as a hard break. Reintroducing a reserved word would
mean paying it again, which is what the gate prevents. If the gate fires on a name
you believe is legitimate: **weaken nothing**, add a narrow, named, justified
exception with the reasoning beside it. A word reserved *only* by "SQL Server
future keywords" is the weakest case and the most likely legitimate exception;
every other source in `internal/sqlreserved` is a live constraint.

## Time: one encoding, everywhere

**Every persisted instant in Aperture is a signed 64-bit count of NANOSECONDS
since the Unix epoch, in UTC.** `created_at`, `updated_at`, and the audit log's
`occurred_at` alike. There is no text timestamp and no per-dialect timestamp type
anywhere in `storage/`.

| Property | Value |
|---|---|
| Column type | `INTEGER` (SQLite), `BIGINT` (Postgres) — the same int64 |
| Nullability | `NOT NULL DEFAULT 0` on every entity table |
| **`0` means UNSET**, not the Unix epoch | `Encode(time.Time{}) == 0`, `Decode(0) == time.Time{}` |
| Representable window | `1677-09-21T00:12:43.145224192Z` .. `2262-04-11T23:47:16.854775807Z` |
| Outside that window | refused with `APERTURE_INVALID_INPUT` **before** the write |
| Go-facing type | still `time.Time`; the encoding is invisible above `storage/` |

`storage/storagetime` owns both mappings and is the **only** place in the storage
layer where a `time.Time` becomes an integer or an integer becomes a `time.Time`.
That exclusivity is gated: `TestStorageTimeIsTheOnlyTimeIntegerConversion` scans
every non-test file under `storage/` with `go/ast` and bans `Unix*` conversions
elsewhere. Do not call `UnixNano` on a value that might be zero — the zero
`time.Time` is year 1, outside the window, and `UnixNano` silently overflows.

**Why the `0` sentinel is safe.** The accepted cost is that an instant of exactly
the Unix epoch is indistinguishable from unset. Aperture stamps from a real clock
and never writes the epoch itself, so nothing in the model can collide with it.
That is what lets every timestamp column stay `NOT NULL` with a `DEFAULT`.

**Why integers and not text.** Comparison is the point: range filters
(`AuditFilter.Since/Until`) and newest-first ordering compare numerically, while
RFC3339 text mis-sorts variable-length fractional seconds. A per-dialect timestamp
type would also truncate — Postgres `TIMESTAMPTZ` is microsecond resolution and
cannot carry the nanosecond-exact round trip `storagetest` asserts.

**The one exception to `DEFAULT 0`:** `apt_audit_log.occurred_at` is `NOT NULL`
with **no default**. An audit record with no instant is not a record.

**`stampedEntities()` in `storage/storagetest/storagetest.go` is the suite's
definition of "every stamped entity"** — today fourteen: Account, Membership,
ObjectType, Permission, Principal, Role, Group, Grant, Template, Rule, and the
four stamped wiring entities WiringConnection, WiringProvider, WiringFieldType,
WiringAttributeProvider. Adding a `CreatedAt`/`UpdatedAt` pair to a new model
entity means **adding it there**. If you do not, the unset round-trip, precision,
boundary and out-of-range cases silently skip it and nothing goes red.

There are fourteen and not fifteen because `apt_wiring_provider_references`
carries no timestamps — its history is its provider entry's, and the `CASCADE`
edge means it cannot outlive it. The wiring entries' `put` is a whole-set
`ReplaceWiring` carrying one entity, because that is the only write those tables
have; see "The wiring read and write surface" below.

`normTime(t) = t.UTC().Round(0)` is the suite's normalisation: it fixes location
and strips the monotonic reading, and it does **not** truncate. No tolerance or
precision knob may be added — `TestConformanceSuiteHasNoPrecisionKnob` enforces
that.

## Referential integrity: eleven SQL edges

Eleven relationship columns carry a **real** foreign key, declared as a table
constraint beside the `PRIMARY KEY`, identically in both dialects.

| # | Child column | Parent | ON DELETE | ON UPDATE |
|---|---|---|---|---|
| 1 | `apt_memberships.principal_id` | `apt_principals(id)` | RESTRICT | RESTRICT |
| 2 | `apt_permissions.object_type` | `apt_object_types(name)` | RESTRICT | RESTRICT |
| 3 | `apt_principal_roles.principal_id` | `apt_principals(id)` | **CASCADE** | RESTRICT |
| 4 | `apt_principal_roles.role_id` | `apt_roles(id)` | RESTRICT | RESTRICT |
| 5 | `apt_role_permissions.role_id` | `apt_roles(id)` | **CASCADE** | RESTRICT |
| 6 | `apt_role_permissions.permission_id` | `apt_permissions(id)` | RESTRICT | RESTRICT |
| 7 | `apt_group_members.group_id` | `apt_groups(id)` | **CASCADE** | RESTRICT |
| 8 | `apt_group_members.principal_id` | `apt_principals(id)` | RESTRICT | RESTRICT |
| 9 | `apt_grants.permission_id` | `apt_permissions(id)` | RESTRICT | RESTRICT |
| 10 | `apt_wiring_providers.object_type` | `apt_object_types(name)` | RESTRICT | RESTRICT |
| 11 | `apt_wiring_provider_references.object_type` | `apt_wiring_providers(object_type)` | **CASCADE** | RESTRICT |

**Four CASCADE, seven RESTRICT, `ON UPDATE RESTRICT` throughout.**

- **CASCADE only on an owner.** The four cascading edges are the ones where an
  entity owns its own child rows: a principal's role list, a role's permission
  list, a group's member list, and a provider entry's `references:` map. The
  child row has no meaning without its owner.
  **The key is the only implementation of that cleanup in the SQL backends** —
  `DeletePrincipal`, `DeleteRole` and `DeleteGroup` write no join-table `DELETE`
  of their own. They used to, and the two halves covered for each other:
  breaking *either* alone left the conformance suite green, so neither was
  actually proven. Removing the Go half is what makes
  `ReferentialCascadeRemovesTheJoinRowsWithTheirOwner` a real gate — relaxing any
  one of the three edges to RESTRICT now turns its own named subtest red, in both
  dialects. Re-adding a hand-written `DELETE` restores the blind spot.
  `storage/memory` is the exception and must keep its hand-written cascades: it
  has no schema, so there is nothing else to carry them.
- **RESTRICT everywhere else.** Deleting a permission a grant still cites, a role
  a principal still holds, or a principal still in a group is refused with
  `APERTURE_STORAGE_CONSTRAINT`. **That refusal is the feature**: before these
  keys existed the same delete succeeded and left the child rows pointing at
  nothing, and a grant naming a deleted permission is not a tidy leftover — it is
  authority nobody can read or revoke.
- **`ON UPDATE RESTRICT` without exception.** An id in Aperture is immutable;
  nothing in the model renames one. An `UPDATE` that moved a parent key would be
  a bug, and RESTRICT makes it a loud one rather than a quiet re-parenting.

Two consequences that are easy to trip over:

- **No parent table is written with `INSERT OR REPLACE`** (or any
  DELETE-then-INSERT upsert). REPLACE deletes the conflicting row first, firing
  the children's `ON DELETE` actions — re-saving a group would have **wiped its
  members**. Every entity upsert is `ON CONFLICT DO UPDATE`, which mutates in
  place. This applies to both backends.
- **SQLite only enforces foreign keys when `PRAGMA foreign_keys` is ON**, which
  SQLite defaults OFF and scopes **per connection**. `sqlite.Open` therefore
  forces `_pragma=foreign_keys(1)` into every DSN it opens — stripping whatever
  the caller wrote — and `Setup` verifies it and refuses a non-enforcing
  connection with `APERTURE_STORAGE_CONSTRAINT`. **Postgres has no analogue**: it
  enforces unconditionally, there is nothing to switch on, and porting the
  SQLite dance there would be cargo cult.
- **Keys are unnamed and immediate.** Postgres derives a deterministic,
  self-describing constraint name (`apt_memberships_principal_id_fkey`), which is
  what a `23503` carries. Nothing is `DEFERRABLE`: a deferred key would move the
  refusal from the statement to the `COMMIT` and change which operation
  `storagetest` sees fail.

### Three checks SQL cannot express

Three columns carry no foreign key **and could not**, so they are enforced in the
application layer instead — in **both directions** (a write naming a missing
parent is refused, *and* a delete that would strand a child is refused, which is
the `ON DELETE RESTRICT` they could not declare), inside the same transaction as
the statement they guard, in **every** backend.

`storage/sqlite/integrity.go` is the reference; `storage/postgres/integrity.go`
and `storage/memory` reproduce it. The refusals are asserted **verbatim** by the
shared contract suite, not merely by code, so a caller cannot tell which backend
or which mechanism refused them.

1. **`apt_memberships.account_id`** — must name an `apt_accounts` row **or** be
   exactly `model.AccountWildcard`.
2. **`apt_grants.account_id`** — same rule.
3. **`apt_grants.(subject_kind, subject_id)`** — polymorphic: `subject_kind`
   selects which table `subject_id` must exist in (`apt_principals`, `apt_roles`,
   or `apt_groups`), and no single-table foreign key expresses that.

## The columns that deliberately carry NO foreign key

**Read this section before adding a foreign key anywhere.** Each of these is a
decision, not an oversight, and the schema files carry the reasoning as comments
so it survives a reader who never saw this document.

### `apt_memberships.account_id` and `apt_grants.account_id` — the wildcard

`model.AccountWildcard` is the reserved account id `"*"`. A grant stamped `"*"`
applies in **every** account; a membership stamped `"*"` enrolls its principal in
every account (`engine.requireMembership` falls back to `IsMember(p, "*")` before
denying). Both are shipped, documented features — the cross-account super-admin
depends on them.

And `"*"` is deliberately **not** an account row: `ValidateAccount` refuses it, so
no `Account` can ever shadow the wildcard. **The parent a foreign key would
reference is forbidden by the model**, so the key would reject every wildcard
grant and every wildcard membership at the INSERT — killing two shipped features.

SQL has no partial or conditional foreign key. Postgres has none either, so this
is not a SQLite gap a "real" database fixes. The only shapes that would work are
*model* changes — NULL-encode the wildcard (changing what every account-scoped
query binds, including the hot-path index), or mint a real `"*"` account row
(which the model forbids). Neither is a schema decision.

**The trap:** a naive existence check in Go refuses `"*"` and breaks both
features just as thoroughly as the foreign key would. `checkAccountRef`'s first
line is the whole of the defence, and it has a test of its own.

### `apt_grants.(subject_kind, subject_id)` — polymorphic

`subject_id` points at a principal, a role, or a group depending on
`subject_kind`. One column, three possible parents. The check dispatches on the
kind and runs against exactly one table per grant.

Do not "fix" this with nullable per-kind columns plus a `CHECK`: that would break
`idx_apt_grants_account_subject (account_id, subject_kind, subject_id)`, the
decision engine's hot-path index for `GrantsForSubjects`, whose column list and
order are load-bearing and identical in both dialects.

### `apt_audit_log` — no foreign keys **at all**

`actor`, `effective_subject`, `account` and `target` name entities an audit record
must **outlive**. The trail's whole purpose is to record what was done to a
principal or an account that has since been deleted.

A key on any of them would either **refuse the delete** (RESTRICT — making the
audit trail the reason you cannot remove a user) or **erase the evidence**
(CASCADE). Both defeat an append-only trail. The columns hold plain identifier
strings, on purpose.

The table is append-only through the interface too: `AppendAudit` is the only
single-event write, there is no update, and there is no single-event delete —
only bulk retention pruning.

### JSON value columns — `apt_object_types.apt_actions`, `apt_templates.apt_grants`, `apt_wiring_attribute_providers.declared_keys`

These are **value lists, not relationships**. An object type's action verb set and
a template's parameterized grant templates ride as JSON text. `apt_templates`'s
column is called `apt_grants` because `grants` is a reserved word, **not** because
it references `apt_grants` the table — it does not, and must not.

They are plain `TEXT` in both dialects, never `json`/`jsonb`: Aperture stores the
rules and template packages' own canonical serialization **verbatim** so a round
trip is byte-stable, and `jsonb` normalises key order and whitespace, which would
silently rewrite it. `apt_rules.ast` is `TEXT` for the same reason.

### `apt_permissions.scope_strategy`

Not an id at all. It is an opaque scope reference in the `scope` package's own
small grammar — `""`, `"literal"`, `"inclusive;ids=a,b"`,
`"inclusive;rule=quarantine"` (`scope.ParseSpec`). The strategy key resolves
against an **in-process registry of resolvers**, not a table, and hosts register
their own. A rule name that may appear inside one is a parameter buried in the
string, not the column's value. There is nothing for a key to point at.

### `apt_wiring_providers.apt_connection` and `apt_wiring_attribute_providers.apt_connection`

The column names an `apt_wiring_connections` row, and still takes **no** key. The
reason is the empty string: an entry of a kind that reads no database names no
connection, so an edge would demand either a NULL-encoded absence — the one
encoding this schema does not use **anywhere** — or a fake `''` connection row in
every deployment. The name is resolved when the wiring is *built*, where an
unknown one is a coded error naming the typo and the manifest it is missing from.
A `23503` could not say that.

### `apt_wiring_field_types.object_type`

No key, and the asymmetry with `apt_wiring_providers.object_type` (which has one)
is the decision. A field-type declaration may name a type a provider entry serves
**or** a type whose objects a seed document lists inline, and an inline type needs
no `object_types:` row at all — so the parent an edge would require may
legitimately not exist, and the edge would refuse a declaration the loader
accepts.

### `apt_wiring_provider_references.target_type`

No key. A reference target is resolved against the **registry the wiring builds**,
not against a table: the target must be a type that registry serves, and an inline
`objects:` type belongs to that set without appearing in any wiring table. An
unknown target is `APERTURE_PROVIDER_REFERENCE_INVALID` at build, naming the field
and the target.

## The five shared-wiring tables

Fourteen of the nineteen tables are **model state** — who exists and who may do
what. The other five are **runtime wiring**: where a decision's object metadata
and attribute bags are read *from*. They are the database-backed home for a seed
document's `connections:`, `providers:`, `field_types:` and `attribute_providers:`
sections, so a second instance can boot with **no seed file** and decide
identically.

| Table | Holds | Key |
|---|---|---|
| `apt_wiring_connections` | one row per connection **name**, and nothing else | `name` |
| `apt_wiring_providers` | a `providers:` entry: kind, connection, `get_one`, `get_all`, `id_column`, `ttl`, `max_size` | `object_type` |
| `apt_wiring_provider_references` | a provider's `references:` map, flattened | (`object_type`, `field`) |
| `apt_wiring_field_types` | the `field_types:` section, flattened | (`object_type`, `field`) |
| `apt_wiring_attribute_providers` | an `attribute_providers:` entry, plus the optional declared key set | `subject` (the slot) |

Four properties are load-bearing, and three of them are about what is **absent**:

- **No column can carry a secret.** There is no DSN, no password and no
  credential anywhere in these five tables — not even the `dsn_env:` variable
  *name*. A connection's credentials are a per-instance fact each instance
  resolves for itself, so there is nothing here to leak, nothing to rotate, and no
  credential in a backup of this database. That is why `apt_wiring_connections`
  holds names and nothing else: the name is the only part a provider entry refers
  to.
- **There is no `path` column.** `kind: csv` is refused in shared wiring. A
  filesystem path is machine-local, both loaders resolve a relative one against
  the *seed file's* directory, and a stored path is a guess about the other
  instance's filesystem. csv stays a local-file affordance.
- **A `ttl` is stored as the Go duration TEXT the operator wrote** (`"30s"`,
  `"5m"`), never as an integer. This is wiring an operator pushes and reads back,
  and the read back must be re-pushable byte for byte; an integer would round-trip
  `30s` as `30000000000`. Same reason `apt_rules.ast` is `TEXT`. A duration is
  **not** an instant — the timestamp encoding above governs `created_at` and
  `updated_at` here and nothing else in these tables.
- **`declared_keys` distinguishes "not declared" from "declared empty".** It
  carries the attribute keys a shared slot guarantees, as a JSON array of names —
  the simplest shape that round-trips, with **no** per-key type information,
  because the value model already governs shape. `''` means *not declared* (the
  slot is opted out and behaves exactly as it did before the column existed);
  `'[]'` means *declared empty* (opted in, permitting no keys). Do not collapse
  them. The column landed with the tables rather than with its first reader
  because `Setup` creates and never migrates, so adding it later would be a second
  hard break.

`apt_wiring_provider_references` carries no timestamps, for the reason the other
owned child tables carry none: its history is its provider entry's, and the
`CASCADE` edge means it cannot outlive it.

### The wiring read and write surface

The Go side lives in `model/wiring.go` (`WiringConnection`, `WiringProvider`,
`WiringReference`, `WiringFieldType`, `WiringAttributeProvider`, `DeclaredKeys`,
`WiringSet`) and on `model.Storage`:

| Method | What it is |
|---|---|
| `ReplaceWiring(ctx, WiringSet)` | the **only** write. All of it or none of it. |
| `GetWiring(ctx)` | the boot read: all four sections, one snapshot. |
| `ListWiring{Connections,Providers,FieldTypes,AttributeProviders}` | the per-section reads `wiring show` uses. |
| `GetWiring{Connection,Provider,FieldType,AttributeProvider}` | per-entity reads, `APERTURE_NOT_FOUND` for an absent key. |

Five things about that surface are decisions rather than details:

- **There is no per-row `Put`/`Delete`.** Wiring is only meaningful whole. A
  provider entry naming a connection the manifest does not list is not half-valid
  wiring, and an instance booting against a half-written set would build its
  registries missing exactly the entries whose write failed, while reporting
  nothing. `ReplaceWiring` validates the whole set, empties all five tables, and
  writes the new one in a single transaction. Pushing a zero `WiringSet` clears
  the wiring; **that** is the delete.
- **`GetWiring` is transactional for the mirror reason.** A boot that read the
  provider list from before a push and the field-type list from after it would
  build a registry that never existed in the database at any instant.
- **An empty set is an answer, not a failure.** `WiringSet.IsEmpty()` on a
  database nothing has been pushed to is what tells a booting instance to fall
  back to its local seed file.
- **Every read returns canonical order** — connections by name, providers by
  object type, a provider's references by field, field types by object type then
  field, attribute slots by subject. A read back has to be re-pushable byte for
  byte, so map or insertion order is not an option, and Postgres pins it with
  `COLLATE "C"` like every other `ORDER BY` in that backend.
- **Two refusals, deliberately different codes.** A malformed set — an empty key,
  a negative `max_size`, the same object type or slot declared twice — is
  `APERTURE_INVALID_INPUT` from `model.ValidateWiringSet`, because a collision
  inside one pushed set is a bad push and not a database failure, and saying so in
  the model is what lets the in-memory backend refuse the same set with the same
  code as the two with primary keys. A provider entry serving an object type the
  model does not have is `APERTURE_STORAGE_CONSTRAINT`, from the real foreign key.

**`model.DeclaredKeys` is a struct and not a `[]string`,** and that is the
`''`-versus-`'[]'` distinction made unlosable. A nil-versus-empty slice expresses
it and loses it silently: `storage/memory`'s `cloneStrings` returns nil for an
empty input, and `encoding/json` renders a nil slice as `null` and an empty one as
`[]`, so three different layers could flatten *declared empty* into *never
declared* with nothing going red. The explicit `Declared` bit cannot be dropped by
accident, and `DeclaredKeys.Encode` / `model.ParseDeclaredKeys` are the one
encoder both SQL backends call — the single place this repository's
"every statement is written twice" rule is suspended, because a drifted twin here
would not produce a visibly wrong row, it would produce a slot that silently
stopped being enforced.

**Storage validates STRUCTURE only.** It does not check that a `kind` is one the
builder implements, that a `connection` appears in the manifest (the column
carries no foreign key on purpose), that a `declared_type` is `date` or
`datetime`, or that a `ttl` parses as a duration. Those belong to the layer that
BUILDS the wiring, which owns the vocabulary; duplicating them here would put each
rule in two places that can disagree.

**The `apt_wiring_provider_references` CASCADE is asymmetric between the
backends, on purpose.** The two SQL backends must **not** write the reference
cleanup by hand — the schema's `ON DELETE CASCADE` is the only implementation
there, for the reason every cascading edge gives: two halves that cover for each
other leave the conformance suite green when either one breaks. `storage/memory`
has no schema to do it, so it holds the references *inside* the provider entry
(`WiringProvider.References`), exactly as a principal's role list lives inside the
principal — which makes the cascade structural rather than remembered, because
there is no separate reference map to forget to clear.

## The two dialects, and where they legitimately differ

`storage/postgres/schema.sql` is the peer of the SQLite file: same 19 tables, same
columns, same 11 edges with the same actions, same 7 indexes. Every difference is a
dialect requirement and is written down at the point it occurs.

1. **Integer widths.** SQLite's `INTEGER` is 64-bit whatever the column holds;
   Postgres's is 32-bit. So **every** column SQLite spells `INTEGER` is `BIGINT`
   here — timestamps *and* `seq`, `version`, `delegatable`, `max_size` — rather than only the
   timestamps. Uniform on purpose: a column that silently narrowed its domain
   would be a behavioural difference between backends `storagetest` asserts are
   identical, and a uniform rule is one an automated gate can check.
   `delegatable` stays a 0/1 integer rather than `BOOLEAN` for the same reason —
   `BOOLEAN` would make the Go scan target differ per backend.
2. **Table order.** SQLite resolves `REFERENCES` lazily; Postgres resolves it at
   `CREATE` time (a forward reference is `42P01`). The Postgres file therefore
   declares **parents first**. `ALTER TABLE ADD CONSTRAINT` afterwards was
   rejected: Postgres has no `IF NOT EXISTS` for it, which would cost the file its
   idempotency. Declaration order is not a fact the parity gate collects.
3. **Primary-key nullability.** SQLite lets a non-`INTEGER` `PRIMARY KEY` hold
   NULL (a backwards-compatibility quirk), so the twelve single-column TEXT keys
   read back nullable there and `NOT NULL` in Postgres. Nothing relies on the
   looseness and no statement writes a NULL key; it is recorded rather than
   "fixed", because fixing it would mean editing the SQLite schema for a value
   neither backend can produce.

**How the Postgres schema is applied.** The whole file is one `ExecContext` with
**zero bind arguments**. That precondition is load-bearing and was settled by
measurement (PostgreSQL 18.4, pgx v5.10.0): pgx forces the simple query protocol
when the argument list is empty, and the simple protocol accepts multiple
commands. Add **one** argument and the entire script is refused with `42601` — a
total refusal at boot, not a partial apply. Nothing may route the script through
`Prepare` either. This is why the schema qualifier is a **textual** placeholder
(`apt_schema.`) substituted before execution and can never become a parameter.

`CREATE ... IF NOT EXISTS` is **not** race-free in Postgres — two booting
processes can both pass the existence check and the loser fails `42P07`/`23505` —
so `Setup` takes a transaction-scoped advisory lock around the script, at
`sql.LevelReadCommitted` (under `REPEATABLE READ` the verify step reads the
pre-schema snapshot and reports all 19 tables missing).

`Atomic` **flattens**: a nested `Atomic` reuses the open transaction rather than
taking a second connection. Both SQL backends do this, and in Postgres it is what
stops the pool deadlocking against itself.

## Recreating the database

Aperture ships **no migration tool, no schema versioning, no `user_version`
pragma, and no migration table**. `Setup` applies `CREATE ... IF NOT EXISTS`
statements: on a database that already carries the current schema it creates
nothing and changes nothing. That idempotence is exactly why it cannot upgrade.

**A schema change is a hard break, and the remedy is always the same:** move or
delete the old database, let `Setup` create a fresh one, and re-seed it. Export
first with the seed/portability tooling if you need the contents.

The SQLite backend's `Setup` **inspects an existing database first and refuses one
it cannot read**, with `APERTURE_STORAGE_SCHEMA_INCOMPATIBLE` naming what it found
and the remedy — at startup, not at the first query. It checks declared column
types, **stored** value types (SQLite's `INTEGER` affinity does not convert text
that is not a well-formed integer, so a declaration-only guard misses a copied
database), the retired `ts_nanos` column name (now `occurred_at`), and the older
unprefixed schema — fingerprinted by Aperture's own column sets, not by table
name, so a host schema that merely owns a table called `accounts` is left alone.

The guard is **SQLite-only**. No database predating the Postgres backend can
exist, because the backend is newer than every schema change it would look for.

## The gates

All of these run in `make test`. CI is container-free and node-free, so nothing
here needs a server.

| Gate | What it proves |
|---|---|
| `TestSchemaUsesNoReservedIdentifiers` | each dialect obeys the `apt_` convention, per-dialect subtests |
| `TestEveryDialectSchemaIsGoverned` | globs `storage/*/schema.sql` and fails in **both** directions, so a third dialect cannot arrive ungoverned |
| `TestDialectSchemasDeclareTheSameTables` / `...Columns` / `...ForeignKeys` | the two hand-written files describe the same database, edges including their ON DELETE/ON UPDATE |
| `TestEveryDialectHasATypeMapping` | every dialect declares what its physical type spellings mean |
| `TestTheTypeMappingRefusesTheNarrowingSpellings` | Postgres `INTEGER`, `BOOLEAN`, `JSONB`, `TIMESTAMP*` are refused **by name, with a reason** |
| `TestStorageTimeIsTheOnlyTimeIntegerConversion` | no `Unix*` conversion under `storage/` outside `storagetime` |
| `TestConformanceSuiteHasNoPrecisionKnob` / `...IsBackendBlind` | `storagetest` carries no tolerance and no backend-conditional assertion |
| `storagetest.Run` | the whole behavioural contract, per backend: timestamps, every edge, both cascade directions, the three Go checks |

The gates **parse** — a SQL tokenizer for the schema files, `go/ast` for Go
source. They never grep, following the house precedent. A missing or unparseable
schema file **fails**; it never skips.

The parity gate is the standing mitigation for an accepted risk: CI runs no
containers, so nothing in `make test` proves the Postgres backend *behaves*. It
cannot prove that either. What it proves is that Postgres has not silently fallen
**behind** its twin.

Behaviour against a real server is the gated run, outside `make test`:

```
APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./storage/postgres/
```

It **skips** when ungated and **fails** when gated with an empty DSN, so asking
for the run and silently not getting it cannot happen. Never put a DSN in a file.

**Known gap, deliberately open: index parity is not gated.** The parser reads only
`CREATE TABLE`, so an index added to one dialect and not the other is invisible.
Both headers claim the same 7 indexes and that was verified by hand, but the claim
rests on prose. It matters:
`idx_apt_grants_account_subject` serves the engine's hot-path query. Closing it
needs a `CREATE INDEX` reader plus a per-dialect index floor.

## Operator notes

Facts that are otherwise diagnosed as bugs.

- **The Postgres schema name is used verbatim and case-sensitively.**
  `APERTURE_POSTGRES_SCHEMA=Aperture` creates and addresses `"Aperture"`, not
  `aperture`. Aperture does not apply SQL's lower-casing rule to an environment
  variable, because an environment variable is not SQL text — and if it did, the
  quoted qualifier and the unquoted `pg_namespace.nspname` lookups would disagree
  and `Setup` would create fourteen tables and then report all fourteen missing.
- **The value is not trimmed**, unlike other `APERTURE_*` variables. The string
  that is *validated* must be, byte for byte, the string that is *interpolated*.
  A stray space is a boot failure that names the whitespace, which beats a silent
  success on a value the operator did not quite type. Names must match
  `\A[A-Za-z_][A-Za-z0-9_]*\z` and be at most 63 bytes; anything else is
  `APERTURE_CONFIG_INVALID` before the pool is created.
- **Unset means the ambient `search_path`** — the zero-configuration path, and
  the reason the `apt_` table prefix exists.
- **`CREATE SCHEMA IF NOT EXISTS x` fails `42501` for a role without CREATE on
  the database even when `x` already exists.** Relevant to least-privilege
  deployments; any refactor to an unconditional CREATE breaks them.
- **The CLI selects Postgres only on `postgres://` or `postgresql://`.** A libpq
  **keyword** DSN (`host=… dbname=…`) still selects **SQLite**, even though pgx
  would accept it. `--store`'s other value is a file path, and sniffing for
  keyword pairs would mean a path containing `=` started opening databases over
  the network. One sentence, no accidents.
- **Deleting an account requires deleting its grants first — including the
  admin's own.** RESTRICT is doing its job, but an admin whose grant is stamped
  to a *concrete* account revokes its own authority partway through the teardown
  and the next step fails `APERTURE_AUTHZ_DENIED`, not
  `APERTURE_STORAGE_CONSTRAINT`. Account teardown only works end to end for an
  admin holding a `model.AccountWildcard` grant. This is correct behaviour and it
  will be reported as a bug.
- **A blocked delete is a 412 over Twirp** (`twirp.FailedPrecondition`), not a
  500, and it is not logged as an internal error. The canonical code still rides
  in `meta["code"]` with its registry fixups, which enumerate the children to
  remove first.
- **`seed.Document.Apply` has a write-order dependency** — parents before
  children. `storage/memory` enforces nothing, so every `Apply` test against it
  passes regardless; only an enforcing backend reveals a regression. This
  ordering was broken once and `go test ./...` did not notice. If you change
  `Apply`'s write order, exercise it against a real SQLite file.
- **`apt_permissions.object_type`'s write side answers `APERTURE_NOT_FOUND`**,
  not `APERTURE_STORAGE_CONSTRAINT`, in every backend: `PutPermission` validates
  the action verb against the object type first. Deliberate parity of observable
  behaviour, pinned by its own case.

## Changing any of this

Update-Demand rows live in `CLAUDE.md`. In short:

- **The timestamp encoding** (`storage/storagetime`) → both `schema.sql` headers'
  "Time" sections, this document, and `docs/src/concepts/storage.md`.
- **The foreign-key edge set or an action** → both `schema.sql` files (the parity
  gate makes half a change build-red), the edge table above,
  `docs/src/concepts/storage.md`, and `wantEdges` in
  `storage/sqlite/foreign_keys_test.go` (which reads the actions back out of a
  live database, plus an exact CASCADE count).
- **A wiring column** → both `schema.sql` files, the wiring table above, and
  `docs/src/concepts/storage.md`. A column that could carry a DSN, a credential or
  a filesystem path is not a wiring column; see the section above for why.
- **The wiring read/write surface** (`model/wiring.go`, or a wiring method on
  `model.Storage`) → all four implementors, `stampedEntities()` if the entity is
  stamped, the wiring conformance cases in `storage/storagetest`,
  "The wiring read and write surface" above, and the same section in
  `docs/src/concepts/storage.md`. Two properties are the contract rather than the
  implementation: `ReplaceWiring` is **all-or-nothing** and a read is **canonical
  order**, because both are what make a push-and-read-back round trip
  re-pushable.
- **`physicalTypes` / `refusedTypes`** in `internal/schemagate` → the dialect
  divergence list above and the affected `schema.sql` header. Changing that table
  changes what "the dialects agree" *means*, which is not a refactor.
- **A new stamped model entity** → `stampedEntities()` in
  `storage/storagetest/storagetest.go`, or its timestamp cases silently skip it.
- **A new `model.Storage` method** → four implementors, not two:
  `storage/sqlite`, `storage/memory`, `storage/postgres`, and
  `service.overlayStore` in `service/simulate.go`.

## Related

- `docs/src/concepts/storage.md` — the same contract for a reader outside the
  repo.
- `skills/sql-provider.md` — the unrelated *host* data source.
- `internal/schemagate/doc.go` — the gates' own rationale, in full.
- `storage/postgres/doc.go` — the measurements behind the single-Exec schema
  apply.
