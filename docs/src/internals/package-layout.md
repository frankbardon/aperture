# Package layout

**Audience:** contributors navigating the source tree for the first time.

Aperture keeps its public packages at the **module root** (like Pulse) rather
than under `internal/`. The root packages are the product; the `internal/`
packages are the surfaces and generators that translate to and from them. Each
row below links the concept chapter that explains the package's domain in depth —
this page is a map, not a re-explanation.

## Root packages (the product)

| Package | Owns | Concept chapter |
|---|---|---|
| `errors/` | `APERTURE_*` coded errors; `codes.go` holds the `Registry` + `AllCodes`. The doc-generation source and CI gates live here. | [Error taxonomy](../concepts/errors.md) |
| `engine/` | The decision engine — `Check` / `Enumerate` / `Explain`, single and bulk. | (drives) [Decision API](../library/decision-api.md) |
| `service/` | The service facade over the engine; the surface-neutral `Query`/`Overlay`/`Actor` types every surface translates into. | [The service facade](../library/service-facade.md) |
| `rules/` | The rule AST → `expr-lang/expr` compiler + program cache. No Pulse import. | [Rules engine](../concepts/rules.md) |
| `identity/` | Principal and object identities and specificity-ranked patterns. | [Identity patterns & specificity](../concepts/identity.md) |
| `model/` | The RBAC domain model: object types, permissions, roles, groups, grants. | [RBAC domain model](../concepts/model.md) |
| `scope/` | Pluggable scope-strategy resolvers (implicit / inclusive / exclusive) + a registry. | [Scopes & scope strategies](../concepts/scopes.md) |
| `provider/` | The object-provider registry + per-type metadata cache. | [Providers](../concepts/providers.md) |
| `csvprovider/` | A concrete `ObjectProvider` backed by a CSV file — the reference implementation. | [Providers](../concepts/providers.md) |
| `auth/` | Authentication adapters (dev / OIDC / parsec) that turn a bearer into a principal. | [Authentication](../concepts/auth.md) |
| `authz/` | The authorization gate that surfaces call to guard mutations. | [The authz gate](../concepts/authz.md) |
| `audit/` | The decision/mutation audit log. | [Audit trail](../concepts/audit.md) |
| `delegation/` | Grant delegation ("bestow"). | [Delegation](../concepts/delegation.md) |
| `impersonation/` | Scoped act-as-another-principal grants. | [Impersonation](../concepts/impersonation.md) |
| `filter/` | Scoped read-visibility filtering of entity lists. | [Filtering entity lists](../concepts/filter.md) |
| `seed/` | Seed/fixture data and portability import/export. | [Seed & portability](../concepts/seed.md) |
| `mcp/` | The SDK-free MCP core: typed tool contract + handlers over the facade. | [MCP surface](../surfaces/mcp.md) |
| `storage/` | The `Storage` interface + hand-written SQL (`modernc.org/sqlite`) + in-memory impl. | [Storage](../concepts/storage.md) |

## Internal packages (surfaces and tooling)

| Package | Owns |
|---|---|
| `cmd/aperture/` | The binary entry point (`main.go`) and e2e tests. Thin — no business logic. |
| `internal/cli/` | The `urfave/cli/v3` command tree (`NewApp`); the CLI-reference generation source. |
| `internal/server/` | `net/http` ServeMux + Twirp handlers + admin-UI static serving + middleware. |
| `internal/server/static/` | The admin UI (Alpine + BERA + Rete.js); `vendor/rete/` is a committed JS bundle. |
| `internal/wire/rpc/` | `service.proto` plus the **committed** generated `service.pb.go` / `service.twirp.go`. |
| `internal/docsgen/` | The on-demand documentation generators (`errcodes`, `cliref`) run by `make docs-gen`. |
| `mcp/gosdk/` | The one adapter that imports the MCP protocol SDK; a firewall test keeps the core SDK-free. |
| `mcp/toolmeta/` | The pure-data tool identity table (names + descriptions) shared by the core and the adapter. |
| `bench/` | The performance suite and the `TestCheckNFR` gate (kept out of `make test`). |
| `skills/` | The Update-Demand surface docs and their coverage gates. See [The Update-Demand rule](update-demand.md). |

## Dependency direction

The root packages form a layered graph pointing downward. `scope`, `provider`,
and `identity` are **leaves**: `scope` imports only `identity` and `errors`;
`provider` imports only `identity` and `errors`. The engine adapts the model onto
these leaves rather than the leaves reaching up. This is what lets you add a
scope strategy or a provider without touching the engine — the seams described in
[Extending Aperture](extending.md) exist precisely because the dependency arrows
never point up.
