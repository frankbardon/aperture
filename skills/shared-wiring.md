---
name: shared-wiring
description: The shared-wiring contract — the four of a seed document's six wiring sections that live in the deployment's database (`connections:` as names only, `providers:`, `field_types:`, `attribute_providers:`) and the two that stay file-local (`objects:`, `attributes:`), the `aperture wiring push` / `show` / `pull` / `diff` command tree and why it is CLI-only, what a stored row may never carry (no DSN, no credential, not even a `dsn_env:` variable name, no filesystem path — which is why `kind: csv` is refused), the connection NAME versus the three per-instance ROUTES, empty tables meaning "use the local file", the merge authority where the database wins and a local file may only ADD with a collision refused in both directions, why an instance that cannot honour the shared wiring refuses to boot rather than degrading, `ReplaceWiring` being all-or-nothing with every read in canonical order and no per-row write, `declared_keys` telling "not declared" from "declared empty", and `wiring diff` exiting 2 on drift with no `APERTURE_*` code — plus the HOT-SWAP contract for a running instance: opt-in polling that defaults off, a version rebuilt and installed whole with one atomic pointer store so a request pins one version and never blocks on a wiring read, the connection NAME SET frozen for the life of a process with a name-set-changing push held whole (`APERTURE_WIRING_RESTART_REQUIRED`), and a failed refresh keeping last-good wiring while alarming loudly through the system-admin-gated wiring posture read (`APERTURE_WIRING_REFRESH_FAILED`).
applies_to: [cli, library]
---

# Shared wiring

A seed document carries two kinds of section. Ten are **model state** — who exists
and who may do what — and six are runtime **wiring**: where a decision reads object
metadata and subject attribute bags *from*. `seed.Document.Apply` writes the model
state and has never written any wiring, and `seed.Export` reproduces the model and
has never reproduced any wiring. Neither of those facts has changed.

What has changed is that the seed **file** is no longer the only home for wiring.
Four of the six sections belong to the **deployment** rather than to one instance,
and they now live in the database every instance of that deployment already shares.

## The four/two split

| Section | Home | Shared by | Read back by |
|---|---|---|---|
| `connections:` | **shared** — the manifest of **names**, and nothing else | `aperture wiring push` | `aperture wiring pull` |
| `providers:` (with its `references:`) | **shared** | `aperture wiring push` | `aperture wiring pull` |
| `field_types:` | **shared** | `aperture wiring push` | `aperture wiring pull` |
| `attribute_providers:` | **shared** | `aperture wiring push` | `aperture wiring pull` |
| `objects:` | **local** to the instance whose file lists it | nothing | nothing |
| `attributes:` | **local** to the instance whose file lists it | nothing | nothing |

The line is **a pointer to data versus the data itself**, and it is not a staging
order. A `providers:` or `attribute_providers:` entry says *where* metadata or an
attribute bag is read from; a `field_types:` entry says *how* to read a field; a
`connections:` name says *which* pool an entry cites. All four are facts about the
deployment and all four are safe to copy to a second host. An `objects:` entry
carries the metadata and an `attributes:` entry carries the bag, and Aperture's own
database is never the source of truth for a host's domain data — the same Non-Goal
that keeps the provider cache out of an export. The two local sections are not
waiting for a later story; there is nowhere for them to go.

Three read-backs exist, and they answer three different questions:

| Read-back | Emits | Surface |
|---|---|---|
| `seed.Export` / `aperture export` | the **model**, no wiring | library, CLI, and Twirp with an admin-tier token |
| `seed.MarshalWiring` / `aperture wiring pull` | the four **shared wiring** sections, no model | CLI only, gated by the store credential |
| — | nothing emits `objects:` or `attributes:` | — |

Keeping them apart is deliberate. Export is reachable over the network with a token,
so it must emit no deployment configuration; the wiring read-back requires the store
credential at a shell, which is the authority the change it describes actually needs.

## The five tables, and the command tree

The rows live in the five `apt_wiring_*` tables. Their columns, their canonical
ordering and the `ReplaceWiring` / `GetWiring` surface are
`skills/storage-schema.md`'s subject ("The five shared-wiring tables", "The wiring
read and write surface"); this document is about the **contract** over them.

| Command | Question it answers |
|---|---|
| `aperture wiring push --store <dsn> --seed <file>` | make the deployment's wiring this document's four shared sections |
| `aperture wiring show --store <dsn>` | what is deployed, in words, one table per section |
| `aperture wiring pull --store <dsn> --out <path>` | what is deployed, as a file a push accepts unchanged |
| `aperture wiring diff --store <dsn> --seed <file>` | does the deployment match the repository |

**There is no RPC method, no MCP tool and no admin-UI panel**, and that is a scope
decision rather than an unfinished edge. A push changes what *every* decision in
*every* instance of the deployment sees, at once: it is the shape of change that
should require a human at a shell holding both the store credential and the
document. MCP in particular is read + decide + simulate only, and always will be.

None of the four takes an actor. The `--store` credential is the authority, exactly
as it is for `aperture import`, and `show` / `pull` / `diff` restate wiring that
credential already grants full write access to — so gating them would mean nobody
could diagnose "is anything even deployed?" without already holding the authority the
diagnosis explains. That is the same reason `aperture attributes slots` is ungated. A
push is nevertheless **audited**: `Action: "WiringPush"`, account
`model.AccountWildcard`, target `wiring:shared`, with the per-section counts and the
deployed connection names, object types and attribute slots in `Details`. All of that
is deployment-wide wiring vocabulary and none of it is account data.

`--store` has no default on any of the four: an in-memory store is private to the
process that opened it, so there is nothing shared about its wiring to write or to
read. `show` takes no `--seed`, deliberately — a listing that merged the document on
the command line would answer "what *would* a push deploy?" while looking like it
answered "what is deployed?".

## A row carries no secret and no path

This is the load-bearing absence, and it is a property of the schema rather than of a
validator: there is **no column** for a DSN, a credential, a `dsn_env:` variable
*name*, or a filesystem path, and there is not going to be one.

- A row is **copied to a second instance** that resolves its own credentials. A
  secret column would therefore put a credential in every backup of this database
  and give the wiring surface a rotation problem it has no business having.
- A path column would be a **guess about another host's disk**. A relative `path:`
  is resolved against the seed *file's* own directory, which database-sourced wiring
  has none of; an absolute one is an assertion about a filesystem this instance
  cannot see.

`kind: csv` is refused at the push for exactly that reason
(`APERTURE_WIRING_KIND_UNSHAREABLE`), because its only data source is a path and an
entry stored pathless would read back as wiring and then serve nothing. It stays
**perfectly legal in a local seed file**, where the instance that reads the path is
the instance the path belongs to. The same code is raised again on the **boot** that
reads such a row back — written by hand, or by an older build — because without that
half the row reaches `seed`'s own builder and is refused for the missing `path:`, a
true statement whose remedy cannot be carried out.

## The name is shared; the route is per-instance

`apt_wiring_connections` holds one row per connection **name**, and nothing else.
Everything else a `connections:` entry declares — `dsn_env:`, the pool bounds,
`query_timeout:` — is that instance's **route** for the name, and a route is a
per-instance fact: two instances may legitimately reach one logical database through
different hosts, credentials and pool sizes.

Three routes answer for a name, and they are local to each instance:

1. **`seed.WithConnectionOpener`** — a Go host builds the pool for the name itself.
   The documented seam, and the **only one a library host needs**.
2. **A `connections:` entry under the same name in this instance's own seed file** —
   used verbatim. A local entry under a shared name is that name's *route* and not a
   competing declaration, which is why `connections:` is the one section a local
   entry may restate without colliding.
3. **`APERTURE_CONNECTION_<NAME>_DSN`** — every character that is not a letter or a
   digit becomes an underscore, so `main` reads `APERTURE_CONNECTION_MAIN_DSN`. It is
   the **CLI's route of last resort, not a second mechanism**.

A shared name no route answers for refuses the **boot** with
`APERTURE_WIRING_CONNECTION_UNROUTED`, naming the connection and the variable the
conventional route would have read — **every** unrouted name in one refusal, so a new
instance is configured in one pass rather than one restart per connection.

That is why a pulled document comes back with an empty `dsn_env:` for every
connection: it is **re-pushable but not bootable** until the routes are filled in.
The asymmetry is the security rule made visible, not a defect in the pull.

`APERTURE_WIRING_CONNECTION_UNROUTED` and `APERTURE_WIRING_CONNECTION_UNDECLARED`
are opposite halves of one question and must not be merged. The *undeclared* one is a
**push-time** refusal — an entry cited a connection the pushed manifest does not
list, which is a mistake in the document and the same mistake on every instance. The
*unrouted* one is a **boot-time** refusal — the manifest lists the name perfectly
well and *this host* has nowhere to point it, which is routinely true on one instance
of a fleet and false on its peers.

## Empty tables mean "use the local file"

An empty `model.WiringSet` is an **answer**, not a failure: `WiringSet.IsEmpty()`
reports it, and the boot path treats it as "the local seed file's wiring is used
exactly as it always was". There is no flag for this and nothing to configure.

Every deployment that has never run `aperture wiring push` is in that branch, which
is why turning it into a refusal — or adding a flag to select it — would break every
existing single-instance deployment at once.

`aperture wiring show` says so plainly for the same reason. `aperture wiring pull`
is the one command that **refuses** an empty store
(`APERTURE_WIRING_NOTHING_DEPLOYED`), because the file it would write is a file whose
purpose is to be pushed back, and an empty one pushed back replaces the deployment's
wiring with nothing. `aperture wiring diff` writes no file and pushes nothing, so it
reports every local entry as only-local and says that nothing is deployed. In all
three cases an empty read is **also** exactly what a mistyped `--store` naming a
database `Setup` just created looks like, so check the DSN before concluding a push
was lost.

## With both, the database wins and the file may only ADD

An instance can have wiring rows *and* a `--seed` file, and most will.

- The **database is authoritative**. Every pushed entry is built.
- The **local file may ADD** an object type or an attribute slot the database never
  declared. It is built exactly as it would be on an instance that has never been
  pushed to, `kind: csv` and all.
- A local entry for an object type or slot the database **already declares** fails
  the boot with `APERTURE_WIRING_LOCAL_COLLISION`, naming the entry and both
  sections.

**Additive is what makes the surface usable at all.** A Go host's hand-written object
providers are code no document can describe — Arc registers `wave` and `metric`
itself — so "the database is the only source" would mean such a host could never read
a pushed wiring. A local `kind: csv` provider is the same case in YAML: a path cannot
*be* shared wiring, so it has to stay addable.

**A collision is refused in both directions, never resolved.** Either precedence is
silent and either one changes what a decision reads: letting the database win
discards wiring somebody checked into this instance's file, and letting the file win
means one instance in a fleet answers from a source its peers cannot see. Neither
surfaces as an error on any later decision — it surfaces as **a different verdict**,
on an instance that looks identically configured. Fix it by deleting the local
declaration, or by pushing a document that omits the shared one.

The rule is about the **registry**, not about the syntax that declared an entry. A Go
`Register` call passes through no document, so its collision is refused by
`provider.Registry.Register` / `AttributeRegistry.Register`'s own duplicate check
(`APERTURE_PROVIDER_INVALID` / `APERTURE_ATTRIBUTE_PROVIDER_INVALID`) — one arbiter,
two messages, because the remedies differ: a coded wiring collision names two
configuration sources an operator can edit, where the registry's names a duplicate
registration a developer has to remove.

For an attribute slot the two sources are not merely refused-or-accepted: a shared
`attribute_providers:` entry and a local `attributes:` block are the slot's **two
registration layers**, and `skills/attribute-providers.md` owns which layer wins
which key.

## An instance that cannot honour the shared wiring refuses to boot

Three things in the shared wiring can be beyond what *this* instance can construct,
and all three fail the boot rather than being skipped: a `kind:` no second host can
build from a row (today `kind: csv`), a connection name with no route, and the
ordinary seed-builder refusals — an incomplete statement set, an unparseable `ttl:`,
a `references:` target no provider serves, a `field_types:` word outside
`date`/`datetime`.

Degrading is not the gentler option; it is the silent one.

- An object type with **no working provider** does not *error* on a decision. A rule
  reading `object.tier` reads a **missing path**, every predicate over it is false,
  and the grant denies. Nothing in the verdict says a provider was missing.
- An attribute slot with no working provider is **worse**. The leniency contract
  collapses a provider failure to an empty bag, and an empty bag does not deny — a
  rule that *excluded* on an attribute it can no longer see stops excluding, so the
  grant **widens**. Nothing in the verdict, the trace or the notes says so.
- Failing **per decision** would be the worst of the three: a misconfigured instance
  would keep serving traffic for the types it could reach, so the pair would answer
  one question two ways and the only symptom would be a verdict that differs by which
  instance the request landed on.

A boot that fails is a deployment that fails, in front of whoever is deploying it,
with a coded error naming the entry. That is the trade, and it is not configurable.

## The wiring is written and read whole

`model.Storage` has **one** wiring write, `ReplaceWiring(ctx, model.WiringSet)`, and
it is **all or nothing**. Every rule is checked before anything is written and the
write replaces the whole set in one transaction, so a refusal leaves the deployed
wiring exactly as it was. Wiring is only meaningful whole — an entry citing a
connection the manifest does not list is not half-valid wiring, it is broken wiring —
and an instance booting against a half-written set would build a registry missing
exactly the entries whose write failed, while reporting nothing.

It follows that a push is **REPLACE and not merge**: an entry dropped from the
document is dropped from the store. Push the whole wiring every time.

There is deliberately **no per-row `Put` or `Delete`**. Adding one makes "the
deployment's wiring" something a caller can leave half-applied, which is the state
the all-or-nothing write exists to make unreachable.

Every read returns **canonical order**, which is what makes a `pull` re-pushable byte
for byte and a `diff` report stable. Formatting is therefore never drift: both sides
of a diff are compared as `model.WiringSet` values, so re-ordering the document's
entries, re-ordering a `fields:` or `references:` map, or re-indenting the file
changes nothing in the report. The push timestamps are excluded too — a stamp is a
fact about the last push and not about the wiring.

`model.DeclaredKeys` stays a **struct with an explicit `Declared` bit**, and
`declared_keys` distinguishes `''` (not declared) from `'[]'` (declared empty). A
nil-versus-empty slice expresses the same difference and loses it silently through a
clone or through `encoding/json`, and losing it opts a slot out of the key
enforcement it asked for. `aperture wiring show` prints all three states as words —
`(not declared)`, `(declared empty)`, or the list — because two of them are different
answers a blank column would merge.

## `wiring diff` exits 2, and carries no code

Drift is the **answer** to the question asked, not a failure to answer it. So it
carries no `APERTURE_*` code: a coded error means "Aperture could not do what you
asked, and here is the remedy", and there is no remedy for a report. It follows
`aperture check`'s clean-deny precedent exactly — a `DENY` prints its verdict and
returns a non-zero exit, because a verdict is not an error either.

| Exit | Meaning |
|---|---|
| `0` | the deployment matches the repository |
| `2` | it does not |
| `1` | I could not tell — an unreadable document, a `--store` that names nothing, a schema this build cannot read |

**The code is 2 and not 1, and that is the useful half.** Every coded refusal this
binary makes exits 1, so signalling drift with 1 would merge "they differ" into "I
could not tell whether they differ" — and a pipeline gate that fails closed on drift
would then fail closed on a typo'd DSN and report it *as* drift.

A `kind: csv` entry is **reported as local-only by design**, not as drift and not as
an error: no push can ever make the deployment match it, so counting it as drift would
leave a permanently red gate on every deployment that legitimately keeps a local csv
provider, and a gate that cannot go green is a gate somebody switches off.

A diff compares **wiring sets** and never rendered documents. The deployed side comes
from the same atomic `GetWiring` snapshot a `pull` takes; the local side goes through
the same projection a `push` takes. Two notions of "the same wiring" would be two
answers to one question, and the one that is wrong is whichever the operator did not
run.

## Noticing a push without a restart

The wiring read happens at startup and, by default, **only** at startup. A push from
another host is picked up by restarting the instances, which is what a deploy pipeline
does anyway and is the whole story for a single-instance deployment.

A long-lived `aperture serve` can be told to notice a push instead, with
`--wiring-poll` or `APERTURE_WIRING_POLL`. It is **opt-in and off unless
configured**, and off means no background reader and no periodic query, so an
instance that cannot use it pays nothing for it. It is declared on `serve` and not on
every command that decides, because it configures a process that outlives a decision
and there is no tick in the life of an `aperture check` for one to happen on.

The interval vocabulary, how a change is detected, and what a tick costs are all on
`docs/src/cli/serve.md` ("Noticing a push without a restart") and
`docs/src/cli/global-options.md` ("The wiring is read once, unless you ask for
more"). They are not restated here.

## What a swap replaces, and what a decision sees

A change that a tick notices is **adopted**, not merely reported. The unit of
adoption is a **version**: the object provider registry, the field-type
declarations folded into it, the attribute providers, the rules engine over them,
the decision engine and the service facade, all built together and installed with
a single `atomic.Pointer` store (`internal/cli/wiring_swap.go`'s `liveWiring` /
`wiringVersion`).

Four properties are the contract, and each of them is a thing a plausible
alternative implementation would get wrong:

- **A version is installed whole or not at all.** A rebuild that fails installs
  nothing. There is no per-section swap and no partial install, so no decision can
  observe half of a push.
- **A request pins one version at its entry and finishes on it.** `liveWiring` is
  the `serve` handler, and it resolves the version once per request. A swap landing
  mid-request does not change what that request decides.
- **A decision never blocks on a wiring read.** Readers do one
  `atomic.Pointer.Load`; the mutex in `liveWiring` is **writers-only**, so two
  concurrent swaps serialise and no reader ever waits for one.
- **The facade extras are re-composed from the same function as the boot** —
  `internal/cli/serve.go`'s `serveFacadeOptions`. Four of them are built over the
  stack's *engine*, so composing them anywhere else leaves the authority gate,
  delegation and impersonation deciding through the superseded engine while `Check`
  answers through the new one. That is not a torn read inside one decision; it is
  two engines in one process, and no verdict, trace or note reports it.

A superseded version is never mutated and never `Close`d. Its pools are
**borrowed** from the boot (`borrowBootPools`), so closing it would close the
serving instance's connections; one pool per declared connection serves the life of
the process however many pushes it sees.

Caches do **not** carry across. Each version gets fresh per-type metadata caches
and fresh per-slot attribute caches, which is a security property before it is a
performance one: a rebuilt slot must never answer from an entry fetched under the
superseded configuration's `ttl:`, the window a *revoked* clearance would otherwise
keep authorizing for. The price is a brief cold period after each push, bounded and
measured — see
`docs/src/operations/wiring-refresh.md`.

## The connection name set is frozen for the life of a process

The shared tables carry a connection's **name** and nothing else, because a route
is a per-instance fact (see [The name is shared; the route is
per-instance, above). That makes the name
set a **boot-time** contract between the shared manifest and the routes this
instance can supply locally, and not a runtime one: a running process can neither
open a pool for a name that appeared while it was working nor drain one for a name
that vanished.

So the name set is frozen, in **both** directions, and a push that changes it is
refused with `APERTURE_WIRING_RESTART_REQUIRED` naming the added and dropped names.
Two halves of that refusal are decisions rather than consequences:

- **The whole push is held, not the connection part of it.** The providers, field
  types and attribute providers deployed beside a connection change stay
  outstanding with it. A push is adopted whole or not at all — applying the parts
  that happen to fit would leave the instance running a wiring version that was
  nobody's, and no diff anywhere would show it.
- **Nothing is torn down.** A name the push *removed* keeps its pool and the
  instance goes on deciding through it.

The remedy is a restart, after supplying a route for each added name. The boot-time
counterpart is `APERTURE_WIRING_CONNECTION_UNROUTED`, and the two are deliberately
different codes: a boot **refuses to start**, and a running process keeps deciding.

## A failed refresh keeps last-good, and says so

An access engine that stops answering takes its host down with it, and Aperture is
embedded in-process in its first real host. So **no refresh failure stops an
instance deciding.** An unreachable store, a set that cannot be digested, wiring
this host cannot build, a rebuild that fails, a frozen name set — all five keep the
wiring already in memory, and the digest does **not** advance, so the change is
re-detected and re-reported on every tick until it is adopted or the push is
corrected.

The price is staleness, and staleness is never silent. Every failure routes through
one reporter (`wiringPoll.alarm`) and is recorded on a `service.WiringHealth`, which
the system-admin-gated `service.WiringPosture` read answers from with the code, the
consecutive failure count and — the half an operator escalates on — **how long**.
`StaleFor` is computed at read time, so an age cannot under-report a loop that
stopped dead.

Two things about it are easy to get wrong and are asserted:

- **A failed adoption is stale, not healthy, and its age keeps growing.** A tick
  declares a refresh COMPLETE in exactly two places — the no-change branch, and
  after a successful swap — and nowhere else. A tick that read the tables, found a
  change and could not adopt it has completed nothing: it is the worst of the three
  postures, because the instance *knows* the wiring changed and is knowingly running
  superseded wiring. Clearing on the strength of the READ alone is the subtle
  version of the same bug: the alarm still fires, but `WiringHealth.Refreshed`
  zeroes the window and the count, so a push refused for four hours reports one
  failure and an age of one tick, forever — and the age is the half an operator
  escalates on. Staleness runs continuously from the first refusal until an adoption
  succeeds (`TestARefusedPushGetsOLDERRatherThanRestartingEveryTick`).
- **A completed refresh clears it, including one that saw no change.** An alarm that
  needed a *change* to clear would latch forever on a deployment whose wiring is
  stable, which is most of them.
- **The posture's digest is what the instance RUNS.** It advances with the adoption
  and not with the read, so a fleet-wide digest comparison reads a caught-up
  instance as caught up and a refusing one as behind.

The alarm **passes the underlying code through** (`wiringRefreshAlarm`'s
pass-through guard) and `APERTURE_WIRING_REFRESH_FAILED` is the classification of
last resort, for a failure nothing beneath coded. Burying
`APERTURE_WIRING_CONNECTION_UNROUTED`, whose fixups name the environment variable
to export, would cost the operator the remedy.

Staleness is deliberately **not** on `Capabilities` — that surface is open because
it carries booleans of immutable boot-time configuration, and this is mutable
runtime fault state whose useful half is a duration. The full reasoning is in
`service/wiring_posture.go`'s file header and `skills/api-surface.md` ("The wiring
posture read"). There is no
metrics subsystem in this repository; the posture read and the poller's stderr lines
are the two channels.

## The codes

| Code | Raised at | For |
|---|---|---|
| `APERTURE_WIRING_NO_MODEL_STATE` | push | the store holds no model state at all, so there is nothing for the wiring to be wiring *for* — almost always the wrong DSN |
| `APERTURE_WIRING_OBJECT_TYPE_UNKNOWN` | push | a provider entry serves an object type the model has no row for (provider entries only — a `field_types:` entry may name a type whose objects a seed lists inline) |
| `APERTURE_WIRING_CONNECTION_UNDECLARED` | push | an entry cites a connection the pushed manifest does not declare |
| `APERTURE_WIRING_KIND_UNSHAREABLE` | push **and** boot | a `kind:` that cannot be shared wiring — today `kind: csv` |
| `APERTURE_WIRING_CONNECTION_UNROUTED` | boot | a shared connection name *this* instance has no route for |
| `APERTURE_WIRING_RESTART_REQUIRED` | a running instance's refresh | the deployed wiring changes this process's connection NAME SET, which is frozen for its life — nothing in the push is applied, and the instance keeps deciding |
| `APERTURE_WIRING_REFRESH_FAILED` | a running instance's refresh | a background re-read did not complete and nothing beneath it was coded; the instance keeps last-good wiring and goes on deciding |
| `APERTURE_WIRING_LOCAL_COLLISION` | boot | the local file declares an object type or slot the shared wiring already declares |
| `APERTURE_WIRING_NOTHING_DEPLOYED` | pull | the store has no shared wiring, and a pull's file is meant to be pushed back |
| `APERTURE_WIRING_OUTPUT_EXISTS` | pull | `--out` names an existing path and `--force` was not given — the likeliest file there is the document the pull is to be compared with |

## Where the behaviour is pinned

Most of this contract is **reviewed rather than gated**, and the places that is not
true are worth knowing:

- `internal/cli/wiring_boot_test.go` — the boot projection, the three connection
  routes, and empty tables meaning "use the local file".
- `internal/cli/wiring_layer_test.go` — the merge authority: DB-only, local-only,
  disjoint, all four colliding axes, the inline-`objects:` axis, and the Go
  registration.
- `internal/cli/wiring_identical_test.go` — two decision stacks over one store, one
  wired from the database rows and one from the equivalent seed file, diffed across
  `Check`, `Enumerate`, `Search` and `Explain`. This is the proof that the projection
  produces the *same* registry and not an equivalent one.
- `internal/cli/wiring_diff_test.go`, `wiring_pull_test.go`, `wiring_refuse_test.go`,
  `wiring_declared_keys_test.go` — the report, the file, each refusal, and the three
  `declared_keys` states.
- `internal/cli/wiring_poll_test.go` — the poll: the closed interval vocabulary, and
  polling being off by default asserted by **counting store reads** rather than by
  reading the configuration back.
- `internal/cli/wiring_swap_test.go`, `wiring_frozen_test.go`, `wiring_stale_test.go`
  — the swap installed whole, a request pinning one version, the frozen name set in
  both directions, and last-good with the alarm (including the case that matters
  most: a refused adoption reports **stale**, not healthy).
- `bench/wiring_test.go` — what the machinery costs. `TestCheckNFRWiringPoll` runs
  the hard NFR gate with a live poll loop underneath it, and
  `TestCheckNFRAfterAWiringSwap` bounds the cold period after a swap and asserts a
  swapped version still clears the gate. Both are gated, and named so the one
  documented invocation (`APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/`)
  picks them up.
- `storage/storagetest`'s six `Wiring*` conformance cases, plus the wiring rows in
  the referential-integrity cases, run against all three backends;
  `storage/postgres/gate_test.go`'s `requiredConformanceCases` is a **floor**, so
  removing one is build-red.
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/`
  — the `push → pull → push` **fixed point** against a real server, plus the parity
  half: both backends must pull the same wiring as the **same document**, and
  `wiring diff` must produce the **same report** from both for the same drift. The
  dialect-parity gates cannot reach that.

## Related

- `skills/storage-schema.md` — the five tables, their columns, and the
  `ReplaceWiring` / `GetWiring` surface.
- `skills/attribute-providers.md` — the two registration layers a slot holds, and
  which one wins which key.
- `skills/sql-provider.md` — the statement contract a `kind: sql` entry carries, on
  either side of the push.
- `docs/src/cli/wiring.md` — the operator-facing narrative for the command tree.
- `docs/src/cli/serve.md` ("Noticing a push without a restart") — the flag, the
  interval vocabulary and what a tick costs.
- `docs/src/operations/wiring-refresh.md` — the fleet-facing account of a push
  against running instances: the rollout order a connection change needs, how
  staleness surfaces, and what the poll and the swap were measured to cost.
- `docs/src/operations/two-instance-topology.md` — the four steps of standing up a
  second instance against a store that already has one.
