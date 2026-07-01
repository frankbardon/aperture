# Benchmarks & the performance NFR (FR-31)

Aperture's decision hot path carries a hard Success Metric:

> **p99 cached `Check` < 1 ms** and **≥ 10 000 checks/sec/instance.**

This document describes how that NFR is measured, how it is asserted without
flaking CI, and the committed numbers from the optimization pass (E4-S4).

## The suite

Everything lives in [`bench/`](../bench). It seeds a **sizable** authorization
model (not a three-grant toy) and exercises the full decision facade
(`service.Service.Check`), so the numbers reflect what a surface actually pays.

### Fixture (`bench/fixture_test.go`)

`buildModel` seeds an in-memory store with the generic org → project → document
domain at scale:

- 8 accounts, 60 roles, 60 groups, 480 principals;
- per account, every role carries wildcard allow-read / allow-write on its own
  project plus a more-specific deny-read carve-out, every group a
  document-scoped read, and `group0` an account-wide broad read plus a broad
  deny — overlapping candidates of differing specificity;
- 60 concrete (wildcard-free) document grants per account so `Enumerate` has a
  real, bounded candidate set to materialise.

The representative cached `Check` (`user0` reading a document under `proj0`)
resolves a subject set of six subjects to **~73 applicable grants**, of which
several match at different specificities — so deny-overrides + the specificity
tiebreak genuinely run, rather than short-circuiting on a single grant.

### Benchmarks (informational — `make bench`)

| Benchmark | What it reports |
|---|---|
| `BenchmarkCheckCachedAuditOff` / `...AuditOn` | single cached `Check` ns/op, allocs/op, and a computed `p99-ns` |
| `BenchmarkCheckThroughputAuditOff` / `...AuditOn` | sustained parallel throughput as a `checks/sec` metric |
| `BenchmarkEnumerateBounded` | bounded `Enumerate`; asserts the result never exceeds `engine.DefaultEnumerateLimit` |

`make bench` runs them all with `-benchmem`. The **audit toggle** is the
benchmark axis: audit-off is the `s.audit == nil` path; audit-on wires a sampled
(1%), asynchronous `audit.Recorder` — the production shape where decision audit
is off the critical path (E4-S2).

### The hard NFR assertion (gated — `TestCheckNFR`)

Wall-clock assertions are environment-sensitive, so the **hard** gate is a test
that is **off by default**:

- it `t.Skip`s under `go test -short`, **and**
- it `t.Skip`s unless `APERTURE_BENCH_ASSERT=1` is set.

The default `make test` therefore never runs a wall-clock assertion and stays
fast and deterministic. Run the gate explicitly:

```sh
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Methodology inside the gate:

- **p99:** time 100 000 cached `Check`s on a warm engine, sort the per-op
  latencies, take the 99th percentile, assert `< 1 ms`.
- **throughput:** run 200 000 cached `Check`s, divide by wall time, assert
  `≥ 10 000 checks/sec` (a conservative single-goroutine floor; a real instance
  parallelises well above it — see the throughput benchmark).
- both are run with audit **on** and **off**.

## Committed results

Measured on an Apple M1 Max (`go test -benchtime=2s`); absolute numbers are
hardware-dependent, but the **headroom** and the **allocation profile** are the
durable signal.

| Metric (cached `Check`) | audit off | audit on |
|---|---|---|
| mean latency | ~66 µs/op | ~70 µs/op |
| allocations | 34 allocs/op | 34 allocs/op |
| p99 (gated `TestCheckNFR`, 100k samples) | ~0.275 ms | ~0.265 ms |
| throughput (single goroutine) | ~15 100 checks/sec | ~14 700 checks/sec |
| throughput (parallel benchmark) | ~20 000 checks/sec | ~30 000 checks/sec |

Both targets are met with comfortable headroom — p99 sits ~3.6× under the 1 ms
ceiling, and even the single-goroutine throughput clears the 10 k/s floor by
~1.5×, before parallelism. **Audit-on does not regress the target:** sampling is
a single `Sampler.Sample()` call on the un-kept path and the kept event is built
lazily and written asynchronously, so the decision never blocks on audit.

## The optimization pass

`go test -bench -benchmem` identified the dominant per-`Check` allocator: the
literal/scope coverer re-parsed each grant's object **pattern on every candidate
of every Check** (`identity.ParsePattern(g.Object)`), so a principal resolving
~73 grants paid ~73 fresh pattern parses — each a string split plus a new
segment slice.

The fix is a parsed-pattern cache in the engine
([`engine/patterncache.go`](../engine/patterncache.go)): a concurrency-safe
`sync.Map` keyed by the object string, shared by the literal and scope coverers.
A parsed `Pattern` is immutable and a pure function of its source, so a cache hit
returns the identical pattern a fresh parse would — **decision semantics are
unchanged** and the full existing test suite stays green.

Effect on the cached `Check`: **172 → 34 allocs/op** (~5× fewer), with the
re-parse churn and its GC pressure removed from the hot path.

Measure-first discipline: the literal decision hot path does not exercise the
compiled-rule cache (`rules`, E2-S3) or the provider metadata cache (`provider`,
E2-S2) — those already bound their own costs behind hash-keyed / TTL+LRU caches —
so no change was made there absent a benchmark showing a win.

## Regression guard

`TestCheckNFR` is the threshold guard: it fails if p99 ever crosses 1 ms or
throughput drops below 10 k/s. It is gated (see above) so it never flakes the
default build, but it is wired and runnable on demand and in a dedicated CI
job/cron where the runner is known to be unloaded.
