# Attributes

**Audience:** operators who need to know which host directories a deployment
reads `principal.*` and `account.*` out of — and how fast a revocation lands.

An **attribute slot** is a host directory: the user table, the service-account
registry, the tenant catalogue. There are exactly three — `user`, `machine`, and
`account` — and each caches the bags it has fetched, with its own `ttl:` and
`max_size:`. See [Providers](../concepts/providers.md#attribute-providers) for
the seam itself and [Seed &
portability](../concepts/seed.md#external-attribute-sources) for the YAML that
wires it.

```text
aperture attributes <slots|query|invalidate>
```

All three read through the **same** wiring builder every other command uses, so
the slots a listing reports are the slots a decision resolves through: the CLI
cannot describe a wiring it does not itself run. All three therefore take
`--seed` (and `--store`) exactly as [`check`](decisions.md) does.

| Subcommand | Tier | What it does |
|---|---|---|
| `attributes slots` | none | one row per slot: source, `ttl`, `max-size`, and how many bags this process has cached |
| `attributes query <slot>` | system-admin | a page of that slot's directory as `[{id, attributes}]`, narrowed by attribute predicates |
| `attributes invalidate <slot>` | system-admin | drop cached bags so the next decision re-reads them |

## The cache window is a security property

Object metadata going stale for a TTL is usually tolerable — a document's
category is a fact about a thing. An attribute bag is the **asker's standing**:
the clearance, the department, the plan. Until a cached bag expires, every
decision about that subject keeps evaluating against access the host may have
**already taken away**, so a revocation that takes effect `ttl:` later is a
revocation that has not happened yet.

That is the frame for both of the interesting columns below and for the whole of
`invalidate`: pick a slot's `ttl:` for how fast its revocations must land, read
back what the deployment is actually running, and close the window explicitly
when you cannot wait.

## `slots` — what is wired, and how stale it may be

```bash
bin/aperture attributes slots --seed ./seed.yaml
```

```text
slot     source  ttl    max-size  cached
user     sql     1m0s   10000     12
machine  csv     30s    10000     0
account  inline  never  10000     3
```

(An unwired slot renders as `(unwired)` with `-` in the three columns that
describe a cache it does not have.)

- **`source`** is where that slot's bags come from: `csv` or `sql` (an
  `attribute_providers:` entry), `inline` (the `attributes:` block), `(host)` for
  a registry a host wired in Go rather than from the seed, and `(unwired)` for a
  slot this deployment declares no source for. An unwired slot is not an error:
  every decision for it resolves the [floor
  bag](../concepts/rules.md#the-floor-bag-and-principalkind) and proceeds. When a
  slot is filled **both** ways it reports the **winner** — the
  `attribute_providers:` kind — because that is the source a contested key is
  answered from; the inline bags are still there, layered under it.
- **`ttl` is the revocation window.** `never` means a fetched bag is dropped only
  by eviction or by an explicit `invalidate` — correct for a fixed inline block,
  dangerous for a live directory. On a slot with two layers it is the **winning**
  layer's window, since each layer caches on its own declaration.
- **`cached` counts *this* process.** A one-shot invocation starts cold and reads
  `0`; it is the number that matters in a long-running [`serve`](serve.md). It sums
  a slot's layers, so a subject both layers serve is counted twice — it really is
  cached twice, and two `ttl`s will expire it.

`slots` needs **no actor**. It discloses nothing the caller did not already
supply: it reads the seed file named on the command line plus the cache
configuration this process built from it, contacts no provider, names no key, and
prints no bag. Requiring system-admin authority to read back a file you just
passed in would only mean nobody could diagnose *"is the user slot even wired?"*
without already holding the authority the diagnosis exists to explain.

When a seed declares one slot in **both** `attribute_providers:` and
`attributes:`, every command that builds the stack prints a warning naming the
affected slots. Nothing is discarded: the `attribute_providers:` entry is that
slot's **shared** layer, the inline bags **layer under it**, and the shared layer
wins every key both serve. The warning exists to say **which layer answers a
contested key** — the one thing no verdict, trace or note says. Only slot names are
named, never keys. See [Precedence: two layers, and the shared layer
wins](../concepts/seed.md#precedence-two-layers-and-the-shared-layer-wins).

## `query` — read a directory (system-admin)

```bash
bin/aperture attributes query user \
  --principal alice --account acme \
  --field department=eng --fields-json '{"clearance":3}' --limit 100
```

Prints a JSON array of `{id, attributes}`.

Unfiltered, this returns the head of the host's user table — keys and bags
together — so it is a **system-tier read**: `--principal` must hold system-admin
authority in `--account`. A refusal returns **nothing**: no partial page, no
count, and no way to tell an empty slot from a full one or from an unwired one.
That is deliberate — the authority check runs *before* the slot name is even
parsed, so a refused caller cannot use the error to probe which directories a
deployment wires. If a refusal is unexpected, ask
[`aperture explain`](decisions.md) about your own authority.

`--field` and `--fields-json` narrow by **attribute**, on exactly the predicate
[`aperture enumerate`](decisions.md) applies to object metadata: predicates are
ANDed, a field the bag does not carry never matches, a list-valued field matches
by membership, and everything else matches by **typed equality**, so the string
`"5"` never matches the number `5`. `--field` always sends a string; reach for
`--fields-json` when a number, bool, or list is genuinely meant. Given both,
`--fields-json` is merged first and `--field` entries override it by key.

A slot whose `attribute_providers:` entry declares no `get_all:` is
**fetch-only** by design — it answers the decision path without exposing the
whole table to an enumeration — and `query` reports that provider's coded refusal
rather than an empty page.

## `invalidate` — close the window now (system-admin)

```bash
bin/aperture attributes invalidate user --id alice --principal root --account acme
bin/aperture attributes invalidate user --principal root --account acme
bin/aperture attributes invalidate --all --principal root --account acme
```

The three forms — one subject, one slot, everything — are **mutually exclusive**,
and a conflict is refused rather than resolved by precedence: "`--all` plus a
slot" has two plausible readings, and guessing the broader one would clear caches
the operator did not ask to clear.

"Nothing was cached" is **reported**, not silently succeeded
(`no cached user bag for "alice"`): an operator invalidating a subject they
believe is cached wants to know their key did not match. Note that `--id` takes
the **bare** subject id — `alice`, never `user:alice`.

Every form clears **both layers** of every slot it names. A slot fed by an external
source and an inline block caches each independently, and dropping one would leave the
revoked value still being read out of the other — a window reported shut with half of
it open.

It is gated for the same reason `query` is, even though it writes nothing and
discloses no bag: the result says whether *this process* had that key cached,
which is a fact about who has recently been decided about, and clearing a large
slot costs the next wave of decisions a provider round-trip each.
`invalidate --all` names no slot, so it cannot be used to probe which exist.

**Invalidation is process-local.** It clears the caches of the process that runs
it. That makes it exact for a host embedding Aperture, and self-contained for a
one-shot CLI invocation (which starts cold and exits cold) — but it **cannot
reach a running [`aperture serve`](serve.md)**. The controls that reach that
process's cache are the slot's `ttl:` and a restart.

Full flags: [`attributes`](../reference/cli.md#aperture-attributes).

## Related

- [Global options](global-options.md) — `--principal` / `--account` / `--seed` / `--store`.
- [Providers](../concepts/providers.md#attribute-providers) — the attribute seam,
  its leniency contract, and why enumerating a slot can never be scope resolution.
- [Seed & portability](../concepts/seed.md#external-attribute-sources) — the
  `attributes:` and `attribute_providers:` blocks these commands report on.
- [Rules engine](../concepts/rules.md#the-floor-bag-and-principalkind) — what a
  rule reads when a slot answers nothing.
- [Command-Line Reference](../reference/cli.md#aperture-attributes) — the
  generated flag tables.
