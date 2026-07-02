# RPC / HTTP overview

**Audience:** engineers integrating a non-CLI consumer (a service, a script, or
the admin UI) against a running `aperture serve`.

Aperture exposes its full access-control API over HTTP as a
[Twirp](https://github.com/twitchtv/twirp) service. Twirp is a plain
request/response RPC framework: every method is an HTTP `POST` to a fixed URL,
with a JSON (or protobuf) body and a JSON (or protobuf) reply. There is no
streaming, no custom verbs, and no URL-encoded parameters — just one POST per
call. This makes the surface trivially reachable from `curl`, any HTTP client,
or a generated Twirp stub.

## The `service` facade is the one code path

Everything on the wire is a thin translation onto the `service.Service` facade —
the same facade the CLI drives and the same one the admin UI calls. HTTP / Twirp
/ CLI therefore share **one** decision engine, **one** mutation path, and **one**
auth + admin-tier policy. A handler decodes the request, calls exactly one facade
method, and encodes the result; there is no business logic in the transport
layer (`internal/server/`).

Concretely, `internal/server/twirp.go` implements the generated
`rpc.ApertureService` interface, and each method body is a few lines: decode,
call `h.svc.<Method>(...)`, encode. If a behaviour is not described here, it is
governed by the facade and documented under
[The service facade](../library/service-facade.md).

## Transport and endpoints

The Twirp service is mounted on a `net/http` `ServeMux` in
`internal/server/server.go`, under a fixed base path:

```text
/twirp/aperture.ApertureService/<Method>
```

- **Method** is the RPC name exactly as it appears in the proto (`Check`,
  `PutGrant`, `Enumerate`, …).
- **Content-Type** selects the codec: `application/json` for JSON bodies (the
  form shown throughout these docs) or `application/protobuf` for the binary
  form. Both are the standard Twirp codecs; the JSON encoding is identical to the
  library's own JSON.
- **HTTP status** is always `200` for a successful call — including a decision of
  *deny*, which is a successful answer, not an error. Failures map an Aperture
  coded error onto a Twirp error code and its HTTP status (see
  [Errors](#errors-and-status-codes) below).

Two smaller routes share the same mux for convenience:

| Route | Purpose |
|---|---|
| `POST /check` | The minimal plain-HTTP decision path (a single `Check`), preserved so the simplest decision call needs no Twirp client. It calls the same facade with identical fail-closed semantics. |
| `GET /healthz` | Liveness probe; returns `200 ok`. |
| `GET /` (and everything more specific losing to the API routes) | The embedded admin UI shell (documented in the Admin UI chapter). |

The server is started by [`aperture serve`](../cli/serve.md), which listens on
`--addr` (default `:8080`) and wraps the whole mux in the authentication
middleware.

## `service.proto` is the source of truth

The canonical, machine-readable contract is
[`internal/wire/rpc/service.proto`](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto).
It declares roughly 60 RPCs and every request/response message. The committed
`service.pb.go` / `service.twirp.go` are generated from it (`make proto`).

The reference in the [next page](rpc-reference.md) is **hand-authored**:
generating a page from the proto is out of scope, so the catalog summarises each
RPC's purpose and points back to the proto for exact field lists. When in doubt
about a field name or a message shape, read the proto — it is authoritative. If
an RPC is added to the proto and this chapter is not updated, the proto wins;
treat any discrepancy as a docs bug, not a contract change.

## Auth model

Authentication is applied as `net/http` middleware (`server.Authenticate`,
`internal/server/middleware.go`), wired in front of the whole mux by the `serve`
command. It reads a bearer credential from the `Authorization` header and, on
success, attaches the resolved Aperture principal to the request context:

- **No credential** → the request proceeds *anonymously* (no principal in
  context).
- **A valid credential** → the resolved principal is attached and the request
  proceeds as that identity.
- **A bad credential** → the request is refused `401` with a coded error
  (`APERTURE_INVALID_TOKEN` / `APERTURE_UNAUTHENTICATED`). A bad token is a hard
  failure, never silently downgraded to anonymous.

The authenticator adapter is chosen by `--auth` / `APERTURE_AUTH_MODE`; the
default `dev` adapter treats the bearer token *as* the principal id, so Aperture
runs with no external IdP out of the box (`oidc` and `parsec` are opt-in). See
[`serve`](../cli/serve.md).

On top of that, each RPC enforces its own requirement, owned by the Twirp
handler and the facade gate:

| Class of RPC | Requirement |
|---|---|
| **Decision RPCs** (`Check`, `Enumerate`, `Explain`, and their batch forms) | **Open** — no authenticated principal required. This preserves the simple decision path; a decision is answered fail-closed regardless. |
| **Entity reads** (`Get*`, `List*`, `ObjectIdentifiers`, rule reads, `EvaluateRule`, `Simulate*`, `ValidateRule`) | Require an **authenticated principal** (they are admin/config reads and tooling). Account-scoped reads (`ListPrincipals`, `ListAccounts`, `GetGrant`, `ListGrants`) additionally resolve read visibility against the caller's admin authority. |
| **Schema mutations** (object types, permissions, principals, roles, groups, accounts, rules, templates definitions, `Import`, `Export`) | Require **system-admin** authority (`system:*`). |
| **Account-scoped mutations** (grants, memberships, `BulkPutGrants`/`BulkDeleteGrants`, `ApplyTemplate`) | Require **account-admin** authority *in the target account* (a system-admin supersedes and may drive any account). |
| **Delegation** (`Bestow`, `Revoke`) and **Impersonation** (`ImpersonationStart`/`Stop`) | Not routed through the admin gate; each carries its **own** finer-grained authorization (the delegation subset rule / the impersonation guardrails), where the actor is the delegator / operator, not an admin. |

**The actor is always the authenticated principal.** For any mutation, the
principal a change is attributed to and authorized against is the identity the
middleware resolved from the request — never a value taken from the request body.
The wire `Actor.account` field is honoured (it selects the active account), but a
caller cannot act as someone else by editing the body.

The two administrative tiers themselves are ordinary in-scheme authority
(documented in `authz/`): SYSTEM authority is a holder of `system:*` (or broader);
ACCOUNT authority is a holder of `account:<acct>/admin:*` within one account, and
is confined to that account. System supersedes account for account-tier
mutations.

## Errors and status codes

Every failure is an `APERTURE_*` coded error. The handler maps the code onto a
Twirp error code (and thus an HTTP status) and attaches the canonical code as
`meta["code"]` so a client can dispatch without parsing the message:

| Aperture code (examples) | Twirp code | HTTP |
|---|---|---|
| `APERTURE_INVALID_INPUT`, `APERTURE_RULE_INVALID`, `APERTURE_TEMPLATE_PARAM`, … | `invalid_argument` | 400 |
| `APERTURE_UNAUTHENTICATED`, `APERTURE_INVALID_TOKEN` | `unauthenticated` | 401 |
| `APERTURE_AUTHZ_DENIED`, `APERTURE_DELEGATION_DENIED`, `APERTURE_IMPERSONATION_DENIED`, … | `permission_denied` | 403 |
| `APERTURE_NOT_FOUND`, `APERTURE_RULE_NOT_FOUND`, `APERTURE_PROVIDER_UNREGISTERED` | `not_found` | 404 |
| `APERTURE_UNIMPLEMENTED` | `unimplemented` | 501 |
| anything else | `internal` | 500 |

See [Error Codes](../reference/error-codes.md) for the full registry.

## A first call

An anonymous decision needs no credential:

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Check \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read","object":"doc:42"}'
```

```json
{ "allow": true, "reason": "grant g-123 allows read", "deciding_grant_ids": ["g-123"] }
```

A mutation needs a bearer token that resolves to a principal with the required
tier — here, a system-admin creating an object type:

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/PutObjectType \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer root' \
  -d '{"actor":{"account":"acme"},"entity_json":"{\"ID\":\"doc\",\"Actions\":[\"read\",\"write\"]}"}'
```

```json
{}
```

## Related

- [RPC reference](rpc-reference.md) — the endpoint catalog by area.
- [The service facade](../library/service-facade.md) — the single code path every surface shares.
- [`serve`](../cli/serve.md) — running the server; `--addr`, `--auth`, `--store`.
- [Error Codes](../reference/error-codes.md) — the `APERTURE_*` registry.
