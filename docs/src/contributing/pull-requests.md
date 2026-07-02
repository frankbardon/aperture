# Pull requests

**Audience:** contributors opening a PR against Aperture.

This page complements the root
[`CONTRIBUTING.md`](https://github.com/frankbardon/aperture/blob/main/CONTRIBUTING.md),
which is the canonical entry point for the mechanics (branching, bug reports).
The pages in this chapter expand on the parts a first-time contributor most often
gets wrong; where they differ in detail, they are the more specific reference for
that topic. Nothing here contradicts the root guide.

## The flow

1. **Branch off `main`.** One feature or fix per PR — keep it focused.
2. **Make your change** in the library first (see
   [Style & testing conventions](conventions.md)).
3. **Add tests in the same PR.** New functionality ships with its tests; there
   are no follow-up-PR commitments for test gaps.
4. **Regenerate committed artifacts** if you touched their sources — error codes,
   CLI flags, the RPC proto, or the Rete bundle. See
   [Regenerating artifacts](regenerating-artifacts.md). Remember the reference
   docs have **no CI drift gate**, so this is on you.
5. **Update the matching `skills/*.md` doc** if you changed a registered surface —
   the [Update-Demand rule](../internals/update-demand.md) makes this a
   non-skippable CI gate.
6. **Run the local gates:**

   ```bash
   make fmt
   make test
   make vet
   make lint
   ```

   If you changed docs, also confirm `make docs` builds cleanly.
7. **Commit, push, and open the PR.** Describe the change and note any manual
   regeneration you performed (since CI cannot verify it).

## What CI will check

- The full `go test ./...` suite via `make test`, including the five
  non-skippable gates: `TestCodesHaveFixups`, `TestRegistryHasNoOrphans`,
  `TestCodesAreScreamingSnakeNamespaced`, `TestUpdateDemandDocPresent`, and
  `TestEverySkillHasFrontmatter` (see [gates](gates.md)).
- `go vet` and `staticcheck` via `make lint` (CI installs `staticcheck`).
- **Not** checked: staleness of the generated reference pages. Regenerate them
  yourself.

## Reviewer checklist

A reviewer will look for:

- Business logic in the library, not in `cmd/aperture/`.
- Cross-boundary errors wrapped as `APERTURE_*` coded errors, with no
  cross-account data in messages.
- No CGO, no Pulse dependency, no predecessor-named symbols.
- Tests co-located with the code and covering the new behaviour.
- Regenerated `error-codes.md` / `cli.md` when error codes or CLI flags changed.
- The matching `skills/*.md` update when a registered surface changed.

## Reporting bugs

Follow the root
[`CONTRIBUTING.md`](https://github.com/frankbardon/aperture/blob/main/CONTRIBUTING.md#reporting-bugs):
include your config (secrets redacted), Go version, OS, and the full `APERTURE_*`
code of any surfaced error.
