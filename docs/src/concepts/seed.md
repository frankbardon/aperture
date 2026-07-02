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
providers:     [ ... ]   # runtime wiring, not model state (see below)
```

Every field mirrors its `model` counterpart in declarative form. The `Document`
started as a minimal seed shape and was generalized to the complete model, so **an
export file is a strict superset of a seed file**: a seed that omits
`templates`/`rules`/`providers` loads unchanged, and a full export reloads through
the very same path. The field set is additive-only, so old seeds keep loading.

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

## What is *not* in the file

Two things are deliberately excluded from the model state file:

- **Live host domain-object metadata** — that is the [provider](providers.md)
  cache: derived, disposable, never source of truth. Because `Export` reads storage
  back, and a provider produces no model rows, it is never reproduced.
- **Provider *wiring*** — the `providers:` section is runtime wiring, not model
  state. `Apply` never writes it to storage; instead `Document.BuildRegistry(baseDir)`
  turns it into a live `*provider.Registry`. The seed file is the source of truth
  for provider wiring, exactly as auth config is. A declared provider names an
  `object_type`, a `kind` (currently only `csv`), a `path` (resolved relative to
  the seed file), and optional cache `ttl`/`max_size`. A malformed entry is
  `APERTURE_CONFIG_INVALID` / `APERTURE_PROVIDER_INVALID`.

## Related

- [The RBAC model](model.md) — the entities the document mirrors.
- [Rules engine](rules.md) — the canonical AST a rule's `ast` field carries.
- [Providers](providers.md) — the registry `providers:` wiring builds, and the
  cache that is never exported.
- [Storage](storage.md) — the `Storage` backend `Apply` writes through and `Export`
  reads back.
- [Portability CLI](../cli/portability.md) — the command surface over import/export.
