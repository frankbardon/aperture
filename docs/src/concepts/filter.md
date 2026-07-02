# Filtering entity lists

The `filter` package applies **server-side field predicates** to a list of
entity JSON bodies before they are returned to a client. It is the business
logic behind the admin UI's data-grid filters: the client sends a `Spec` (a set
of predicates), the server evaluates it against each entity, and only the
matches cross the wire. Rows filtered out are never sent — the filter runs where
the data lives, not in the browser.

## Why it is dynamic, not per-type

Aperture's entities are heterogeneous — accounts, principals, roles, grants, and
so on. Rather than a typed filter per entity, `filter` addresses fields by their
**JSON key** and evaluates over the decoded `map[string]any`. One predicate
engine works across every entity list. Two consequences fall out of this:

- **Text comparisons are case-insensitive.** Both the field value and the
  predicate value are lower-cased before comparing.
- **Array fields match if *any* element satisfies the predicate.** A principal's
  `roles` array matches `roles contains admin` when any single role does. JSON
  numbers and booleans are rendered to strings for the comparison (an integer
  prints without a trailing `.0`).

## Predicates and the spec

A `Predicate` is one field test — a `Field` (JSON key), an `Op`, and a `Value`:

```go
type Predicate struct {
    Field string
    Op    string
    Value string
}

type Spec struct {
    Predicates []Predicate
    MatchAny   bool // OR when true; AND (the default) when false
}
```

A `Spec` combines its predicates with **AND** by default, or **OR** when
`MatchAny` is set. `Spec.Empty()` reports whether the spec would constrain
nothing, so a caller can skip the work entirely.

## The operators

The operator set is deliberately small — "core":

| Op | Constant | Matches when the field… |
|---|---|---|
| `eq` | `OpEq` | equals the value (case-insensitive) |
| `contains` | `OpContains` | contains the value as a substring (case-insensitive) |
| `starts` | `OpStarts` | starts with the value as a prefix (case-insensitive) |
| `empty` | `OpEmpty` | is empty or absent — the value is ignored |

For `empty`, a field counts as empty when it is absent, `null`, an empty string,
or an empty array. For the three value operators, a predicate with an empty
`Value` is treated as **unusable** and ignored. An **unknown operator** also
makes its predicate a no-op — it is simply skipped, never an error.

## Evaluation and the fail-open rule

`Apply(entities, spec)` returns the subset of `entities` (each a JSON object
body) that satisfy the spec:

- An **empty spec** — no usable predicates — returns the input unchanged.
- Usable predicates are collected; then each entity is decoded and tested,
  combining predicate results with AND or OR per `MatchAny`.
- An entity whose JSON does **not** decode to an object is **kept**. This is the
  deliberate **fail-open** rule: a filter must never silently hide a row it could
  not evaluate. (Contrast the decision engine, which fails *closed*; a display
  filter is not an authorization boundary, so it errs toward showing data rather
  than hiding it.)

```go
matches := filter.Apply(bodies, filter.Spec{
    Predicates: []filter.Predicate{
        {Field: "kind", Op: filter.OpEq, Value: "user"},
        {Field: "roles", Op: filter.OpContains, Value: "analyst"},
    },
    // MatchAny false → both must hold (AND).
})
// matches holds only the user principals that carry an "analyst" role.
```

## Scope note

`filter` narrows a list the caller is *already* authorized to see — it is a
display convenience, not an access-control gate. The authorization decision that
determines *which* entities a caller may list at all is made upstream by the
[decision engine](../library/decision-api.md); a grant's own object membership is
decided by [scope strategies](scopes.md). Because the lists a filter runs over
are account-scoped upstream, filtering never widens visibility across the
[account isolation boundary](model.md#accounts-and-the-isolation-invariant).

## Related

- [The RBAC model](model.md) — the entities whose JSON bodies are filtered.
- [Scopes & scope strategies](scopes.md) — the authorization-side "which objects"
  question, distinct from display filtering.
- [Admin UI shell](../surfaces/admin-ui.md) — the data-grid this filtering backs.
