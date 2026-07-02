# RPC reference

**Audience:** engineers wiring specific calls against `aperture serve`.

This is a hand-authored catalog of the `ApertureService` RPCs, grouped by area.
It summarises each method's purpose, its request/response messages, and its auth
requirement. The **canonical, exact** field lists live in
[`internal/wire/rpc/service.proto`](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto)
— read it for message shapes; this page is maintained by hand and may lag the
proto (the proto wins on any discrepancy).

Every method is a `POST` to
`/twirp/aperture.ApertureService/<Method>` with a JSON body. See the
[overview](rpc-overview.md) for transport, auth, and error mapping. Auth
shorthand used below:

- **open** — no authenticated principal required.
- **auth** — requires an authenticated principal.
- **system** — requires system-admin authority (`system:*`).
- **account** — requires account-admin authority in the target account (system
  supersedes).
- **own rule** — not gated by the admin tiers; carries its own delegation /
  impersonation authorization.

## Wire-shape convention

Simple hot-path messages (`Check`, `Enumerate`) carry their fields directly.
Rich or recursive shapes — the model entities with their timestamps, the
`Explain` trace, the rule AST — ride as a **canonical JSON string** in a `*_json`
field rather than being modelled in proto. That JSON is identical to the
library's own encoding of the corresponding `model.*` struct. So an
`EntityRequest.entity_json` is just the JSON of a `model.ObjectType`,
`model.Grant`, etc., and an `EntityResponse.entity_json` is the same on the way
back.

Mutations carry an `Actor { principal, account }`. On the wire the `principal` is
**ignored** — the authenticated identity from the middleware is always used —
while `account` selects the active account. Reads that are account-scoped resolve
their authority from the authenticated principal directly.

## Decision RPCs (open)

The core decision API, single and bulk. These are open (no principal required)
and always answer fail-closed.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Check` | `CheckRequest` → `Decision` | Is `principal` allowed `action` on `object` in `account`? Returns `allow`, a `reason`, and the deciding grant ids. |
| `CheckBatch` | `CheckBatchRequest` → `CheckBatchResponse` | Many `Check`s in one call; results are index-aligned, each either a `Decision` or a per-item error code+message. |
| `Enumerate` | `EnumerateRequest` → `EnumerateResponse` | Which object ids matching `pattern` may `principal` take `action` on? Optional `limit`. |
| `EnumerateBatch` | `EnumerateBatchRequest` → `EnumerateBatchResponse` | Batched `Enumerate`; index-aligned results. |
| `Explain` | `CheckRequest` → `ExplainResponse` | The full decision derivation for a query, as `trace_json` (the recursive engine `Trace`, not modelled in proto). |
| `ExplainBatch` | `CheckBatchRequest` → `ExplainBatchResponse` | Batched `Explain`; index-aligned `trace_json` or per-item error. |

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Enumerate \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read","pattern":"doc:*","limit":50}'
```

```json
{ "object_ids": ["doc:42", "doc:77"] }
```

## Entity CRUD

Full create/read/list/delete for each model entity. The write body is
`entity_json` (a `model.*` struct as JSON); reads return `entity_json` (single)
or `entities_json` (list). List RPCs accept an optional server-side `Filter`
(field predicates ANDed or ORed) applied before the response is returned.

**Writes are system-tier** (managing the global schema); **reads require auth**.

| Entity | Put (system) | Get (auth) | List (auth) | Delete (system) |
|---|---|---|---|---|
| Object type | `PutObjectType` | `GetObjectType` | `ListObjectTypes` | `DeleteObjectType` |
| Permission | `PutPermission` | `GetPermission` | `ListPermissions` | `DeletePermission` |
| Principal | `PutPrincipal` | `GetPrincipal` | `ListPrincipals`¹ | `DeletePrincipal` |
| Role | `PutRole` | `GetRole` | `ListRoles` | `DeleteRole` |
| Group | `PutGroup` | `GetGroup` | `ListGroups` | `DeleteGroup` |
| Account | `PutAccount` | `GetAccount` | `ListAccounts`¹ | `DeleteAccount` |

¹ `ListPrincipals` and `ListAccounts` resolve read visibility against the
caller's admin authority, so an account-admin sees only what their tier permits.

Requests: `PutX` uses `EntityRequest { actor, entity_json }`; `GetX`/`DeleteX`
use `GetRequest`/`DeleteRequest { actor, id }`; `ListX` uses
`ListRequest { filter }`. Responses: `EntityResponse` / `EntityListResponse` /
`Empty`.

`ObjectIdentifiers` (`ObjectIdentifiersRequest` → `ObjectIdentifiersResponse`,
**auth**) enumerates every instance id of an object type from its provider,
optionally minus an `exclude` list — an admin/config read over all objects of a
type.

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/PutGrant \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer acme-admin' \
  -d '{"actor":{"account":"acme"},"entity_json":"{\"ID\":\"g-1\",\"Account\":\"acme\",\"Principal\":\"alice\",\"Action\":\"read\",\"Object\":\"doc:*\",\"Effect\":\"allow\"}"}'
```

## Grants and memberships (account-tier)

Grants and memberships are account-scoped: **writes require account-admin** in
the target account; reads resolve against the caller's authority.

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutGrant` | `EntityRequest` → `Empty` | account | Create/replace one grant (`model.Grant` as `entity_json`). |
| `GetGrant` | `GetRequest` → `EntityResponse` | auth | Read one grant, visibility-scoped to the caller. |
| `ListGrants` | `ListGrantsRequest` → `EntityListResponse` | auth | Grants in `account_id`, with optional `Filter`. |
| `DeleteGrant` | `DeleteRequest` → `Empty` | account | Delete one grant by id. |
| `PutMembership` | `EntityRequest` → `Empty` | account | Add/replace a principal's membership in an account. |
| `DeleteMembership` | `MembershipKeyRequest` → `Empty` | account | Remove a membership by `(principal_id, account_id)`. |

## Rules (definition writes system; reads auth)

Rules are global schema: named, persisted rule-AST definitions the node editor
authors and the rule-backed scope strategies resolve. The AST rides as a
`model.Rule` JSON in `rule_json`.

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutRule` | `RuleRequest` → `Empty` | system | Persist a rule definition. |
| `GetRule` | `GetRequest` → `RuleResponse` | auth | Read one rule as `rule_json`. |
| `ListRules` | `Empty` → `RuleListResponse` | auth | Every stored rule, each as JSON. |
| `DeleteRule` | `DeleteRequest` → `Empty` | system | Delete a rule by id. |
| `ValidateRule` | `RuleRequest` → `Empty` | auth | Compile/validate a rule AST **without persisting**. Returns `Empty` on success, an `APERTURE_RULE_*` coded error (for the canvas) on failure. Touches no storage. |

## What-if / simulation (auth)

Read-only previews. Nothing is written and nothing is audited.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Simulate` | `SimulateRequest` → `Decision` | The decision a query WOULD get under a hypothetical overlay (unsaved rules + synthetic grants/permissions/principals layered over the live model). Backs the rule editor's live preview. |
| `SimulateExplain` | `SimulateRequest` → `ExplainResponse` | Same overlay, returning the full `Explain` trace. |
| `EvaluateRule` | `EvaluateRuleRequest` → `EvaluateRuleResponse` | Run an UNSAVED rule AST directly against one object's provider metadata (no account/principal/grant); returns the boolean `result` plus the object metadata snapshot the rule saw. |

## Templates

Reusable grant bundles. **Definition writes are system-tier**; **apply is
account-tier** (it materialises grants into a target account).

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutTemplate` | `EntityRequest` → `Empty` | system | Persist a template definition. |
| `GetTemplate` | `TemplateKeyRequest` → `EntityResponse` | auth | Read template `(name, version)`; `version <= 0` selects the latest. |
| `ListTemplates` | `ListRequest` → `EntityListResponse` | auth | List templates, with optional `Filter`. |
| `DeleteTemplate` | `TemplateKeyRequest` → `Empty` | system | Delete by name/version (`version <= 0` deletes all versions). |
| `ApplyTemplate` | `ApplyTemplateRequest` → `EntityListResponse` | account | Expand a template transactionally into `account`, filling `params` and optionally prefixing generated grant ids; returns the applied grants as JSON. |

## Bulk grant / revoke (account-tier)

Transactional multi-grant mutations, both account-tier.

| RPC | Request → Response | Purpose |
|---|---|---|
| `BulkPutGrants` | `BulkGrantsRequest` → `Empty` | Create/replace many grants (`grants_json`) in one transaction. |
| `BulkDeleteGrants` | `BulkDeleteGrantsRequest` → `Empty` | Delete many grants by id in one transaction. |

## Declarative state (system-tier)

Whole-model portability.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Export` | `ExportRequest` → `ExportResponse` | Serialize the entire model to one declarative state file as `document_json` (a system-tier read). |
| `Import` | `ImportRequest` → `Empty` | Apply a state file (`document_json`) as an idempotent, transactional upsert (the most privileged mutation; system-tier). |

## Audit query (gated read)

| RPC | Request → Response | Purpose |
|---|---|---|
| `QueryAudit` | `QueryAuditRequest` → `QueryAuditResponse` | The append-only audit events matching a filter, newest first. Records nothing. A **system-admin** may query the whole trail; an **account-admin** must set `account` to their own account (which also gates the read). Filters: `filter_actor`, `account`, `event_type` (`mutation`/`decision`/`impersonation`/`delegation`), `outcome` (`allow`/`deny`/`success`/`failure`), `since`/`until` (RFC3339), `limit`. |

## Delegation (own rule)

Not admin-gated; authorized by the delegation subset rule, with the actor = the
authenticated delegator.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Bestow` | `BestowRequest` → `Empty` | Hand on a subset of the delegator's own grants (`grant_json`). |
| `Revoke` | `RevokeRequest` → `Empty` | Revoke a previously bestowed grant by id. |

## Impersonation (own rule)

Not admin-gated; authorized by the impersonation guardrails, with the actor = the
authenticated operator. Sessions are stateless, time-boxed values.

| RPC | Request → Response | Purpose |
|---|---|---|
| `ImpersonationStart` | `ImpersonationStartRequest` → `ImpersonationSession` | Begin impersonating `target` in `account` under `mode` (`augment` or `become`); returns the session with `started_at` / `expires_at` (RFC3339). |
| `ImpersonationStop` | `ImpersonationStopRequest` → `Empty` | Discard a session (client-side; echoed for symmetry/audit). |

## Related

- [RPC / HTTP overview](rpc-overview.md) — transport, auth model, error mapping.
- [`service.proto`](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto) — the canonical contract.
- [The service facade](../library/service-facade.md) — the shared code path.
- [Error Codes](../reference/error-codes.md) — the `APERTURE_*` registry.
