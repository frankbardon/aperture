# Build, test & lint gates

**Audience:** contributors preparing a change for review.

Run the local gates before every PR. They are fast, pure-Go, and require no
services. CI runs the same commands plus the non-skippable gate tests described
below.

## The `make` targets

| Target | Runs | Notes |
|---|---|---|
| `make build` | `go build` → `bin/aperture` | Default goal. `CGO_ENABLED=0`, `-ldflags="-s -w" -trimpath`. |
| `make test` | `go test ./...` | The full unit/integration suite. Does **not** include the NFR benchmark gate (see below). |
| `make fmt` | `go fmt ./...` | Formats the tree. |
| `make vet` | `go vet ./...` | Standard vet checks. |
| `make lint` | `go vet` + a static analyser | Runs `staticcheck` if present, else `golangci-lint`, else prints a notice and runs vet only. CI installs `staticcheck` explicitly, so lint is real in CI even though it degrades locally. |

A minimal pre-PR loop:

```bash
make fmt
make test
make vet
make lint
```

### The benchmark / NFR gate is separate

`make test` deliberately excludes the hard performance assertion so a loaded CI
machine never flakes the build. The informational benchmark suite and the gated
NFR test live under `bench/`:

```bash
make bench                                              # informational: ns/op, p99, checks/sec
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/   # the hard NFR assertion
```

`TestCheckNFR` asserts p99 cached `Check` < 1ms **and** ≥ 10k checks/sec/instance.
See [Performance & NFR](../operations/performance.md) and the committed numbers in
`docs/benchmarks.md` for methodology.

## The non-skippable CI gates

Five gate tests protect Aperture's error taxonomy and its Update-Demand rule.
They are ordinary Go tests, so `make test` already runs all of them — you do not
need a special command. To run them in isolation:

```bash
go test ./errors/ -run 'TestCodesHaveFixups|TestRegistryHasNoOrphans|TestCodesAreScreamingSnakeNamespaced'
go test ./skills/ -run 'TestUpdateDemandDocPresent|TestEverySkillHasFrontmatter'
```

| Gate | Enforces | Trips when you… |
|---|---|---|
| **`TestCodesHaveFixups`** | Every `APERTURE_*` code has a `Registry` entry with a `Message` and at least one `Fixup` (or `FixupNotApplicable: true`). | Add an error code without its remediation metadata. |
| **`TestRegistryHasNoOrphans`** | The `Registry` contains nothing absent from `AllCodes`. | Add a `Registry` entry but forget to list the code in `AllCodes` (or vice versa). |
| **`TestCodesAreScreamingSnakeNamespaced`** | Every code is `SCREAMING_SNAKE` and `APERTURE_`-prefixed. | Name a code `apertureFoo` or drop the `APERTURE_` prefix. |
| **`TestUpdateDemandDocPresent`** | The Update-Demand seed doc `skills/update-demand.md` exists with frontmatter. | Delete or de-frontmatter the rule's own documentation. |
| **`TestEverySkillHasFrontmatter`** | Every `skills/*.md` has a `name` (matching its file stem) and a `description`. | Add or edit a `skills/` doc without valid YAML frontmatter. |

The first three live in `errors/codes_test.go`; the last two enforce the
[Update-Demand rule](../internals/update-demand.md) over the `skills/` surface
docs. None of them can be skipped — a red gate blocks the PR.

## What CI does not gate

The generated reference pages —
[`docs/src/reference/error-codes.md`](../reference/error-codes.md) and
[`docs/src/reference/cli.md`](../reference/cli.md) — have **no CI drift gate**.
Nothing fails if they go stale. Regenerating them is a manual step you own; see
[Regenerating artifacts](regenerating-artifacts.md).
