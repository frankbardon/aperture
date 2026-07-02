# Impersonation

The engine exposes an impersonation-aware sibling of each decision operation:
`CheckAs`, `EnumerateAs`, and `ExplainAs`. They resolve a decision over an
*effective* subject set — the target's authority, borrowed by an operator — while
recording the real operator for audit. They never mutate stored grants; an
`ImpersonationContext` only steers **which subject set** the engine resolves over.

## ImpersonationContext

The decorator that carries a session into the engine:

```go
type ImpersonationContext struct {
	RealActor        string    // the operator's principal id (the audit identity)
	EffectiveSubject string    // the target's principal id (whose authority is used)
	Mode             Mode      // augment or become (or none, which is inert)
	ExpiresAt        time.Time // the session's hard expiry instant
}
```

- **RealActor** is the operator — the principal that truly issued the request and
  under whose identity audit attributes the action.
- **EffectiveSubject** is the target — the principal whose authority the decision
  borrows.
- **Mode** selects augment vs become (below).
- **ExpiresAt** is a hard time-box the engine enforces with its injected clock. A
  presented-but-expired context fails closed to **no** elevation (the operator's
  own authority), never to the target's.

## Mode

```go
const (
	ModeNone    Mode = ""        // no impersonation — the inert zero value
	ModeAugment Mode = "augment" // ADD the target's permissions to the operator's own
	ModeBecome  Mode = "become"  // FULLY assume the target's identity for the decision
)
```

- **`ModeAugment`** resolves over the **union** of the operator's and the target's
  subject sets, but the operator keeps acting under its own identity. Use it to
  "see what they can see" while retaining your own authority.
- **`ModeBecome`** resolves over the target's subject set **alone**, as if the
  target had asked — the operator's own grants do not apply. Become is the
  strictly stronger mode and is gated by a stronger right (see the
  `impersonation` package). The audit trail still records the real operator.
- **`ModeNone`** is the inert default: a zero `ImpersonationContext` confers no
  elevation, and the `*As` operation delegates straight to its plain sibling.

`Mode.Valid()` reports whether a mode is recognised (none counts as valid).

## The operations

```go
func (e *Engine) CheckAs(ctx context.Context, req Request, ic ImpersonationContext) (Decision, error)
func (e *Engine) EnumerateAs(ctx context.Context, req EnumerateRequest, ic ImpersonationContext) ([]string, error)
func (e *Engine) ExplainAs(ctx context.Context, req Request, ic ImpersonationContext) (Trace, error)
```

Each takes the same request its plain sibling takes, plus the
`ImpersonationContext`. Behaviour is identical to the plain operation except for
the subject set the decision resolves over.

### Rules for an active session

An `ImpersonationContext` is **active** when its mode is augment or become and its
`ExpiresAt` is still in the future (per the engine's clock). For an active
session:

- The request's principal **must** be the operator: `req.Principal ==
  ic.RealActor`. A mismatch is a caller bug and surfaces as
  `APERTURE_INVALID_INPUT`, not a deny.
- Augment resolves over operator ∪ target subjects; become resolves over the
  target alone.
- The operator **and** the target must both be members of the active account,
  else the decision is a fail-closed deny — cross-account impersonation is
  refused. (`CheckAs`/`ExplainAs` return a deny with no deciding grant;
  `EnumerateAs` returns the empty set.)
- The returned `Decision` / `Trace` carries `ic` on its `Impersonation` field for
  audit. In a `Trace`, `Subjects` is the *effective* subject set while
  `Request.Principal` remains the real operator — so a trace shows both who asked
  and whose authority answered.

### Inert sessions fail closed

When `ic` is inert — mode none, or an **expired** session — the `*As` operation
delegates straight to its plain sibling. An expired become session therefore
resolves as the operator's own authority with no elevation, never as the
target's. Elevation never outlives its time-box.

```go
ic := engine.ImpersonationContext{
	RealActor:        "alice",  // the operator issuing the request
	EffectiveSubject: "bob",    // the target whose access is borrowed
	Mode:             engine.ModeBecome,
	ExpiresAt:        time.Now().Add(15 * time.Minute),
}

dec, err := eng.CheckAs(ctx, engine.Request{
	Account:   "acme",
	Principal: "alice", // MUST equal ic.RealActor
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
}, ic)
if err != nil {
	return err
}
// dec resolves over bob's authority; dec.Impersonation records alice as the real actor.
fmt.Println(dec.Allow, dec.Impersonation.RealActor)
```

## Carrying impersonation on the context

The engine also exposes context helpers so a middleware layer — most importantly
the audit layer — can read the real actor and effective subject of any decision
made while a session is set:

```go
func WithImpersonation(ctx context.Context, ic ImpersonationContext) context.Context
func ImpersonationFromContext(ctx context.Context) (ImpersonationContext, bool)
```

The `*As` entry points set this on the context they evaluate under; a surface may
also set it before calling so audit middleware wrapping the engine sees it.

## Related

- [Decision API](decision-api.md) — the plain operations these mirror.
- [The service facade](service-facade.md) — surfaces reach impersonation through
  the facade's impersonation service (wired with `WithImpersonation`).
- The `impersonation` package — session issuance and the rights that gate augment
  vs become.
