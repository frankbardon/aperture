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
- **CLI** (`urfave/cli/v3`) — `check`, `enumerate`, `explain`, `identifiers`
  (decisions + provider reads) and
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
- **Audit query** (read): `QueryAudit(AuditFilter)` returns the append-only audit
  events matching the filter (actor, account, event type, outcome, since/until,
  limit), newest-first, each as canonical JSON. It is a GATED read — a
  system-admin reads the whole trail; an account-admin reads only events scoped to
  their own account (the filter must name it). It records nothing (not itself an
  audited mutation) and backs the E6-S4 audit viewer.
- **Entity CRUD**: `Put/Get/List/Delete` for `ObjectType`, `Permission`,
  `Principal`, `Role`, `Group`, `Account`; `Put/Delete` for `Membership`;
  `Put/Get/List/Delete` for `Grant`.
- **Object identifiers (read)**: `ObjectIdentifiers(objectType, exclude...)`
  enumerates a type's INSTANCE ids from its provider (the `providers:` section a
  seed declares, wired with `WithProviders`) — the complete, unbounded set, minus
  any `exclude` ids. It materialises the positive allow-list an EXCLUSIVE
  allowance ("all objects of this type except these ids") expands to. An
  object-type with no declared provider → `APERTURE_PROVIDER_UNREGISTERED`; a
  facade built without `WithProviders` → `APERTURE_UNIMPLEMENTED`.
- **Rules (E7-S3)**: `Put/Get/List/Delete` for `Rule` (the named rule-AST
  definitions the node editor authors and rule-backed scope strategies resolve;
  the AST rides as `rule_json`/`rules_json`, the exact `rules.Node` serialization).
  `PutRule` DEEP-validates the AST (structure + compile pass) and rejects an
  invalid rule with its `APERTURE_RULE_*` code before persisting. `ValidateRule`
  runs that same validation WITHOUT persisting, so the editor can check before it
  saves. Rule DEFINITION is SYSTEM tier; reads require auth only.
- **What-if (read-only, E7-S3)**: `Simulate` / `SimulateExplain` render the
  decision (and full Explain trace) for a query under a hypothetical `Overlay`
  (rules / grants / permissions / principals) layered over the live model,
  persisting nothing. They back the rule editor's live preview of an UNSAVED rule:
  the overlay rule shadows the stored one of the same name, so a preview reflects
  the edit against grants that reference it. Requires an authenticated principal.
- **Rule what-if against an object (read-only)**: `EvaluateRule(ast, objectID)`
  compiles an UNSAVED rule AST and evaluates it directly against ONE object's
  provider metadata (`WithProviders`), returning the boolean result plus the
  metadata snapshot. No account/principal/grant is involved — the rule reads only
  `object.*` — so the rule builder can sample an object (via `ObjectIdentifiers`)
  and show whether the rule selects it. Requires an authenticated principal.
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
- **Entity reads require an authenticated principal**, and account-scoped reads
  are additionally SCOPED to the caller's visibility (a customer's admin must not
  enumerate another customer's data). `ListAccounts` / `ListPrincipals` /
  `ListGrants` / `GetGrant` resolve through `service.readScope`: a system-admin
  (`aperture.admin` on `system:schema`, resolved in the `"*"` account) sees
  everything; any other principal is scoped to the accounts it can SEE — the
  accounts it is a MEMBER of plus the accounts it ADMINISTERS — and within those,
  their grants and the principals who are members of them (plus itself). Platform
  (`"*"`) grants are system-admin-only. A read for an
  account the caller does not administer returns `APERTURE_AUTHZ_DENIED`. The
  shared-schema catalogs (`ObjectType`, `Permission`, `Role`, `Group`, `Rule`,
  `Template`) stay readable by any authenticated principal — they are the
  vocabulary an account-admin needs, not per-account data. `ObjectIdentifiers` is
  likewise an auth-required read (all of a type's instance ids, not a
  principal-scoped decision). Scoping only engages when a gate is wired and a
  principal is identified, so the local CLI/MCP facades stay unrestricted.
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
