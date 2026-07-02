# Portability

**Audience:** administrators moving or backing up a whole Aperture model.

`export` and `import` are the declarative-state commands: `export` serializes the
entire model to a single JSON/YAML state file, and `import` applies such a file
back as an idempotent, transactional upsert. Both are **system-admin tier** and
drive exactly the path the Twirp `Export` / `Import` RPCs drive, so they need an
acting principal via `--principal` (or `APERTURE_PRINCIPAL`) and `--account` for
authority resolution:

```bash
export APERTURE_PRINCIPAL=root
```

## `export` — serialize the whole model

```text
aperture export [options]
```

`export` writes the full model to stdout by default, or to `--out <path>`. The
format is JSON unless you pass `--format yaml` (or `--out` a `.yaml`/`.yml`
path). Writing to a file prints a one-line summary; writing to stdout emits the
raw document.

```bash
# To stdout as JSON:
bin/aperture export --account acme --principal root > model.json

# To a YAML file (format inferred from the extension):
bin/aperture export --account acme --principal root --out model.yaml
```

```text
exported <model summary> -> model.yaml
```

Full flags: [`export`](../reference/cli.md#aperture-export).

## `import` — apply a state file

```text
aperture import [options]
```

`import` reads a state file from `--file <path>` or, when no file is given, from
stdin (treated as JSON). It applies the document as an **idempotent** upsert in
one transaction — re-importing the same file is a no-op — and prints a one-line
summary. The file format is inferred from the `--file` extension.

```bash
# From a file:
bin/aperture import --account acme --principal root --file model.yaml

# From stdin (JSON):
bin/aperture export --account acme --principal root \
  | bin/aperture import --account acme --principal root
```

```text
imported <model summary>
```

Round-tripping through `export | import` — or between two `--store` DSNs — is the
supported way to snapshot, back up, or migrate a model. Because both ends run as
a system-admin actor scoped to `--account`, no cross-account data crosses the
boundary implicitly.

Full flags: [`import`](../reference/cli.md#aperture-import).

## Related

- [Global options](global-options.md) — `--principal` / `--account` / `--store` for the source/target store.
- [Provisioning](provisioning.md) — apply incremental grants rather than a whole model.
- [Mutations](mutations.md) — single-entity edits.
- [Command-Line Reference](../reference/cli.md) — the generated flag tables.
