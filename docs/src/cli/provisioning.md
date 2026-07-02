# Provisioning

**Audience:** administrators granting access at scale from a shell.

The provisioning commands turn repeated grant-making into one transactional
call: `template` manages and applies parameterized grant bundles, and `bulk`
applies or removes many grants atomically. Both build the same fully-wired,
tier-gated facade the [mutation](mutations.md) commands use, and both need an
acting principal via `--principal` (or `APERTURE_PRINCIPAL`) plus, in most cases,
`--account`. Set the principal once:

```bash
export APERTURE_PRINCIPAL=root
```

## `template` — parameterized grant bundles

```text
aperture template <put|get|list|delete|apply>
```

A template is a named, versioned bundle of grants with named parameters. The
subcommands split into CRUD and apply:

| Subcommand | Tier | What it does |
|---|---|---|
| `template put` | system-admin | Create or update a template (from `--json` / `--file` / stdin). Prints `put template <name> v<version> ok`. |
| `template get <name>` | none | Read a template as JSON (latest unless `--version`). |
| `template list` | none | List every template version as JSON. |
| `template delete <name>` | system-admin | Delete one `--version`, or all versions when `--version` is 0. |
| `template apply` | account-admin | Instantiate a template's grants transactionally into `--account`. |

`template apply` binds `--param name=value` (repeatable) into the template and
writes the resulting grants in one transaction; `--id-prefix` prefixes the
generated grant ids, and `--version 0` (the default) applies the latest. It
prints the applied grants as JSON.

```bash
# Define a template (system-admin tier):
bin/aperture template put --account acme --file ./project-onboarding.json

# Apply it into an account, binding parameters (account-admin tier):
bin/aperture template apply --account acme \
  --name project-onboarding \
  --param project=atlas \
  --param team=engineering \
  --id-prefix onboard-
```

Full flags: [`template`](../reference/cli.md#aperture-template).

## `bulk` — many grants in one transaction

```text
aperture bulk <grant|revoke>
```

`bulk grant` applies a JSON **array** of grant bodies atomically — either all
land or none do — from `--json`, `--file`, or stdin. It prints `bulk grant <n> ok`.

```bash
bin/aperture bulk grant --account acme --json '[
  {"id":"g-a","accountId":"acme","subject":{"kind":"role","id":"analyst"},"permissionId":"perm-doc-read","object":"account:acme/project:atlas/**","effect":"allow"},
  {"id":"g-b","accountId":"acme","subject":{"kind":"role","id":"analyst"},"permissionId":"perm-doc-write","object":"account:acme/project:atlas/document:42","effect":"allow"}
]'
```

`bulk revoke` deletes many grants atomically by id. Ids come from repeated
`--grant` flags, positional arguments, or both; it prints `bulk revoke <n> ok`.

```bash
bin/aperture bulk revoke --account acme --grant g-a --grant g-b
# or positionally:
bin/aperture bulk revoke --account acme g-a g-b
```

Both `bulk` subcommands are account-admin tier.

Full flags: [`bulk`](../reference/cli.md#aperture-bulk).

## Related

- [Global options](global-options.md) — `--principal` / `--account` / `--seed` / `--store`.
- [Mutations](mutations.md) — single-entity `put` / `delete` and delegation.
- [Portability](portability.md) — export/import a whole model at once.
- [Command-Line Reference](../reference/cli.md) — the generated flag tables.
