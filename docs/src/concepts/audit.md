# Audit trail

Aperture keeps an **append-only audit trail** (FR-25). The trail is weighted
toward safety-critical events without sinking the decision hot path, so it records
under two disciplines depending on the event class.

## Two recording disciplines

**Always + synchronous.** Every mutation, every impersonation event, and every
delegation is recorded the moment it happens, reliably, on the calling goroutine.
Mutations are not the hot path, so a synchronous, durable write is the right
trade — a caller can even surface a failed mutation-audit write if it chooses.

**Sampled + asynchronous.** Decision checks (`Check` / `Enumerate` / `Explain`)
are the hot path. They are recorded only when the configured `Sampler` keeps them,
and the keep is handed to a background writer over a buffered channel. **The
decision never blocks on the audit write:** if the buffer is full the event is
dropped best-effort, so an audit backlog can never regress the decision NFR.

```go
rec := audit.New(store,
    audit.WithSampleRate(0.01), // keep ~1% of decisions
    audit.WithBuffer(4096),
)
defer rec.Close() // flushes buffered decision events; call on shutdown
```

The `Recorder` owns one background writer goroutine, is safe for concurrent use,
and is drained deterministically by `Close`. The background writer runs detached
from any request context, so a cancelled request never aborts an in-flight audit
write.

| Method | Discipline | Notes |
|---|---|---|
| `Record(ctx, ev)` | always, synchronous | stamps id + timestamp, writes through the sink, returns the storage error |
| `RecordDecision(ctx, fn)` | sampled, asynchronous | invokes `fn` **only on a keep**, so an un-sampled decision pays nothing but the `Sampler` call; returns whether it was sampled |
| `Close()` | — | idempotent; flushes the buffer, waits for the writer. After `Close`, `RecordDecision` is a no-op; `Record` still writes |

### Sampling and determinism

The `Sampler` and the clock are **injected**, so tests are not flaky — inject a
deterministic sampler (e.g. keep 1-in-N via `SamplerFunc`) and a fixed clock
rather than relying on wall-clock time or an unseeded global rand. Production uses
`WithSampleRate`, a probabilistic sampler over `math/rand/v2`. With no sampling
option, decision audit is **off** (rate 0) while always-on events are still
recorded.

Construction options:

| Option | Effect |
|---|---|
| `WithSampleRate(r)` | probabilistic decision sampler, `r` clamped to `[0,1]` (0 disables decision audit, 1 records every decision) |
| `WithSampler(s)` | explicit `Sampler` (takes precedence over `WithSampleRate`; used in tests) |
| `WithBuffer(n)` | async writer buffer capacity (default 1024) |
| `WithClock(fn)` | override the timestamp clock (default `time.Now`) |
| `WithIDFunc(fn)` | override the event-id generator (default: random hex) |
| `WithErrorHandler(fn)` | observability callback for async write failures and buffer-overflow drops; never affects the decision path |

## The event shape

The `Recorder` persists through a `Sink` (`AppendAudit(ctx, ev)`), which
`model.Storage` satisfies. Each entry is a `model.AuditEvent` — a **public
contract** the audit viewer reads, so its field set is additive-only:

| Field | Meaning |
|---|---|
| `ID`, `Timestamp` | assigned by the recorder |
| `EventType` | broad category the query filters on: `mutation`, `decision`, `impersonation`, `delegation` |
| `Action` | the specific operation, e.g. `PutGrant`, `Check`, `Bestow`, `ImpersonationStart` |
| `Actor` | the principal that **really** acted |
| `EffectiveSubject` | the target whose authority was borrowed under impersonation (empty otherwise) |
| `ImpersonationMode` | `augment` or `become` under an impersonation session (empty otherwise) |
| `Account` | the active account the event was scoped to |
| `Target` | the entity, object, or resource the event concerns |
| `Outcome` | `allow`/`deny` for a decision, `success`/`failure` for a mutation/impersonation/delegation |
| `Reason` | human-readable explanation (deciding grants, or the failure cause) |
| `Details` | optional structured blob backends persist as JSON |

A record made under impersonation carries **both** the real actor and the
effective subject, so an impersonated action is never mis-attributed to the target
alone.

## Append-only, at the storage layer

The trail is append-only *by contract* in `model.Storage`, not by convention:

- `AppendAudit` — the only single-event write.
- `QueryAudit(filter)` — reads events matching an `AuditFilter` (actor, account,
  event type, outcome, time bounds, limit — each optional, ANDed together),
  returned **newest-first**. A zero filter returns the whole trail.
- `PruneAudit(policy)` — the only delete, and only in bulk: retention pruning by
  age (`Before`) and/or size (`MaxCount`), returning the count removed.

There is no update and no single-event delete, so a recorded event **cannot be
silently altered**. Backend failures surface as `APERTURE_STORAGE`.

## Related

- [The authz gate](authz.md) — the gate that guards the mutations this trail records.
- [Impersonation](impersonation.md) — why events carry both a real actor and an
  effective subject.
- [Delegation ("bestow")](delegation.md) — always-recorded bestow/revoke events.
- [Storage](storage.md) — the `Storage` backend the trail is persisted through.
