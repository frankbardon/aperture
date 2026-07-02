# Batch operations

Each of the three single decision operations has a bulk form that resolves many
requests in one call: `CheckBatch`, `EnumerateBatch`, and `ExplainBatch`. They
exist so a surface — the RPC bulk RPCs, the MCP tools, the what-if simulator — can
answer a list of questions in one round trip while keeping each answer isolated:
**one bad query never fails the whole batch**.

The batch forms live on both layers with the same shape. This page shows the
`engine` methods; the [facade](service-facade.md) exposes the same three over its
surface-neutral `Query` / `EnumerateQuery` types.

## BatchResult[T]

Every batch op returns a slice of `BatchResult[T]`, aligned by index with the
input: `result[i]` is the outcome of `reqs[i]`.

```go
type BatchResult[T any] struct {
	Result T     // the item's answer when Err is nil; the zero value otherwise
	Err    error // the item's coded error when it failed, or nil on success
}
```

Exactly one of the two fields is meaningful per item. When `Err` is non-nil the
`Result` is the zero value and the caller reads `Err`; otherwise `Result` holds
the answer. The generic parameter is the per-op result type: `Decision` for
`CheckBatch`, `[]string` for `EnumerateBatch`, and `Trace` for `ExplainBatch`.

Iterate a batch by checking each item's `Err` before reading its `Result`:

```go
for i, item := range results {
	if item.Err != nil {
		log.Printf("query %d failed: %v", i, item.Err)
		continue
	}
	use(item.Result)
}
```

## CheckBatch

```go
func (e *Engine) CheckBatch(ctx context.Context, reqs []Request) []BatchResult[Decision]
```

Resolves many `Check` requests, returning `[]BatchResult[Decision]` aligned with
`reqs`. A request that errors yields an item with `Err` set and a zero `Decision`;
its siblings are unaffected. A `nil` `reqs` yields a `nil` result.

```go
results := eng.CheckBatch(ctx, []engine.Request{
	{Account: "acme", Principal: "alice", Action: "read", Object: "account:acme/project:atlas/document:42"},
	{Account: "acme", Principal: "alice", Action: "write", Object: "account:acme/project:atlas/document:42"},
})
for i, item := range results {
	if item.Err != nil {
		continue // a malformed request — item.Result is the zero Decision
	}
	fmt.Printf("query %d: allow=%v\n", i, item.Result.Allow)
}
```

## EnumerateBatch

```go
func (e *Engine) EnumerateBatch(ctx context.Context, reqs []EnumerateRequest) []BatchResult[[]string]
```

Resolves many `Enumerate` requests, aligned with `reqs` — `result[i]` is the id
list for `reqs[i]`. A request that errors yields an item with `Err` set and a
`nil` list; the rest are unaffected.

```go
results := eng.EnumerateBatch(ctx, []engine.EnumerateRequest{
	{Account: "acme", Principal: "alice", Action: "read", Pattern: "account:acme/project:atlas/**"},
	{Account: "acme", Principal: "alice", Action: "read", Pattern: "account:acme/project:nimbus/**"},
})
```

## ExplainBatch

```go
func (e *Engine) ExplainBatch(ctx context.Context, reqs []Request) []BatchResult[Trace]
```

Resolves many `Explain` requests, aligned with `reqs`. A request that errors
yields an item with `Err` set and a zero `Trace`; the rest are unaffected.

## Facade batch forms

The [service facade](service-facade.md) exposes the same three over its
surface-neutral query types, so a surface never touches the engine's `Request`
type:

```go
func (s *Service) CheckBatch(ctx context.Context, qs []Query) []engine.BatchResult[Result]
func (s *Service) EnumerateBatch(ctx context.Context, qs []EnumerateQuery) []engine.BatchResult[[]string]
func (s *Service) ExplainBatch(ctx context.Context, qs []Query) []engine.BatchResult[engine.Trace]
```

Note where the fail-closed contract lands. `Service.CheckBatch` renders each item
exactly as `Service.Check`: an *operational* failure folds into a deny `Result`
(with `Err` nil), while an *input-validation* failure sets the item's `Err`. So a
`CheckBatch` item's `Err` is only ever a caller-bug error, never a storage fault
— the storage fault already became a fail-closed deny. `EnumerateBatch` and
`ExplainBatch` carry engine errors verbatim in each item's `Err`. See
[The service facade](service-facade.md) for the full rendering rules.

Like the engine forms, every facade batch method returns `nil` for a `nil` input
slice.

## Related

- [Decision API](decision-api.md) — the single operations these batch.
- [The service facade](service-facade.md) — the fail-closed rendering the facade
  batch forms inherit.
