# Library overview

Aperture is **library-first**: the public Go packages at the module root are the
product, and every other surface — the [CLI](../getting-started/first-decision-cli.md),
the RPC/HTTP API, the MCP server, and the admin UI — is a thin translator over
them. This part of the book is the reference for embedding Aperture directly in a
Go host program.

If you have not run the [Library quickstart](../getting-started/library-quickstart.md)
yet, start there: it wires a store, an engine, and the facade end to end and asks
one `Check`. This section then documents the full decision API those pieces
expose.

## Two layers, one decision

A host program drives Aperture through two collaborating packages:

| Package | Type | Role |
|---|---|---|
| `engine` | `*engine.Engine` | The Policy Decision Point (PDP). It resolves a raw authorization question against storage with deny-overrides plus a specificity tiebreak. It is stateless beyond its storage handle and safe for concurrent use. |
| `service` | `*service.Service` | The decision **facade** every surface calls instead of touching the engine directly. It adds one shared fail-closed rendering policy, decision auditing, the what-if `Simulate` path, and — when wired with options — the mutation surface. |

The engine answers the pure question; the facade is where the surface-facing
policy lives (how an operational error becomes a rendered deny, when a decision
is audited, how a what-if overlay is layered). A host that wants the raw PDP can
call the engine; a host that is building its own surface should call the facade,
so it inherits the same fail-closed contract every built-in surface has.

```go
import (
	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

store := memory.New()
_ = store.Setup(ctx)

eng := engine.New(store)     // the PDP
svc := service.New(eng)      // the facade every surface calls
```

`engine.New(store, opts...)` and `service.New(eng, opts...)` both take functional
options; see [Constructing the engine](decision-api.md#constructing-the-engine)
and [The service facade](service-facade.md) for the options each exposes.

## The decision API

Both layers expose the same three operations, in single and bulk-batched forms,
with an impersonation-aware variant of each on the engine:

| Operation | Question it answers | Single | Bulk | Impersonated |
|---|---|---|---|---|
| **Check** | May this principal take this action on this one object? | `Check` | `CheckBatch` | `CheckAs` |
| **Enumerate** | Which objects under a pattern may this principal act on? | `Enumerate` | `EnumerateBatch` | `EnumerateAs` |
| **Explain** | *Why* was this decision reached — which grants and at what specificity? | `Explain` | `ExplainBatch` | `ExplainAs` |

The pages that follow cover each cluster:

- **[Decision API](decision-api.md)** — the single `Check` / `Enumerate` /
  `Explain` operations: real signatures, input and result shapes, and when to
  reach for each.
- **[Batch operations](batch.md)** — `CheckBatch` / `EnumerateBatch` /
  `ExplainBatch` and the generic `BatchResult[T]` type that keeps one bad query
  from failing a whole batch.
- **[Impersonation](impersonation.md)** — `CheckAs` / `EnumerateAs` /
  `ExplainAs` and the `ImpersonationContext` decorator that steers which subject
  set a decision resolves over.
- **[The service facade](service-facade.md)** — how surfaces call the facade, its
  fail-closed rendering, and the read-only `Simulate` / `SimulateExplain` what-if
  path with a worked example.

## Errors are always coded

Every failure Aperture returns across a package boundary is an `APERTURE_*`
coded error, never a bare string. Recover the code with `errors.CodeOf` rather
than matching on the message:

```go
import aerr "github.com/frankbardon/aperture/errors"

if err != nil {
	switch aerr.CodeOf(err) {
	case aerr.APERTURE_INVALID_INPUT:
		// the query was malformed — a caller bug
	case aerr.APERTURE_NOT_FOUND:
		// an unknown principal, permission, or entity
	default:
		// an operational failure (storage, an unresolvable strategy, ...)
	}
}
```

The engine and the facade differ in how they treat these errors — the engine
returns them, while the facade folds operational failures into a fail-closed
deny. The distinction is spelled out per operation on the pages below and in
[The service facade](service-facade.md). The full catalog lives in the
[Error Codes](../reference/error-codes.md) reference.

The vocabulary these operations are built from — principal, action, object,
pattern, specificity, grant, account — is defined in the
[Concepts primer](../getting-started/concepts.md) and expanded in the **Concepts**
section of this book.
