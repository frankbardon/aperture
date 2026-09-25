# Refreshing wiring on a live fleet

[Two instances, one store](two-instance-topology.md) is about standing a fleet up.
This page is about what happens to it afterwards, when somebody runs
[`aperture wiring push`](../cli/wiring.md) against a store those instances are
already serving from.

The flag-level account — the value vocabulary, what a tick costs, what a swap
replaces — is on the command page:
[serve, "Noticing a push without a restart"](../cli/serve.md#noticing-a-push-without-a-restart).
This page does not repeat it. It answers the three questions an operator has about a
*fleet* rather than about a flag: what a push does to instances that are already
running, what it cannot do without a restart, and how you find out that an instance
stopped keeping up.

## The default is boot-only, and a restart is a real answer

An instance reads the shared wiring **once**, at startup. Unless you set
`--wiring-poll` / `APERTURE_WIRING_POLL`, that is the whole behaviour: no background
reader, no periodic query, nothing on a timer. A push is picked up by restarting the
instances.

That is not a degraded mode. If your deployment already rolls instances on a config
change, the pipeline you have is the refresh mechanism, and turning polling on adds a
second one. Polling earns its place when a restart is expensive or not yours to
order — an Aperture **embedded in a host process**, where restarting Aperture means
restarting the host, is the case it was built for.

```bash
# Boot-only: the default. Nothing to configure.
aperture serve --store "$DSN"

# Opt in, at the default 30s:
aperture serve --store "$DSN" --wiring-poll on

# Opt in, at an interval you chose:
APERTURE_WIRING_POLL=2m aperture serve --store "$DSN"
```

A value the vocabulary does not accept fails the command with
`APERTURE_CONFIG_INVALID` **before any connection is opened**, so a typo in a
deployment manifest costs you a refused start and not a half-configured instance.

### The interval is a staleness budget, not a tuning knob

The interval is the window in which your fleet is allowed to **disagree with
itself**: from the moment of the push until a given instance re-reads and adopts,
that instance is still enforcing the wiring it booted on. It is the same kind of
number as an attribute slot's `ttl:` — a bound on how long something already
replaced keeps being honoured — and it belongs in the same conversation as your
other revocation windows.

It is not a performance number. A tick is one `GetWiring` and one digest
comparison, and the measurement is in [What it costs](#what-it-costs) below: driven
at **1 ms**, thirty thousand times the default rate, it costs a few percent of
single-goroutine throughput and nothing the p99 can distinguish from noise. Choose
the interval by how long you are willing for two instances to answer differently, and
by nothing else.

## What a push does to a running fleet

Each instance notices independently, on its own tick, and adopts on its own. There is
no coordination, no leader and no barrier — so for up to one interval after a push,
some instances are answering from the new wiring and some from the old.

That is the honest shape of the feature and it is worth planning for: a push is
**eventually** consistent across a fleet, with the interval as the bound. If a
change must be simultaneous everywhere, a push plus a coordinated restart is the
mechanism, not polling.

Within a single instance, however, nothing is partial:

| Property | What it means for you |
|---|---|
| A version is installed whole or not at all | A rebuild that fails installs nothing. There is no instance running "half of" a push. |
| A request pins one version at entry | A push landing mid-request cannot change what that request decides. Requests in flight finish on the wiring they started with. |
| A decision never waits for a wiring read | The rebuild happens on the poll's own goroutine. A request pays one atomic pointer load. |
| The listener and the authenticator survive | Connections are not dropped and nobody is re-authenticated. Only what sits under the HTTP layer is replaced. |

Each instance reports what it did, on stderr:

```text
wiring poll: the deployed wiring CHANGED (3f9a1c72 -> 8b40e5de) and this instance
ADOPTED it; decisions already in flight finish on the wiring they started with
```

The two short hashes are digests of the wiring itself, and they are the handle for
"did this instance get the push?" across a fleet: after everyone has adopted, every
instance names the same new digest. They contain no account, principal or object
data.

## What a push cannot change without a restart

**The set of connection names.** The shared tables carry a connection's *name* and
nothing else, because which server, which credential and how big a pool are facts
each instance resolves for itself at boot — from `seed.WithConnectionOpener` in a Go
host, from a `connections:` entry in that instance's own `--seed` file, or from
`APERTURE_CONNECTION_<NAME>_DSN`. A running process can neither invent a route for a
name that appeared while it was working nor drain a pool for a name that vanished.

So the name set is frozen for the life of the process, in **both** directions, and a
push that changes it is reported and **not applied**, with
`APERTURE_WIRING_RESTART_REQUIRED` naming the added and dropped names. The
consequence that matters operationally: **the rest of that push is held too.** The
providers, field types and attribute providers pushed alongside a connection change
stay outstanding with it, because a push is adopted whole or not at all.

That makes the order of operations for a connection change a three-step rollout, and
the first step is the one people skip:

1. **Give every instance its route first**, while the old wiring is still deployed.
   Export `APERTURE_CONNECTION_<NAME>_DSN`, or add the `connections:` entry to that
   instance's local file. Nothing has changed yet, so nothing can break yet.
2. **Push.** Every polling instance now reports `APERTURE_WIRING_RESTART_REQUIRED`
   on each tick, and every one of them keeps deciding through the wiring it has.
3. **Restart the instances**, in whatever order your rollout allows. Each one
   re-reads from scratch, resolves the new name against the route you supplied in
   step 1, and adopts the whole push.

Skip step 1 and step 3 fails instance by instance: a boot that cannot route a
declared connection **refuses to start** with
`APERTURE_WIRING_CONNECTION_UNROUTED`. That is the deliberate difference between the
two codes — a boot refuses, and a running process keeps deciding.

## When a refresh fails

**No refresh failure stops an instance deciding.** An unreachable store, wiring this
host cannot build, a rebuild that fails, a frozen name set: all of them keep the
wiring already in memory and the instance goes on answering. That is not leniency —
an access engine embedded in a host process takes the host down with it if it stops
answering, and a fleet that stops deciding because one operator pushed one bad row
is a worse outcome than a fleet deciding from wiring one push behind.

The price is staleness, and the instance is loud about it. It keeps re-detecting the
change on every tick — the digest does not advance until an adoption succeeds — so
the condition repeats rather than scrolling past once:

```text
wiring poll: re-reading the shared wiring failed, so this instance keeps the wiring
it has: [APERTURE_STORAGE_SCHEMA_INCOMPATIBLE] ...
```

### Reading the posture without reading logs

Every failure is also **recorded**, and the record is a read on the facade:
`WiringPosture`, over Twirp or in Go, requiring **system-admin** authority. It
reports whether this instance polls at all, the interval, the digest it is deciding
from, whether the last refresh failed, the `APERTURE_*` code and message it failed
with, the consecutive failure count, and — the field you escalate on — **how long**
the current run of failures has lasted.

There is deliberately **no metrics subsystem in Aperture**, and nothing here emits
one. The two channels are this read and the poller's stderr lines; a fleet-wide
health sweep is a `WiringPosture` call per instance.

Four things about the read are worth knowing before you build a sweep on it:

- **It is authenticated on purpose, where `Capabilities` is open.** "This instance
  has been enforcing configuration its operator already replaced, for four hours" is
  operational intelligence about a fault, and not the kind of deployment fact
  `Capabilities` publishes anonymously. The full reasoning lives in
  `service/wiring_posture.go`.
- **An instance that does not poll answers the zero posture** — not polling, not
  stale — rather than refusing. That is what makes one sweep work across a fleet
  where some instances have opted in and some have not: a boot-only instance is not
  stale, it is running the wiring it was told to run.
- **The duration keeps growing while the loop is broken**, because it is computed
  when you read it and not accumulated on each tick. An age that only advanced when
  the failing thing managed to run would under-report exactly the failure that
  stopped the loop dead. It keeps growing across a **refused** push too: the window
  runs from the first refusal to now, however many ticks have read the tables fine
  in between.
- **A refused push reports as stale, not healthy.** This is the case that is easy to
  assume the other way: the posture is not only about a store the instance could not
  reach. An instance that read the tables perfectly well and then *refused to adopt*
  what they said — a frozen connection name set, a provider kind it cannot build — is
  knowingly running superseded wiring, which is a worse posture than not having
  looked, and it says so.

### Recovery needs nothing from you

The next **completed** refresh clears the alarm, resets the duration and the failure
count, and adopts normally. A refresh that observes no change counts as completed
too, which is what stops the alarm latching on a deployment whose wiring is stable.
So a store that was down for ten minutes produces an alarm ten minutes long and then
a clean posture, with no intervention and no restart.

A push this instance *refuses* is the case that does not clear itself, and that is
correct: reading the tables is not the same as adopting them. The alarm stays, and
its age keeps counting from the first refusal, until either the push is corrected or
the instance is restarted. Those two are the remedies the code's own fixups name.

Restarting is still the way to force the matter, and it is safe in the direction
that counts: a boot re-reads the shared wiring from scratch and **refuses to start**
on wiring it cannot build, rather than starting degraded.

### Symptom to first look

| What you see | Where to look first |
|---|---|
| A push had no effect on an instance | Is polling on there at all? A boot-only instance is correct and needs a restart. `WiringPosture.Polling` says. |
| Some instances answer differently from others | Expected for up to one interval after a push. Compare `WiringPosture.Digest` across the fleet; if they disagree for longer than the interval, at least one is stale. |
| `APERTURE_WIRING_RESTART_REQUIRED` on every tick | A connection name was added or dropped. Follow the three-step rollout above — and remember nothing else in that push has applied either. |
| `APERTURE_WIRING_CONNECTION_UNROUTED` at boot | That instance has no route for a name the deployment declares. Supply it, then start. |
| `APERTURE_WIRING_REFRESH_FAILED` itself | Nothing underneath it carried a code, which is worth reporting. Usually the store is unreachable from that host. |
| A stale alarm that is minutes old | Ordinary — a restarting database. The number to act on is the duration, not the boolean. |
| A stale alarm that is hours old | The fleet is enforcing policy somebody already retired. Treat it as a drifted deployment, not an outage: decisions are still being made, just from old wiring. |

## What it costs

Both halves are measured, and the numbers are committed in `docs/benchmarks.md`
alongside the rest of the NFR suite. They come from two gated cases,
`TestCheckNFRWiringPoll` and `TestCheckNFRAfterAWiringSwap`, that the one documented
invocation already runs:

```bash
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

**The poll loop.** Measured with a live loop doing a tick's work every **1 ms** —
thirty thousand ticks for every one the 30s default makes — the loop's cost is
visible but small. Across two runs the decision path's p99 stayed inside the
run-to-run spread of the same fixture with no loop at all (0.27–0.32 ms, against a
1 ms budget it clears by 3×), and single-goroutine throughput ran 5–15 % lower
(12 900–14 700 checks/sec, against a 10 000 floor). That is the cost of a loop being
paid thirty thousand times more often than any deployment pays it; at an interval you
would actually deploy it is a thirty-thousandth of that, which is not a number you
can measure or plan around.

**The swap.** A new version's caches start **empty** — fresh per-type metadata
caches and fresh per-slot attribute caches. That is a security property before it is
anything else: a rebuilt slot must never answer from an entry fetched under the
superseded configuration's `ttl:`, which is the window a *revoked* clearance would
otherwise keep authorizing for. The price is a brief cold period after every push.
On the benchmark fixture it is small and bounded: the first decision on a new version
cost 2.6× the warm median (129–134 µs against 49–52 µs across two runs), the window
settled within fifty to ninety decisions, and the **whole** cold period cost
0.7–1.1 ms of extra work — about one decision's worth of the 1 ms p99 ceiling, once,
per push.

One caveat, because the fixture cannot stand in for your deployment: those figures
are for in-memory providers, where filling a cache entry is cheap. With `kind: sql`
providers, the first decision that touches each object type after a push pays a real
query round trip to fill that type's entry. The cold cost then scales with *how many
object types get touched*, not with microseconds — still one round trip per type per
push, and still nothing that accumulates. If you are pushing wiring at human cadence
this is not a number you need to manage; if something is pushing wiring on a timer,
the cold period is one of the reasons not to.

## Related

- [serve](../cli/serve.md#noticing-a-push-without-a-restart) — the flag, the value
  vocabulary, what a swap replaces and what a tick costs.
- [Two instances, one store](two-instance-topology.md) — standing the fleet up:
  one model, one pushed wiring, per-instance connection routes.
- [wiring](../cli/wiring.md) — the `push` / `show` / `pull` / `diff` command tree.
- [Global options](../cli/global-options.md#it-means-the-same-thing-for-wiring) —
  where a booting instance's wiring comes from, and the three connection routes.
- [Performance & NFR](performance.md) — the decision hot-path budget these numbers
  are measured against.
- [Troubleshooting](troubleshooting.md) — reading an `APERTURE_*` error and acting
  on its fixups.
