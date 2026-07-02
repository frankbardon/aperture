# CLAUDE.md — Aperture

Conventions catalog for `github.com/frankbardon/aperture`. Authority order:
`.planning/access-control/context.md` > `.planning/access-control/brief.md` >
this file > the orbit/pulse reference repos.

## Project overview

Aperture is a fine-grained access-control engine for the frankbardon/* family.
It is **library-first**: the public Go packages at the module root are the
product, and every surface (CLI, Twirp/HTTP, MCP) is a thin translator over a
single decision engine. Aperture mirrors **Orbit** structurally and **Lattice**
for the MCP boundary, but keeps its public packages at the module root like
**Pulse** rather than under `internal/`.

The decision API is `Check` / `Enumerate` / `Explain`, each available single and
bulk-batched.

## Stack & build

- **Go 1.26.1**, `CGO_ENABLED=0`, pure-Go end to end. Module
  `github.com/frankbardon/aperture`.
- **CLI** is `urfave/cli/v3`; `cmd/aperture/main.go` only assembles the command
  tree (no business logic).
- **Rules** use the `github.com/expr-lang/expr` evaluator **directly** (pure-Go,
  the same engine Pulse wraps) — Aperture has no dependency on Pulse. The rules
  package renders its AST to an expr-lang expression and compiles it in-process.
- **Storage**: hand-written SQL, `modernc.org/sqlite` (pure-Go) + an
  in-memory impl behind one `Storage` interface. No ORM / sqlc / migration tool.
- **RPC/HTTP**: `net/http` ServeMux + Twirp (`internal/server/`, proto at
  `internal/wire/rpc/service.proto`), with an admin UI shell served from
  `internal/server/static/`.
- **MCP**: SDK-free core (`mcp/`, surfaced as `aperture mcp` over stdio); one
  adapter package may import the protocol SDK, enforced by a firewall test.

```bash
make build   # produce bin/aperture (-ldflags="-s -w" -trimpath, CGO off)
make test    # go test ./...
make fmt     # go fmt ./...
make vet     # go vet ./...
make lint    # vet + staticcheck (degrades to vet-only when not installed)
```

## Coding conventions

### Errors

- Every failure is an `APERTURE_*` coded error from the root `errors/` package.
- Codes are **SCREAMING_SNAKE**, namespaced `APERTURE_*`, defined in
  `errors/codes.go` and listed in `AllCodes`.
- Each code has a `Registry` entry with a `Message` and either at least one
  `Fixup` or `FixupNotApplicable=true`. Gated by `TestCodesHaveFixups`.
- Construct via `errors.New` / `Newf` / `Wrap` / `Wrapf`; recover the code with
  `errors.CodeOf`. Any error already carrying an `APERTURE_*` code passes through
  verbatim — the wrappers never re-stamp it.

### Library-first

- Business logic lives in the public root packages. `cmd/aperture/` is a thin
  adapter; later stories add manual DI in the `serve` command (no wire/fx/dig).
- Config is env vars (`APERTURE_*`) + optional YAML; `.env` via dotenv.

### Naming

- No predecessor references (no `Aperture2` / `LegacyX`).

## Update Demand

Any change to a registered surface MUST update the matching `skills/` document in
the **same PR**. A surface change that lands without its doc update is a
non-skippable CI failure. The rule itself is documented in
`skills/update-demand.md` and is self-protecting.

| If you change... | You MUST also update... | Enforced by |
|---|---|---|
| An Aperture error code | `errors/codes.go` (`AllCodes` + `Registry` entry with Message + Fixups) | `TestCodesHaveFixups` |
| A `skills/*.md` doc | its YAML frontmatter (`name` matching the file stem + `description`) | `TestEverySkillHasFrontmatter` |
| The Update-Demand rule | `skills/update-demand.md` (must remain present with frontmatter) | `TestUpdateDemandDocPresent` |

As real surfaces land (identity, model, engine, scope, provider, rules, account,
auth, audit, mcp), each story adds a `skills/<feature>.md` doc and a coverage
gate in `skills/skills_test.go` that walks the surface's registry, then a row
here.

## Non-skippable CI gates

- `TestCodesHaveFixups` — every `APERTURE_*` code has a Registry entry with a
  Message and a Fixup (or `FixupNotApplicable`).
- `TestRegistryHasNoOrphans` — `Registry` contains nothing absent from `AllCodes`.
- `TestCodesAreScreamingSnakeNamespaced` — every code is SCREAMING_SNAKE and
  `APERTURE_`-prefixed.
- `TestUpdateDemandDocPresent` — the Update-Demand seed doc exists with
  frontmatter.
- `TestEverySkillHasFrontmatter` — every `skills/*.md` has a `name` (matching its
  file stem) and a `description`.

## What NOT to do

- Don't put business logic in `cmd/aperture/`.
- Don't add a dependency on Pulse — the rules engine uses `expr-lang/expr`
  directly; keep `CGO_ENABLED=0` (no geo/h3 or other CGO packages).
- Don't return bare `errors.New`/`fmt.Errorf` across package boundaries — wrap in
  an `APERTURE_*` coded error.
- Don't commit `.planning/`.
- Don't leak cross-account data through error messages.
