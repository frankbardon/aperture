# serve

**Audience:** operators running Aperture as a long-lived service.

```text
aperture serve [options]
```

`serve` hand-wires the full dependency graph (`storage → engine → service →
HTTP handler`) and boots a `net/http` server exposing the HTTP + Twirp API and
the admin UI. It shuts down gracefully on `SIGINT` / `SIGTERM`, draining
in-flight requests within a 10-second window. This is the same fully-wired facade
the mutation CLI commands build — the server just puts it behind a listener and
an authenticator.

```bash
bin/aperture serve --addr :8080
```

```text
aperture serving on :8080
```

Press `Ctrl-C` to trigger a graceful shutdown (`shutting down...`).

## What the flags control

- `--addr` — the TCP address to listen on (default `:8080`).
- `--seed` / `--store` — the model to serve, exactly as elsewhere (see
  [Global options](global-options.md)). With no `--store`, the server runs
  against an in-memory model seeded from `--seed` or, when that is omitted too,
  the embedded example — the zero-flag demo. With a `--store` DSN and no
  `--seed`, **nothing is seeded**: the server reads the model already in that
  database and writes no model rows on startup. See
  [Booting against a database](#booting-against-a-database) below.
- `--auth` — the authenticator adapter that maps each request to a principal:
  `dev` (the default — the bearer token *is* the principal id, no external IdP),
  `oidc`, or `parsec`. It overrides the `APERTURE_AUTH_MODE` env var. Because the
  default is `dev`, `serve` runs with **no external identity provider** out of
  the box; `oidc` and `parsec` are opt-in.
- `--enforce-membership` — defence-in-depth: deny any decision whose principal
  is not a member of the active account *before* grants are consulted. This lets
  a single shared role (manager, analyst, …) be reused across accounts without
  one account's grants leaking to another's members. Also settable via
  `APERTURE_ENFORCE_MEMBERSHIP`.
- `--wiring-poll` — re-read the [shared wiring tables](../concepts/storage.md) on
  an interval instead of only at startup, so a `aperture wiring push` from another
  host is *noticed* without a restart. **Omitted means off**, and off means off:
  no background reader is started and no periodic query is made. Also settable via
  `APERTURE_WIRING_POLL`; the flag wins when both are given. See
  [Noticing a push without a restart](#noticing-a-push-without-a-restart) below.
- `--enumerate-limit` — the ceiling one `Enumerate` is bounded by: the number a
  request with a non-positive `limit` receives, and the number a larger request
  `limit` is clamped **down** to. It also bounds the scope member gather, so the
  gather and the result cap are one value. Unset leaves the engine on its
  documented default of `1000`. Also settable via `APERTURE_ENUMERATE_LIMIT`; the
  flag wins when both are given.

  ```bash
  bin/aperture serve --enumerate-limit 1500
  APERTURE_ENUMERATE_LIMIT=1500 bin/aperture serve
  ```

  **It is not a `serve` flag.** It describes the deployment, not the server, so
  the same flag and the same variable are carried by `check`, `enumerate`,
  `identifiers`, `explain` and `mcp`, and all of them resolve it identically —
  one binary cannot be configured to answer 1500 over HTTP and 1000 on the
  command line. The wiring lives in the decision stack every command builds, not
  in `serve`'s own options (which hold only `--enforce-membership`, a posture
  that really is the server's alone).

  ```bash
  APERTURE_ENUMERATE_LIMIT=1500 bin/aperture enumerate alice list 'account:acme/**'
  ```

  A value that is not a whole number **greater than zero** fails the command with
  `APERTURE_CONFIG_INVALID` naming the setting and the value it rejected — under
  `serve`, before the store is opened — rather than quietly serving the default.
  That covers `banana`, and it covers `0` and `-5` too: the engine's
  `WithEnumerateLimit` normalises a non-positive bound to the default, which is
  the right answer for a Go embedder passing a computed number and the wrong one
  for a human who typed one. An operator who wrote `-5` would be served `1000`
  while believing otherwise, so the CLI refuses at the boundary what the library
  would have absorbed. To get the default, omit the setting.

## Booting against a database

`serve` with a `--store` DSN and no `--seed` seeds **nothing**. It runs `Setup`
(which creates missing tables and never migrates), reads the model that is
already there, and writes no model rows of its own.

```bash
# A second instance, against a database another process provisioned:
bin/aperture serve --store 'postgres://aperture@db/aperture'
```

This is what makes a long-lived deployment safe and what lets two instances
share one database. Passing a `--seed` alongside a durable `--store` still
applies that document in full, on **every** boot — which is how you provision a
database on purpose, and which two instances pointed at the same database must
not both do, or each restart re-asserts one instance's model over the other's.

```bash
# Provisioning, deliberately and once:
bin/aperture serve --store 'postgres://aperture@db/aperture' --seed ./model.yaml
```

### Where its wiring comes from

Seeding nothing is not the same as being wired by nothing. After `Setup`, `serve`
reads the [shared wiring tables](../concepts/storage.md) and builds its object
providers, field types and attribute slots from them — so an instance with a
`--store` DSN and no seed file on disk at all is a fully wired instance.

When those tables hold no rows — every deployment that has never run
`aperture wiring push` — the local seed file's wiring is used exactly as it always
was. There is no flag and nothing to configure; see
[What an omitted `--seed` means](global-options.md#what-an-omitted---seed-means)
for the whole rule, including how a shared connection **name** is routed to this
instance's own DSN.

With both — wiring rows *and* a `--seed` file — the **database is authoritative
and the local file may only ADD**. A local `providers:`, `field_types:`,
`attribute_providers:` or `attributes:` entry for an object type or slot the
database never declared is built normally, which is how a `kind: csv` provider (a
path cannot be shared wiring) and a Go host's hand-written providers survive a
push. A local entry for one the database **already declares** fails the boot with
`APERTURE_WIRING_LOCAL_COLLISION` naming it, rather than one side quietly winning.
See
[With both, the database wins and the file may only ADD](global-options.md#with-both-the-database-wins-and-the-file-may-only-add).

### Noticing a push without a restart

The wiring read above happens **once**, at startup. That is the whole behaviour
unless you ask for more, and for most deployments it is the right one: a
`wiring push` is picked up by restarting the instances, which is what a deploy
pipeline already does.

`--wiring-poll` (env `APERTURE_WIRING_POLL`) turns on a background re-read:

```bash
bin/aperture serve --store 'postgres://aperture@db/aperture' --wiring-poll 30s
bin/aperture serve --store 'postgres://aperture@db/aperture' --wiring-poll on
APERTURE_WIRING_POLL=2m bin/aperture serve --store 'postgres://aperture@db/aperture'
```

| Value | Meaning |
|---|---|
| *omitted* | **off** — wired once at boot. No background reader, no periodic query. |
| `off` | off, said out loud. Useful when a variable is inherited and cannot be unset. |
| `0`, `0s` | off. A zero interval *is* "never". |
| `on` | on, at the default interval of **30s**. |
| a Go duration (`45s`, `2m`) | on, at that interval. |

Anything else — `banana`, `-5m` — fails the command with
`APERTURE_CONFIG_INVALID` naming the setting and the value it rejected, **before
any connection is made**, so a typo costs nothing but the typo.

When it is on, the instance reports it on stderr at startup and reports each
change it sees:

```text
wiring poll: re-reading the shared wiring every 30s; a change will be reported here
wiring poll: the deployed wiring CHANGED (3f9a1c72 -> 8b40e5de). This instance is
still running the wiring it booted on; restart it to pick the change up
```

**Today it detects and reports; it does not yet adopt.** The registries, the
connection pools and the engine this process decides through stay the ones it
booted with, which is why the report says so rather than leaving you to assume
otherwise.

#### Choosing an interval

The interval is the window a fleet is allowed to **disagree with itself**: from
the push until this instance re-reads, it is still answering from the wiring it
booted on. That makes it the same kind of number as an attribute slot's
[`ttl:`](../concepts/providers.md) — a bound on how long a revoked thing keeps
being honoured — and not a performance knob.

Longer is the more tempting mistake and the worse one. At five minutes a push
reads as having had no effect, and the operator reaches for the rolling restart
that polling exists to remove. Shorter buys nothing measurable: a push is a human
act at human cadence, and a one-second interval has every instance in the fleet
query five tables every second, forever, against wiring that changes perhaps
weekly. The default of `30s` sits where readiness probes do, so "within half a
minute of the push" needs no new unit of trust.

There is deliberately **no minimum**. A very short interval is a cost you can
read about here rather than a refusal you cannot override.

#### What a tick costs, and what it cannot miss

A tick is one `GetWiring` — the same single, consistent snapshot of the five
tables the boot takes — followed by a comparison of a content **digest** against
the digest this instance was wired from. The read is full and the comparison is
cheap; nothing is reconstructed to find out whether it needed to be.

A cheaper *probe* was considered and rejected, because every one available can be
wrong in the direction that matters, and a change an instance does not see is an
instance that is silently stale while reporting itself healthy:

- A newest-timestamp probe would trust the clock of whichever host ran the push.
  A push from a host whose clock lags writes rows *older* than the ones it
  replaced, and the probe reports "unchanged".
- A row-count probe misses every change that keeps the count — a re-pointed
  statement, a narrowed declared key set, a renamed connection.
- Four per-section reads can straddle a concurrent push and compose a wiring set
  that never existed.

The digest ignores the `created_at` / `updated_at` stamps on purpose. A push
rewrites every row, so an identical re-push — the same pipeline running twice —
is **not** a change, and is not reported as one.

Under `serve`, the facade is wired with everything the other surfaces expect: the
admin gate, delegation and impersonation mutators, the append-only audit trail,
the rules engine over a storage-backed rule source, and the object providers the
wiring declares. A rule saved through the admin UI takes effect on the next
decision with no separate rule store.

Full flags: [`serve`](../reference/cli.md#aperture-serve).

## Related

- [Global options](global-options.md) — `--seed` / `--store`.
- [mcp](mcp.md) — the read-only stdio surface, for MCP clients rather than HTTP.
- [Command-Line Reference](../reference/cli.md#aperture-serve) — the generated flag table.
