# Global options

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` declares **no persistent global flags**. The five options that recur
across the command tree — `--seed`, `--store`, `--account`, `--principal`, and
`--enumerate-limit` — are defined *per command*, so they appear in each
command's flag table in the [Command-Line Reference](../reference/cli.md). They
carry the same meaning wherever they appear; this page is the single explanation
the family pages link back to.

## Selecting a model: `--seed` and `--store`

Every command resolves its model from these two options, in this order:

| Option | Default | Meaning |
|---|---|---|
| `--seed` | see below | Path to a JSON/YAML seed model. When given, the document is applied to the store on **every** invocation. |
| `--store` | in-memory | DSN for a persistent backing store: a `postgres://` / `postgresql://` URL for PostgreSQL, any other value as a SQLite path. When omitted, an in-memory store is built and seeded, then discarded when the command exits. |

Use `--seed` to point at your own model file for a one-shot decision, and
`--store` when you want changes to persist across invocations.

### What an omitted `--seed` means

**It depends on `--store`, and the difference is deliberate.**

| `--store` | `--seed` omitted |
|---|---|
| omitted (in-memory) | the committed [example fixture](../getting-started/first-decision-cli.md) is loaded — this is the zero-flag demo |
| a SQLite path or a `postgres://` URL | **nothing is seeded**; the model already in that database is used as it stands |

Applying a seed document upserts the *entire* model — accounts, principals,
roles, groups, grants, rules. On an in-memory store there is nothing to
overwrite and the fixture is the only model there could be. On a database there
usually is: it is somebody's production model, or a model a sibling instance
provisioned. So Aperture never guesses. Writing model state is an explicit act —
`--seed`, [`import`](portability.md), or a mutation command — and never a side
effect of starting up.

That is what lets a second instance boot against a database it does not own:

```bash
# Reads the model another process provisioned. Writes nothing on startup.
bin/aperture serve --store 'postgres://aperture@db/aperture'
```

```bash
# Decide against a model file, no persistence:
bin/aperture check alice read account:acme/project:atlas/document:42 \
  --seed ./my-model.yaml

# Provision a SQLite store from a document, then persist mutations to it:
bin/aperture put grant --principal root --account acme \
  --store ./aperture.db --seed ./my-model.yaml --file ./grant.json
```

Note that `--seed` is re-applied on every invocation, not just the first, so a
multi-step session against a persistent store should pass the document once and
then drop the flag — otherwise each step's deletions are undone by the next
step's seed.

### It means the same thing for *wiring*

A seed document carries two kinds of section, and the table above is about the
first: the ten **model-state** sections, which `--seed` applies to storage. The
other six are runtime **wiring** — `providers:`, `objects:`, `field_types:`,
`connections:`, `attributes:` and `attribute_providers:` — which say where a
decision reads object metadata and subject attribute bags *from*. Those are never
written to storage by `--seed`, so the file is read a second time for them.

An omitted `--seed` means the same thing for both halves:

| `--store` | `--seed` omitted |
|---|---|
| omitted (in-memory) | the example fixture supplies the wiring too |
| a SQLite path or a `postgres://` URL | **no local wiring**; the instance is wired by the [shared wiring tables](../concepts/storage.md) if they hold rows, and by nothing if they do not |

So `aperture serve --store 'postgres://aperture@db/aperture'` with no `--seed`
reads its object providers, field types and attribute slots out of the database
it opened. When those tables are empty — which is every deployment that has never
run `aperture wiring push` — the local seed file's wiring is used exactly as it
always was. There is no flag for this and nothing to configure.

A shared connection carries its **name** and nothing else: no DSN, no credential,
not even the `dsn_env:` variable name. Each instance supplies its own route for a
name, in one of three ways — a Go host's `seed.WithConnectionOpener`, a
`connections:` entry under the same name in this instance's own seed file, or the
conventional environment variable `APERTURE_CONNECTION_<NAME>_DSN` (every
character that is not a letter or digit becomes an underscore, so `main` reads
`APERTURE_CONNECTION_MAIN_DSN`). A name with no route at all refuses the boot,
naming the variable it wanted.

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

## The deployment's enumeration ceiling: `--enumerate-limit`

`--enumerate-limit` (env `APERTURE_ENUMERATE_LIMIT`, default `1000`) is the
maximum number of object ids one enumeration may return, and the ceiling a
larger request limit is clamped **down** to. It also bounds the scope member
gather, so one number governs both halves of an enumeration.

It configures the **process**, not the command. Every command that decides
carries it — `check`, `enumerate`, `identifiers`, `explain`, `serve`, `mcp` —
and all of them resolve it identically, so one binary can never answer `1500`
over HTTP and `1000` at the shell. Set it once in the environment:

```bash
export APERTURE_ENUMERATE_LIMIT=1500
bin/aperture enumerate alice list 'account:acme/**' --seed ./my-model.yaml
bin/aperture serve --seed ./my-model.yaml
```

The flag wins over the variable when both are given. A value that is not a whole
number **greater than zero** — `banana`, `0`, `-5` — fails the command with
`APERTURE_CONFIG_INVALID` naming the setting and the value it rejected, rather
than silently falling back to `1000`. To get the default, omit the setting.

On `enumerate` this is distinct from `--limit`, which caps a **single request**
and is itself clamped by this ceiling. See [Decisions](decisions.md).

## Related

- [Command-Line Reference](../reference/cli.md) — every command's full flag table.
- [Decisions](decisions.md) — where the principal is positional.
- [Mutations](mutations.md) — where `--principal` is the acting caller.
- [First decision (CLI)](../getting-started/first-decision-cli.md) — the note on subject vs actor, worked through.
