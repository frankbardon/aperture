# Storage

`model.Storage` is Aperture's **persistence boundary** — the single seam every
backend implements. Two backends ship, both behind the one interface:

- **`storage/memory`** — a map-backed, concurrency-safe store for tests, seeding,
  and any deployment that does not need durability.
- **`storage/sqlite`** — the durable reference backend on
  **`modernc.org/sqlite`**, a *pure-Go* driver, so `CGO_ENABLED=0` holds end to
  end. It uses a hand-written, embedded schema (`schema.sql`) — no ORM, no sqlc, no
  migration tool.

The interface is deliberately free of any backend-specific concept, so a future
Postgres backend slots in unchanged. Both backends enforce the **same** validation
and typed-action rules and pass the shared conformance suite
(`storage/storagetest`), so behavior is identical across them.

## The interface contract

```go
type Storage interface {
    Setup(ctx context.Context) error // create/migrate schema; idempotent; call once
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
| `Delete*` | returns `APERTURE_NOT_FOUND` when the id is unknown |
| any backend failure | surfaces as `APERTURE_STORAGE` |

`PutPermission` additionally enforces typed-action validation against the
referenced object type (`APERTURE_ACTION_UNDECLARED`), and `PutGrant` validates
that `Object` parses as an identity pattern and that `AccountID` is present.

All methods are safe for concurrent use by multiple goroutines. The in-memory
backend guards its maps with a single `RWMutex`; the SQLite backend caps its
connection pool at one connection, since SQLite is a single-writer engine and one
connection avoids "database is locked" contention.

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
Both backends give real atomicity — SQLite via `BEGIN`/`COMMIT`/`ROLLBACK`, the
in-memory backend via a staged snapshot committed only on success. It is the
primitive the bulk grant/revoke endpoints and template apply build on. `fn` **must**
use the `tx` handed to it (not the outer `Storage`); a nested `Atomic` flattens
into the current transaction, so an outer rollback still covers everything.

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
delete, so a recorded event cannot be silently altered.

## Construction

```go
mem := memory.New()                 // in-memory
db, _ := sqlite.Open("aperture.db") // durable file
// db, _ := sqlite.OpenMemory()     // ephemeral SQLite (tests)
_ = db.Setup(ctx)                   // once, before any other call
```

## Related

- [The decision engine](../library/decision-api.md) — the hot-path reader of
  `GrantsForSubjects` / `GroupsForPrincipal`.
- [The RBAC model](model.md) — the entities `Storage` persists.
- [Seed & portability](seed.md) — the declarative loader/exporter over `Storage`.
- [Audit trail](audit.md) — the append-only trail this interface carries.
- [Error codes](../reference/error-codes.md) — `APERTURE_STORAGE`,
  `APERTURE_NOT_FOUND`, `APERTURE_ACTION_UNDECLARED`.
