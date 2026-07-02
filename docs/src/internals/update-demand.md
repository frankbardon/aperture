# The Update-Demand rule

**Audience:** contributors changing a registered surface.

Aperture has one house rule about documentation: **any change to a registered
surface must ship the matching `skills/*.md` document in the same PR.** A surface
change that lands without its doc update is a non-skippable CI failure. This page
is descriptive — it explains the rule and the gates that enforce it. The rule
itself lives in `skills/update-demand.md` and is self-protecting; this page does
not modify `skills/*.md` or the gates.

## Why the rule exists

A surface's behaviour and its documentation are two halves of one contract. If
they can drift, the docs rot silently and callers get surprised. Update-Demand
removes the option to drift: the same PR that changes a surface changes its doc,
and CI refuses the PR otherwise. The `skills/` docs are the human- and
agent-readable description of each surface; keeping them lockstep with the code is
the whole point.

## What the rule requires

| If you change… | You must also update… | Enforced by |
|---|---|---|
| An Aperture error code | `errors/codes.go` — the `AllCodes` slice and a `Registry` entry with a Message + Fixups | `TestCodesHaveFixups` |
| A `skills/*.md` doc | its YAML frontmatter (`name` matching the file stem + `description`) | `TestEverySkillHasFrontmatter` |
| The Update-Demand rule | `skills/update-demand.md` (it must stay present with frontmatter) | `TestUpdateDemandDocPresent` |

As real surfaces land, each adds a `skills/<feature>.md` doc and a coverage gate
in `skills/skills_test.go` that walks that surface's registry, plus a row in the
`CLAUDE.md` table.

## The enforcing CI gates

These gates are non-skippable and stay green regardless of any single change —
they are the mechanical enforcement behind the rule:

- **`TestCodesHaveFixups`** — every `APERTURE_*` code has a `Registry` entry with
  a `Message` and at least one `Fixup` (or `FixupNotApplicable: true`). Adding an
  error code without its remediation metadata fails here. See
  [Adding an error code](extending.md#adding-an-error-code).
- **`TestRegistryHasNoOrphans`** — the `Registry` contains nothing that is absent
  from `AllCodes`; the two lists cannot diverge.
- **`TestCodesAreScreamingSnakeNamespaced`** — every code is `SCREAMING_SNAKE`
  and `APERTURE_`-prefixed.
- **`TestUpdateDemandDocPresent`** — the Update-Demand seed doc
  (`skills/update-demand.md`) exists with frontmatter. The rule's own
  documentation cannot be deleted without failing CI — this is what makes the
  rule self-protecting.
- **`TestEverySkillHasFrontmatter`** — every `skills/*.md` has a `name` (matching
  its file stem) and a `description` in its YAML frontmatter.

## How this differs from the doc generators

Do not confuse Update-Demand with the mdBook doc generators. The error-code table
(`docs/src/reference/error-codes.md`) and the CLI reference
(`docs/src/reference/cli.md`) are regenerated on demand with `make docs-gen` and
have **no CI drift gate** — you regenerate and commit them yourself. Update-Demand
is different: it is a hard CI gate on the `skills/*.md` surface docs and the error
`Registry`. The generators keep the *reference book* fresh; Update-Demand keeps
the *surface skill docs* honest.

The full conventions catalog, including the authoritative Update-Demand table,
lives in
[`CLAUDE.md`](https://github.com/frankbardon/aperture/blob/main/CLAUDE.md).
