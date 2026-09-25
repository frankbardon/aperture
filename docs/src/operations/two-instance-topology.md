# Two instances, one store

**Audience:** operators standing up a second Aperture instance against a database
that already has one.

This is the topology shared wiring exists for. Two processes — a host application
that owns the model and a second service that only asks questions — share one
store, hold one copy of the wiring, and must answer every question identically.
Getting there is four steps, and the order matters.

Throughout, the deployment is the house example: one account, `acme`, whose
objects are spelled `account:acme/project:atlas/document:42`.

## The shape

| | **Instance A** — the host | **Instance B** — the consumer |
|---|---|---|
| Command | `aperture serve --store <dsn> --seed model.yaml` | `aperture serve --store <dsn>` |
| Model state | applies it (its file is the source) | reads it (writes nothing on boot) |
| Shared wiring | authored it, pushed it | reads it out of the database |
| Local sections | `objects:`, `attributes:` from its own file | its own, or none at all |
| Connection routes | its own `connections:` block, or the environment | the environment |

Instance B is a **pure consumer**. There is nothing to build for it and nothing to
configure beyond its store DSN and its connection routes: `aperture serve --store
<dsn>` with no `--seed` boots, reads the model that is already there, reads the
wiring that is already there, and serves.

Both instances hold the whole decision engine. There is no "primary" — a decision
is resolved locally in each process against the shared store, so the pair scales
reads by adding processes and the two never talk to each other.

## 1. Apply the model once

Model state — accounts, principals, roles, groups, permissions, grants, rules,
object types — is applied by whoever owns the document, on one instance, and
nowhere else.

```bash
aperture serve --store 'postgres://…/acme' --seed model.yaml
```

`seed.Document.Apply` upserts the **entire** document, so two instances that both
carried a `--seed` would re-assert their own model over each other on every
restart, and the last one to boot would win. That is why **a durable `--store`
with no `--seed` seeds nothing** — see
[What an omitted `--seed` means](../cli/global-options.md#what-an-omitted---seed-means). Instance B's boot
writes no model rows at all.

## 2. Push the wiring once

Runtime wiring is not model state: `Apply` never writes it, and an export never
reproduces it. Four of its sections are nevertheless the same on every instance of
a deployment, so they get a home in the store:

```bash
aperture wiring push --store 'postgres://…/acme' --seed model.yaml
```

That writes `connections:` (names only), `providers:`, `field_types:` and
`attribute_providers:`. Read them back with `aperture wiring show`, snapshot them
to a file with `aperture wiring pull`, and compare a repository document against
what is deployed with `aperture wiring diff`. The flags and descriptions of the
whole command tree are in the generated
[Command-Line Reference](../reference/cli.md#aperture-wiring).

Push once, from one document. With wiring rows present, **the database is
authoritative** on every instance, including the one the push came from.

## 3. Give each instance its own connection routes

A shared connection carries its **name** and nothing else. No DSN, no credential,
not even the `dsn_env:` variable name — because which server, which credential and
how large a pool are per-instance facts, and a wiring row is copied to a second
host that resolves its own credentials.

So each instance routes each name itself, in one of three ways:

1. A Go host passes `seed.WithConnectionOpener` and builds the pool for the name
   itself. The documented seam, and the only one a library host needs.
2. This instance's own seed file declares a `connections:` entry under the same
   name. It is used verbatim — `dsn_env:`, pool sizes, `query_timeout:` — because a
   route is exactly what a local `connections:` entry is.
3. The conventional environment variable
   `APERTURE_CONNECTION_<NAME>_DSN`. Every character that is not a letter or digit
   becomes an underscore, so the connection named `main` reads
   `APERTURE_CONNECTION_MAIN_DSN`.

For instance B, which has no seed file, route 3 is the whole configuration:

```bash
export APERTURE_CONNECTION_MAIN_DSN='postgres://reader:…@replica/acme?sslmode=require'
aperture serve --store 'postgres://…/acme'
```

A name **no** route answers for refuses the boot with
`APERTURE_WIRING_CONNECTION_UNROUTED`, naming the connection and the variable it
wanted — every unrouted name in one refusal, so a new instance is configured in one
pass rather than one restart per connection.

## 4. Keep the two local sections local

Two sections of a seed document are **never** pushed, because they carry data
rather than a pointer to data:

- **`objects:`** — inline object metadata. `account:acme/project:atlas` and its
  `name`, `tier`, `reviewed_at`.
- **`attributes:`** — inline subject attribute bags. What `principal.department`
  and `account.plan` read.

They belong to the instance whose file lists them, and each instance keeps its own.
An instance that needs the metadata a rule reads must therefore either list it
inline, or reach it through a `providers:` entry — which is shared, and which
is the answer for anything two instances both need.

The same applies to any wiring only one machine can describe. A local file **may
add** an object type or an attribute slot the database never declared — a
`kind: csv` provider, whose data source is a filesystem path the shared tables have
no column for, or a provider a Go host registers itself — and it is built exactly as
it would be on an instance that has never been pushed to. What it may **not** do is
redeclare something the database already declares: that fails the boot with
`APERTURE_WIRING_LOCAL_COLLISION`, naming the entry and both sections. The
collision is refused rather than resolved because either precedence is silent and
either one changes what a decision reads. See
[With both, the database wins and the file may only ADD](../cli/global-options.md#with-both-the-database-wins-and-the-file-may-only-add).

## An instance missing a declared provider refuses to boot

This is the part to read before deciding an instance is "mostly configured".

If the shared wiring declares something this instance cannot turn into a working
provider — a `kind:` no second host can construct from a row, a connection name it
has no route for, an incomplete statement set, an unparseable `ttl:`, a
`references:` target nothing serves, a `field_types:` word outside
`date`/`datetime` — the process **exits non-zero at startup**. It does not skip the
entry, it does not serve the types it happened to manage, and it does not wait to
find out on the first decision that needed the data.

Refusing is not the tidier option; it is the only one that is not silent.

- An object type with no working provider does not *error* on a decision. A rule
  reading `object.tier` reads a **missing path**, every predicate over it is false,
  and the grant denies. Nothing in the verdict says a provider was missing.
- An attribute slot with no working provider is worse. The
  [leniency contract](../concepts/providers.md) collapses a provider failure to an
  **empty bag**, and an empty bag does not deny — a rule that *excluded* on an
  attribute it can no longer see stops excluding, so the grant **widens**. Nothing
  in the verdict, the trace or the notes says so.
- Failing per-decision instead would be the worst of the three: a misconfigured
  instance would keep serving traffic for the types it could reach, so the pair
  would answer one question two ways, and the only observable symptom would be a
  verdict that differs by which instance the request landed on.

A boot that fails is a deployment that fails, in front of whoever is deploying it,
with a coded error naming the entry and a fixup naming the remedy. A boot that
degrades is an authorization difference nobody is looking for. That is the trade,
and it is not configurable.

## Verifying the pair agrees

The claim that the two instances decide identically is a test, not a hope:
`internal/cli/wiring_identical_test.go` builds two decision stacks over one store —
one wired from the database rows, one from the equivalent seed file — and diffs
`Check`, `Enumerate`, `Search` and `Explain` across fixtures that read object
metadata, both attribute roots and a shared `field_types:` declaration. It runs
under `make test` against the in-memory and SQLite backends, and under the gated
live run against a real PostgreSQL server (see
[Build, test & lint gates](../contributing/gates.md)).

Operationally, three commands answer the three questions worth asking of a pair:

| Question | Command |
|---|---|
| What wiring is deployed? | `aperture wiring show --store <dsn>` |
| Does it match the document in the repository? | `aperture wiring diff --store <dsn> --seed model.yaml` |
| Which attribute slots did *this* instance actually wire? | `aperture attributes slots --seed <file>` |

Run the last one on **each** instance. It restates what that process resolved,
which is the one thing a shared store cannot tell you.
