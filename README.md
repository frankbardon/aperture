# Aperture

Fine-grained access control engine for the frankbardon/* family. Aperture is a
library-first authorization system: the Go packages at the module root are the
deliverable, and the CLI, Twirp/HTTP, and MCP surfaces are thin translators over
a single decision engine.

## Status

Pre-release. The repository is being stood up story by story under the
`access-control` effort. This scaffold establishes the module, the error
taxonomy, the build tooling, the thin CLI adapter, and the CI skeleton; the
engine and its surfaces land in later stories.

## Quick start

```bash
make build   # produce bin/aperture
make test    # run unit tests
make vet     # go vet
make lint    # vet + staticcheck (degrades gracefully if not installed)
```

## What lives where

Library-first layout — public packages at the module root (mirrors Pulse), not
all-under-`internal/` (Orbit's shape):

- `cmd/aperture/` — thin `urfave/cli/v3` entry point; zero business logic.
- `errors/` — `APERTURE_*` coded error taxonomy with per-code message + fixups.
- `skills/` — embedded markdown skill pack, governed by the Update-Demand rule.
- `engine/`, `model/`, `storage/`, `identity/`, `scope/`, `provider/`, `rules/`,
  `auth/`, `audit/`, `mcp/` — added by their respective stories.

## Conventions

See [CLAUDE.md](CLAUDE.md) for the full conventions catalog. Highlights: every
failure is an `APERTURE_*` coded error; the library is the product and surfaces
are translators over one engine; rules consume the published
[Pulse](https://github.com/frankbardon/pulse) expression evaluator; any surface
change ships its `skills/` doc in the same PR (Update-Demand).

## License

Proprietary. © frankbardon.
