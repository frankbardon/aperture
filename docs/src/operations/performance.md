# Performance & the NFR

Aperture's decision hot path carries a hard success metric (FR-31):

> **p99 cached `Check` < 1 ms** *and* **≥ 10 000 checks/sec/instance.**

This chapter summarizes how that budget is measured and asserted. The full
methodology, the optimization pass, and the committed hardware numbers live in
the repository at **`docs/benchmarks.md`** (repo root, alongside the book — it is
not part of the mdBook source tree, so read it directly in the repo or on GitHub).

## The benchmark suite (`make bench`)

The suite lives in the `bench/` package. It seeds a **sizable** authorization
model — not a three-grant toy — and drives the full decision facade
(`service.Service.Check`), so the numbers reflect what a real surface pays.

The fixture seeds 8 accounts, 60 roles, 60 groups, and 480 principals, with
overlapping wildcard allows, more-specific deny carve-outs, and 60 concrete
document grants per account. The representative cached `Check` resolves a
six-subject subject set to roughly 73 applicable grants at differing
specificities, so deny-overrides and the specificity tiebreak genuinely run
rather than short-circuiting.

```bash
make bench     # go test -run '^$' -bench=. -benchmem ./bench/
```

`make bench` is **informational** — it prints, but never asserts:

| Benchmark | Reports |
|---|---|
| `BenchmarkCheckCachedAuditOff` / `…AuditOn` | single cached `Check` ns/op, allocs/op, and a computed `p99-ns` |
| `BenchmarkCheckThroughputAuditOff` / `…AuditOn` | sustained parallel throughput as `checks/sec` |
| `BenchmarkEnumerateBounded` | bounded `Enumerate`; asserts the result never exceeds `engine.DefaultEnumerateLimit` |

The **audit toggle** is the axis: audit-off is the `s.audit == nil` path;
audit-on wires a sampled (1 %), asynchronous `audit.Recorder` — the production
shape where decision audit sits off the critical path.

## The hard NFR gate (`TestCheckNFR`)

Wall-clock assertions are environment-sensitive, so the **hard** gate is a test
that is **off by default** and never runs in the routine `make test`. It
self-skips under `go test -short` **and** skips unless `APERTURE_BENCH_ASSERT=1`
is set. Run it explicitly on a known-unloaded machine:

```bash
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Inside the gate:

- **p99** — time 100 000 cached `Check`s on a warm engine, sort the per-op
  latencies, take the 99th percentile, assert `< 1 ms`.
- **throughput** — run 200 000 cached `Check`s, divide by wall time, assert
  `≥ 10 000 checks/sec` (a conservative single-goroutine floor; a real instance
  parallelises well above it).
- both are run with audit **on** and **off**.

`TestCheckNFR` is the regression guard: it fails if p99 ever crosses 1 ms or
throughput drops below the floor. Because it is gated it never flakes the default
build, but it is wired and runnable on demand and in a dedicated CI job/cron
where the runner is known to be idle.

## Committed numbers

Measured on an Apple M1 Max (`go test -benchtime=2s`). Absolute numbers are
hardware-dependent; the durable signal is the **headroom** and the **allocation
profile**.

| Metric (cached `Check`) | audit off | audit on |
|---|---|---|
| mean latency | ~66 µs/op | ~70 µs/op |
| allocations | 34 allocs/op | 34 allocs/op |
| p99 (gated, 100k samples) | ~0.275 ms | ~0.265 ms |
| throughput (single goroutine) | ~15 100 checks/sec | ~14 700 checks/sec |
| throughput (parallel benchmark) | ~20 000 checks/sec | ~30 000 checks/sec |

Both targets are met with comfortable headroom — p99 sits ~3.6× under the 1 ms
ceiling, and even the single-goroutine throughput clears the 10 k/s floor by
~1.5× before any parallelism. **Audit-on does not regress the target:** sampling
is a single call on the un-kept path and the kept event is built lazily and
written asynchronously, so the decision never blocks on audit.

## Where the headroom came from

The optimization pass (recorded in `docs/benchmarks.md`) found the dominant
per-`Check` allocator: the coverer re-parsed each grant's object pattern on every
candidate of every `Check`, so a principal resolving ~73 grants paid ~73 fresh
pattern parses. A concurrency-safe parsed-pattern cache in the engine
(`engine/patterncache.go`) removed the churn — a parsed pattern is immutable and a
pure function of its source, so a cache hit returns exactly what a fresh parse
would and **decision semantics are unchanged**. Effect: **172 → 34 allocs/op**
(~5× fewer), with the re-parse GC pressure gone from the hot path.

The change was measure-first: caches that already bound their own cost (the
compiled-rule cache, the provider metadata cache) were left untouched absent a
benchmark showing a win.

## Related

- Repository file `docs/benchmarks.md` — the authoritative methodology, the
  optimization write-up, and the latest committed numbers.
- [Deployment](deployment.md) — running the instance whose throughput these
  numbers describe.
- [Rules engine](../concepts/rules.md), [Providers](../concepts/providers.md) —
  the caches referenced by the measure-first note.
