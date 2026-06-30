---
name: check-surface
description: The thin decision surfaces — the aperture check CLI command and the HTTP POST /check endpoint — both translate to one service.Check call over the engine.
applies_to: [cli, http]
---

# Check surfaces

Aperture's first demoable slice exposes the engine's single decision — "may
this principal take this action on this object?" — through two thin surfaces.
Both translate to exactly one call into the `service` facade
(`service.New(engine.New(store)).Check`), so the fail-closed decision policy
lives in one place and never drifts between surfaces.

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

## CLI: `aperture check <principal> <action> <object>`

Prints `allow` or `deny` plus the reason. Exit code reflects the decision:
allow = 0, deny = non-zero, so checks compose in shell pipelines. Flags:

- `--seed <file>` — JSON/YAML model to load (defaults to the embedded example).
- `--store <dsn>` — sqlite DSN (defaults to in-memory).
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
