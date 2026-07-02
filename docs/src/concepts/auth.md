# Authentication

The `auth` package turns an **incoming external credential** into a known
Aperture principal. Authentication is always external: Aperture *consumes*
credentials, it never issues them. There is no login, signup, or
credential-issuance surface — a caller arrives already holding a bearer token,
and `auth` decides who that token names.

## The Authenticator seam

Every adapter implements one small interface:

```go
type Authenticator interface {
    Authenticate(ctx context.Context, bearer string) (principalID string, claims Claims, err error)
}
```

The input is the raw bearer token — the value after `Bearer ` in the
`Authorization` header. The HTTP middleware in `internal/server` extracts it from
the request and calls `Authenticate`; on success it attaches a `Principal` to the
request context. Handlers (and the decision surface) recover the caller with
`PrincipalFromContext`.

```go
type Principal struct {
    ID     string // the resolved Aperture principal id
    Claims Claims // the verified assertions behind it (empty for dev)
}
```

`Claims` is always a generic `map[string]any`, keyed by claim name, so downstream
code reads any claim uniformly regardless of which adapter produced it.

**Every adapter fails closed.** A missing, malformed, or unverifiable credential
is an error, never a silently-empty principal:

- an empty or unresolvable credential →
  [`APERTURE_UNAUTHENTICATED`](../reference/error-codes.md)
- a presented credential that fails verification →
  [`APERTURE_INVALID_TOKEN`](../reference/error-codes.md)

An anonymous request (no credential at all) surfaces as `ok == false` from
`PrincipalFromContext`; callers that require a principal treat that as
`APERTURE_UNAUTHENTICATED`.

## The claim → principal mapping

The two verifying adapters (OIDC and Parsec) resolve the principal id through the
**same configurable mapping**: `PrincipalClaim` names which verified claim — `sub`,
`email`, or a custom claim — becomes the Aperture principal id. Which claim is the
principal is *configuration, not code*. It defaults to `sub` when unset.

A verified token that does not carry the configured claim (or carries it empty, or
non-string) is `APERTURE_UNAUTHENTICATED` — the token verified, but it does not
name a principal Aperture can use.

The dev adapter is the one exception by construction: its bearer *is* the
principal, so there is no claim to map.

## The three adapters

### dev / static (`NewDev`)

Trusts any non-empty bearer as the principal id — `Authorization: Bearer alice`
resolves to principal `alice`. It performs no verification and returns nil claims.
This is the adapter that makes Aperture runnable with **no external IdP**: fixtures,
demos, and CI. It is the default when no auth mode is configured.

> It is a development / single-tenant trust shortcut, **not a security boundary**.
> Never select it for a deployment facing untrusted callers. Even so, it still
> fails closed on an empty bearer.

### oidc / JWT (`NewOIDC`)

Verifies a bearer JWT against an OIDC provider's published keys using the pure-Go
`github.com/coreos/go-oidc` verifier: signature, issuer (`iss` must equal
`Issuer`), audience (`aud` must contain `Audience`), and expiry. Signing keys are
established **up front** — by JWKS discovery against `Issuer`, or from an explicit
`JWKSURL` when the provider has no discovery document — so per-request
verification is offline against the cached key set.

`Issuer` and `Audience` are both required: an empty audience would accept tokens
minted for any client. Discovery performs network I/O, so `Build`/`NewOIDC` take a
`ctx`. A missing issuer or audience is `APERTURE_CONFIG_INVALID`; a discovery
failure at startup is `APERTURE_BOOT`.

### parsec (`NewParsec`)

Verifies a token minted by the in-house `github.com/frankbardon/parsec` token
broker against the broker's signing **keyring**. This is Orbit's realtime
token-broker pattern: the broker issues a client a short-lived access token, and
Aperture loads the *same* keyring to verify that token and learn who the caller is.

The keyring is the integration seam — the broker and Aperture must share it,
because a parsec key carries a per-key id (`kid`) the verifier matches the token
against. Point Aperture at the broker's persisted keyring exactly one of two ways
(`KeyringPath` wins):

- **`KeyringPath`** — the broker's `keyring.json` directly.
- **`StateDir`** — the broker's state directory, inside which `keyring.json` is
  resolved (the Orbit `serve.go` shape, `StateDir = <DataDir>/parsec`).

The verifier follows the ring by reference, so a broker key rotation that rewrites
the ring takes effect **without restarting Aperture**. By default the adapter
accepts the parsec *access* token type — the connection token a caller presents —
and maps the `sub` claim (which parsec stamps with the user id) to the principal.
A missing keyring source is `APERTURE_CONFIG_INVALID`; a load failure is
`APERTURE_BOOT`; a verification failure (bad signature, wrong type, expired) is
`APERTURE_INVALID_TOKEN`.

> Parsec is an in-house credential source. This page documents only what the
> `auth/parsec.go` adapter exposes — the keyring-verification seam. The broker's
> own token-minting protocol lives in the `parsec` module, not here.

## Selecting an adapter: config

Which adapter is built is chosen by configuration through `auth.Config`, whose
zero value selects the dev adapter — the documented default that makes Aperture
runnable with no IdP. `Config.Build(ctx)` returns the matching `Authenticator`; an
unrecognised mode is `APERTURE_CONFIG_INVALID`.

`ConfigFromEnv` reads the configuration from `APERTURE_*` environment variables:

| Env var | `Config` field | Applies to | Notes |
|---|---|---|---|
| `APERTURE_AUTH_MODE` | `Mode` | all | `dev` \| `oidc` \| `parsec`; empty ⇒ `dev` |
| `APERTURE_AUTH_PRINCIPAL_CLAIM` | `PrincipalClaim` | oidc, parsec | empty ⇒ `sub` |
| `APERTURE_OIDC_ISSUER` | `OIDCIssuer` | oidc | required (issuer URL + discovery root) |
| `APERTURE_OIDC_AUDIENCE` | `OIDCAudience` | oidc | required (Aperture's client id at the IdP) |
| `APERTURE_OIDC_JWKS_URL` | `OIDCJWKSURL` | oidc | optional; set to skip discovery |
| `APERTURE_PARSEC_KEYRING` | `ParsecKeyringPath` | parsec | path to `keyring.json` (wins over state dir) |
| `APERTURE_PARSEC_STATE_DIR` | `ParsecStateDir` | parsec | broker state dir; `keyring.json` resolved inside |

```bash
# OIDC against an IdP with a discovery document.
export APERTURE_AUTH_MODE=oidc
export APERTURE_OIDC_ISSUER=https://accounts.example.com
export APERTURE_OIDC_AUDIENCE=aperture
export APERTURE_AUTH_PRINCIPAL_CLAIM=email
```

## Related

- [The RBAC model](model.md) — the principal an authenticated credential resolves to.
- [Audit trail](audit.md) — where an authenticated actor's actions are recorded.
- [The authz gate](authz.md) — how administrative authority is enforced on the
  resolved principal.
- [Error codes](../reference/error-codes.md) — `APERTURE_UNAUTHENTICATED`,
  `APERTURE_INVALID_TOKEN`, `APERTURE_CONFIG_INVALID`, `APERTURE_BOOT`.
