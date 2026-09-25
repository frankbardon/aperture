# wiring

**Audience:** operators who deploy Aperture, and anyone answering "is what is
deployed what is in the repository?".

`aperture wiring` is the operator's window onto **shared wiring**: the part of a seed
document that belongs to the *deployment* rather than to one instance. Push it once
and every instance sharing that database reads the same wiring — including an instance
that has no seed file at all.

Four subcommands, and the tree is complete:

| Command | Question it answers |
|---|---|
| [`push`](#push--deploy-a-documents-shared-wiring) | make the deployment's wiring this document's four shared sections |
| [`show`](#show--read-what-is-deployed) | what is deployed, in words |
| [`pull`](#pull--snapshot-what-is-deployed-to-a-file) | what is deployed, as a file a push accepts unchanged |
| [`diff`](#diff--compare-the-deployment-against-the-repository) | does the deployment match the repository |

All four take `--store` and **no acting principal**. The store credential is the
authority, exactly as it is for [`import`](portability.md), and the three read
commands restate wiring that credential already grants full write access to. `--store`
has no default on any of them: an in-memory store is private to the process that
opened it, so there is nothing shared about its wiring to write or to read.

Throughout, the deployment is the house example: one account, `acme`, whose objects
are spelled `account:acme/project:atlas/document:42`.

## Four sections are shared and two are local

A seed document's ten model-state sections are applied by `--seed` (or
[`import`](portability.md)). Its **six wiring sections** say where a decision reads
object metadata and subject attribute bags *from*, and they split four/two:

| Section | Home |
|---|---|
| `connections:` | **shared** — the manifest of connection **names**, and nothing else |
| `providers:` (with its `references:`) | **shared** |
| `field_types:` | **shared** |
| `attribute_providers:` | **shared** |
| `objects:` | **local** to the instance whose file lists it |
| `attributes:` | **local** to the instance whose file lists it |

The line is **a pointer to data versus the data itself**. A `providers:` or
`attribute_providers:` entry says *where* to read metadata or an attribute bag from; a
`field_types:` entry says *how* to read a field; a `connections:` name says *which*
pool an entry cites. All four describe the deployment and all four are safe to copy to
a second host. An `objects:` entry carries the metadata and an `attributes:` entry
carries the bag — and Aperture's own database is never the source of truth for a
host's domain data. Those two are not waiting for a later release; there is nowhere
for them to go.

Wiring is also **not model state**, and the two are pushed by different commands.
`aperture export` emits the model and no wiring; `aperture wiring pull` emits the
wiring and no model. Wiring for a model that is not there is refused rather than
stored — see [`push`](#push--deploy-a-documents-shared-wiring).

## What is never stored

No DSN, no credential, **not even the name of the environment variable** holding one,
and no filesystem path. This is a property of the schema — there is no column for any
of it — rather than of a validator, and it is the reason the surface is safe at all:

- A row is **copied to a second instance** that resolves its own credentials, so a
  secret column would put a credential in every backup of this database.
- A path column would be a **guess about another host's disk**. A relative `path:` is
  resolved against the seed *file's* directory, which database-sourced wiring has none
  of.

`kind: csv` is therefore refused by `push` (`APERTURE_WIRING_KIND_UNSHAREABLE`), and
stays **perfectly legal in a local seed file** — the instance that reads the path is
the instance the path belongs to.

A shared connection carries its **name**, and each instance supplies its own
**route** for that name: `seed.WithConnectionOpener` in a Go host, a `connections:`
entry under the same name in this instance's own seed file, or the conventional
`APERTURE_CONNECTION_<NAME>_DSN`. That is why a pulled document comes back with an
empty `dsn_env:` for every connection: it is **re-pushable but not bootable** until
the routes are filled in. The three routes are set out in full under
[It means the same thing for *wiring*](global-options.md#it-means-the-same-thing-for-wiring).

## `push` — deploy a document's shared wiring

```text
aperture wiring push --store <dsn> --seed <file>
```

Reads `providers:`, `field_types:`, `connections:` and `attribute_providers:` out of
`--seed`, validates every entry, and **replaces** the store's wiring with them in one
transaction. The document's other sections are not read: no model state is applied,
and the two local wiring sections are untouched.

```bash
bin/aperture wiring push --store 'postgres://…/acme' --seed model.yaml
```

**The push is all or nothing.** Every rule is checked before anything is written, so a
refusal leaves the deployed wiring exactly as it was. Wiring is only meaningful whole
— an entry citing a connection the manifest does not list is not half-valid wiring, it
is broken wiring — and an instance booting against a half-written set would build a
registry missing exactly the entries whose write failed, while reporting nothing.

**It replaces rather than merges.** An entry dropped from the document is dropped from
the store. Push the whole wiring every time. There is deliberately no per-row edit.

A push is refused when:

| Refusal | Because |
|---|---|
| `APERTURE_WIRING_NO_MODEL_STATE` | the store holds no model state at all, so there is nothing for the wiring to be wiring *for*. A typo'd DSN opens — and `Setup` creates — a perfectly valid, perfectly empty database, and without this the wiring lands there and the instance that serves decisions never sees it. |
| `APERTURE_WIRING_KIND_UNSHAREABLE` | an entry selects `kind: csv`, whose only data source is a machine-local path. |
| `APERTURE_WIRING_OBJECT_TYPE_UNKNOWN` | a provider entry serves an `object_type` the store has no row for. The refusal names the type. |
| `APERTURE_WIRING_CONNECTION_UNDECLARED` | an entry cites a `connection:` the pushed manifest does not declare. The refusal names it and lists the ones that were. |
| `APERTURE_SQL_PROVIDER_DSN_LITERAL` | anything carries a literal `dsn:`. Only `dsn_env:`, a variable *name*, is ever accepted — and shared wiring stores neither. |

A push is **audited**: one `WiringPush` mutation record against
`model.AccountWildcard`, carrying the per-section counts and the deployed connection
names, object types and attribute slots. All of that is deployment-wide wiring
vocabulary; none of it is account data.

Full flags: [`wiring push`](../reference/cli.md#aperture-wiring-push).

## `show` — read what is deployed

```text
aperture wiring show --store <dsn>
```

Prints the five shared-wiring tables, one table per section, so "what is this
deployment actually wired to?" is answerable without a database client. It takes no
`--seed`: a listing that merged the document on the command line would answer "what
*would* a push deploy?" while looking like it answered this question.

```bash
bin/aperture wiring show --store 'postgres://…/acme'
```

**An empty store is an answer, not an error.** A store nothing has been pushed to says
so plainly, and every instance booting against it builds its wiring from its own
`--seed` file instead. It is also what a mistyped `--store` looks like, because a DSN
naming a database that does not exist yet is one `Setup` creates — so check the DSN
before concluding a push was lost.

Two columns are spelled as words rather than left blank. `ttl` and `max-size` read
`(default)` when the entry sets neither, because the registry's own default applies and
`0` would read as "caches nothing". The declared key set distinguishes three states:
`(not declared)` is a slot that opted out of key enforcement entirely,
`(declared empty)` is a slot that opted **in** and permits no keys at all, and a list
is the keys the slot guarantees.

Full flags: [`wiring show`](../reference/cli.md#aperture-wiring-show).

## `pull` — snapshot what is deployed to a file

```text
aperture wiring pull --store <dsn> --out <path> [--format json|yaml] [--force]
```

Reads the deployed wiring in **one atomic snapshot** and writes it as the same four
sections a push reads. The file is a seed document `push` accepts unchanged, so
`push` → `pull` → `push` deploys identical wiring, and two pulls of an unchanged
deployment are byte-identical.

```bash
bin/aperture wiring pull --store 'postgres://…/acme' --out deployed.yaml
diff -u model.yaml deployed.yaml
```

`show` is the better command for *reading*; this one produces a file for a tool. Three
behaviours are worth knowing before you script it:

- **The file is re-pushable but not bootable.** Every connection comes back with an
  empty `dsn_env:`, because shared wiring holds a name and nothing else. Fill the
  routes in from your own deployment's environment before booting an instance from the
  file; one built from it as written refuses at registry build and names the unset
  variable.
- **An existing `--out` file is refused** unless `--force` is given
  (`APERTURE_WIRING_OUTPUT_EXISTS`). The likeliest thing at that path is the
  version-controlled document the pull is meant to be compared with. The path is
  checked before the store is opened, so the refusal reads nothing.
- **A store with no wiring deployed is refused**
  (`APERTURE_WIRING_NOTHING_DEPLOYED`) — the one place `pull` disagrees with `show`. A
  pull's file exists to be pushed back, and an empty one pushed back replaces the
  deployment's wiring with nothing.

Full flags: [`wiring pull`](../reference/cli.md#aperture-wiring-pull).

## `diff` — compare the deployment against the repository

```text
aperture wiring diff --store <dsn> --seed <file>
```

Names, per section and per entry, what is only deployed, what is only local, and what
differs. The `section` column is the document's own section key, so a reported entry is
one you can go and find. Nothing is written, to the store or to a file.

```bash
bin/aperture wiring diff --store 'postgres://…/acme' --seed model.yaml
```

**It is meant for a pipeline**, and the exit codes are the interface:

| Exit | Meaning |
|---|---|
| `0` | the deployment matches the repository |
| `2` | it does not |
| `1` | I could not tell — an unreadable document, a `--store` that names nothing, a schema this build cannot read |

Drift is the **answer** to the question asked, not a failure to answer it, so it
carries **no `APERTURE_*` code** — there is no remedy for a report, and which side is
wrong is your call. It follows [`check`](decisions.md)'s precedent: a clean `DENY`
prints its verdict and exits non-zero, because a verdict is not an error either.

**The code is 2 and not 1, and that is the useful half.** Every coded refusal this
binary makes exits 1, so a gate that failed closed on 1 and 2 alike would report drift
for a typo in its own DSN.

Two more properties keep a gate green when it should be:

- **Formatting is never drift.** Both sides are compared as *wiring*, in the one
  canonical order a read returns, so re-ordering the document's entries, re-ordering a
  `fields:` or `references:` map, or re-indenting the file changes nothing. The push
  timestamps are excluded too — a stamp is a fact about the last push, not about the
  wiring.
- **A `kind: csv` entry is reported as local-only by design**, not as drift and not as
  an error. No push can make the deployment match it, so counting it as drift would
  leave a permanently red gate on every deployment that legitimately keeps a local csv
  provider — and a gate that cannot go green is a gate somebody switches off.

Make the deployment agree with the document by pushing it.

Full flags: [`wiring diff`](../reference/cli.md#aperture-wiring-diff).

## Booting against pushed wiring

An instance reads the shared wiring at startup, and what it does with what it finds is
documented once, under [Global options](global-options.md):

- [Empty tables mean "use the local file"](global-options.md#it-means-the-same-thing-for-wiring)
  — an answer, not a failure, and the state of every deployment that has never pushed.
- [With both, the database wins and the file may only ADD](global-options.md#with-both-the-database-wins-and-the-file-may-only-add)
  — a collision is refused in *both* directions with
  `APERTURE_WIRING_LOCAL_COLLISION`, because either precedence is silent and either
  one changes what a decision reads.
- [An instance will not start on wiring it cannot construct](global-options.md#an-instance-will-not-start-on-wiring-it-cannot-construct)
  — refusing to boot beats degrading, because a missing provider does not *error* on a
  decision and a missing attribute bag **widens** an exclusive grant.
- [The wiring is read once, unless you ask for more](global-options.md#the-wiring-is-read-once-unless-you-ask-for-more)
  — `serve --wiring-poll` is **opt-in and off unless configured**; see
  [Noticing a push without a restart](serve.md#noticing-a-push-without-a-restart).

## Related

- [Two instances, one store](../operations/two-instance-topology.md) — the four steps
  of standing up a second instance against a database that already has one.
- [Seed & portability](../concepts/seed.md#the-file-is-not-the-only-home-for-wiring) —
  the six sections, and the three read-backs that answer three different questions.
- [Storage](../concepts/storage.md#the-five-shared-wiring-tables) — the five
  `apt_wiring_*` tables and what they may never carry.
- [Global options](global-options.md) — `--seed` / `--store`, and what an omitted
  `--seed` means for wiring.
- [Portability](portability.md) — `export` / `import`, the model half of the same
  story.
- [Command-Line Reference](../reference/cli.md#aperture-wiring) — the generated
  per-command flag tables.
