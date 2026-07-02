# Global options

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` declares **no persistent global flags**. The four options that recur
across the command tree — `--seed`, `--store`, `--account`, and `--principal` —
are defined *per command*, so they appear in each command's flag table in the
[Command-Line Reference](../reference/cli.md). They carry the same meaning
wherever they appear; this page is the single explanation the family pages link
back to.

## Selecting a model: `--seed` and `--store`

Every command resolves its model from these two options, in this order:

| Option | Default | Meaning |
|---|---|---|
| `--seed` | embedded example | Path to a JSON/YAML seed model to load. When omitted, the committed [example fixture](../getting-started/first-decision-cli.md) is used. |
| `--store` | in-memory | SQLite DSN for a persistent backing store. When omitted, an in-memory store is built and seeded, then discarded when the command exits. |

Use `--seed` to point at your own model file for a one-shot decision, and
`--store` when you want changes to persist to disk across invocations. A store
built from `--store` is seeded from `--seed` (or the embedded example) the first
time it is populated.

```bash
# Decide against a model file, no persistence:
bin/aperture check alice read account:acme/project:atlas/document:42 \
  --seed ./my-model.yaml

# Persist mutations to a SQLite file so a later command sees them:
bin/aperture put grant --principal root --account acme \
  --store ./aperture.db --file ./grant.json
```

## Scoping a decision: `--account`

`--account` names the active account a decision or mutation is scoped to. Its
behaviour differs by command family:

- On the **decision** commands (`check`, `enumerate`, `explain`), `--account`
  defaults to `acme` (the example account) and bounds which grants the decision
  considers.
- On **mutation**, **provisioning**, and **portability** commands, `--account`
  has no default and is the active account used to resolve the acting
  principal's admin **tier** (account-admin vs system-admin). Several of those
  commands require it.

Aperture never lets one account's data surface in another account's decision,
and error messages never leak cross-account detail.

## The acting principal: `--principal`

This is the option most worth getting right.

On the **read decision** commands, the principal is a **positional argument** —
the *subject* of the question you are asking:

```bash
bin/aperture check alice read account:acme/project:atlas/document:42
#                  ^^^^^ the subject principal, positional — NOT --principal
```

On the **write / mutation** commands (`put`, `delete`, `bestow`, `revoke`,
`impersonate`, `template`, `bulk`, `export`, `import`), `--principal` is a
**flag** that names the *authenticated caller performing the mutation* — who is
acting, not who is being asked about. It is sourced from the
`APERTURE_PRINCIPAL` environment variable, so you can set it once per shell:

```bash
export APERTURE_PRINCIPAL=root
bin/aperture put role --account acme --file ./role.json
bin/aperture delete grant g-old --account acme
```

A mutation with no `--principal` (and no `APERTURE_PRINCIPAL`) fails with
`APERTURE_UNAUTHENTICATED`. A few commands name the acting principal with a
purpose-specific flag instead — `bestow`/`revoke` use `--delegator`, and
`impersonate` uses `--operator` — but each of those still reads from
`APERTURE_PRINCIPAL` as its default source.

## Related

- [Command-Line Reference](../reference/cli.md) — every command's full flag table.
- [Decisions](decisions.md) — where the principal is positional.
- [Mutations](mutations.md) — where `--principal` is the acting caller.
- [First decision (CLI)](../getting-started/first-decision-cli.md) — the note on subject vs actor, worked through.
