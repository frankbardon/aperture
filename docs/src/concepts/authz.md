# The authz gate

The `authz` package is the **authorization gate** the model-mutation API calls
before every mutation. It expresses Aperture's internal administrative authority —
the right to change the system itself — and enforces the required authority on
each mutation (FR-17).

## Authority is in-scheme, not a parallel system

The gate's defining property: an admin right is **just an ordinary grant**. It is
an allow grant on a reserved admin action verb, `aperture.admin`, whose object
pattern covers a tier's authority identity. The gate decides authority by
resolving that grant through the **same engine** that answers every other question
— `engine.Check` / `engine.Explain` — so an admin check is an ordinary decision:
wildcard-resolvable, auditable, and explainable, with **no special-cased bypass
path**.

This follows the same idiom as delegation (`aperture.delegate`) and impersonation
(`aperture.impersonate.*`): a reserved action verb plus an identity-pattern-scoped
grant.

## Two tiers

| Tier | Authority anchor | Governs | Holder |
|---|---|---|---|
| **System** | `system:schema` (spelled `system:*` as a grant) | the global schema — object-types, permission types, roles, groups, principals, providers, templates, rules — plus tenancy (accounts) | anyone whose effective allow grants in their active account include an allow on `aperture.admin` covering `system:*` (or the all-covering `**`) |
| **Account** | `account:<acct>/admin:all` (spelled `account:<acct>/admin:*`) | grants and delegation **within one account only** | anyone whose effective allow grants **in that account** include an allow on `aperture.admin` covering `account:<acct>/admin:*` |

**Account-admin authority is confined to its own account.** Because an
account-tier check resolves against the *target* account, an account-admin of
account A is refused any mutation scoped to account B — A's grants are not even
loaded for B, and `account:A/admin:*` does not cover `account:B/admin:*`.

**System supersedes account.** A system-admin may drive any account-tier mutation
in any account — including a freshly-created account that holds no admin grants of
its own yet. `Authorize` checks system-admin first for account-tier mutations and
only falls back to the per-account check for non-system actors (so its richer
denial context surfaces).

> **On the address spelling.** The identity grammar accepts `**` only as a
> standalone path *segment*, not inside an id component, so the in-scheme spelling
> of a tier's authority is the single-component wildcard the grammar supports:
> `system:*` and `account:<acct>/admin:*`. A broader holder (`account:acme/**`, or
> the all-covering `**`) still resolves, so the authority is genuinely
> wildcard-resolvable. A principal wanting both tiers at once holds an allow on
> `aperture.admin` over `**`, which covers every tier anchor.

## The mutation → tier policy lives in one place

`Mutation` is the key the mutation API passes to the gate, so the gate — not each
endpoint — owns the mutation→tier policy. A single map, `mutationTier`, is the
authoritative source of truth:

- **System tier:** `put_object_type`, `put_permission`, `put_role`, `put_group`,
  `put_principal`, `put_provider`, `put_template`, `put_rule`, `put_account` (and
  their `delete_*` pairs), plus `import` — applying a whole declarative state file,
  the most privileged mutation there is.
- **Account tier:** `put_grant` / `delete_grant`, `bestow` / `revoke` (delegation),
  and `put_membership` / `delete_membership`.

`TierOf(m)` reports a mutation's tier and whether it is known. **An unknown
mutation fails closed** — the gate refuses an operation it has no policy for.

## The API

```go
gate := authz.NewGate(eng) // holds only the decision engine

// Enforce the tier a mutation requires. For an account-tier mutation, account
// is the target account (e.g. the grant's AccountID) and is mandatory.
err := gate.Authorize(ctx, authz.Actor{Principal: "alice", Account: "acme"},
    authz.MutationPutGrant, "acme")
```

| Method | Returns nil when… | Otherwise |
|---|---|---|
| `Authorize(ctx, actor, m, account)` | the actor holds the tier `m` requires | `APERTURE_AUTHZ_DENIED`; unknown mutation ⇒ `APERTURE_INVALID_INPUT` |
| `RequireSystemAdmin(ctx, actor)` | the actor holds system-admin authority | `APERTURE_AUTHZ_DENIED` |
| `RequireAccountAdmin(ctx, actor, account)` | the actor holds account-admin in the **target** account | `APERTURE_AUTHZ_DENIED` |
| `ExplainSystemAdmin(ctx, actor)` | — | the engine `Trace` behind the system-admin decision |
| `ExplainAccountAdmin(ctx, actor, account)` | — | the engine `Trace` behind the account-admin decision |

`Actor` is the principal id plus the active account it is operating in. The
`Account` is mandatory for a system-tier check (it is where the actor's `system:*`
grant is resolved); for an account-tier check the *target* account governs
instead, so confinement holds regardless of `Actor.Account`.

Because every authority question resolves through the normal engine,
`ExplainSystemAdmin` / `ExplainAccountAdmin` return a full derivation `Trace`
whose verdict matches the corresponding `Require*` — the admin check is
explainable on admin identities exactly like any other decision.

## Related

- [The decision engine](../library/decision-api.md) — the `Check`/`Explain` every
  authority question resolves through.
- [Delegation ("bestow")](delegation.md) — the `bestow`/`revoke` mutations this
  gate guards, and the `aperture.delegate` idiom it mirrors.
- [Impersonation](impersonation.md) — the `aperture.impersonate.*` idiom.
- [Audit trail](audit.md) — where a gated mutation's outcome is recorded.
- [Error codes](../reference/error-codes.md) — `APERTURE_AUTHZ_DENIED`,
  `APERTURE_INVALID_INPUT`.
