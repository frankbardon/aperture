---
name: rules-engine
description: The rules engine evaluates a JSON rule AST as a Pulse expression over object metadata and principal/action context, compiling once and caching, to back the inclusive/exclusive scope resolvers.
applies_to: [cli, http, mcp]
---

# Rules engine

The `rules` package decides object-membership selection (and, by extension,
allow/deny) from a domain object's metadata plus the principal/action context. It
is the rule-backed variant of the inclusive/exclusive scope resolvers (E2-S1): an
`*rules.Engine` satisfies `scope.RuleEvaluator`, so wiring it as
`engine.ScopeDeps{Rules: eng}` turns on rule-driven scope membership.

Expressions are evaluated by Pulse's expression evaluator (`expr-lang/expr`, the
same pure-Go engine Pulse uses for its `FILTER_EXPRESSION` predicate). Aperture
does not hand-roll a parser and stays `CGO_ENABLED=0` — it never pulls Pulse's
geo/h3 packages.

## The rule AST (the editor + state-file contract)

A rule is a tree of typed `rules.Node` values. The set is small, explicit, and
closed, so the Rete.js editor (E7-S2) maps its palette one-to-one and the state
file (E5-S2) persists the same shape. There is no second rule format.

| `type` | Fields | Meaning |
|---|---|---|
| `and` / `or` | `children` (>= 2) | logical conjunction / disjunction |
| `not` | `children` (exactly 1) | logical negation |
| `compare` | `op`, `left`, `right` | binary comparison |
| `var` | `name` (dotted path) | context variable reference |
| `literal` | `value` (scalar JSON) | string / number / bool / null constant |
| `list` | `items` | list literal (right side of `in`/`nin`) |
| `call` | `name`, `items` (args) | call to a registered pure function |

`compare` ops: `eq ne lt le gt ge in nin`. The JSON form is stable and
round-trips (marshal -> unmarshal -> marshal is byte-identical), including falsy
literals (`false`, `0`, `""`, `null`).

```json
{"type":"compare","op":"eq",
 "left":{"type":"var","name":"object.classification"},
 "right":{"type":"literal","value":"public"}}
```

## Context variables

The expression environment exposes four roots; a variable under any other root is
an unknown variable, rejected at validation:

- `object` — the object's metadata fields (read-only snapshot from the provider).
- `principal` — principal attributes; `principal.id` always present. Richer
  attributes come from a `PrincipalResolver` (`WithPrincipalResolver`).
- `account` — account attributes (reserved; empty until wired).
- `action` — the action verb (a string).

## Validation before evaluation

`Compiler.Compile` (and `Engine.Compile`) validate and type-check before any
evaluation, surfacing coded errors:

- `APERTURE_RULE_INVALID` — malformed AST (bad arity, missing operand, non-scalar
  literal, non-identifier variable path).
- `APERTURE_RULE_UNKNOWN_VARIABLE` — a variable root outside `object` /
  `principal` / `account` / `action`.
- `APERTURE_RULE_TYPE_ERROR` — type-incompatible comparison, non-boolean result,
  or a call to an unregistered function (caught by the expression type-checker).
- `APERTURE_RULE_EVAL` — a runtime failure (e.g. an ordered comparison against a
  metadata field the object lacks) or a non-boolean result.
- `APERTURE_RULE_NOT_FOUND` — a scope rule reference the `RuleSource` cannot
  resolve.

Evaluation is pure and deterministic over a fixed metadata snapshot: all of
`expr-lang`'s builtins are disabled, so no wall-clock or random function is
reachable. The only callable functions are the curated pure set (`lower`,
`upper`, `contains`, `startsWith`, `endsWith`, `len`) plus any a host registers
with `rules.Function` / `WithFunction` (the parallel to Pulse's
`Options.Extensions.ExprFunctions`).

## Compile-once, cache

A rule is rendered to its canonical Pulse expression, hashed (sha256), and the
compiled program is cached by that hash — so distinct rule references whose ASTs
render identically share one compiled program, and per-`Check` cost is bounded
(the NFR lever E4-S4 tunes). The cache is concurrency-safe with an optional TTL
read from an injected `Clock` (`WithCacheTTL` / `WithClock`); `CacheStats`
exposes hit/miss/eviction counters.

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
