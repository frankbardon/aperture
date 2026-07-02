# Decision API

The `engine` package is Aperture's Policy Decision Point. It exposes three single
operations — `Check`, `Enumerate`, and `Explain` — on `*engine.Engine`. This page
gives their real signatures, input and result shapes, and the rule for choosing
between them. Their bulk and impersonation-aware forms live in
[Batch operations](batch.md) and [Impersonation](impersonation.md).

All three are methods on an engine you construct once and reuse. The engine is
stateless beyond its storage handle and safe for concurrent use to whatever
degree the underlying `Storage` is.

## Constructing the engine

```go
func New(store model.Storage, opts ...Option) *Engine
```

With no options the engine uses the literal identity-pattern coverer — a grant's
object pattern is matched directly against the requested object. The options
extend that behaviour:

| Option | Effect |
|---|---|
| `WithScopeResolution(registry *scope.Registry, deps ...ScopeDeps)` | Consult each grant's pluggable scope resolver (selected by its permission's scope-strategy) for object membership, instead of only literal pattern matching. A `nil` registry uses `scope.DefaultRegistry()`. |
| `WithMembershipEnforcement()` | Require the request's principal to be a member of the active account before any grant is consulted. A non-member is denied at the door (a fail-closed default-deny), rather than erroring. Off by default. |
| `WithClock(now func() time.Time)` | Override the engine clock. It governs impersonation time-box expiry only; the non-impersonated path never reads it. Production uses `time.Now`. |

Two further seams return a **shallow copy** of the engine rather than mutating it,
for the read-only what-if paths: `(*Engine).WithStore(store)` re-points the copy
at a different (e.g. overlay) store, and `(*Engine).WithRuleEvaluator(re)`
redirects rule-backed scope strategies at a different rule evaluator. Both leave
the original engine untouched, so a live engine and a transient what-if engine
never interfere. The facade's [`Simulate`](service-facade.md#simulate--what-if)
path is built on exactly these.

## Check

```go
func (e *Engine) Check(ctx context.Context, req Request) (Decision, error)
```

`Check` resolves a single authorization decision: *may this principal take this
action on this one object, in this account?*

### Request

`Request` is a value type; every field is mandatory.

```go
type Request struct {
	Account   string // active account the decision is scoped to
	Principal string // id of the principal requesting access
	Action    string // the verb being attempted, e.g. "read"
	Object    string // canonical object-identity string
}
```

`Principal` is a principal **id** (the key storage and the subject set are keyed
on), not the principal's identity string. `Object` is a canonical object-identity
string such as `account:acme/project:atlas/document:42`. Grants stamped to any
account other than `Account` are never consulted (the sole exception is a grant
stamped to the account wildcard `*`, which spans all tenancies).

### Decision

```go
type Decision struct {
	Allow            bool                  // the verdict: true permits, false denies
	Reason           string                // human-readable explanation naming the deciding grant(s)
	DecidingGrantIDs []string              // ids of the grant(s) that produced the verdict, sorted; empty on a default-deny
	Impersonation    *ImpersonationContext // non-nil only under an active impersonation session (see Impersonation)
}
```

`Reason` names the deciding grant(s), their specificity, and how many grants were
considered. `DecidingGrantIDs` is sorted for determinism and is empty on a
default-deny. `Impersonation` is `nil` on the ordinary path.

### Error contract

`Check` **never** returns an allow-on-error. Any operational failure — a
malformed request, an unknown principal, a storage fault — is returned as an
`APERTURE_*` coded error and the caller treats it as a *non-decision*. A
well-formed request that simply matches no grant is a clean default-deny
(`Allow: false`, no error). Default-deny is the floor: with no candidate grant
the answer is DENY.

```go
dec, err := eng.Check(ctx, engine.Request{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	// operational failure — not a decision
	return err
}
fmt.Println(dec.Allow, dec.Reason)
```

> Building your own surface? Prefer the facade's
> [`Service.Check`](service-facade.md#check-fail-closed), which folds operational
> errors into a fail-closed deny so a decision point can never fail open. The raw
> `engine.Check` here returns those errors for the facade to render.

## Enumerate

```go
func (e *Engine) Enumerate(ctx context.Context, req EnumerateRequest) ([]string, error)
```

`Enumerate` is the inverse of `Check`: it returns the object ids under a pattern
that the principal may take the action on, in the active account. Every id it
returns is one `Check` would allow — a denied object is **never** returned — so
the two operations agree by construction.

### EnumerateRequest

```go
type EnumerateRequest struct {
	Account   string // active account the enumeration is scoped to
	Principal string // id of the principal whose access is enumerated
	Action    string // the verb being enumerated, e.g. "read"
	Pattern   string // identity pattern bounding the search
	Limit     int    // caps the number of returned ids; <= 0 means the default bound
}
```

`Pattern` both bounds the candidate set and is intersected with each grant's own
scope — for example `account:acme/**` (everything in the account) or
`account:acme/document:*` (every document at the account root). `Limit` caps the
result; a non-positive `Limit` (or one above the default) is clamped to
`engine.DefaultEnumerateLimit` (1000), so an enumeration can never materialise an
unbounded set. Object order is deterministic (sorted by canonical id).

```go
ids, err := eng.Enumerate(ctx, engine.EnumerateRequest{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/project:atlas/**",
	Limit:     100,
})
```

An operational failure — a storage fault, an unresolvable scope strategy, or an
unconfigured object lister an implicit/exclusive grant needs — is returned as a
coded error, never a silent partial set.

## Explain

```go
func (e *Engine) Explain(ctx context.Context, req Request) (Trace, error)
```

`Explain` resolves the request *exactly* as `Check` does but records the full
derivation instead of only the verdict. Use it as a diagnostic — the "why" behind
a verdict — not as an enforcement gate. It takes the same `Request` as `Check`.

### Trace

`Trace` is a **stable public contract**: the RPC surface, the MCP inspect tool,
and the what-if simulator all serialize it, so its fields are part of the API.

```go
type Trace struct {
	Request        Request               // the question that was asked
	Subjects       []model.Subject       // the principal's expanded subject set (itself, roles, groups)
	Considered     []GrantEvaluation     // every grant loaded, each tagged with how it fared
	MaxSpecificity int                   // top specificity among covering candidates; 0 when nothing covered
	Decision       Decision              // the final verdict — identical to what Check returns
	Impersonation  *ImpersonationContext // non-nil only under an active impersonation session
}
```

Each entry in `Considered` is a `GrantEvaluation` recording one grant's
contribution — its subject, permission, effect, object pattern, whether its
action matched, whether it covered the object and at what specificity, which
scope strategy it used, whether it was a deciding grant, and a short
human-readable `Outcome`. A grant that failed the action match is still listed
(with `ActionMatched: false`) so the trace shows what was ruled out.

`Trace` implements `String()`, which renders an operator-readable, deterministic
report:

```go
tr, err := eng.Explain(ctx, engine.Request{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	return err
}
fmt.Print(tr.String())
```

`tr.Decision` is byte-for-byte the decision `Check` returns for the same request,
so a surface can render a verdict and its explanation from a single `Explain`
call.

## When to use each

| Question | Operation |
|---|---|
| "May this principal do this one thing?" — an enforcement gate on the hot path. | `Check` |
| "Which of these objects may this principal act on?" — building a filtered listing or a picker. | `Enumerate` |
| "Why did that decision come out the way it did?" — a diagnostic, an audit view, a support tool. | `Explain` |

`Check` is the allocation-conscious hot path; reach for it in enforcement.
`Enumerate` is the most cache-sensitive op and is deliberately bounded — use it to
answer "what can they see", not as a substitute for repeated `Check`s on a known
object. `Explain` does the same work as `Check` plus recording the derivation, so
use it when a human (or a machine) needs to understand the verdict, not on every
hot-path call.

## Related

- [Batch operations](batch.md) — resolve many of these in one call.
- [Impersonation](impersonation.md) — the `*As` variants that resolve over an
  elevated subject set.
- [The service facade](service-facade.md) — the fail-closed wrapper surfaces
  call.
- [Concepts primer](../getting-started/concepts.md) — deny-overrides, specificity,
  and the subject set these operations resolve over.
