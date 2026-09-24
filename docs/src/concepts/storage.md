# Storage

`model.Storage` is Aperture's **persistence boundary** — the single seam every
backend implements. Three backends ship, all behind the one interface:

- **`storage/memory`** — a map-backed, concurrency-safe store for tests, seeding,
  and any deployment that does not need durability.
- **`storage/sqlite`** — the durable reference backend on
  **`modernc.org/sqlite`**, a *pure-Go* driver, so `CGO_ENABLED=0` holds end to
  end.
- **`storage/postgres`** — a full peer of the SQLite backend on
  **`jackc/pgx/v5/stdlib`** through `database/sql`, also pure Go. Not a variant:
  the same tables, the same keys, the same behaviour.

Both SQL backends use a hand-written, embedded `schema.sql` — **19 tables and 7
indexes**, no ORM, no sqlc, no migration tool. Fourteen of those tables are model
state; the other five are [shared wiring](#the-five-shared-wiring-tables).

The interface is deliberately free of any backend-specific concept. All three
backends enforce the **same** validation and typed-action rules and pass the
shared conformance suite (`storage/storagetest`), which contains no
backend-conditional assertions — so behaviour is identical across them by proof,
not by intention.

## The interface contract

```go
type Storage interface {
    Setup(ctx context.Context) error // CREATES the schema; idempotent; never migrates
    Close() error

    // Account, Membership, ObjectType, Permission, Principal, Role, Group, Grant,
    // Template, Rule — each with Put/Get/List/Delete as applicable.
    // ... plus decision-engine queries, Atomic, and the audit trail.
}
```

Shape and error conventions, uniform across every entity:

| Operation | Contract |
|---|---|
| `Put*` | **upsert** keyed on the entity's id (object types on name): create when absent, replace when present; validates its argument |
| `Get*` | returns `APERTURE_NOT_FOUND` when the id is unknown |
| `List*` | returns every entity of the kind (grants are listed **per account**) |
| `Delete*` | returns `APERTURE_NOT_FOUND` when the id is unknown, and `APERTURE_STORAGE_CONSTRAINT` when children still reference the row |
| any backend failure | surfaces as `APERTURE_STORAGE` |

`PutPermission` additionally enforces typed-action validation against the
referenced object type (`APERTURE_ACTION_UNDECLARED`), and `PutGrant` validates
that `Object` parses as an identity pattern and that `AccountID` is present.

All methods are safe for concurrent use by multiple goroutines. The in-memory
backend guards its maps with a single `RWMutex`; the SQLite backend caps its
connection pool at one connection, since SQLite is a single-writer engine and one
connection avoids "database is locked" contention; the Postgres backend runs an
ordinary pool.

## Timestamps are int64 nanoseconds

Every persisted instant — `CreatedAt`, `UpdatedAt`, and an audit event's
timestamp alike — is stored as a **signed 64-bit count of nanoseconds since the
Unix epoch, in UTC**. `INTEGER` in SQLite, `BIGINT` in Postgres, carrying the
identical `int64`. There is no text timestamp and no per-dialect timestamp type
anywhere in Aperture's storage.

The Go-facing types are unchanged: `CreatedAt` is still a `time.Time`, and the
encoding is invisible above the storage layer.

| Property | Value |
|---|---|
| Unset | **`0`** — not the Unix epoch. It is what the zero `time.Time` encodes to and what decodes back to it, which is what lets every timestamp column stay `NOT NULL DEFAULT 0`. |
| Representable window | `1677-09-21T00:12:43.145224192Z` .. `2262-04-11T23:47:16.854775807Z` |
| Outside that window | refused with `APERTURE_INVALID_INPUT` **before** the write — never wrapped, clamped, or stored as an overflow value |
| Round trip | nanosecond-exact, asserted per backend by the conformance suite |

The accepted cost of the `0` sentinel is that an instant of exactly the Unix
epoch is indistinguishable from unset. Aperture stamps from a real clock and
never writes the epoch, so nothing in the model can hit that collision.

Integers rather than text because **comparison is the point**: audit range
filters and newest-first ordering compare numerically, while RFC3339 text
mis-sorts variable-length fractional seconds. A native timestamp type would also
truncate — PostgreSQL's `TIMESTAMPTZ` is microsecond resolution and cannot carry
the nanosecond-exact round trip.

`storage/storagetime` owns the conversion and is the only place in the storage
layer where a `time.Time` becomes an integer, or an integer a `time.Time`.

## Referential integrity is in the database

Eleven relationship columns carry a **real foreign key**, identically in both SQL
dialects:

| Child column | Parent | ON DELETE |
|---|---|---|
| `apt_memberships.principal_id` | `apt_principals(id)` | RESTRICT |
| `apt_permissions.object_type` | `apt_object_types(name)` | RESTRICT |
| `apt_principal_roles.principal_id` | `apt_principals(id)` | **CASCADE** |
| `apt_principal_roles.role_id` | `apt_roles(id)` | RESTRICT |
| `apt_role_permissions.role_id` | `apt_roles(id)` | **CASCADE** |
| `apt_role_permissions.permission_id` | `apt_permissions(id)` | RESTRICT |
| `apt_group_members.group_id` | `apt_groups(id)` | **CASCADE** |
| `apt_group_members.principal_id` | `apt_principals(id)` | RESTRICT |
| `apt_grants.permission_id` | `apt_permissions(id)` | RESTRICT |
| `apt_wiring_providers.object_type` | `apt_object_types(name)` | RESTRICT |
| `apt_wiring_provider_references.object_type` | `apt_wiring_providers(object_type)` | **CASCADE** |

`ON UPDATE RESTRICT` throughout, without exception: an id in Aperture is
immutable, so an `UPDATE` that moved a parent key is a bug and RESTRICT makes it
a loud one.

**CASCADE appears only where an entity owns its own child rows** — a principal's
role list, a role's permission list, a group's member list, and a provider
entry's `references:` map. The child row has no meaning without its owner.

**Everywhere else the delete is refused**, with `APERTURE_STORAGE_CONSTRAINT`.
Deleting a permission a grant still cites, a role a principal still holds, or a
principal still in a group now fails instead of silently orphaning the child
rows. That refusal is the feature: a grant naming a permission that no longer
exists is not a tidy leftover, it is authority nobody can read or revoke. Over
Twirp the refusal is a **412 Failed Precondition**, not a 500, and the code's
registry fixups name the children to remove first.

Three integrity rules cannot be expressed as a foreign key and are enforced in Go
instead — in **both directions**, in every backend, with the same code and the
same wording, so a caller cannot tell which mechanism refused:

- **`apt_memberships.account_id` and `apt_grants.account_id`** legitimately hold
  `model.AccountWildcard` (`"*"`), and `"*"` is deliberately not an account row —
  `ValidateAccount` refuses it. The parent a foreign key would reference is
  forbidden by the model, so the key would reject every wildcard grant and every
  wildcard membership. The rule enforced instead is "an `apt_accounts` row **or**
  exactly `"*"`".
- **`apt_grants.(subject_kind, subject_id)`** is polymorphic: the kind selects
  whether the id must exist in `apt_principals`, `apt_roles`, or `apt_groups`.

### Columns that deliberately carry no foreign key

This list is load-bearing. Adding a key to any of these breaks something.

- **`apt_audit_log` carries no foreign keys at all.** `actor`,
  `effective_subject`, `account` and `target` name entities an audit record must
  **outlive** — recording what was done to a principal since deleted is the whole
  point. A key would either refuse the delete (making the trail the reason you
  cannot remove a user) or erase the evidence.
- **JSON value columns** — `apt_object_types.apt_actions`,
  `apt_templates.apt_grants` and
  `apt_wiring_attribute_providers.declared_keys` — are value lists, not
  relationships.
  `apt_templates.apt_grants` is spelled that way because `grants` is a reserved
  word, not because it references the `apt_grants` table.
- **`apt_grants.subject_id`** — polymorphic, as above.
- **`apt_permissions.scope_strategy`** — not an id. It is an opaque scope
  reference resolved against an in-process registry of resolvers, not a table.
- **`apt_wiring_providers.apt_connection`** and
  **`apt_wiring_attribute_providers.apt_connection`** — the column names a row in
  the connection manifest, but an entry of a kind that reads no database names
  no connection, so a key would demand a NULL-encoded absence (an encoding this
  schema uses nowhere) or a fake `''` row in every deployment. An unknown name
  is a coded error when the wiring is built, naming the typo.
- **`apt_wiring_field_types.object_type`** — a field-type declaration may name a
  type whose objects are listed inline, and such a type needs no object-type row
  at all, so the parent a key would require may legitimately not exist.
- **`apt_wiring_provider_references.target_type`** — a reference target is
  resolved against the registry the wiring builds, not against a table.

Each refusal is written into both `schema.sql` files as a comment, next to the
column it concerns.

**SQLite enforces foreign keys only when `PRAGMA foreign_keys` is ON**, which it
defaults OFF and scopes per connection. `sqlite.Open` therefore forces
`_pragma=foreign_keys(1)` into every DSN it opens, whatever the caller passed,
and `Setup` verifies it and refuses a non-enforcing connection. PostgreSQL
enforces unconditionally and has no equivalent switch.

## The five shared-wiring tables

Fourteen tables are **model state** — who exists and who may do what. The other
five are **runtime wiring**: where a decision's object metadata and attribute bags
are read *from*. They are the database-backed home for a seed document's
`connections:`, `providers:`, `field_types:` and `attribute_providers:` sections,
so a second instance can boot with **no seed file** and decide identically.

| Table | Holds | Key |
|---|---|---|
| `apt_wiring_connections` | one row per connection **name**, and nothing else | `name` |
| `apt_wiring_providers` | a `providers:` entry: kind, connection, `get_one`, `get_all`, `id_column`, `ttl`, `max_size` | `object_type` |
| `apt_wiring_provider_references` | a provider's `references:` map, flattened | (`object_type`, `field`) |
| `apt_wiring_field_types` | the `field_types:` section, flattened | (`object_type`, `field`) |
| `apt_wiring_attribute_providers` | an `attribute_providers:` entry, plus the optional declared key set | `subject` (the slot) |

What is **absent** from them is the load-bearing part:

- **No column can carry a secret** — no DSN, no password, no credential, not even
  the `dsn_env:` variable *name*. A connection's credentials are a per-instance
  fact each instance resolves for itself, so there is nothing here to leak and no
  credential in a backup of this database. That is why the connection table holds
  names and nothing else: the name is the only part a provider entry refers to.
- **There is no `path` column.** A filesystem path is machine-local, and a stored
  one is a guess about another instance's filesystem. File-backed providers stay a
  local-file affordance.
- **A `ttl` is stored as the Go duration text the operator wrote** (`"30s"`),
  never as an integer, because wiring read back out has to be re-pushable byte for
  byte. A duration is not an instant: the nanosecond encoding above governs
  `created_at` and `updated_at`, and nothing else in these tables.
- **`declared_keys` tells "not declared" apart from "declared empty".** It holds
  the attribute keys a shared slot guarantees, as a JSON array of names. `''` means
  *not declared*; `'[]'` means *declared empty*. They are different answers.

## Account stamping is enforced in the queries

Cross-account isolation is a **data-layer** guarantee, not just a service-layer
convention. Every grant carries an `AccountID`, and the account-scoped queries —
`ListGrants(account)` and the engine's hot-path `GrantsForSubjects(account,
subjects)` — mean a grant stamped to one account can never surface in another.

## Decision-engine queries

Two methods exist specifically for the decision hot path:

- `GrantsForSubjects(ctx, account, subjects)` — the engine expands a principal into
  its subject set (the principal, its roles, its groups) and asks for exactly the
  account-scoped grants bound to that set.
- `GroupsForPrincipal(ctx, principal)` — the group half of a principal's subject
  set.

`IsMember(ctx, principal, account)` is a tight existence check the engine uses to
enforce membership without materializing the full membership list.

## Transactions

`Atomic(ctx, fn)` runs `fn` inside a transaction against a **tx-scoped `Storage`**,
committing when `fn` returns nil and rolling the *whole* batch back on any error.
All three backends give real atomicity — the SQL backends via
`BEGIN`/`COMMIT`/`ROLLBACK`, the in-memory backend via a staged snapshot committed
only on success. It is the primitive the bulk grant/revoke endpoints and template
apply build on. `fn` **must** use the `tx` handed to it (not the outer `Storage`);
a nested `Atomic` flattens into the current transaction, so an outer rollback
still covers everything — and in Postgres that flattening is also what stops the
pool deadlocking against itself.

```go
err := store.Atomic(ctx, func(tx model.Storage) error {
    if err := tx.PutGrant(ctx, g1); err != nil {
        return err // rolls back g1 and anything else in the batch
    }
    return tx.PutGrant(ctx, g2)
})
```

## The audit trail

The same `Storage` seam carries the append-only [audit trail](audit.md):
`AppendAudit` (the only single-event write), `QueryAudit` (newest-first, filtered),
and `PruneAudit` (bulk retention delete). There is no update and no single-event
delete, so a recorded event cannot be silently altered — and, as above, no foreign
key ties a record to entities it must outlive.

## Construction

```go
mem := memory.New()                 // in-memory
db, _ := sqlite.Open("aperture.db") // durable file
// db, _ := sqlite.OpenMemory()     // ephemeral SQLite (tests)

pg, _ := postgres.Open("postgres://user:pw@host:5432/db", postgres.WithSchemaFromEnv())

_ = db.Setup(ctx)                   // once, before any other call
```

From the CLI, `--store` picks the backend:

| `--store` | Backend |
|---|---|
| empty | in-memory |
| `postgres://…` or `postgresql://…` | PostgreSQL |
| anything else | SQLite at that path |

Note that a libpq **keyword** DSN (`host=… dbname=…`) still selects SQLite, even
though the driver would accept it. `--store`'s other value is a file path, and
sniffing for keyword pairs would mean a path containing `=` started opening
databases over the network.

### Choosing a PostgreSQL schema

`APERTURE_POSTGRES_SCHEMA` (or `postgres.WithSchema`) pins Aperture's tables into
a named schema. **Unset means "use whatever the connection's `search_path`
resolves to"** — the zero-configuration path, and the reason every table Aperture
owns is prefixed `apt_`: that prefix is what makes an unqualified deployment safe
in a database shared with a host application, so pinning a schema is a choice
about tidiness and grants rather than a requirement.

Three properties surprise people, and all three are deliberate:

- **The name is used verbatim and case-sensitively.**
  `APERTURE_POSTGRES_SCHEMA=Aperture` addresses a schema literally named
  `Aperture` — the one `CREATE SCHEMA "Aperture"` makes, not the one
  `CREATE SCHEMA Aperture` makes, which is `aperture`. Aperture applies no folding
  rule to an environment variable.
- **The value is not trimmed.** A stray space is a boot failure that names the
  whitespace, rather than a silent success on a value you did not quite type.
- **The name is validated before anything connects.** It must match
  `\A[A-Za-z_][A-Za-z0-9_]*\z` and be at most 63 bytes; anything else is
  `APERTURE_CONFIG_INVALID` at boot. It is the one piece of configuration
  interpolated into SQL text, because SQL has no bind parameter in an identifier
  position.

It is configured by **environment** rather than by a flag because it is a property
of the deployment, not of an invocation: a per-command flag would be a way to
write half a model into the wrong namespace.

One deployment note: `CREATE SCHEMA IF NOT EXISTS x` fails with SQLSTATE `42501`
for a role that lacks CREATE on the *database*, **even when `x` already exists**.
Grant CREATE, or pre-create the schema and use a role that only needs table
privileges within it.

### A schema break is a hard break

Aperture ships **no migration tool, no schema versioning, and no `user_version`
pragma**. `Setup` applies an embedded schema of `CREATE ... IF NOT EXISTS`
statements: on a database that already has Aperture's current tables it creates
nothing and changes nothing.

That idempotence is also why it cannot upgrade. SQLite is *dynamically typed*, so
a table an older build created keeps its old column types and its old values even
where the current schema declares something different — the engine raises no
objection, and the mistake would surface much later, as a scan error or a wrong
timestamp, far from its cause.

So the SQLite backend's `Setup` **inspects an existing database first and refuses
one it cannot read**, returning `APERTURE_STORAGE_SCHEMA_INCOMPATIBLE` naming
what it found and the remedy. It looks for:

- **declared column types** — a timestamp column not declared `INTEGER`;
- **stored value types** — a column declared `INTEGER` whose rows still hold
  text, which SQLite permits and which a type declaration alone would miss;
- **retired column names** — `ts_nanos`, the audit log's old timestamp column,
  now `occurred_at`;
- **the older, unprefixed schema** — databases predating the `apt_` table prefix
  have no `apt_` table at all, so nothing above would notice them and Aperture
  would come up *empty* beside the operator's data. These are recognized by
  fingerprinting Aperture's own column sets, not by table name, so a host schema
  that merely owns a table called `accounts` or `roles` is left alone.

A fresh database and an up-to-date one both proceed normally.

**The remedy is always the same: move or delete the old database, let `Setup`
create a fresh one, and re-seed it.** Export first with the
[seed/portability](seed.md) tooling if you need the contents. The guard is
SQLite-only — the Postgres backend is newer than every schema change it would
look for, so no database predating it can exist.

### Two processes booting at once

`CREATE ... IF NOT EXISTS` is *not* race-free in PostgreSQL: two sessions can both
pass the existence check and the loser fails on a unique violation. Two servers
booting against one database is an ordinary deployment, so the Postgres `Setup`
takes a transaction-scoped advisory lock around the whole schema script. SQLite,
being single-writer, does not need one.

## A note on ordering when you seed

Foreign keys mean **writes have an order**: a parent must exist before a child
references it. The in-memory backend enforces nothing, so a document that seeds in
the wrong order will load cleanly there and fail against SQLite or PostgreSQL. If
you author a loader or change `seed.Document.Apply`'s write order, exercise it
against a real file-backed database, not the in-memory store.

The same applies to teardown, in reverse — and there is one case worth naming
because it looks like a bug and is not. **Deleting an account requires deleting
its grants first, including the admin's own.** An admin whose grant is stamped to
a *concrete* account therefore revokes its own authority partway through the
teardown, and the next step fails with `APERTURE_AUTHZ_DENIED` rather than a
constraint error. Account teardown only works end to end for an admin holding a
`model.AccountWildcard` grant.

## Related

- [The decision engine](../library/decision-api.md) — the hot-path reader of
  `GrantsForSubjects` / `GroupsForPrincipal`.
- [The RBAC model](model.md) — the entities `Storage` persists.
- [Seed & portability](seed.md) — the declarative loader/exporter over `Storage`.
- [Audit trail](audit.md) — the append-only trail this interface carries.
- [Error codes](../reference/error-codes.md) — `APERTURE_STORAGE`,
  `APERTURE_STORAGE_CONSTRAINT`, `APERTURE_STORAGE_SCHEMA_INCOMPATIBLE`,
  `APERTURE_NOT_FOUND`, `APERTURE_ACTION_UNDECLARED`, `APERTURE_CONFIG_INVALID`.
