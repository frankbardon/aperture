---
name: rules-engine
description: The rules engine evaluates a JSON rule AST as an expr-lang expression over object metadata and principal/action context, compiling once and caching, to back the inclusive/exclusive scope resolvers.
applies_to: [cli, http, mcp]
---

# Rules engine

The `rules` package decides object-membership selection (and, by extension,
allow/deny) from a domain object's metadata plus the principal/action context. It
is the rule-backed variant of the inclusive/exclusive scope resolvers (E2-S1): an
`*rules.Engine` satisfies `scope.RuleEvaluator`, so wiring it as
`engine.ScopeDeps{Rules: eng}` turns on rule-driven scope membership.

Expressions are evaluated by [`expr-lang/expr`](https://github.com/expr-lang/expr)
**directly**: `rules` renders each AST to an expr-lang expression and compiles it
in-process with expr-lang's pure-Go evaluator. Aperture does not hand-roll a
parser, has **no dependency on Pulse**, and stays `CGO_ENABLED=0`. Any older doc
calling this a "Pulse expression" is stale — `rules` imports
`github.com/expr-lang/expr` and that is the whole engine.

## The rule AST (the editor + state-file contract)

A rule is a tree of typed `rules.Node` values. The set is small, explicit, and
closed, so the Rete.js editor (E7-S2) maps its palette one-to-one and the state
file (E5-S2) persists the same shape. There is no second rule format.

| `type` | Fields | Meaning |
|---|---|---|
| `and` / `or` | `children` (>= 2) | logical conjunction / disjunction |
| `not` | `children` (exactly 1) | logical negation |
| `compare` | `op`, `left`, `right` | comparison; `right` is omitted for the unary ops, and is a two-item `list` for `between` |
| `var` | `name` (dotted path) | context variable reference |
| `literal` | `value` (scalar JSON) | string / number / bool / null constant |
| `list` | `items` | list literal (right side of a collection op) |
| `call` | `name`, `items` (args) | call to a registered pure function |
| `relativeDate` | `anchor`, `n`, `unit`, `snap` (**all four, always**) | a date relative to the decision's reference instant; an **operand**, legal only inside a date operator |

The JSON form is stable and round-trips (marshal -> unmarshal -> marshal is
byte-identical), including falsy literals (`false`, `0`, `""`, `null`) and the
unary operators, whose `right` key stays absent.

```json
{"type":"compare","op":"eq",
 "left":{"type":"var","name":"object.classification"},
 "right":{"type":"literal","value":"public"}}
```

## Operators

Scalar comparisons — `left` and `right` are both required, any operand node:

| `op` | Reads as | Renders to |
|---|---|---|
| `eq` `ne` | `object.tier == "gold"` | `==` `!=` |
| `lt` `le` `gt` `ge` | `object.level >= 3` | `<` `<=` `>` `>=` |

Collection operators. `left` is the collection (or, for `exists`, any path);
`right` is the operand, and is **omitted entirely** for the three unary ops:

| `op` | Reads as | `left` applies to | `right` |
|---|---|---|---|
| `in` | `object.region in ["us","eu"]` | any | list or var |
| `nin` | `principal.id not in object.blocklist` | any | list or var |
| `has` | `object.tags has "urgent"` | array | one element (never a list) |
| `hasAll` | `object.tags has all ["a","b"]` | array | list or var |
| `hasAny` | `object.tags has any ["a","b"]` | array | list or var |
| `hasNone` | `object.tags has none ["a","b"]` | array | list or var |
| `subsetOf` | `object.tags subset of ["a","b"]` | array | list or var |
| `hasKey` | `object.owner has key "dept"` | object | one element (never a list) |
| `isEmpty` | `object.tags is empty` | array, object | **omitted** |
| `isNotEmpty` | `object.tags is not empty` | array, object | **omitted** |
| `exists` | `object.owner.dept exists` | any path | **omitted** |

Date operators. `left` is the date-valued field; `right` is another date operand
— a literal, a variable, or a `relativeDate` — except for `between`, which takes
**two bounds**:

| `op` | Reads as | `right` |
|---|---|---|
| `before` | `object.hired_at before "2026-01-01"` | one element (strict) |
| `after` | `object.hired_at after "2026-01-01"` | one element (strict) |
| `onOrBefore` | `object.hired_at on or before "2026-01-01"` | one element (inclusive) |
| `onOrAfter` | `object.hired_at on or after "2026-01-01"` | one element (inclusive) |
| `between` | `object.hired_at between ["2026-01-01","2026-12-31"]` | a `list` of exactly two bounds |
| `sameDay` | `object.hired_at same day as "2026-03-04"` | one element |
| `sameMonth` | `object.hired_at same month as "2026-03-04"` | one element |
| `sameYear` | `object.hired_at same year as "2026-03-04"` | one element |

A date operand is one of the two canonical strings the value model defines —
`2006-01-02` or `2006-01-02T15:04:05Z` (see the `metadata-values` skill). Four
properties are worth stating outright:

- **`between` is INCLUSIVE AT BOTH ENDS.** `between [lo, hi]` is exactly
  `onOrAfter lo && onOrBefore hi`. Bounds are never reordered: a `lo` above the
  `hi` is a range that matches nothing, which is what the author wrote — and it
  is **noted**, because "matched nothing" is otherwise indistinguishable from a
  window nothing happens to fall in.
- **Granularity never affects ordering.** `"2026-03-04"` and
  `"2026-03-04T00:00:00Z"` name the same instant and compare **equal**. Ordering
  is over instants, never over text — which is also why no date operator renders
  to a native `<`.
- **`sameDay` / `sameMonth` / `sameYear` are calendar-bucket EQUALITY, in UTC.**
  Not distance: `2026-03-31` and `2026-04-01` are a day apart and are **not** in
  the same month, and `2026-03-04` and `2025-03-04` share a month number but are
  **not** in the same month. There is no timezone conversion anywhere and no
  timezone knob — every instant in the model is already UTC.
- **A date operand that will not parse denies, and never raises** — see
  "Malformed dates" below.

`between` needs two right-hand operands and `compare` carries one `right`, so
`right` is a `list` of exactly two items. **No new node type, no new field, and
no new JSON key**: every rule persisted before dates existed loads and
re-marshals byte-identically, and the author's one `between` node reads back as
one node rather than as an `and` of two comparisons.

```json
{"type":"compare","op":"between",
 "left":{"type":"var","name":"object.hired_at"},
 "right":{"type":"list","items":[
   {"type":"literal","value":"2026-01-01"},
   {"type":"literal","value":"2026-12-31"}]}}
```

Operand rules are enforced by `Validate` and return `APERTURE_RULE_INVALID`:

- The three unary ops require `right == nil`; supplying one is an error, not
  something ignored, so a rule has exactly one spelling.
- Every other op requires both operands.
- `in` `nin` `hasAll` `hasAny` `hasNone` `subsetOf` need a `list` or a `var` on
  the right — a set.
- `has` `hasKey` and the eight single-operand date ops need a single element on
  the right; a `list` there is an error.
- `between` needs a `list` of **exactly two** bounds on the right — one bound,
  three bounds, an empty list, or a bare operand are all rejected.
- A `relativeDate` operand is legal only inside a date operator (either side, or
  either `between` bound); see "Relative dates".

Build a unary node with `rules.Unary(op, left)`, a `between` node with
`rules.Between(left, low, high)`, and everything else with
`rules.Compare(op, left, right)`.

## Relative dates

A rule that says "touched in the last 90 days" must keep meaning that tomorrow,
so the cutoff cannot be a literal. The `relativeDate` node expresses a date
**relative to the decision's reference instant** (`NOW` — see "The clock, and one
`NOW` per decision") as structured fields, not as an expression an author has to
learn to write:

```json
{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}
```

| field | vocabulary |
|---|---|
| `anchor` | `NOW` \| `TODAY` |
| `n` | any whole number; **negative goes into the past** |
| `unit` | `years` \| `quarters` \| `months` \| `weeks` \| `days` \| `hours` \| `minutes` |
| `snap` | `none` \| `startOfYear` \| `endOfYear` \| `startOfQuarter` \| `endOfQuarter` \| `startOfMonth` \| `endOfMonth` \| `startOfWeek` \| `endOfWeek` \| `startOfDay` \| `endOfDay` |

The two worked examples:

| Intent | Node |
|---|---|
| three months prior to today | `{"type":"relativeDate","anchor":"NOW","n":-3,"unit":"months","snap":"none"}` |
| year to date plus five years of history | a `between` whose lower bound is `{"anchor":"NOW","n":-5,"unit":"years","snap":"startOfYear"}` and whose upper bound is `{"anchor":"TODAY","n":0,"unit":"days","snap":"endOfDay"}` |

```json
{"type":"compare","op":"between",
 "left":{"type":"var","name":"object.hired_at"},
 "right":{"type":"list","items":[
   {"type":"relativeDate","anchor":"NOW","n":-5,"unit":"years","snap":"startOfYear"},
   {"type":"relativeDate","anchor":"TODAY","n":0,"unit":"days","snap":"endOfDay"}]}}
```

Build one with `rules.RelativeDate(anchor, n, unit, snap)`; the vocabularies have
exported constants (`rules.AnchorNow`, `rules.UnitMonths`, `rules.SnapStartOfYear`,
…).

Six rules, each of which is API rather than convention:

- **A negative `n` goes into the past.** "Three months ago" is `n: -3`. There is
  no separate direction field anywhere that could disagree with the sign — a sign
  error in a date rule is silent and grants access backwards in time, so the
  convention is stated once and used by every surface.
- **All four fields are always present.** "No offset" is `n: 0` (with whatever
  unit — zero of anything is the anchor itself) and "no snap" is the vocabulary
  member `"none"`, never an absent key. Absence never means anything, so the
  editor's four controls are never empty and both validators run the same four
  uniform checks.
- **`TODAY` is a distinct persisted anchor**, defined as `NOW` snapped to the
  start of its UTC day. It is never rewritten into `NOW` + `snap: startOfDay`: an
  author who chose `TODAY` reads back `TODAY`.
- **A snap composes on top of the anchor.** `anchor: TODAY, snap: startOfYear` is
  legal and means the start of the year — the wider snap simply subsumes the
  anchor's start-of-day. The snap is applied **before** the offset (see "UTC,
  clamping, and the order of operations"), so the node reads left to right the
  way it is spoken: `NOW, n: -5, unit: years, snap: startOfYear` is "the start of
  the year, five years back".
- **The node is an operand, and only a date operator's operand.** It is legal on
  either side of `before` / `after` / `onOrBefore` / `onOrAfter` / `sameDay` /
  `sameMonth` / `sameYear`, and as either bound of a `between` — the two bounds
  vary independently, so any mix of literal and relative is fine. Anywhere else —
  the right of `eq`, an item of an `in` list, a `call` argument, a child of
  `and` — is `APERTURE_RULE_INVALID`. It resolves to a date the author never
  sees, so being compared as anything but a date is a silent wrong answer.
- **Anchors, units, and snaps are closed sets checked in the AST**, not free
  strings resolved at runtime, so a typo fails when the rule is **saved** rather
  than denying quietly on every decision after. Each field has its own message:
  `relative date has an unknown anchor` / `… an unknown offset unit` / `… an
  unknown snap` / `relative date offset must be a whole number`.

**How it compiles.** The node renders to the internal dispatcher
`$rel(__now, anchor, n, unit, snap)` — the reference instant, then the four fields
as **literals**:

```
$date("before", __notes, "object.hired_at", object?.hired_at, "",
      $rel(__now, "NOW", -3, "months", "none"), "", nil)
```

`$rel` returns a canonical date **string** — byte-for-byte what a literal in the
same position would be — so `$date` parses both operands through one path and a
relative date is interchangeable with a fixed one. `$rel` is the **only**
dispatcher that reads `__now`; `$date` never sees it and its arity is unchanged.

The literal arguments are the whole point. Exposing the instant as a `NOW`
variable root instead would make `NOW.AddDate(0, -3, 0)` a well-formed var path,
because reflective method calls survive `expr.DisableAllBuiltins` — an unclamped
calendar walk reachable from any rule. `allowedRoots` is unchanged, and `$rel` is
unreachable from a rule for the same structural reason as `$op` and `$date`.

**Showing an author what a relative date currently means** is
`ResolveRelativeDates(ast, now) []ResolvedRelativeDate` — one entry per
relative-date operand, in document order, each carrying its AST path, the four
field values as authored, and `Resolved` (the canonical date, or `""` for an
operand that resolves to nothing, which is the deny the evaluation applies). It
goes through the **same** `resolveRelativeDate` the `$rel` dispatcher calls, so a
preview and a decision cannot disagree about what an operand means at a given
instant. It never fails and never raises. The rule builder's what-if
(`service.EvaluateRulePreview`) is its caller; anything else that renders a
relative date must use it rather than re-deriving the arithmetic.

### UTC, clamping, and the order of operations

Resolution is three steps in a fixed order: **anchor → snap → offset**.

**Snap first, then offset.** The node reads left to right the way it is spoken —
"the start of the year, five years back". The two orders are different functions,
not a rearrangement: `startOfMonth` then `+1 day` is the **2nd of this month**,
while `+1 day` then `startOfMonth` is the **1st of next month** when the anchor is
a month end. The order is API, and a test pins a case where the two disagree.

**Calendar offsets clamp at month end.** Offsetting by the three calendar units
(`years`, `quarters`, `months`) pins the day to the last valid day of the target
month rather than normalising the way `time.AddDate` does:

| | this engine | `time.AddDate` |
|---|---|---|
| `2026-03-31` − 1 month | `2026-02-28` | `2026-03-03` |
| `2026-05-31` − 1 month | `2026-04-30` | `2026-05-01` |
| `2024-02-29` − 1 year | `2023-02-28` | `2023-03-01` |
| `2026-01-31` + 1 month | `2026-02-28` | `2026-03-03` |
| `2024-01-31` + 1 month | `2024-02-29` (leap) | `2024-03-02` |

Clamping matches `java.time`, Luxon, and `date-fns`, so it is what an author who
has met any other date library expects — and the standard library's answer is
wrong here in the worst way: silently, and only when the anchor happens to fall on
a long month end. Clamping never *extends* a date: `2023-02-28` + 1 year is
`2024-02-28`, not the 29th. `weeks` / `days` / `hours` / `minutes` are
fixed-length, are added as durations, and never clamp.

**`start*` is midnight; `end*` is `23:59:59` on the period's last day** — the last
representable instant, not the start of the next period. `between` is inclusive at
both bounds, so an `endOfMonth` upper bound has to admit the whole final day; an
exclusive next-boundary would admit the following day as well. Seconds rather than
nanoseconds because the value model floors to whole seconds.

**Week boundaries are ISO 8601 — Monday through Sunday.** A Sunday anchor snaps
*back* six days to the Monday that started its week; `endOfWeek` is that Sunday at
`23:59:59`. Quarters are calendar quarters (Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec),
so any day in February snaps back to January 1st.

**Everything is UTC.** No `time.Local`, no `time.LoadLocation`, no timezone knob —
asserted structurally over the package's sources, not just by fixture. DST is a
non-issue by construction: UTC has no transitions, so a day offset is always
exactly 24 hours.

**Out of range denies.** An offset that leaves the four-digit year range the
canonical forms can write (or that overflows a duration) resolves to **nothing**,
and the enclosing operator denies with the ordinary `shape_mismatch` note — the
same answer a missing reference instant gets. An instant that cannot be written in
the canonical form is not a date this system has.

**No reference instant, no date.** `rules.Input.Now`'s zero value means "no
reference instant is available" — a hand-built `Input`; an `Engine` always
supplies one. A relative date then resolves to nothing and **denies** rather than
resolving against the year 1, and it never raises.

### How each operator compiles

`expr.DisableAllBuiltins()` is on, so nothing is assumed reachable. Each operator
either renders to native expr syntax or calls a pure function the compiler
registers:

- **Native.** `eq ne lt le gt ge in nin` are infix. `has` and `hasKey` render as
  a *flipped* `in` (`"urgent" in object?.tags`) — expr's `in` is element
  membership over an array **and key membership over an object**, so `hasKey`
  needs no new machinery. `exists` renders as `(left != nil)`.
- **Registered pure functions.** `hasAll` `hasAny` `hasNone` `subsetOf`
  `isEmpty` `isNotEmpty` have no native spelling with builtins off, so each is
  backed by a deterministic, side-effect-free function in the curated set.
- **Guarded.** Whichever of the two forms above applies, a collection operator
  whose collection operand is not *statically* a collection — anything but a
  list literal, so in practice any operand reading metadata — renders instead to
  the internal dispatcher `$op(op, __notes, leftPath, left, rightPath, right)`,
  which applies the deny-safe shape policy below and records the note. The
  common `object.region in ["us","eu"]` keeps its native `in`: a list literal is
  an array by construction and cannot mismatch, so the decision path pays
  nothing. Neither `$op` nor `__notes` is reachable from a rule — `$` is outside
  the name grammar `Validate` enforces, and `__notes` is not an exposed context
  root — so the guard is compiler-only by construction, not by denylist.
- **Dates are always guarded.** Every date operator renders to the internal
  dispatcher `$date(op, __notes, leftPath, left, rightPath, right, right2Path,
  right2)` — the last pair is `between`'s upper bound and is `""`/`nil` for
  every other date operator, so one arity covers the binary and the ternary
  operators. There is **no** native fallback, and that is a hard rule: the values
  are canonical date *strings*, so a native `<` would compare text (which orders
  `"2026-03-04"` before `"2026-03-04T00:00:00Z"` although they are the same
  instant), and parsing to `time.Time` first is worse — `time.Time < string` is a
  **compile** error in expr, so one mistyped operand would make the entire rule
  uncompilable instead of degrading one comparison. `$date` is unreachable from a
  rule for the same structural reason as `$op`.

**expr's predicate builtins are denied.** `expr.DisableAllBuiltins()` does not
reach `all`, `any`, `none`, `one`, `filter`, `map`, `count`, `sum`, `find`,
`findIndex`, `findLast`, `findLastIndex`, `groupBy`, `sortBy`, `reduce` — the
parser resolves those names before consulting the disabled table. `Validate`
rejects a `call` node naming any of them with `APERTURE_RULE_INVALID`.

## Context variables

The expression environment exposes four roots; a variable under any other root is
an unknown variable, rejected at validation:

- `object` — the object's metadata fields (read-only snapshot from the provider).
- `principal` — principal attributes. The **floor** is `{id, kind}` and the engine
  stamps it over every resolver's answer, so both are always present and a host
  bag carrying its own `id` / `kind` key cannot shadow them (a directory with an
  internal `id` column would otherwise silently redefine what
  `principal.id == object.owner` compares). Richer attributes come from a
  `PrincipalResolver` (`WithPrincipalResolver`), whose
  `Attributes(ctx, kind, principal)` receives the principal's kind (`"user"` /
  `"machine"`, empty = unknown) so one resolver can dispatch per kind; it returns
  the host's bag alone, and `nil` is a complete answer.
  A `*provider.AttributeRegistry` is wired straight in: the kind picks the
  attribute **slot** (`user` → the user slot, `machine` → the machine slot;
  `"account"` is a slot but not a principal kind, so it never resolves here). A
  slot with **no registered provider** — and a registered provider with no record
  for the key — yields the floor and **no error**, so a deployment with a human
  directory and no machine directory keeps deciding. Any other failure surfaces
  verbatim and the resolver treats it as a non-decision: an outage must not read
  as "this principal has no attributes".
  A slot may hold **two layers** — a `shared` one (the deployment's wiring row or
  `attribute_providers:` entry) and a `local` one (this instance's `attributes:`
  block or its own Go registration) — and the bag the resolver returns is their
  merge, with the **shared layer winning every key both serve**. That is beneath the
  floor, not beside it: the tiers compose in one direction, **floor over shared over
  local**, so `principal.id` and `principal.kind` are unshadowable by either layer.
  `skills/attribute-providers.md` ("Precedence: two layers, and the shared layer
  wins") has the argument.
  `principal.kind` exists because per-kind providers make a rule silently
  kind-dependent — a rule written against the user directory reads nothing for a
  machine principal, which denies safely in an **inclusive** grant but **widens**
  access in an **exclusive** one, where not-selected means not-excluded. Say the
  dependence out loud:
  `principal.kind == "user" && principal.tier == "gold"`.
  The bag is resolved **once per decision**, not once per object — see "One bag
  per decision".
  Under an **active impersonation** the root describes the EFFECTIVE subject, not
  the requesting principal: `become` resolves the target's id and the target's
  kind (so `principal.*` comes from the target's directory), `augment` the
  operator's. The rule and the grant set must describe the same principal — a
  decision resolving the target's grants while reading the operator's attributes
  is an authorization bug that leaves no mark in a trace. `Request.Principal` and
  `Decision.Impersonation` still name the real operator, so audit is unaffected;
  the target's kind costs no extra read (it arrives with the subject-set
  expansion become already performs). An inert or expired session elevates
  nothing and reads the operator's own bag.
- `account` — the **active** account's attributes: the tenancy the decision is
  being made in, never the account a grant is stamped to (a wildcard-stamped
  grant evaluates inside whatever account is active, so reading the stamp would
  give one tenant's decision another tenant's plan). The **floor** is `{id}` and
  the engine stamps it over every resolver's answer, so a host account table with
  its own `id` column cannot shadow it. It is `{id}` and not `{id, kind}` on
  purpose: there is one account slot, so a `kind` key would be the same constant
  in every bag in every deployment — a value that can never discriminate is
  noise, not information. Richer attributes come from an `AccountResolver`
  (`WithAccountResolver`), whose `AccountAttributes(ctx, account)` is spelled with
  that name — not `Attributes` — precisely so ONE `*provider.AttributeRegistry`
  can satisfy both this seam and `PrincipalResolver`; Go has no overloading, and
  the registry already holds both directories and both caches. The account slot
  with **no registered provider**, and a registered one with no record for the
  account, both yield the floor and **no error**, exactly as for a principal. The
  account slot layers exactly as a principal slot does — a `shared` layer over a
  `local` one, the shared winning every contested key, and the floor `{id}` over
  both.
  **`"*"` is never an attribute fetch key.** It is the all-accounts grant
  sentinel, not an account — `ValidateAccount` refuses to store a row for it, and
  the only bag that could answer "the attributes of every account" is one
  tenant's data served as every other's. It is still a live *active* account
  (platform authority is anchored there, so `Gate.RequireSystemAdmin` runs a
  `Check` with it), so a platform-scope decision sees the floor and nothing else:
  `account.id == "*"`, every host field absent. The engine short-circuits before
  consulting a resolver — refusing instead would make every rule-backed grant
  undecidable at platform scope, including the ones that never mention `account`
  — and `provider.AttributeRegistry` refuses `"*"` with
  `APERTURE_ATTRIBUTE_PROVIDER_INVALID`, so that refusal is the backstop rather
  than the mechanism. Like `principal`, the bag is resolved **once per
  decision** — see "One bag per decision".
- `action` — the action verb (a string).

There is deliberately **no `NOW` root**. See "The clock, and one `NOW` per
decision" below: the reference instant reaches evaluation as an argument, never as
a variable.

## The clock, and one `NOW` per decision

Date comparisons need a reference instant, and evaluation still has to be pure —
every `expr-lang` builtin is disabled, so nothing a rule can *call* reads a clock.
The instant is therefore **data**, fixed before the program runs.

**One clock.** `rules.WithClock` injects the engine's **single** time source. It
drives two things:

1. compiled-rule cache **TTL expiry** (`WithCacheTTL`), and
2. the per-decision reference instant, `NOW`.

Two independent clocks inside one engine can disagree — an entry expiring against
one instant while a date rule decides against another — and that is a worse
failure than the coupling. **The coupling is real and intended: pinning the clock
so a month-end date fixture is reproducible also freezes cache expiry**, so a TTL'd
entry under a frozen clock never expires. `TestPinnedClockAlsoFreezesCacheExpiry`
asserts both halves so the consequence cannot quietly stop being true.

**One instant per decision.** The clock is read **once per decision**, not once per
reference and not once per rule:

```
Clock → DecisionInstant (scope) → Input.Now → __now (evaluation environment)
```

A decision evaluates a rule per candidate grant — twice per candidate for a
rule-backed `Enumerate` — so the decision engine opens a scope
(`rules.WithDecisionInstant`) at each decision boundary (`Check`, `Enumerate`,
`Explain`) and every evaluation underneath shares the **first** instant taken.
Nesting is idempotent, so a per-candidate decision inside an enumeration keeps the
enumeration's instant. Outside a scope — a host driving `rules.Engine` directly —
each evaluation takes its own snapshot from the same clock. Either way a rule that
refers to `NOW` twice **cannot straddle a tick**.

The scope is a **memo, not an injection point**: the value always comes from the
engine's `Clock`. There is no API for a caller to supply its own instant, because
two callers disagreeing about "now" is exactly the defect the single clock exists
to prevent. Likewise there is **no timezone knob**: the instant is converted to
UTC at the snapshot boundary *and* again when the evaluation environment is built,
so no zone can reach a comparison however an injected clock is implemented.

**`NOW` is not a variable root, and must never become one.** It is exposed to the
compiled program as `__now`, which is absent from the four roots above — exactly
as the notes sink `__notes` is. That is a security boundary, not a style choice:
reflective **method calls survive `expr.DisableAllBuiltins`**, so an exposed root
would make `NOW.AddDate(0, -3, 0)` and `NOW.Unix()` well-formed var paths — an
unclamped calendar walk reachable from any rule. The `relativeDate` node renders
literal arguments to the `$rel` dispatcher and reads `__now` as one of them, the
way `$date` already reads `__notes` — see "Relative dates".
`TestNowIsNotReachableFromARule` fails if the identifier is ever added to
`allowedRoots`.

**The cache never carries an instant.** The rendered source names the identifier,
never a resolved timestamp, so a cached program cannot answer against a stale
`NOW`: compile, advance the clock, re-evaluate, and the cache **hits** while the
evaluation sees the new instant.

**`Explain` records it.** `Trace.Now` is the instant the decision's rules resolved
against — always UTC, taken once for the whole trace, zero when the decision
evaluated no rule. It makes a date-sensitive verdict reproducible: replay the
request with the clock pinned to it and the verdict must be identical. It is
deliberately **not** rendered by `Trace.String()`, which promises byte-identical
output for the same decision.

**Testing.** Time-dependent tests pin the clock and never assert against
`time.Now()` — an expectation derived from the real clock only exercises the
interesting calendar path on the days the calendar happens to cooperate.

## One bag per decision

`object` is legitimately per object — a decision over a thousand objects reads a
thousand metadata bags, each describing something different. `principal` and
`account` are not: both are **constant for the whole decision**, so they are
resolved **once per decision**, exactly as `NOW` is snapshotted once:

```
PrincipalResolver → DecisionAttributes (scope) → principalBag → Input.Principal
AccountResolver   → DecisionAttributes (scope) → accountBag   → Input.Account
```

The decision engine opens the scope (`rules.WithDecisionAttributes`) at the same
three boundaries it opens `WithDecisionInstant` at — `Check`, `Enumerate`,
`Explain` — and every evaluation underneath shares the **first** bags resolved.
Nesting is idempotent, so a per-candidate decision inside an enumeration keeps the
enumeration's bags. Outside a scope — a host driving `rules.Engine` directly, a
hand-built `rules.Input` — each evaluation resolves its own, which is the same
guarantee narrowed to one evaluation. **The scope is never mandatory**; unscoped
is correct, merely unmemoized.

**It is a correctness property, not only a cost one.** The cost is plain: a
rule-backed `Enumerate` evaluates its rule twice per candidate, so N objects
without the scope means 2N principal fetches and 2N account fetches — a host
round-trip inside a loop, and the `Check` NFR broken outright. The correctness
half is that an attribute bag is served through a cache **with a TTL**: a TTL
expiring halfway through an enumeration would judge the first candidates against
one version of the principal and the last against another, returning a set no
single view of the principal justifies, with no error anywhere. That is the same
defect as a decision straddling a tick, and it takes the same fix.

**A memo, not a cache, and not an injection point.** It holds ONE principal bag
and ONE account bag rather than a map of them, so it cannot grow with what a
decision looks at and it dies with the request. There is no API for a caller to
supply a bag: the value always comes from the engine's resolver, for the reason
there is no API to supply an instant. It is also **keyed** — it records which
subject each bag was resolved for and re-resolves on a mismatch — so a scope that
travels somewhere its author did not picture cannot serve one principal's
attributes as another's; the failure mode is a re-fetch, which is correct and
slow. **Failures are not memoized**: a directory that blinks is retried, and what
a failure means to a decision stays the resolver's contract.

**Read-only, and more so than before.** A resolved bag was always read-only
transitively; memoizing widens who holds it to *every evaluation in the decision*,
on top of every concurrent decision for the same key through the provider's
cache. One write at any depth is therefore every rule, for every object, in every
in-flight decision for that subject. The engine stamps the floor into a **fresh**
map (`principalBag` / `accountBag`) and never into the resolver's.

A decision at the account wildcard resolves no account bag at all — `"*"` never
reaches a resolver — so there is nothing to memoize there.

## Missing fields: nested access is nil-safe, but `nin` grants

Metadata is ragged — one object carries `owner`, the next does not. Two rules of
thumb, and the second is a trap.

**Nested access is nil-safe.** A `var` may read a nested path
(`object.owner.dept`). Every segment after the root renders with optional
chaining — `object?.owner?.dept` — so a **missing intermediate yields nil and the
enclosing comparison goes false**, never a runtime error. Without it, expr-lang
raises `cannot fetch dept from <nil>` and the rule fails with
`APERTURE_RULE_EVAL` at `Check` time on exactly the objects that lack the field.
A present path is unaffected. This is **render-time only**: the AST and its JSON
still store the plain dotted path, so the editor/state-file contract is unchanged.

**⚠️ `x not in <absent field>` is `true` — a `nin` rule GRANTS on objects missing
the field.** This is pre-existing expr-lang behavior (the correct dual of
`x in <nil>` being `false`), not something Aperture added, but list-valued and
nested metadata make it easy to hit: a deny-list over a column an object does not
have passes everything. Require the field explicitly when that matters:

```go
rules.And(
    rules.Compare(rules.OpNe, rules.Var("object.blocklist"), rules.Lit(nil)),
    rules.Compare(rules.OpNin, rules.Var("principal.id"), rules.Var("object.blocklist")),
)
```

Other missing-field behavior: equality against a missing field is `false`; an
ordered comparison (`lt le gt ge`) against one is an `APERTURE_RULE_EVAL` runtime
error.

**Every collection operator follows the same rule: an absent field reads as an
EMPTY collection, never an error.** Which way that falls depends on the
operator's polarity, and the negative ones grant just like `nin`:

| Field absent | `has` | `hasAll` | `hasAny` | `hasKey` | `isNotEmpty` | `exists` | `hasNone` | `subsetOf` | `isEmpty` |
|---|---|---|---|---|---|---|---|---|---|
| result | `false` | `false` | `false` | `false` | `false` | `false` | **`true`** | **`true`** | **`true`** |

## Wrong-shaped fields: deny-safe, and noted

A field of the **wrong shape** — `has` over a string, `hasAll` over a number,
`isEmpty` over a bool — **evaluates to `false`**. Every collection operator, no
exceptions, including the negative ones: `nin` over a string is `false`, not
`true`. A mismatch never matches, whatever the operator's polarity, so mistyped
data can only ever deny.

It is never an `APERTURE_RULE_EVAL` error. Load-time validation of the value
model stops mistyped data from the CSV and inline loaders, but it cannot cover a
host-implemented `ObjectProvider` or the `principal` attribute bag, which bypass
loaders entirely — and one mistyped field must not break every `Check` that
touches it.

Note the asymmetry with the absent-field table above, and that it is deliberate:

| operand | policy |
|---|---|
| absent (`nil`) | reads as an **empty collection**; the operator's own semantics decide (negative ops match) |
| wrong shape | the comparison is **`false`**, whatever the operator |

Which shapes each operator accepts:

| operators | accepts | expected-shape wording in a note |
|---|---|---|
| `in` `nin` `has` `hasKey` | array (elements) or object (keys) | `collection` |
| `hasAll` `hasAny` `hasNone` `subsetOf` | array | `array` |
| `isEmpty` `isNotEmpty` | array, object, or string | `array, object, or string` |
| `exists` | anything — a nil test cannot mismatch | — |

## Malformed dates: deny-safe, and noted

Date operators follow the same policy, for the same reason: metadata reaches the
evaluator from host-implemented `ObjectProvider`s and from the principal
attribute bag, neither of which any loader validates, so one malformed date must
not break every `Check` that touches the field.

**Any operand that will not parse makes the comparison `false`.** Never an
`APERTURE_RULE_EVAL` error. Both operands are parsed to instants on every
evaluation — comparing the canonical strings is not a permissible shortcut,
because the date-only form is a strict prefix of the timestamp form.

| operand | result | note |
|---|---|---|
| absent (`nil`) | **`false`** | `shape_mismatch`, `expected date, got absent` |
| a number, bool, array, or object | **`false`** | `shape_mismatch`, `expected date, got number` |
| a string that is not one of the two canonical forms | **`false`** | `date_invalid` |
| `between` whose lower bound is after its upper bound | **`false`** | `date_bounds_inverted` |
| a `relativeDate` that cannot be resolved (no reference instant, or an offset that leaves the four-digit year range) | **`false`** | `shape_mismatch`, `expected date, got absent`, path `(expression)` |

**Note the asymmetry with the collection operators, and that it is deliberate.**
A collection operator gives an absent field a meaning — it reads as an *empty
collection*, and the operator's own polarity decides from there, which is why
`hasNone` grants on missing data. A date operator has no such reading: there is
no empty date, and all eight date operators are **positive**, so none of them can
match on an absence. Absent is therefore uniformly `false`, exactly as a
wrong-shaped operand is, and no date operator ever records `absent_field`. (A
negative date operator, were one ever added, would need it.)

An unparseable string is kept **separate** from a shape mismatch because the fix
differs: the shape was right and the content was not, so the answer is to
canonicalise the data — or to declare the field as a date in the loader (a CSV
`:date` / `:datetime` column, or the seed document's `field_types:`) so it is
validated at load instead of denying silently at decision time.

### Evaluation notes

A silent `false` is how an access-control bug hides, so every mismatch is
**recorded** and surfaced in `Explain` on every decision surface (library, CLI,
Twirp `trace_json`, MCP):

```
object.tags: expected collection, got string
object.hired_at: not a canonical date; before expects 2006-01-02 or 2006-01-02T15:04:05Z
object.hired_at: between bounds are inverted; the lower bound is after the upper bound, so nothing can match
```

Notes are `rules.Note` values — `Kind`, `Rule`, `Op`, `Path`, `Expected`,
`Actual` — carrying **shape and path only, never a metadata value**. That last
point is not a nicety for dates: a date is often personal data (a birth date, a
termination date), and the same trace crosses the Twirp and MCP surfaces. Six
kinds are recorded today:

- `shape_mismatch` — a collection operator met a non-collection, **or** a date
  operator met a value that is not a string (including an absent field).
- `absent_field` — an operator **matched because the field is missing** (the
  `nin` / `hasNone` / `subsetOf` / `isEmpty` grant above), which is otherwise
  invisible in the verdict. No date operator records this.
- `date_invalid` — a date operator met a **string** that is not one of the two
  canonical forms. `Expected` names the forms; the offending value never appears.
- `date_bounds_inverted` — a `between` was written with its lower bound after its
  upper bound, so it matches nothing. `Path` names the compared field.
- `dangling_reference` — a **declared object reference** pointed at an identity
  the target type's provider no longer serves, so that identity was **skipped**
  and the decision proceeded. `Path` is the declaration
  (`dataset.current_brands`), `Expected` the declared target object-type,
  `Actual` `"absent"`. The missing identity itself is never carried — it goes to
  the operator's warning log instead. This is the one kind **not** recorded by
  rule evaluation: it comes from the engine's enumeration path
  (`engine/reference.go`), but it is the same class of observation and rides the
  same collector, so it renders identically. See `skills/object-references.md`.
- `attributes_floor_only` — a rule read a **host-defined field** off `principal`
  or `account` while that root carried nothing but the engine's floor
  (`principal.id` + `principal.kind`, `account.id`). `Path` is the root; there is
  nothing else to say, because there was nothing there. It is the mitigation the
  attribute leniency contract promises: a missing bag makes every comparison
  against it false, which is deny-safe in an **inclusive** grant and
  access-**widening** in an **exclusive** one, and nothing in the verdict says so.

  Two things about it are deliberate. It is **one kind, not two** — "nothing was
  wired" and "a directory answered and had nothing" are different operator
  situations, but the distinction this layer can draw ("is a resolver
  installed?") is not that one: an `AttributeRegistry` with no slot for this
  principal's kind is installed and answers identically, so a second kind would
  be confidently wrong a lot of the time. And it fires only when the rule
  **names** a non-floor path on the root — otherwise every trace of every
  rule-backed grant in the many deployments that wire no attribute provider
  would carry it, and the traces where it matters would be unfindable.

The channel is opt-in and costs the decision path nothing: `Check` and
`Enumerate` install no collector, so nothing is recorded and nothing is
allocated; `Explain` installs one per grant.

> **A `dangling_reference` therefore needs a collector the caller installs.**
> `Explain` takes a single object and never dereferences, so there is no
> `Explain`-of-an-enumeration to read it back from; wrap the `Enumerate` call in
> `rules.WithNoteCollector` to see it. No non-Go surface does that today, so over
> Twirp, the CLI, and MCP the **warning log is the only signal**.

Direct library use:

```go
ctx, notes := rules.WithNoteCollector(ctx) // engine.Explain does this for you
allowed, err := compiled.Eval(ctx, in)     // notes land in the collector
_ = notes.Notes()

ok, ns, err := compiled.EvalWithNotes(ctx, in) // or take them back directly
```

Notes are **diagnostic only** — they never influence a verdict.

## Validation before evaluation

`Compiler.Compile` (and `Engine.Compile`) validate and type-check before any
evaluation, surfacing coded errors:

- `APERTURE_RULE_INVALID` — malformed AST (bad arity, missing operand, a right
  operand on a unary op, the wrong operand shape for a collection op, non-scalar
  literal, non-identifier variable path, a `call` naming a denied predicate
  builtin, a `relativeDate` outside a date operator or carrying an anchor / unit
  / snap outside its closed set or a non-integer offset).
- `APERTURE_RULE_UNKNOWN_VARIABLE` — a variable root outside `object` /
  `principal` / `account` / `action`.
- `APERTURE_RULE_TYPE_ERROR` — type-incompatible comparison, non-boolean result,
  or a call to an unregistered function (caught by the expression type-checker).
- `APERTURE_RULE_EVAL` — a runtime failure (e.g. an ordered comparison against a
  metadata field the object lacks) or a non-boolean result. A wrong-shaped
  collection operand is **not** one of these, and neither is an absent,
  wrong-shaped, or unparseable **date** operand: both deny with a note.
- `APERTURE_RULE_NOT_FOUND` — a scope rule reference the `RuleSource` cannot
  resolve.
- `APERTURE_RULE_UNDECLARED_ATTRIBUTE` — the rule reads an attribute key off
  `principal` or `account` that the deployment's shared wiring does not **declare**
  for that root. See "Declared attribute keys" below.

### Declared attribute keys

A deployment's shared wiring may declare, per attribute slot, the keys that slot
**guarantees** (`declared_keys:` on an `attribute_providers:` entry). When it does,
`rules.ValidateASTDeclaring` — and so `service.ValidateRule`, `service.PutRule` and
`service.EvaluateRulePreview` — refuses a rule naming any other key, with
`APERTURE_RULE_UNDECLARED_ATTRIBUTE` naming the key and the slot.

**It is entirely opt-in.** A slot that declares no key set is not enforced at all,
so `rules.ValidateAST` (a zero-value `rules.DeclaredAttributeKeys`) refuses nothing
and every rule that validated before declaring existed still validates.

Why it exists: rules are **model state** and live in the shared database, so every
instance evaluates the same rules — while an attribute slot's **local** layer may
add keys the shared directory does not carry. A rule naming one of those decides on
the instance that has the key and reads a **missing path** on the instance that does
not, and a missing path neither denies nor errors: it makes every predicate over it
false, so an inclusive grant denies and an **exclusive grant stops excluding**, with
nothing in any verdict, trace or note to say why. The declared set makes the extra
keys **unreachable from any rule the deployment can validate**, so they are inert.

The refusal fires on the paths a rule **NAMES** — `rules.walkVarFields` is the one
walk, shared with `readsBeyondFloor` and the `attributes_floor_only` note — so a key
behind a short-circuiting `&&`, inside a list, or under a `not` is still refused. A
bare `principal` / `account` (a whole-bag read) is refused too, and only the first
path segment past the root is compared, so a declared `metadata` permits
`principal.metadata.department`.

**Refused at authoring, never at decision time.** A decision-time refusal would let
a bad rule ship and then fail in production on some instances and not others — the
same divergence, wearing an error message — and a rule already stored keeps deciding
exactly as it did when the declared set later narrows.

The **floor keys** `principal.id`, `principal.kind` and `account.id` are always
readable and are never part of a declared set: they are stamped last by
`principalBag` / `accountBag` and are present in every deployment. The `principal`
root is enforced only when **both** principal slots declare, and then against their
**union** — `skills/attribute-providers.md` ("The floor sits above the declared set")
has the full argument for both halves.

Evaluation is pure and deterministic over a fixed metadata snapshot: all of
`expr-lang`'s builtins are disabled, so no wall-clock or random function is
reachable. The curated pure set is `lower`, `upper`, `contains`, `startsWith`,
`endsWith`, `len`, plus the collection-operator backings `hasAll`, `hasAny`,
`hasNone`, `subsetOf`, `isEmpty`, `isNotEmpty` — all deterministic and
side-effect-free. A host adds its own with `rules.Function` / `WithFunction`.
Note that `contains`, `startsWith` and `endsWith` are reserved as **infix
operators** by expr's grammar, so they are registered but not reachable through a
`call` node; the other nine are.

Date operators do not break that: the reference instant is **data** (`__now`),
fixed for the whole evaluation, not a function a rule could call. See "The clock,
and one `NOW` per decision".

## Compile-once, cache

A rule is rendered to its canonical expr-lang expression, hashed (sha256), and the
compiled program is cached by that hash — so distinct rule references whose ASTs
render identically share one compiled program, and per-`Check` cost is bounded
(the NFR lever E4-S4 tunes). The cache is concurrency-safe with an optional TTL
read from the engine's injected `Clock` (`WithCacheTTL` / `WithClock`);
`CacheStats` exposes hit/miss/eviction counters. That `Clock` is the **same** one
the per-decision `NOW` is snapshotted from — see "The clock, and one `NOW` per
decision" for what pinning it does to expiry.

A cache **hit** takes only the read lock: the counters are `sync/atomic`, so
concurrent evaluations no longer serialise on a counter bump they never read.
That matters most for rule-backed `Enumerate`, which evaluates the rule once per
candidate to gather a grant's members and again per candidate in the decision
walk — see [benchmarks](../docs/benchmarks.md), "Rule-backed `Enumerate`". The
counters' meaning is unchanged, and `CacheStats` samples them atomically rather
than under a lock, so the four fields are not one instant's snapshot; they are
exact for a sequential caller.

What the cache removes is the **expr-lang compile**, not the AST walk: `Selected`
calls `Engine.Compile` per evaluation, and `Compile` re-validates, re-renders, and
re-hashes the AST before it can probe the cache by hash. That walk is still on
every decision, but it is now **flat in literal count** — ~0.65 µs / 5 allocs /
288 B for a 3-node rule and ~0.65 µs / 5 allocs / 320 B for a 6-node one. What
remains is the render buffer, the rendered string, and the sha256 + hex of it.

Getting there was issue #9. Both `Validate` and `render` used to decode every
literal through a `json.NewDecoder`, twice per decision, at ~14 allocs and
~4.9 KB per literal — so a rule's per-`Check` cost scaled with how many literals
it happened to contain (a 6-node rule cost 47 allocs against a 3-node rule's 19,
for expressions that evaluate identically). `rules.classifyScalar` now identifies
the common literal forms straight from the raw JSON bytes with no allocation, and
defers to the decoder only for escaped/non-ASCII strings, composites and
malformed input — so it can never widen what `Validate` accepts. The
`bench/collection_test.go` budget that tracked this is now 2 allocs, measured at
0.0.

## What a rule costs a `Check`

Measured in `bench/` (Apple M1 Max); see [benchmarks](../docs/benchmarks.md) for
the fixture, the full tables, and the two findings.

| Shape | Per-evaluation cost | Notes |
|---|---|---|
| scalar comparison | ~149 ns, 3 allocs | the control |
| list-literal collection (`in [...]`) | ~167 ns, 3 allocs | the render E5-S1 leaves native — the operator itself is free |
| nested read (`object?.owner?.dept`) | ~210 ns, 4 allocs | optional chaining is render-time only |
| collection over a **variable** (guarded `$op`) | ~12 ns and ~16.9 B **per element** | linear in the array; ~530 ns over 12 elements, ~78 µs over 7 281 |

The per-element term is the one to design around: a rule reading a large array
puts an O(n) traversal on the decision hot path, and the array's maximum length is
set by `provider.ValueLimits.MaxBytes` (see the `metadata-values` skill).

## Wiring

```go
eng := rules.NewEngine(ruleSource, providerRegistry) // *provider.Registry is the MetadataFetcher
authz := engine.New(store, engine.WithScopeResolution(nil, engine.ScopeDeps{
    Lister: providerRegistry,
    Rules:  eng,
}))
```

`RuleSource` resolves a scope strategy's opaque `rule=` reference to a `*Rule`
(its AST); `rules.MapSource` is the in-memory default. The metadata fetcher is
any `Fetch(ctx, id) (map[string]any, error)` — `*provider.Registry` fits directly.
