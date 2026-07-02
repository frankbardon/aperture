---
name: decision-api
description: The full decision API beyond Check — Enumerate (which objects a principal may act on), Explain (the structured decision trace), and bulk-batched forms of all three, all behind one service facade.
applies_to: [engine, service, twirp, mcp, what-if]
---

# Decision API

Aperture's Policy Decision Point answers three questions, each single and
bulk-batched (FR-10). All surfaces — the HTTP/Twirp service (E4-S1), the MCP
read-and-simulate tools (E4-S3), and the what-if simulator (E6-S4) — call ONE
facade (`service.Service`) over the engine, so the fail-closed policy and the
trace contract live in one place.

## The three ops

- **Check** `(account, principal, action, object) -> Decision{Allow, Reason,
  DecidingGrantIDs}`. The enforcement gate; deny-overrides with a specificity
  tiebreak. The hot path (NFR p99 < 1ms).
- **Enumerate** `(account, principal, action, pattern, limit) -> []objectID`.
  The inverse of Check: which objects under `pattern` the principal may act on.
  Every id returned is one Check would allow.
- **Explain** `(account, principal, action, object) -> Trace`. The full
  derivation: the subject set, every grant considered with its per-grant
  outcome, which grants decided the verdict, and the final Decision.

Each has a bulk form — `CheckBatch`, `EnumerateBatch`, `ExplainBatch` — that
takes many queries and returns results **aligned by index** (`result[i]` for
`query[i]`).

## Account isolation & membership

Every decision is scoped to an **active account** (`Request.Account`), and the
(principal, active-account) pair is a hard isolation boundary (FR-14): a
multi-account principal's grants in one account NEVER apply in another. This is
guaranteed at the storage seam — `GrantsForSubjects(account, subjects)` and
`GroupsForPrincipal` are account-scoped, so a grant stamped to another account is
never even loaded. The invariant holds identically under direct, role, group,
wildcard, and scope-strategy grants, and switching `Request.Account` changes the
effective grant set deterministically.

Accounts and memberships are first-class (`model.Account`, `model.Membership`):
a membership is the edge admitting a principal to an account. Enforcement of
membership is **opt-in**:

```go
eng := engine.New(store, engine.WithMembershipEnforcement())
```

With it on, a request whose principal is not a member of the active account is
denied at the door — a fail-closed default-deny (Check), an empty result
(Enumerate), and a deny Trace that considers no grants (Explain) — before any
grant is consulted. It is a defence-in-depth layer over the always-on
account-scoped grant query; off by default, deployments that model membership
purely through grants are unaffected.

## Enumerate is bounded

Enumerate is the most cache-sensitive op. It is deliberately bounded and never
enumerates unboundedly:

- Candidates come from each ALLOW grant's covered objects — a scope resolver's
  bounded `Members` (implicit/exclusive enumerate "all of type" through the
  provider `ObjectLister`; inclusive uses its id-list; literal yields a
  concrete-identity grant or an explicit "{a,b,c}" id-set expanded to its
  members, but never a wildcard), intersected with the query pattern.
- Each candidate is then run through the SAME deny-overrides/specificity
  decision as Check, so a candidate carved out by a more-specific or
  equal-specificity deny is dropped. A denied object is **never** returned.
- The result is capped by `Limit` (default `DefaultEnumerateLimit`), and each
  resolver's `Members` is itself bounded. Output order is deterministic
  (sorted by canonical id).

## The Explain trace (public contract)

`engine.Trace` is serialized by E4 and E6, so its shape is part of the API:

- `Request` — the question asked.
- `Subjects` — the principal's expanded subject set (itself, roles, groups).
- `Considered []GrantEvaluation` — every loaded grant with `ActionMatched`,
  `Covered`, `Specificity`, `Strategy` (the scope strategy consulted),
  `Deciding`, and a human-readable `Outcome` note. Action-mismatched and inert
  (dangling-permission) grants are listed too, so the trace shows what was
  ruled out, not only what decided.
- `MaxSpecificity` — the tier the tiebreak resolved at.
- `Decision` — identical to what Check returns.

`Trace.String()` renders an operator-readable report (subjects, each grant's
disposition, the verdict and reason), with the deciding grants marked.

## Fail-closed rendering

The facade renders engine outcomes per op:

- **Check / CheckBatch** keep the original contract: an input-validation error
  (`APERTURE_INVALID_INPUT` / `APERTURE_IDENTITY_INVALID`) is returned; every
  other engine error folds into a **deny** Result. A decision point never fails
  open.
- **Enumerate / Explain** return engine errors verbatim. Enumerate cannot fail
  open by construction (denied objects are excluded inside the engine), so an
  operational failure is a returned error, not a silent partial set. Explain is
  a diagnostic.
- The **batch** ops carry each item's error in its `BatchResult{Result, Err}`,
  so one bad query in a batch yields a per-item error and never fails the whole
  batch.

## Scoped-engine assembly

Enumerate/Explain over implicit/exclusive/rule strategies need the E2 pieces
wired together:

```go
eng := engine.New(store, engine.WithScopeResolution(nil,
    engine.ScopeDeps{Lister: providerRegistry, Rules: rulesEngine}))
svc := service.New(eng)
```

`*provider.Registry` satisfies the `ObjectLister` seam and `*rules.Engine`
satisfies the `RuleEvaluator` seam. The assembly is optional — with no
providers the literal default still works, and Check never needs the lister
(membership is computable without enumeration). E4-S1 builds this graph in the
`serve` DI wiring.
