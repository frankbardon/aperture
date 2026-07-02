# First decision (CLI)

This walkthrough makes real access-control decisions with the `aperture` binary.
It needs nothing beyond the build from [Installation](installation.md): every
command below runs against Aperture's **embedded example model** — a
self-contained fixture for the account `acme` — so there is no store to set up
and no data to load.

## The example model

When you do not pass `--seed`, `aperture check` loads the committed example
fixture. It models one tenant, `acme`, with a project `atlas` full of documents,
and three grants:

| Grant | Subject | Effect | Object pattern |
|---|---|---|---|
| `g-eng-read-atlas` | group `engineering` | allow `read` | `account:acme/project:atlas/**` |
| `g-editor-write-atlas` | role `editor` | allow `write` | `account:acme/project:atlas/**` |
| `g-deny-secret-read` | group `engineering` | deny `read` | `account:acme/project:atlas/document:secret` |

`alice` is an editor in engineering; `bob` is a viewer in engineering. That is
enough to see an allow, a default deny, and a deny-override.

## The command

```text
aperture check <principal> <action> <object>
```

`check` takes three positional arguments and prints a one-word verdict plus a
reason. The process **exit code carries the verdict** — `0` for allow, non-zero
for deny — so a check composes in a shell pipeline
(`aperture check … && deploy`).

Relevant flags:

| Flag | Default | Purpose |
|---|---|---|
| `--seed` | embedded example | Path to a JSON/YAML seed model to decide against. |
| `--account` | `acme` | The active account the decision is scoped to. |
| `--store` | in-memory | SQLite DSN for a persistent backing store. |

The full flag reference lives in the CLI chapter later in this book; the three
above are all this walkthrough needs.

## Allow: engineering reads a document

`alice` is in `engineering`, and `g-eng-read-atlas` lets engineering read
everything under `atlas`:

```bash
bin/aperture check alice read account:acme/project:atlas/document:42
```

```text
allow
reason: allowed by grant g-eng-read-atlas (allow account:acme/project:atlas/**) at specificity 39300; 1 matching grant(s) considered
```

The command exits `0`. The reason names the deciding grant and its
**specificity** — a broad `**` pattern scores low.

## Allow: an editor writes

`alice` also holds the `editor` role, so `g-editor-write-atlas` permits a write:

```bash
bin/aperture check alice write account:acme/project:atlas/document:42
```

```text
allow
reason: allowed by grant g-editor-write-atlas (allow account:acme/project:atlas/**) at specificity 39300; 1 matching grant(s) considered
```

## Default deny: no grant matches

`bob` is only a viewer — nothing grants him write — so the decision falls through
to a fail-closed default deny:

```bash
bin/aperture check bob write account:acme/project:atlas/document:42
```

```text
deny
reason: default deny: no grant matched action "write" on "account:acme/project:atlas/document:42" for principal "bob" in account "acme"
```

The command exits non-zero. A decision with no matching grant is always a deny —
Aperture fails closed.

## Deny by override: specificity wins

`g-deny-secret-read` seals one document. Even though `g-eng-read-atlas` would
allow the read, the deny is more specific and overrides it:

```bash
bin/aperture check alice read account:acme/project:atlas/document:secret
```

```text
deny
reason: denied by grant g-deny-secret-read (deny account:acme/project:atlas/document:secret) at specificity 60300; 2 matching grant(s) considered
```

Two grants matched; the deny at specificity `60300` outranks the allow at
`39300`. This is deny-overrides by specificity — see the
[Concepts primer](concepts.md).

## Why? — `explain`

`check` gives the verdict; `explain` gives the whole trace. It takes the same
three arguments:

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

The `*` marks the deciding grant. `explain` is a first-class operation, not a
debug afterthought — every decision is auditable by construction.

## A note on the acting principal

For `check`, `enumerate`, and `explain`, the principal is a positional argument
— the *subject* of the question. The **write** commands (`put`, `bestow`,
`revoke`, `impersonate`, …) are different: they need to know *who is acting*, and
take that principal from the `--principal` flag or the `APERTURE_PRINCIPAL`
environment variable. For example, `bestow` reads the delegating principal from
`APERTURE_PRINCIPAL`:

```bash
export APERTURE_PRINCIPAL=alice
bin/aperture bestow --help
```

Setting `APERTURE_PRINCIPAL` once in your shell saves passing the flag to every
mutation. The read decisions above never consult it — they ask about a principal
you name explicitly.

## Where to go next

- [Library quickstart](library-quickstart.md) — make the same decision from Go.
- [Concepts primer](concepts.md) — the vocabulary behind the verdicts.
- The CLI section of this book documents every command and flag in full.
