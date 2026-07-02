# Decisions

**Audience:** operators and integrators asking and auditing access-control
questions from a shell.

The decision commands are the read-only core of the CLI. They never change the
model — they ask a question of it. All three of `check`, `enumerate`, and
`explain` take the **subject principal as a positional argument** (the principal
the question is *about*), not a `--principal` flag; see
[Global options](global-options.md#the-acting-principal-principal) for why the
write commands differ. `identifiers` inspects an object type's provider.

## `check` — decide one question

```text
aperture check [options] <principal> <action> <object>
```

`check` prints a one-word verdict (`allow` / `deny`) and a reason, and **carries
the verdict in its exit code** — `0` for allow, non-zero for deny — so it
composes in a pipeline (`aperture check … && deploy`).

```bash
bin/aperture check alice read account:acme/project:atlas/document:42
```

```text
allow
reason: allowed by grant g-eng-read-atlas (allow account:acme/project:atlas/**) at specificity 39300; 1 matching grant(s) considered
```

A question with no matching grant is always a deny — Aperture fails closed:

```bash
bin/aperture check bob write account:acme/project:atlas/document:42
```

```text
deny
reason: default deny: no grant matched action "write" on "account:acme/project:atlas/document:42" for principal "bob" in account "acme"
```

Full flags: [`check`](../reference/cli.md#aperture-check).

## `explain` — why a decision resolved

```text
aperture explain [options] <principal> <action> <object>
```

`explain` takes the same three arguments as `check` and prints the whole
decision trace: the subject set, every grant considered, why each did or did not
apply, and the deciding grant (marked `*`). It is a first-class operation, not a
debug afterthought.

```bash
bin/aperture explain alice read account:acme/project:atlas/document:secret
```

```text
Explain alice/read on account:acme/project:atlas/document:secret in account acme
  subjects: principal:alice, role:editor, group:engineering
  grants considered (3):
     g-eng-read-atlas [allow account:acme/project:atlas/**] allow covers the object via literal scope at specificity 39300
     g-editor-write-atlas [allow account:acme/project:atlas/**] action "write" does not match the requested "read"
   * g-deny-secret-read [deny account:acme/project:atlas/document:secret] deny covers the object via literal scope at specificity 60300
  verdict: DENY (top specificity 60300)
  reason: denied by grant g-deny-secret-read (deny account:acme/project:atlas/document:secret) at specificity 60300; 2 matching grant(s) considered
```

Full flags: [`explain`](../reference/cli.md#aperture-explain).

## `enumerate` — list objects a principal may act on

```text
aperture enumerate [options] <principal> <action> <pattern>
```

`enumerate` turns the question around: instead of one object, it lists the
object ids under a `<pattern>` that the principal may take `<action>` on, one id
per line. `--limit` caps the result count. Enumeration expands objects from the
object providers declared in the model's `providers:` section, so a model with
no providers (like the embedded example) yields an empty list — use `enumerate`
against a seed that declares a provider for the type.

```bash
bin/aperture enumerate alice read 'account:acme/project:atlas/document:*' \
  --seed ./model-with-providers.yaml --limit 100
```

Full flags: [`enumerate`](../reference/cli.md#aperture-enumerate).

## `identifiers` — a type's valid instance ids

```text
aperture identifiers [options] <object_type>
```

`identifiers` lists every valid instance id of an object type, read from the
provider that the model's `providers:` section binds to that type (a CSV file
today, a data source later). `--exclude` drops ids from the result — this is how
an exclusive "all except these" allowance expands into a positive allow-list.
Because it needs a provider, `identifiers` errors with
`APERTURE_PROVIDER_UNREGISTERED` against a model that declares none, so run it
against a seed that binds the type:

```bash
bin/aperture identifiers document --seed ./model-with-providers.yaml
bin/aperture identifiers document --seed ./model-with-providers.yaml --exclude secret
```

Full flags: [`identifiers`](../reference/cli.md#aperture-identifiers).

## Related

- [Global options](global-options.md) — `--seed` / `--store` / `--account`, and why the principal is positional here.
- [First decision (CLI)](../getting-started/first-decision-cli.md) — the same commands walked through against the example model.
- [Mutations](mutations.md) — change the grants these decisions read.
- [Command-Line Reference](../reference/cli.md) — the generated flag tables.
