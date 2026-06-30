---
name: update-demand
description: The house rule that every surface change ships its skills/ doc in the same PR, gated by named CI tests.
applies_to: [all-surfaces]
---

# Update Demand

Aperture is library-first: the Go packages at the module root are the product,
and the CLI / Twirp / MCP surfaces are thin translators over one engine. To keep
the agent-facing documentation honest, Aperture inherits the frankbardon/* house
**Update-Demand** rule.

## The rule

> Any change to a registered surface MUST update the matching `skills/` document
> in the **same pull request**. A surface change that lands without its doc
> update is a non-skippable CI failure.

This file is the seed of the Update-Demand table. As real surfaces land
(identity, model, engine, scope, provider, rules, account, auth, audit, mcp),
each story adds:

1. A `skills/<feature>.md` doc with YAML frontmatter (`name`, `description`).
2. A named coverage gate in `skills/skills_test.go` that fails when the surface
   exists without its doc — the analogue of pulse's `TestSkillsCoverAllComponents`.

## Enforcement today

Until those surfaces exist, the enforced invariants are:

- `TestUpdateDemandDocPresent` — this `update-demand.md` seed doc must exist and
  carry frontmatter, so the rule itself is never silently deleted.
- `TestEverySkillHasFrontmatter` — every `skills/*.md` file must parse to a
  `name` + `description`, so no doc rots into an untitled stub.

## How to extend

When you add a surface, add its `skills/<feature>.md` and extend
`skills/skills_test.go` with a coverage gate that walks the surface's registry
(the constant table, the route set, the tool list) and asserts a matching
`skills/` doc exists. Reference pulse's `skills/coverage_test.go` for the
filesystem-driven pattern.
