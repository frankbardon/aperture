# Style & testing conventions

**Audience:** contributors writing or changing Go code in Aperture.

These are the house rules. The authoritative catalog is
[`CLAUDE.md`](https://github.com/frankbardon/aperture/blob/main/CLAUDE.md) at the
repo root; this page summarises what a reviewer will check.

## Library-first

The product is the public Go packages at the module root. Every surface — the
CLI, Twirp/HTTP, MCP — is a **thin translator** over one decision engine.

- **No business logic in `cmd/aperture/`.** `main.go` only assembles the binary
  and calls into `internal/cli`. Decisions, mutations, and policy live in the
  root packages (`engine/`, `service/`, `rules/`, …).
- New capability goes in the library first; the surface then exposes it.

## Errors

Every failure that crosses a package boundary is an `APERTURE_*` **coded error**
from the root `errors/` package.

- **Never** return bare `errors.New` / `fmt.Errorf` across package boundaries —
  wrap in a coded error via `errors.New` / `Newf` / `Wrap` / `Wrapf`.
- Codes are **SCREAMING_SNAKE**, `APERTURE_`-prefixed, declared in
  `errors/codes.go`, listed in `AllCodes`, and each has a `Registry` entry with a
  `Message` and at least one `Fixup` (or `FixupNotApplicable: true`).
- An error already carrying an `APERTURE_*` code passes through verbatim — the
  wrappers never re-stamp it. Recover the code with `errors.CodeOf`.
- **Do not leak cross-account data through error messages.** Messages describe
  the failure, not another tenant's data.

Adding a code is a recipe on the [Extending Aperture](../internals/extending.md#adding-an-error-code)
page, and is guarded by the `TestCodesHaveFixups`, `TestRegistryHasNoOrphans`,
and `TestCodesAreScreamingSnakeNamespaced` gates.

## Pure-Go, no CGO, no Pulse

- **`CGO_ENABLED=0` is a hard requirement.** Do not introduce a dependency that
  needs CGO (no geo/h3 or other C-linked packages).
- **No dependency on Pulse.** The rules engine renders its AST to an
  `expr-lang/expr` expression and compiles it in-process. Aperture uses
  `expr-lang/expr` directly; it does not import Pulse.
- Storage is hand-written SQL over `modernc.org/sqlite` (pure-Go) plus an
  in-memory implementation behind one `Storage` interface — no ORM, no sqlc, no
  migration tool.

## Naming

- **No predecessor references** — no `Aperture2`, `LegacyX`, or similar. Name for
  what a thing is now.

## Testing

- `make test` runs `go test ./...`. Tests are table-heavy and **co-located** with
  the code they exercise (`*_test.go`).
- The server has `*_smoke_test.go` smoke tests; end-to-end tests live in
  `cmd/aperture/` (`e2e_test.go`).
- Include tests with new functionality. There are **no follow-up-PR commitments
  for test gaps** — cover it in the same PR.
- The performance NFR is asserted by `TestCheckNFR` under `bench/`, kept out of
  `make test`; run it explicitly (see [gates](gates.md)).

## Formatting & lint

Run `make fmt`, `make vet`, and `make lint` before opening a PR. `make lint`
degrades to `go vet` when no static analyser is on PATH locally, but CI runs
`staticcheck`, so fix what it reports.

## The Update-Demand rule

Any change to a **registered surface** must ship the matching `skills/*.md`
document in the **same PR** — a surface change without its doc update is a
non-skippable CI failure. This is fully described in
[The Update-Demand rule](../internals/update-demand.md).
