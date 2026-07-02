# Installation

Aperture ships as a single, statically linked binary with no runtime
dependencies. It is pure Go with `CGO_ENABLED=0` end to end — there is no C
toolchain, no external policy engine, and nothing to install alongside it. Build
it from source and you get one file, `bin/aperture`, that runs anywhere the Go
build targets.

## Prerequisites

| Requirement | Version | Notes |
|---|---|---|
| Go | 1.26.1 | The module targets this toolchain; earlier compilers may reject newer language/std usage. |
| `make` | any | The `Makefile` wraps the build flags; you can also call `go build` directly. |
| C compiler | none | `CGO_ENABLED=0` is enforced — there is nothing to link. |

## Build from source

Clone the repository and build the binary with the provided target:

```bash
git clone https://github.com/frankbardon/aperture.git
cd aperture
make build
```

`make build` compiles the `aperture` command with `CGO_ENABLED=0`,
`-trimpath`, and `-ldflags="-s -w"`, writing the result to `bin/aperture`.
Confirm the binary works:

```bash
bin/aperture --version
```

```text
aperture version <version>
```

## Build directly with `go`

If you prefer not to use `make`, the equivalent invocation is:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/aperture ./cmd/aperture
```

The `cmd/aperture/main.go` entrypoint is a thin adapter: it only stamps the
version and hands off to the CLI command tree. All behavior lives in the
library packages at the module root.

## Put it on your `PATH` (optional)

The examples in this book invoke the binary as `bin/aperture` from the
repository root. To run `aperture` from anywhere, copy the binary onto your
`PATH`:

```bash
cp bin/aperture /usr/local/bin/aperture
aperture --version
```

## Verify the toolchain

Aperture keeps `CGO_ENABLED=0` and stays free of any CGO dependency (no geo/h3,
no SQLite C bindings — the SQLite backend uses the pure-Go `modernc.org/sqlite`).
If a build fails complaining about a C compiler, an environment variable is
forcing CGO on; clear it and rebuild:

```bash
CGO_ENABLED=0 make build
```

## Where to go next

- [Concepts](concepts.md) — the vocabulary the rest of the book assumes.
- [First decision (CLI)](first-decision-cli.md) — run `aperture check` against
  the built-in example model.
- [Library quickstart](library-quickstart.md) — embed the decision engine in a
  Go program.
