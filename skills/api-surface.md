---
name: api-surface
description: The full Aperture API over Twirp + net/http + CLI — decisions and mutations behind one service facade, with auth required and admin tiers enforced on every mutation.
applies_to: [twirp, http, cli, mcp]
---

# API surface

Aperture exposes its whole API — queries AND mutations — over three coordinated
surfaces that all drive ONE facade (`service.Service`), so the auth policy, the
admin-tier enforcement, and the fail-closed decision semantics live in exactly
one place (FR-26, FR-28).

- **Twirp** (`internal/wire/rpc`, package `aperture`) — the generated
  `ApertureService` server, mounted on the net/http `ServeMux` under the path
  prefix `/twirp/aperture.ApertureService/` with `twirp.ServerHooks` request +
  error logging (the orbit pattern). The handler is `internal/server/twirp.go`.
- **net/http** — the minimal plain `POST /check` decision route is preserved
  (E1-S5) alongside the Twirp surface, plus `GET /healthz`.
- **CLI** (`urfave/cli/v3`) — `check`, `enumerate`, `explain` (decisions) and
  `put`, `get`, `list`, `delete`, `bestow`, `revoke`, `impersonate`, `template`
  (`put`/`get`/`list`/`delete`/`apply`), `bulk` (`grant`/`revoke`) (mutations).
  Each command builds the same fully-wired facade in-process; `cmd/aperture`
  stays a thin adapter.

## The facade (`service.Service`)

`service.New(eng, opts...)` returns the facade. With no options it is read-only
(the decision API); the mutation surface turns on with `WithStorage` +
`WithGate` (+ `WithDelegation` / `WithImpersonation`). One facade is the single
seam the read-subset MCP (E4-S3), audit-wrapping (E4-S2), provisioning (E5), and
the UI (E6) all build on.

Full surface:

- **Decisions** (read): `Check`, `Enumerate`, `Explain` + `CheckBatch`,
  `EnumerateBatch`, `ExplainBatch`. Fail-closed (an operational error folds to a
  deny; only input-validation is returned).
- **Entity CRUD**: `Put/Get/List/Delete` for `ObjectType`, `Permission`,
  `Principal`, `Role`, `Group`, `Account`; `Put/Delete` for `Membership`;
  `Put/Get/List/Delete` for `Grant`.
- **Delegation**: `Bestow`, `Revoke`.
- **Impersonation**: `ImpersonationStart`, `ImpersonationStop`.
- **Provisioning (E5-S1)**: `Put/Get/List/Delete` for `Template` (named,
  versioned, parameterized grant bundles); `ApplyTemplate` (resolve params →
  expand → apply transactionally → one audit event); `BulkPutGrants` /
  `BulkDeleteGrants` (provision/deprovision many grants atomically). Template
  DEFINITION is SYSTEM tier; APPLY and BULK write grants, so they are ACCOUNT tier
  in the target account. All three are TRANSACTIONAL via `Storage.Atomic` — a
  partial failure rolls the WHOLE operation back, so no grant persists if any
  fails.

## Auth + admin-tier policy

- **Decision RPCs are open** — `Check` / `Enumerate` / `Explain` require no
  credential, preserving the simple decision path. `POST /check` stays open too.
- **Entity reads require an authenticated principal** (presence only, no tier).
- **Mutations require an authenticated principal AND the admin tier their kind
  needs** (`authz.Gate`): schema entities are SYSTEM tier (`system:*`);
  membership + raw grants are ACCOUNT tier (`account:<acct>/admin:*` in the
  target account). Unauthenticated → `APERTURE_UNAUTHENTICATED` / 401; wrong tier
  → `APERTURE_AUTHZ_DENIED` / 403.
- **Delegation and impersonation are NOT admin-gated** — they carry their own
  finer-grained authorization (the delegation subset rule / the impersonation
  guardrails), where the actor is the delegator / operator, not an admin.

On the Twirp surface the actor's principal is ALWAYS the authenticated identity
from the auth middleware, never a value from the request body — a caller cannot
act as someone else. The wire's `Actor.account` selects the active account.

## Wire encoding

Simple/hot-path messages are typed (`CheckRequest`, `Decision`,
`EnumerateRequest`). Rich or nested shapes ride as a canonical JSON string: model
entities as `entity_json` (the `encoding/json` form of the `model.*` struct), the
decision `Trace` as `trace_json`. This mirrors orbit's JSON-payload convention,
keeps the proto small, and sidesteps modelling the self-referential rule AST.

## Regenerating

`make proto` regenerates `service.pb.go` + `service.twirp.go` from
`service.proto` (needs `protoc` + `protoc-gen-go` + `protoc-gen-twirp`). The
generated files are COMMITTED; CI does not regenerate, so re-run `make proto` and
commit the result whenever the `.proto` changes.
