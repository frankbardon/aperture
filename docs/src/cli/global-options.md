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

### With both, the database wins and the file may only ADD

An instance can have wiring rows *and* a `--seed` file, and most will: some wiring
cannot be shared at all. A `kind: csv` provider's data source is a filesystem
path, so `aperture wiring push` refuses one outright, and a Go host's
hand-written providers are code no document describes.

So the two compose, additively:

- The **database is authoritative**. Every pushed entry is built.
- The **local file may ADD** an object type or an attribute slot the database
  never declared. It is built exactly as it would be on an instance that has
  never been pushed to, `kind: csv` and all.
- A local entry for an object type or slot the database **already declares**
  fails the boot with `APERTURE_WIRING_LOCAL_COLLISION`, naming the entry and
  both sections.

The collision is refused rather than resolved because both resolutions are silent
and both change what a decision reads: the database winning would discard wiring
somebody checked into this instance's file, and the file winning would mean one
instance in the fleet answers from a source its peers cannot see. Neither shows up
as an error later — it shows up as a different verdict. Fix it by deleting the
local declaration, or by pushing a wiring document that omits the shared one.

`connections:` is the exception, in both directions: a local entry under a shared
name is that name's **route** (see below) and not a competing declaration, and a
local entry under a name the shared manifest never mentions is the route for a
connection only this instance's own added providers reach. Both are carried.

A host that registers its own providers in Go gets the same answer from the same
place — `provider.Registry.Register` refuses a second provider for a type it
already serves, with `APERTURE_PROVIDER_INVALID`. The rule is about the registry,
not about which syntax declared the entry.

A shared connection carries its **name** and nothing else: no DSN, no credential,
not even the `dsn_env:` variable name. Each instance supplies its own route for a
name, in one of three ways — a Go host's `seed.WithConnectionOpener`, a
`connections:` entry under the same name in this instance's own seed file, or the
conventional environment variable `APERTURE_CONNECTION_<NAME>_DSN` (every
character that is not a letter or digit becomes an underscore, so `main` reads
`APERTURE_CONNECTION_MAIN_DSN`). A name with no route at all refuses the boot with
`APERTURE_WIRING_CONNECTION_UNROUTED`, naming the connection and the variable it
wanted — every unrouted name in one refusal, so a new instance is configured in one
pass rather than one restart per connection.

### An instance will not start on wiring it cannot construct

A pushed entry whose `kind:` this instance cannot build from a row refuses the boot
with `APERTURE_WIRING_KIND_UNSHAREABLE`, naming the object type or the attribute
slot. In practice that is a stored `kind: csv` — `aperture wiring push` refuses one,
so reaching a boot means the row was written by hand or by an older build, and there
is no `path:` to add because the shared tables have no column for one. The other
refusals a boot can raise come from the seed builders and already name the entry
they refused: an incomplete statement set, an unparseable `ttl:`, a `references:`
target no provider serves, a `field_types:` word outside `date`/`datetime`.

Refusing to start is the deliberate choice, and degrading is not the gentler
option. An object type with no working provider does not error on a decision — a
rule reading `object.tier` reads a *missing path*. An attribute slot with no working
provider is worse: the [leniency contract](../concepts/providers.md) collapses the
failure to an empty bag, so a rule that excluded on an attribute it can no longer
see stops excluding, and the grant **widens**. Nothing in the resulting verdict,
trace or note says a provider was missing. A per-decision refusal was rejected for
the same reason: it would let a misconfigured instance keep serving traffic for the
types it happened to have, so two instances that look identically configured answer
one question two ways.

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
