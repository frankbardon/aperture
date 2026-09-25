<!-- DO NOT EDIT — regenerate with `make docs-gen` -->

# Command-Line Reference

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` — Fine-grained access control engine. This page is generated from the urfave/cli command tree in `internal/cli` (`cli.NewApp`); every command, subcommand, and flag below is read from the live definitions.

## Global flags

`aperture` declares no persistent global flags. The commonly shared options — `--seed`, `--store`, `--account`, and `--principal` (the acting principal on mutations, sourced from `APERTURE_PRINCIPAL`) — are defined per command and appear in each command's flag table below.

## Commands

| Command | Summary |
| --- | --- |
| [`attributes`](#aperture-attributes) | Inspect the attribute directories a seed wires, read one, or drop cached bags |
| [`bestow`](#aperture-bestow) | Bestow (delegate) a grant you hold to another principal |
| [`bulk`](#aperture-bulk) | Provision or deprovision many grants in one transactional call |
| [`check`](#aperture-check) | Decide whether a principal may take an action on an object |
| [`delete`](#aperture-delete) | Delete an entity (object-type\|permission\|principal\|role\|group\|account\|grant\|membership) |
| [`enumerate`](#aperture-enumerate) | List the objects a principal may act on |
| [`explain`](#aperture-explain) | Explain why a decision resolved the way it did |
| [`export`](#aperture-export) | Export the whole model to a single JSON/YAML state file (system-admin tier) |
| [`get`](#aperture-get) | Read one entity by id (object-type\|permission\|principal\|role\|group\|account\|grant) |
| [`identifiers`](#aperture-identifiers) | List all valid instance ids of an object type from its provider |
| [`impersonate`](#aperture-impersonate) | Start a time-boxed impersonation session (prints the session) |
| [`import`](#aperture-import) | Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier) |
| [`list`](#aperture-list) | List entities of a kind (object-types\|permissions\|principals\|roles\|groups\|accounts\|grants) |
| [`mcp`](#aperture-mcp) | Serve the read-only Aperture MCP surface over stdio |
| [`put`](#aperture-put) | Create or update an entity (object-type\|permission\|principal\|role\|group\|account\|membership\|grant) |
| [`revoke`](#aperture-revoke) | Revoke a grant you previously bestowed |
| [`search`](#aperture-search) | Rank the objects a principal may act on by a free-text name |
| [`serve`](#aperture-serve) | Run the Aperture HTTP server |
| [`template`](#aperture-template) | Manage and apply provisioning templates |
| [`wiring`](#aperture-wiring) | Manage the shared wiring a deployment keeps in its database |

## `aperture attributes`

Inspect the attribute directories a seed wires, read one, or drop cached bags

An attribute slot is a HOST DIRECTORY — the user table, the service-account
registry, the tenant catalogue — that a rule reads `principal.*` and `account.*`
out of. There are exactly three slots (user, machine, account) and each caches
the bags it has fetched, per slot, with its own ttl: and max_size:.

THE CACHE WINDOW IS A SECURITY PROPERTY, not only a tuning knob. Object metadata
that goes stale for a TTL is usually tolerable. An attribute bag is the ASKER'S
STANDING — the clearance, the department, the plan — so until a cached bag
expires, every decision about that subject keeps evaluating against access the
host has ALREADY TAKEN AWAY. Principals are the classic revoke case, and a
revocation that takes effect `ttl:` later is a revocation that has not happened
yet. What a slot's ttl: buys in fetch traffic it pays for in that delay.

So: pick a slot's ttl: for how fast its revocations must land, read back what a
deployment is actually running with `aperture attributes slots`, and close the
window on a specific subject with `aperture attributes invalidate`.

Reading a directory in bulk (`query`) and dropping cached bags (`invalidate`)
are SYSTEM-TIER operations: both require --principal holding system-admin
authority in --account, and a refusal returns nothing at all — no partial page,
no count, and nothing that tells an unauthorized caller which slots exist.

```
aperture attributes <command>
```

### `aperture attributes invalidate`

Drop cached attribute bags so the next decision re-reads them (system-admin tier)

Drops cached bags, so the next decision about the affected subjects pulls fresh
ones from the host directory. Three forms:

```text
  aperture attributes invalidate user --id alice   one subject, one slot
  aperture attributes invalidate user             every bag in one slot
  aperture attributes invalidate --all            every bag in every slot
```

INVALIDATION IS A SECURITY CONTROL, NOT A PERFORMANCE KNOB. A cached attribute
bag is the asker's standing, so a REVOKED CLEARANCE KEEPS AUTHORIZING until that
bag expires: for the length of the slot's ttl:, every decision about that
subject is made against access the host has already removed. Waiting the window
out is not a remedy, it is the exposure. An operator who has just removed
someone's access invalidates that subject here, and then the removal is true.

Scope: this drops the caches of THE PROCESS THAT RUNS IT. That makes it exact
for a host embedding Aperture (it is the operator's spelling of
provider.AttributeRegistry.Invalidate, which such a host calls the moment its
directory changes) and it makes a ONE-SHOT invocation self-contained: this
process starts with a cold cache and exits with it, so there is nothing here for
a stale bag to survive in. For a long-running `aperture serve`, the controls
that reach ITS cache are the slot's ttl: — set it to how fast that directory's
revocations must land — and a restart.

Requires --principal holding system-admin authority in --account: the result
reports whether a bag was cached, which is a fact about who has recently been
decided about, and clearing a large slot costs the next wave of decisions a
provider round-trip each.

```
aperture attributes invalidate [options] <slot>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--all` | — | bool | — | clear EVERY slot's cache; takes no &lt;slot&gt; argument and no --id |
| `--id` | — | string | — | drop only this subject's cached bag (a bare principal or account id); omit to clear the whole slot |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture attributes query`

Read a page of one attribute slot's directory (system-admin tier)

Returns up to --limit records of &lt;slot&gt; — user, machine, or account — as a JSON
array of {id, attributes}, narrowed by attribute predicates.

THIS IS A SYSTEM-TIER READ. Unfiltered, it returns the head of the host's user
table, keys and bags together, so it requires --principal holding system-admin
authority in --account. A refusal returns NOTHING — no partial page, no count,
and no way to tell an empty slot from a full one or from an unwired one. Ask
`aperture explain` about your own authority if a refusal is unexpected.

--field and --fields-json narrow the result by ATTRIBUTE, on exactly the
predicate `aperture enumerate` applies to object metadata: predicates are ANDed,
a field the bag does not carry never matches, a list-valued field matches by
membership, and everything else matches by TYPED equality, so the string "5"
never matches the number 5. --field always sends a string; use --fields-json
when a number, bool, or list is genuinely meant:

```text
  aperture attributes query user --principal alice --account acme \
    --field department=eng --fields-json '{"clearance":3}'
```

Both may be given together: --fields-json is merged FIRST and --field entries
then override it by key.

A slot whose sql: entry declares no get_all: is FETCH-ONLY by design — it can
answer the decision path without exposing the whole table to an enumeration —
and this command reports that provider's coded refusal rather than an empty
page.

```
aperture attributes query [options] <slot>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--limit` | — | int | `0` | cap the number of returned records (&lt;=0 means the default; the registry clamps it regardless) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture attributes slots`

List the three attribute slots, the source each is wired to, and its cache settings

Prints one row per slot — user, machine, account — with the source the seed
declares for it (csv, sql, or inline), the cache freshness window, the cached-bag
cap, and how many bags this process currently holds.

THE TTL COLUMN IS THE REVOCATION WINDOW. A slot's cached bag keeps authorizing
until it expires, so `ttl` is the longest a removed clearance can keep working.
`never` means a bag, once fetched, is only dropped by eviction or by an explicit
`aperture attributes invalidate` — correct for a fixed inline block, dangerous
for a live directory.

The `cached` column counts THIS process's cache. A one-shot invocation starts
cold, so it reads 0; it is the number that matters in a long-running
`aperture serve`.

No actor is required: this reports the wiring in the seed file you passed and
the configuration this process built from it. It contacts no provider and prints
no subject key and no attribute value.

```
aperture attributes slots [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture bestow`

Bestow (delegate) a grant you hold to another principal

```
aperture bestow [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal bestowing the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--file` | — | string | — | path to a JSON grant body |
| `--json` | — | string | — | grant body as inline JSON |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture bulk`

Provision or deprovision many grants in one transactional call

```
aperture bulk <command>
```

### `aperture bulk grant`

Apply many grants atomically (account-admin tier)

```
aperture bulk grant [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON array of grant bodies |
| `--json` | — | string | — | a JSON array of grant bodies |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture bulk revoke`

Delete many grants atomically (account-admin tier)

```
aperture bulk revoke [options] [<grant-id>...]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--grant` | — | string | — | grant id to revoke (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture check`

Decide whether a principal may take an action on an object

```
aperture check [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture delete`

Delete an entity (object-type|permission|principal|role|group|account|grant|membership)

```
aperture delete [options] <kind> [<id>]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--account-id` | — | string | — | membership account id (kind=membership) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--principal-id` | — | string | — | membership principal id (kind=membership) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture enumerate`

List the objects a principal may act on

Lists every object id under &lt;pattern&gt; that &lt;principal&gt; may take &lt;action&gt; on.

--field and --fields-json narrow that list by OBJECT METADATA. The predicate is typed:
a field matches only when its value equals the wanted value AND is of the same kind, so
the string "5" never matches the number 5. --field always sends a STRING; use
--fields-json when a number, bool, or list is genuinely meant:

```text
  --field tier=premium --field current_brands=brand:Y
  --fields-json '{"seats":5,"active":true,"tags":["public"]}'
```

Both may be given together: --fields-json is merged FIRST and --field entries then
OVERRIDE it by key. Predicates are ANDed; a field the object does not carry never
matches; a list-valued field matches by membership. Filtering happens before --limit.

--via restricts the list to what a DECLARED REFERENCE names — the other direction:
--field asks "which datasets contain brand Y?", --via asks "which brands does
dataset X list?". It is spelled &lt;holder-identity&gt;.&lt;field&gt;, where the field is
everything after the LAST '.', and it is repeatable (edges are ANDed):

```text
  --via account:acme/dataset:x.current_brands
```

A holder you may not read yields an EMPTY list and no error, which is deliberate:
"you may not see dataset X" and "dataset X lists nothing you may see" must not be
tellable apart. Restriction, like filtering, happens before --limit.

--limit and --enumerate-limit are two different bounds. --limit is THIS REQUEST's
cap; --enumerate-limit is the DEPLOYMENT's ceiling, the same value `aperture serve`
honours, and it is what a --limit larger than it is clamped down to. A --limit of
zero or less asks for the ceiling.

```
aperture enumerate [options] <principal> <action> <pattern>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the enumeration is scoped to |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--limit` | — | int | `0` | cap the number of returned object ids for THIS request, clamped down to the deployment's --enumerate-limit ceiling (&lt;=0 means that ceiling, which is 1000 unless configured) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--via` | — | string | — | restrict the result to the objects a holder's declared reference field names, as &lt;holder-identity&gt;.&lt;field&gt; (e.g. --via account:acme/dataset:x.current_brands); repeatable, and several edges are ANDed. The FIELD is everything after the LAST '.' |

## `aperture explain`

Explain why a decision resolved the way it did

```
aperture explain [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture export`

Export the whole model to a single JSON/YAML state file (system-admin tier)

```
aperture export [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--format` | — | string | — | output format: json (default) or yaml |
| `--out` | — | string | — | write the state file to this path (default: stdout) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture get`

Read one entity by id (object-type|permission|principal|role|group|account|grant)

```
aperture get [options] <kind> <id>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture identifiers`

List all valid instance ids of an object type from its provider

```
aperture identifiers [options] <object_type>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--exclude` | — | string | — | id to omit from the result (repeatable); expands an exclusive allowance |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture impersonate`

Start a time-boxed impersonation session (prints the session)

```
aperture impersonate [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (**required**) |
| `--mode` | — | string | `"augment"` | augment\|become |
| `--operator` | — | string | — | operator principal (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--target` | — | string | — | target principal to impersonate (**required**) |

## `aperture import`

Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier)

```
aperture import [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to the JSON/YAML state file (default: stdin, treated as JSON) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture list`

List entities of a kind (object-types|permissions|principals|roles|groups|accounts|grants)

```
aperture list [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | account to list grants for (required for kind=grant) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture mcp`

Serve the read-only Aperture MCP surface over stdio

Exposes Aperture's decision API (check/enumerate/explain, single + bulk), a read-only what-if simulator, and model inspection as MCP tools over stdio. No tool mutates. Intended to be spawned over stdio by an MCP client.

```
aperture mcp [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture put`

Create or update an entity (object-type|permission|principal|role|group|account|membership|grant)

```
aperture put [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON entity body |
| `--json` | — | string | — | entity body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture revoke`

Revoke a grant you previously bestowed

```
aperture revoke [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal revoking the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--grant` | — | string | — | id of the grant to revoke (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture search`

Rank the objects a principal may act on by a free-text name

Ranks every object id under &lt;pattern&gt; that &lt;principal&gt; may take &lt;action&gt; on AND
whose metadata matches &lt;query&gt;, best match first.

Every result is one `aperture check` would allow: candidates are DECIDED before they
are scored, by the same walk `aperture enumerate` uses, so the result is always a
subset of what that command returns. A score ranks; it authorizes nothing.

Matching is case- and punctuation-insensitive ("Nike, Inc." matches "nike inc") and
tolerates a typo or a transposition on tokens long enough for one to be unambiguous.
Only TEXT is matched — a string field and the string elements of a list field. Match
a number, bool, or date with --field instead.

Aperture has no notion of a "label": a label is an ordinary metadata field whose name
your host chose. By default every field holding text is searched; --in names the ones
to search, and each result reports which field actually matched.

```text
  aperture search alice read 'account:acme/brand:*' nike
  aperture search alice read 'account:acme/brand:*' nike --in label --limit 5
```

--field / --fields-json and --via mean exactly what they mean on `enumerate`, and they
COMPOSE with the query: the predicate and the reference edge narrow the candidate set,
the query ranks what is left. "The brand called Nike in dataset X" is one call.

--min-score drops weak matches (default 0.4). Raising it narrows the shortlist; it
never widens what the principal may see.

--limit caps how many MATCHES come back — the top of a finished ranking, not a bound
on the scan, so the best N are returned rather than the first N found. The scan itself
runs to the deployment's --enumerate-limit ceiling.

```
aperture search [options] <principal> <action> <pattern> <query>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the search is scoped to |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--in` | — | string | — | restrict matching to this metadata field; repeatable (default: every field holding text) |
| `--limit` | — | int | `0` | cap the number of returned MATCHES for THIS request, clamped down to the deployment's --enumerate-limit ceiling (&lt;=0 means that ceiling, which is 1000 unless configured) |
| `--min-score` | — | float | `0` | drop matches scoring below this, 0 to 1 (&lt;=0 means the default, 0.4) |
| `--scores` | — | bool | — | print the score and the matching field/value alongside each id |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--via` | — | string | — | restrict the result to the objects a holder's declared reference field names, as &lt;holder-identity&gt;.&lt;field&gt; (e.g. --via account:acme/dataset:x.current_brands); repeatable, and several edges are ANDed. The FIELD is everything after the LAST '.' |

## `aperture serve`

Run the Aperture HTTP server

```
aperture serve [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--addr` | — | string | `":8080"` | TCP address to listen on |
| `--auth` | — | string | — | authenticator adapter: dev\|oidc\|parsec (overrides APERTURE_AUTH_MODE; defaults to dev — bearer is the principal id, no external IdP) (env: `APERTURE_AUTH_MODE`) |
| `--enforce-membership` | — | bool | — | deny any decision whose principal is not a member of the active account, before grants are consulted (defence-in-depth; lets shared roles be reused across accounts safely) (env: `APERTURE_ENFORCE_MEMBERSHIP`) |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request limit is clamped down to. It configures the PROCESS, not the command: serve and every one-shot decision command honour the same value (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--manage-accounts` | — | bool | — | manage the lifecycle of account records — allow account create/update/delete through the API (default true; overrides APERTURE_MANAGE_ACCOUNTS). Pass --manage-accounts=false when accounts are mastered by an upstream system: Aperture then refuses every account write regardless of the caller's authority, while account reads and every decision stay unaffected. Read once at startup; a restart is required to change it |
| `--manage-memberships` | — | bool | — | manage the lifecycle of principal-to-account memberships — allow membership create/update/delete through the API (default true; overrides APERTURE_MANAGE_MEMBERSHIPS). Independent of the other two, so a deployment can master accounts and principals upstream and still decide who belongs to what, or the reverse. Read once at startup; a restart is required to change it |
| `--manage-principals` | — | bool | — | manage the lifecycle of principal records — allow principal create/update/delete through the API (default true; overrides APERTURE_MANAGE_PRINCIPALS). Pass --manage-principals=false when principals are mastered by an upstream directory or IdP: Aperture then refuses every principal write regardless of the caller's authority, while principal reads and every decision stay unaffected. Read once at startup; a restart is required to change it |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--wiring-poll` | — | string | — | re-read the shared wiring tables on an interval instead of only at startup, so a `wiring push` from another host is noticed without a restart. A Go duration ("30s", "5m"), or on for the default of 30s, or off. Omitted means OFF — the instance is wired once, at boot, and starts no background reader (overrides APERTURE_WIRING_POLL) (env: `APERTURE_WIRING_POLL`) |

## `aperture template`

Manage and apply provisioning templates

```
aperture template <command>
```

### `aperture template apply`

Apply a template transactionally into --account (account-admin tier)

```
aperture template apply [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--id-prefix` | — | string | — | prefix for generated grant ids |
| `--name` | — | string | — | template name to apply (**required**) |
| `--param` | — | string | — | parameter as name=value (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template delete`

Delete a template version, or all versions (system-admin tier)

```
aperture template delete [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version to delete (0 = all versions of the name) |

### `aperture template get`

Read a template by name (latest version unless --version)

```
aperture template get [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template list`

List every template version

```
aperture template list [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture template put`

Create or update a template (system-admin tier)

```
aperture template put [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON template body |
| `--json` | — | string | — | template body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model to apply on startup (when omitted: the embedded example for the in-memory store, and nothing at all for a --store DSN) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture wiring`

Manage the shared wiring a deployment keeps in its database

SHARED WIRING is the part of a seed document that belongs to the DEPLOYMENT
rather than to one instance: which object types are served and by what
statements (`providers:`), which metadata fields hold dates (`field_types:`),
the manifest of connection NAMES an entry may cite (`connections:`), and where
each attribute slot's bags come from (`attribute_providers:`).

Push it once and every instance sharing that database reads the same wiring —
including an instance that has no seed file at all.

WIRING IS NOT MODEL STATE. The model says who exists and who may do what;
wiring says where a decision reads object metadata and attribute bags FROM.
The two are pushed by different commands, and wiring for a model that is not
there is refused rather than stored.

WHAT IS NEVER STORED: no DSN, no credential, not even the NAME of the
environment variable holding one, and no filesystem path. Those are
per-instance facts — each instance resolves its own credentials and sizes its
own pool — so the manifest carries connection names and nothing else, and
`kind: csv` is refused because its only data source is a path. A csv entry
stays legal in the LOCAL seed file, where the path belongs to the instance
that reads it.

The two remaining sections, `objects:` and `attributes:`, are never shared:
they carry inline DATA rather than a pointer to data.

```
aperture wiring <command>
```

### `aperture wiring pull`

Write the store's deployed shared wiring out as the four seed sections, for diffing against version control

Reads the deployed wiring in ONE atomic snapshot and writes it to --out as
`connections:`, `providers:`, `field_types:` and `attribute_providers:` — the same
four sections `wiring push` reads. The file is a seed document `wiring push`
accepts unchanged, so `push` then `pull` then `push` deploys the identical wiring,
and two pulls of an unchanged deployment are byte-identical.

WHAT IT IS FOR is answering "is what is deployed what is in the repository?" with
a diff. `aperture wiring show` answers "what is deployed?" in words and is the
better command for reading; this one produces a file for a tool.

THE FILE IS RE-PUSHABLE BUT NOT BOOTABLE, and the difference is the security rule
made visible. Shared wiring holds a connection's NAME and nothing else — no DSN,
no credential, not even the NAME of the environment variable holding one, and no
filesystem path — so every connection comes back with an empty `dsn_env:`. Fill
those in from your own deployment's environment before booting an instance from
the file; an instance built from it as written refuses at registry build and names
the unset variable. Nothing in the output is a secret, and the format has no
`dsn:` key to put one in.

NO MODEL STATE IS WRITTEN. `aperture export` emits the model and no wiring; this
emits the wiring and no model. The file carries no accounts, principals, objects
or inline data, because the shared wiring holds none — and it does not spell the
model sections out as empty either, since a populated deployment's model is not
empty and a document that said so is one somebody would import.

AN EXISTING --out FILE IS REFUSED unless --force is given. The likeliest thing at
that path is the version-controlled document the pull is meant to be compared
with, and overwriting it silently destroys the left-hand side of the comparison.
The path is checked before the store is opened, so the refusal reads nothing.

A STORE WITH NO WIRING DEPLOYED IS REFUSED, which is the one place this command
disagrees with `wiring show`. `show` only describes an empty store, and nothing
deployed is a useful answer there. A pull produces a file whose purpose is to be
pushed back, and an empty one pushed back replaces the deployment's wiring with
nothing — while an empty read is also exactly what a mistyped --store naming a
database Setup just created looks like.

No actor is required, and none is accepted, for the reason `show` accepts none:
this restates wiring the --store credential already grants full write access to.

```
aperture wiring pull [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--force` | — | bool | — | overwrite the --out file if it already exists |
| `--format` | — | string | — | output format: json or yaml (default: inferred from the --out extension — .json is JSON, anything else is YAML) |
| `--out` | — | string | — | write the wiring document to this path (required; an existing file is refused unless --force is given, and there is no stdout default because `aperture wiring show` is the command for reading) |
| `--store` | — | string | — | DSN for the shared store the wiring lives in: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (required — there is nothing to share about an in-memory store). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema |

### `aperture wiring push`

Validate a seed document's four shared wiring sections and write them to the store in one transaction

Reads `providers:`, `field_types:`, `connections:` and `attribute_providers:` out
of --seed, validates every entry, and REPLACES the store's wiring with them in a
single transaction. The document's other sections are not read: no model state
is applied, and the two local wiring sections (`objects:`, `attributes:`) are
untouched.

THE PUSH IS ALL OR NOTHING. Every rule below is checked before anything is
written, and the write itself replaces the whole set in one transaction, so a
refusal leaves the deployed wiring exactly as it was. Wiring is only meaningful
whole — an entry naming a connection the manifest does not list is not half-valid
wiring, it is broken wiring — and an instance booting against a half-written set
would build a registry missing exactly the entries whose write failed, while
reporting nothing.

REPLACE, not merge. What is in the document is what the deployment will run;
an entry dropped from the document is dropped from the store. Push the whole
wiring every time.

A push is refused when:

```text
  * the store holds NO MODEL STATE at all — apply the model first, and check the
    --store DSN, because a typo names an empty database Setup will create
  * an entry selects `kind: csv` — its only data source is a filesystem path,
    and a path is machine-local
  * a provider serves an `object_type` the store has no row for (named in the
    refusal)
  * an entry names a `connection:` the pushed `connections:` manifest does not
    declare (named in the refusal)
  * anything carries a literal `dsn:` — only `dsn_env:`, a variable NAME, is ever
    accepted, and shared wiring stores neither
```

No actor is required: the store credential is the authority, exactly as it is for
`aperture import`.

```
aperture wiring push [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to the JSON/YAML seed document whose four SHARED wiring sections are pushed (required; no model state is applied from it and there is no embedded-example fallback) |
| `--store` | — | string | — | DSN for the shared store the wiring lives in: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (required — there is nothing to share about an in-memory store). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema |

### `aperture wiring show`

Print the shared wiring this store has deployed, one table per section

Reads the five shared-wiring tables and prints them: the connection manifest, the
provider entries with their reference declarations, the field-type declarations,
the attribute-provider entries, and the statement set every database-backed entry
runs. It answers "what is this deployment actually wired to?" without a database
client.

AN EMPTY STORE IS AN ANSWER, not an error. A store nothing has been pushed to says
so plainly: every instance booting against it builds its wiring from its own
--seed file instead. It is also what a mistyped --store looks like, because a DSN
naming a database that does not exist yet is one Setup creates — so check the DSN
before concluding a push was lost.

THE COLUMNS AN OPERATOR WOULD OTHERWISE HAVE TO GUESS AT are spelled as words
rather than left blank. `ttl` and `max-size` read `(default)` when the entry sets
neither, because the registry's own default applies and `0` would read as
"caches nothing". `connection` and `id-column` read `-` when the kind does not use
them. An attribute slot with no `get_all` is reported as FETCH-ONLY: every
decision path works unchanged and only the system-tier directory read refuses.

THE DECLARED KEY SET distinguishes three states, because two of them are
different answers a blank column would merge: `(not declared)` is a slot that
opted out of key enforcement entirely, `(declared empty)` is a slot that opted IN
and permits no keys at all, and a list is the keys the slot guarantees.

No actor is required, and none is accepted. This restates the wiring that the
--store credential you just supplied already grants full write access to, so
requiring an authority on top of it would only mean nobody could diagnose "is
anything even deployed?" without already holding the authority the diagnosis
explains — the same reason `aperture attributes slots` is ungated. It contacts no
provider, opens no host connection, and prints no account, principal or object
identity, because the shared wiring holds none.

```
aperture wiring show [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--store` | — | string | — | DSN for the shared store the wiring lives in: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (required — there is nothing to share about an in-memory store). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema |

