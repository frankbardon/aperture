---
name: check-surface
description: The thin decision surfaces — the aperture check CLI command and the HTTP POST /check endpoint — both translate to one service.Check call over the engine.
applies_to: [cli, http]
---

# Check surfaces

Aperture's first demoable slice exposes the engine's single decision — "may
this principal take this action on this object?" — through two thin surfaces.
Both translate to exactly one call into the `service` facade (`Service.Check`),
so the fail-closed decision policy lives in one place and never drifts between
surfaces.

The facade is only half of "never drifts": both surfaces must also be built over
the **same engine**. `internal/cli`'s `buildDecisionStack` is that single
assembly — object providers, the rules engine over the storage-backed rule
source, and scope resolution — and `check`, `enumerate`, `identifiers`, `explain`
and `serve` all go through it. A command that builds a bare
`service.New(engine.New(store))` instead has no rule evaluator, so every
rule-backed permission fail-closed denies and the CLI contradicts the server.

## Service facade

`service.Service` is the seam every surface calls. `Check(ctx, service.Query)`
returns a `service.Result` and renders engine errors fail-closed:

- A genuine input-validation error (`APERTURE_INVALID_INPUT` /
  `APERTURE_IDENTITY_INVALID`) is returned as an error — a malformed question,
  surfaced as a usage error (CLI) or `400` (HTTP).
- Every other engine failure (an unknown principal is `APERTURE_NOT_FOUND`, a
  storage fault is `APERTURE_STORAGE`) is folded into a **deny** with the cause
  in the reason. A decision point never fails open.
- A clean engine result passes through unchanged.

The Twirp service in E4-S1 calls this same facade, inheriting the policy.

**A rule-backed grant whose metadata is the wrong shape denies; it does not
fail.** A collection operator applied to a non-collection evaluates to `false`
rather than raising `APERTURE_RULE_EVAL`, so one mistyped field cannot break
every `Check` that touches it. `Check` says only "deny" — the reason is recorded
as an evaluation note and shown by `aperture explain` (see `decision-api`).

## CLI: `aperture check <principal> <action> <object>`

Prints `allow` or `deny` plus the reason. Exit code reflects the decision:
allow = 0, deny = non-zero, so checks compose in shell pipelines. Flags:

- `--seed <file>` — JSON/YAML model to apply on startup. **Omitting it means two
  different things**: with no `--store` the embedded example is loaded (the
  zero-flag demo), and with a `--store` DSN **nothing is seeded at all**.
  Applying a document upserts the whole model, so a durable store is never
  written on the strength of an absent flag; provisioning is always explicit.
  The file is also the source of the `providers:` / `objects:` object-metadata
  wiring, which rules and scope enumeration read.
- `--store <dsn>` — backing store (defaults to in-memory). A `postgres://` or
  `postgresql://` URL selects the PostgreSQL backend; any other value is a
  SQLite path. Set `APERTURE_POSTGRES_SCHEMA` to place Aperture's tables in a
  named PostgreSQL schema — unset means "use the connection's `search_path`",
  which is safe in a shared database because every table Aperture owns is
  prefixed `apt_`. The name is validated at boot against
  `\A[A-Za-z_][A-Za-z0-9_]*\z` (max 63 bytes) and refused with
  `APERTURE_CONFIG_INVALID` otherwise: a schema name cannot be a bind parameter,
  so it is interpolated into SQL text and that pattern is the only thing
  guarding it.
- `--account <id>` — active account (defaults to the example's `acme`).

## HTTP: `POST /check`

`aperture serve` boots a `net/http` ServeMux (Go 1.22 method/pattern routing)
with graceful SIGINT/SIGTERM shutdown, wired by manual constructor DI
(storage -> engine -> service -> server). `POST /check` takes
`{account, principal, action, object}` and returns
`{allow, reason, deciding_grant_ids}`. A deny is a `200` (a valid answer); only
a malformed request is a `400`.

## Seed

`seed` loads a minimal declarative model (object types, permissions,
principals, roles, groups, grants) into a `Storage`. The committed
`org -> project -> document` fixture (`seed/testdata/example.yaml`, account
`acme`) is embedded as `seed.Example` and backs the demo and the end-to-end
test. Full export/import lands in E5-S2.
