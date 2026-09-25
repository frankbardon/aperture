---
name: object-references
description: Declared object references — the seed `references:` block and its Go twin `DeclareReference`, why a reference is declared on the holding side only and carries a closed set of one descriptor kind, why its values are full canonical identities, the enumerate reference edge (`--via`, `EnumerateRequest.references`, `EnumerateQuery.References`, the MCP `References` input) and the security semantics that are the whole point of it — an unreadable holder is EMPTY and never an error, `NOT_FOUND` only for an in-account holder after membership, one hop, edges AND, dangling entries skipped with a warning log and a `dangling_reference` note — plus the four worked questions, the identity-spelling trap, why a rules-engine dereference is deliberately not supported, and the coded errors with their HTTP statuses.
applies_to: [library, cli, http, mcp, seed]
---

# Object references

A **reference** is a declaration that one object-type's metadata *field* holds
identities of another object-type: `dataset.current_brands` points at `brand`.
It is an **application-level foreign key**. Nothing in a database enforces it —
the host's schema may not even have a constraint to enforce — so the declaration
lives beside the provider that serves the field, and Aperture checks it where it
is made rather than discovering it from the first decision that followed it.

One declaration buys one thing: `Enumerate` can be **restricted to what a holder
object names**. "Which brands belong to dataset X?" is a question no metadata
predicate can express, and the rest of this document is about why, and about the
security rules that make answering it safe.

The code is `provider/reference.go` (the declaration and its resolution),
`seed/provider.go` (the `references:` block), and `engine/reference.go` (the
enumerate edge and every rule below).

## Read this part first: the four questions

The feature exists because of four questions, and the fourth one is where people
get lost. One fixture, one principal, four answers:

| # | Question | Mechanism | Spelling |
|---|---|---|---|
| 1 | Which **datasets** do I have access to? | plain enumeration | `enumerate alice read 'account:acme/dataset:*'` |
| 2 | Which **brands** do I have access to? | plain enumeration | `enumerate alice read 'account:acme/brand:*'` |
| 3 | Which **brands belong to dataset X**? | **dereference** | `enumerate … 'account:acme/brand:*' --via account:acme/dataset:x.current_brands` |
| 4 | Which **datasets contain brand Y**? | **filter** | `enumerate … 'account:acme/dataset:*' --field current_brands=account:acme/brand:Y` |

> **Questions 3 and 4 look symmetric and are not.** Q4 is a *filter* — the
> dataset holds `current_brands`, so a predicate on dataset expresses the
> question directly, and the metadata filter answered it before references
> existed. Q3 is a *dereference* — a brand holds **no** field naming its
> datasets, so there is no predicate on brand that can express it at all. That
> asymmetry is not an implementation detail; it is a direct consequence of
> declaring references on the holding side only, and it is the single most
> useful sentence in this document.

`engine.TestTheFourQuestions` asks all four against one fixture, so the
difference is demonstrated rather than asserted.

## Declaring a reference

### In a seed document

A `providers:` entry gains a `references:` block mapping **field name → target
object-type**:

```yaml
providers:
  - object_type: dataset
    kind: sql
    connection: main
    get_one: SELECT d.tier, to_jsonb(d.brand_ids) AS current_brands FROM datasets d WHERE d.id = $1
    get_all: SELECT 'account:acme/dataset:' || d.id AS id,
                    to_jsonb(d.brand_ids) AS current_brands
             FROM datasets d
    references:
      current_brands: brand

  - object_type: brand
    kind: sql
    connection: main
    get_one: SELECT b.region FROM brands b WHERE b.id = $1
    get_all: SELECT 'account:acme/brand:' || b.id AS id, b.region FROM brands b
```

It works identically over a `kind: csv` provider or an inline `objects:` type —
a reference is a property of the *type*, not of the loader.

### In Go

```go
reg.MustRegister("brand", brands)
reg.MustRegister("dataset", datasets)
if err := reg.DeclareReference("dataset", "current_brands", "brand"); err != nil {
    return err
}
```

`DeclareReference` / `MustDeclareReference` are the Go spelling of the same
thing, and **both paths reach that one method**, so anything expressible in YAML
is expressible in Go and neither can drift from the other.

Reading it back is a registry lookup, not a re-parse of the document:

| Call | Answers |
|---|---|
| `reg.ReferenceTarget("dataset", "current_brands")` | `("brand", true)` — "what does this field point at?", and whether it is a declared reference at all |
| `reg.References("dataset")` | every reference on one type, sorted by field |
| `reg.AllReferences()` | the whole wiring picture, sorted by type then field — for a host that logs it at startup |
| `reg.ResolveReference(ctx, id, field)` | the identities one object's field names, fetching through the cache |
| `reg.ResolveReferenceValue(objectType, field, value)` | the same, over a value the caller already has in hand |

### Three closed doors, and why each one is closed

**1. The holding side only.** A reference is declared on the type whose provider
actually **returns the field**. There is no inbound spelling on `brand`
("brand is referenced by dataset"), and that is not an omission:

- `brand` has no column listing its datasets. An inbound declaration would
  describe a derived view with **nothing to attach to** — no value to resolve,
  no field to read.
- It would not buy addressability either. Add a second referencing field
  (`archived_brands: brand`) and the unnamed reverse edge "brand ← dataset"
  becomes ambiguous: which of the two did you mean?

One declaration, one direction, no drift. This is also exactly why Q3 above
needs a dereference rather than a filter.

**2. A closed set of one descriptor kind.** A reference declares its **target
object-type and nothing else**. Adding a `type:` key here is the obvious
reviewer instinct and it is wrong: the **loader is the single typing
mechanism** — a `:list<string>` suffix in a CSV header, a `to_jsonb(...)` cast
in the developer's SQL — and a second place to declare a type is a second place
for the two declarations to disagree. See `skills/sql-provider.md` and
`skills/metadata-values.md` for the one place typing does live.

**3. Values are full canonical identities.** The field holds `"brand:1"`, or
`"account:acme/brand:1"` — never a bare primary key, and never a foreign key
Aperture would template into an identity. The developer composes the identity
**where the data is loaded**:

```sql
SELECT to_jsonb(array(SELECT 'account:acme/brand:' || b.id FROM … )) AS current_brands
```

That choice is what makes Q4 free. Because the stored value already *is* an
identity, the ordinary `Filter.Fields` contract matches it with no new code at
all: `Fields{"current_brands": "account:acme/brand:Y"}` is a membership test
over a list of strings, which the filter already did. A templating scheme would
have needed a second matching path, and two matching paths in an authorization
engine are two chances to authorize differently.

### The identity-spelling trap

Because the restriction is a set-membership test over **canonical id strings**,
the identities in the field must be spelled **exactly as the object lister
yields them**. In an account-scoped deployment:

```
current_brands: ["brand:1"]                    -- WRONG
current_brands: ["account:acme/brand:1"]       -- RIGHT
```

The wrong form passes the declaration check — its terminal segment type *is*
`brand` — and then fails to resolve: `brand:1` is not an object the provider
serves, so it is treated as [dangling](#dangling-references), skipped, and the
enumeration comes back **empty**. An empty enumeration reads as "no access".

The signal is the operator's warning log, which names the identity
(`missing=brand:1`). If a `--via` returns nothing you expected, check the log
before checking the grants.

### What is, and is not, an error at declaration

| Situation | Result |
|---|---|
| the target type has no registered provider | `APERTURE_PROVIDER_REFERENCE_INVALID` at build, naming the field and the target |
| the holding type has no registered provider | `APERTURE_PROVIDER_UNREGISTERED` |
| an empty object-type, field, or target | `APERTURE_PROVIDER_REFERENCE_INVALID` |
| the same field declared twice on one type | `APERTURE_PROVIDER_REFERENCE_INVALID` — a field has **one** target |
| two *different* fields pointing at the same target | fine |
| a type referencing **itself** (a parent link) | fine |
| a field **no object happens to carry** | **not an error.** Metadata fields are discovered at fetch, not declared, so the reference simply resolves to nothing — the same answer an absent field gives |

A seed document's `references:` blocks are applied in a **second pass, after
every type is registered**, so a reference may name a target declared further
down the file or served by the `objects:` section. Fields are declared in sorted
order, so a document with two bad references always fails on the same one.

Like the rest of `providers:`, a `references:` block is **runtime wiring, not
model state**: `Apply` never writes it to storage and an export never reproduces
it. `TestReferenceWiringIsNotModelState` pins that.

## Enumerating through a reference

One input, five spellings, all carrying the same three strings:

| Layer | Spelling |
|---|---|
| `engine.EnumerateRequest` | `References []ReferenceEdge` |
| `service.EnumerateQuery` | `References []service.ReferenceEdge` — forwarded to the engine field-for-field |
| Twirp (`service.proto`) | `EnumerateRequest.references`, `repeated ReferenceEdge` (field 7); `EnumerateBatchRequest` embeds `EnumerateRequest`, so edges ride **per query** |
| CLI | `aperture enumerate --via <holder-identity>.<field>` (repeatable) |
| MCP | `aperture_enumerate` / `aperture_enumerate_batch` input `References`, reflected off `service.EnumerateQuery` |

An edge is `{HolderType, HolderID, Field}`:

- **`HolderID`** — the holder's canonical identity, mandatory.
- **`Field`** — the declared reference field to dereference, mandatory.
- **`HolderType`** — **optional**. Empty means "whatever `HolderID`'s terminal
  segment type is". When it *is* given it must **agree** with `HolderID`, so the
  three-string spelling a non-Go surface uses can never drift from the identity
  it carries. The CLI deliberately does not spell it at all: the engine derives
  it, so the two cannot disagree.

Nothing on the way in parses a holder identity, infers a holder type, or checks
that a field is declared. Every one of those is a decision with a **disclosure
consequence**, and a surface that answered one separately would be a second
place for the answer to differ.

Omitting the edges everywhere means the same thing — a nil Go slice, no
restriction, and an existing client entirely unaffected.

### The CLI spelling

```bash
aperture enumerate alice read 'account:acme/brand:*' \
  --seed ./model.yaml \
  --via account:acme/dataset:x.current_brands
```

The value is split on the **last** `.`: a `.` is legal inside an identity
component (`dataset:2026.q1`) while a reference field is a single metadata key,
so the final dot is the only unambiguous boundary. The flag is repeatable and
edges are ANDed. A malformed value — no `.`, an empty holder, an empty field —
is `APERTURE_INVALID_INPUT` naming the offending text, rejected **before the
store is opened**; it is never silently skipped, because a dropped edge *widens*
the result and an edge that silently widens is an edge that authorizes.

### Composition rules

- **Several edges AND.** `--via …dataset:x.current_brands --via
  …campaign:spring.brands` is "the brands in dataset x *and* in campaign
  spring".
- **An edge composes with `Fields`.** The restriction and the metadata
  predicates are independent and both apply.
- **Both precede `Limit`.** Restriction → decision → predicate → sort →
  truncate. Truncating first would answer a different question and return a
  silently wrong answer.
- **Restriction only subtracts.** It is a set-membership test applied to
  candidates that still go through deny-overrides and specificity, so no edge
  can surface an object `Check` would deny.
- **Exactly one hop.** An edge dereferences the holder's field and stops. The
  identities it yields are never themselves dereferenced, however many
  references their own type declares. (The engine fixture declares
  `brand.related_brands` precisely so a second hop would show up if it ever
  started being taken.) Transitive traversal is a different feature with a
  different cost model, and it is not this one.
- **An empty intersection does not short-circuit.** Every edge is resolved even
  once the running set is empty. Stopping early would make whether a later
  edge's wiring fault, absent holder, or dangling entry is reported depend on
  how much *data* the earlier edges happened to hold — an error that appears and
  disappears with the contents of a table is worse than one more lookup.

The whole restriction is computed **once per enumeration**, before candidates
are gathered — not once per candidate — and against the same grants and subject
set the candidates are decided with. That last part is what makes `EnumerateAs`
check the holder with the **impersonated** authority rather than the operator's.

## The security semantics, and why they are what they are

**This is the section a refactor must not undo.** Every rule below trades
ergonomics away for a reason, and each one is the kind of thing a well-meaning
change "fixes" into a disclosure channel.

| Situation | Answer | Why |
|---|---|---|
| The principal **may not read the holder** | **empty result, no error** | "You may not see dataset X" and "dataset X contains nothing you may see" must be **indistinguishable**. A `PermissionDenied` here turns the edge into an oracle for the existence and contents of objects the caller was never allowed to know about. |
| The holder **does not exist**, is **inside the request's account**, and the caller is a **member** | `APERTURE_NOT_FOUND` (**404** on the wire) | The ergonomics a typo deserves. The disclosure is confined to people already inside the account, who can enumerate its objects anyway. |
| The holder is **outside the request's account** | **empty, whether or not it exists** | The disclosure boundary. Both out-of-account answers are byte-identical, so a caller in one account never learns what does or does not exist in another. |
| The caller is **not a member** of the account | **empty, always** | Membership is decided *before* the holder is ever looked up, so a non-member never sees `NOT_FOUND` — not even for a holder that genuinely is absent. |
| A referenced identity **no longer exists** | **skipped**, warning log, `dangling_reference` note | An application-level foreign key has no constraint behind it. One deleted brand must not take down every decision on the dataset that still lists it. |
| The field is **not a declared reference** | `APERTURE_PROVIDER_REFERENCE_INVALID` (**400**) | A wiring/vocabulary fault describes the **deployment**, not the data. It carries no disclosure, and a caller who cannot see the deployment deserves to hear it loudly — an empty list would read as "no access". |
| The holder's type has **no registered provider** | `APERTURE_PROVIDER_UNREGISTERED` (**404**) | Same reasoning: loud, never empty. |
| The engine has **no reference source configured** | `APERTURE_PROVIDER_UNREGISTERED` | Fail closed *loudly*. An engine wired without `WithReferences` must not answer a `--via` with an empty list. |
| A reference value **does not point where it says** | `APERTURE_PROVIDER_REFERENCE_MISMATCH` (**500**) | The host's own data contradicts the host's own declaration. Dropping the value silently would turn a data fault into a denial one layer up. |

### The ordering is load-bearing

Enumerate resolves these in a fixed order, and each step is where it is for a
reason:

1. **Edge syntax** (`validateEnumerateRequest`) — before membership, storage, or
   any provider. A malformed edge says nothing about the data and everything
   about the caller.
2. **Membership** — a non-member gets an empty list and stops here, which is why
   a non-member can never reach the `NOT_FOUND` in step 5.
3. **Wiring faults** — unregistered holder type, undeclared field. Raised
   **before anything about the holder is read**, because they describe the
   deployment and therefore carry no disclosure at all.
4. **Existence**, then **the check** — *in that order*. The two answers have
   different boundaries: absence is reported to any member of the account the
   holder lives in; permission is never reported. Deciding the check first would
   collapse "no such dataset" into "no access" for everybody and throw the
   ergonomics away for nothing.
5. **Resolution and the dangling sweep**, then the intersection.

> A change that "helpfully" returned `NOT_FOUND` for a holder the caller may not
> read — or for a holder in another account — would silently open a disclosure
> channel that no test outside `engine/` and `internal/server/` can see. The
> empty-vs-`NOT_FOUND` split is therefore asserted **per surface**: over a real
> Twirp JSON round trip, through the facade, through the command tree, and
> through the MCP tool. If you change one of these rules, you will have to change
> four sets of tests, and that is the point.

## Dangling references

A referenced identity the target type's provider no longer serves is **skipped**
and the enumeration proceeds. It is recorded twice, for two audiences:

- **The operator** gets a **warning log** naming the missing identity — fixing
  it means knowing which row to fix, and a log stays inside the deployment:

  ```
  WARN engine: skipped a dangling object reference
       reference=dataset.current_brands target=brand missing=account:acme/brand:gone
  ```

  The sink is `engine.WithLogger(l)`; with none, `slog.Default()`. Nothing on
  the `Check` hot path logs.

- **The caller** gets a `rules.Note` of kind **`dangling_reference`** on the
  shared evaluation-notes channel, naming the **declaration** and the target
  type but **never the missing identity** — a note travels out across account
  boundaries exactly as an error message does. `Path` is
  `dataset.current_brands`, `Expected` is `brand`, `Actual` is `absent`, and it
  renders as:

  ```
  dataset.current_brands: references a brand that no longer exists; the identity was skipped
  ```

Only `APERTURE_NOT_FOUND` from the target provider is the dangling case. Any
other fetch failure is a real non-result and is returned, because silently
dropping ids during a provider outage would read as a **narrower** answer rather
than a broken one.

### The `Explain` caveat, stated honestly

The design intent was that a dangling reference "surfaces as a note visible
through `Explain`". **That is not literally what happens, and the difference
matters.**

`engine.Explain` takes a single `Object` and never dereferences anything, so
there is no `Explain`-of-an-enumeration entry point for the note to come back
through. What actually exists:

- The note rides the **same context-borne collector** `Explain` installs, and it
  projects into `Trace.Notes` through the same `EvaluationNote` mapping. Its
  kind is carried verbatim, so anything that renders trace notes renders this
  one.
- But **`Enumerate` installs no collector of its own** — it returns `[]string`
  and grows no diagnostics channel — so the note is recorded only when the
  **caller** wraps the call:

  ```go
  ctx, notes := rules.WithNoteCollector(ctx)
  ids, err := eng.Enumerate(ctx, req)
  _ = notes.Notes() // the dangling_reference notes, if any
  ```

- **No non-Go surface does this today.** Over Twirp, the CLI, or MCP, the note
  goes nowhere and the **warning log is the only signal**. Costing nothing
  unless something is already listening is the deliberate half of that; the
  missing enumerate-diagnostics channel is the honest half.

If a surface ever needs to hand these back, the fix is a diagnostics channel on
`Enumerate`, not a second note mechanism.

## Why there is no rules-engine dereference

A rule cannot follow a reference — `object.current_brands` is a list of identity
*strings* to a rule, and there is no `object.current_brands[0].region`. That is a
deliberate refusal, not a gap:

- **`Check` owes a p99 under a millisecond** (`bench/`, gated by
  `APERTURE_BENCH_ASSERT=1`). A dereference inside rule evaluation is a **join
  on the decision hot path**, and its cost is a property of the *host's data* —
  one dataset with three brands is cheap, one with three thousand is not, and no
  amount of rule review makes that visible.
- **The cache-miss path becomes recursive.** Every dereferenced identity is
  another `Registry.Fetch`, each of which can miss and pull through the host's
  provider. A rule over a two-hop path fans out multiplicatively, on a code path
  whose whole design premise is one cached fetch per object.
- **Evaluation is pure over a fixed metadata snapshot.** Every builtin that
  could reach the world is disabled and one `NOW` is pinned per decision
  (`skills/rules-engine.md`). A dereference would put I/O back inside the
  evaluator, and with it a decision that can differ between two identical
  `Check`s.

Enumeration is the right place for it: the restriction is computed **once**, off
the `Check` path, against the same per-type cache every candidate in the walk reads.

## The coded errors

| Code | Raised when | Twirp / HTTP |
|---|---|---|
| `APERTURE_PROVIDER_REFERENCE_INVALID` | a declaration is unusable (empty argument, unknown target, a field declared twice), or a request asks to dereference a field no `references:` declaration binds | `invalid_argument` / **400** |
| `APERTURE_PROVIDER_REFERENCE_MISMATCH` | a reference field's **value** is not an identity string or list of them, does not parse, or has a terminal segment type other than the declared target (`"team:7"` in a brand field) | `internal` / **500** |
| `APERTURE_PROVIDER_UNREGISTERED` | the holding type, or the holder type named by an edge, has no registered provider — or the engine has no reference source at all | `not_found` / **404** |
| `APERTURE_INVALID_INPUT` | a malformed edge: empty holder id, empty field, an unparseable identity, a `HolderType` disagreeing with `HolderID`, or a `--via` with no `.` | `invalid_argument` / **400** |
| `APERTURE_NOT_FOUND` | the holder is absent, inside the request's account, and the caller is a member | `not_found` / **404** |

`APERTURE_PROVIDER_REFERENCE_INVALID` is mapped to `invalid_argument` **on
purpose**, out of `codeToTwirp`'s 500 default. The only way it reaches a client
is an edge naming an undeclared field — the caller's own input, the same class
of mistake as a `--via` with no `.` — and retrying the request unchanged can
never succeed. A 5xx is the one status class a client is entitled to retry and
an alert rule is entitled to page on, and a typo must be neither. The Aperture
code rides in the twirp `meta["code"]` either way, so a client dispatching on the
code sees no change. `APERTURE_PROVIDER_REFERENCE_MISMATCH` stays a 500 because
it reports that the **host's own data** contradicts the host's own declaration,
which no change to the request can fix.

Every diagnostic names the developer's own inputs — the object-type, the field,
the declared target, the offending value's *type* — and a value only when the
value is a malformed identity the developer wrote. See the cross-account leak
rule in `CLAUDE.md`.

## Update-Demand

| Change | Also update |
|---|---|
| The `references:` schema (`seed.Provider.References`, `Registry.DeclareReference`, or what a declaration may carry) | this doc, the `Provider.References` and `DeclareReference` doc comments, `docs/src/concepts/seed.md`, `docs/src/concepts/providers.md` |
| The via-reference enumerate input (`engine.ReferenceEdge`, `service.ReferenceEdge`, `rpc.ReferenceEdge`, the `--via` flag, the MCP `References` input) | this doc, `skills/api-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/library/service-facade.md`, `docs/src/surfaces/mcp.md`, `docs/src/cli/decisions.md`, `mcp/skills/mcp-surface.md` — **and `make proto` + `make docs-gen`** |
| Any of the security semantics (empty-vs-`NOT_FOUND`, the account boundary, the ordering, one hop, AND) | "The security semantics" above **with its reasoning**, the `engine/reference.go` file comment, and the per-surface tests in `engine/`, `service/`, `internal/server/`, `internal/cli/`, `mcp/` |
| `rules.NoteDanglingReference`, its `Note.String()` case, or what a note carries | `skills/rules-engine.md`'s note-kind list and this doc's "Dangling references" |
| A reference error code | `errors/codes.go` (`AllCodes` + `Registry` with a Message and Fixups), the table above, then `make docs-gen` |
| `codeToTwirp`'s mapping for a reference code | the table above, `docs/src/surfaces/rpc-reference.md`, and `TestEnumerateReferenceUndeclaredFieldIsAnInvalidArgumentNotAnInternal` |
