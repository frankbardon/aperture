package engine

import "context"

// BatchResult is one item's outcome in a bulk operation: either a Result or an
// Err, aligned by index with the input query that produced it. It is the shape
// every Batch op returns so a single bad query never fails the whole batch — the
// failing item carries its error while its siblings carry their results.
//
// Exactly one of Result / Err is meaningful per item: when Err is non-nil the
// Result is the zero value, and the caller reads Err; otherwise Result holds the
// item's answer. The generic parameter is the per-op result type (Decision for
// CheckBatch, []string for EnumerateBatch, Trace for ExplainBatch).
type BatchResult[T any] struct {
	// Result is the item's answer when Err is nil; the zero value otherwise.
	Result T
	// Err is the item's coded error when it failed, or nil on success.
	Err error
}

// CheckBatch resolves many Check requests in one call, returning results aligned
// with reqs (result[i] is the verdict for reqs[i]). A request that errors yields
// an item with Err set and a zero Decision; the rest are unaffected, so a single
// malformed request never fails the batch. A nil reqs yields a nil result.
func (e *Engine) CheckBatch(ctx context.Context, reqs []Request) []BatchResult[Decision] {
	if reqs == nil {
		return nil
	}
	out := make([]BatchResult[Decision], len(reqs))
	for i, req := range reqs {
		dec, err := e.Check(ctx, req)
		out[i] = BatchResult[Decision]{Result: dec, Err: err}
	}
	return out
}

// EnumerateBatch resolves many Enumerate requests in one call, aligned with reqs
// (result[i] is the id list for reqs[i]). A request that errors yields an item
// with Err set and a nil list; the rest are unaffected.
func (e *Engine) EnumerateBatch(ctx context.Context, reqs []EnumerateRequest) []BatchResult[[]string] {
	if reqs == nil {
		return nil
	}
	out := make([]BatchResult[[]string], len(reqs))
	for i, req := range reqs {
		ids, err := e.Enumerate(ctx, req)
		out[i] = BatchResult[[]string]{Result: ids, Err: err}
	}
	return out
}

// ExplainBatch resolves many Explain requests in one call, aligned with reqs
// (result[i] is the trace for reqs[i]). A request that errors yields an item with
// Err set and a zero Trace; the rest are unaffected.
func (e *Engine) ExplainBatch(ctx context.Context, reqs []Request) []BatchResult[Trace] {
	if reqs == nil {
		return nil
	}
	out := make([]BatchResult[Trace], len(reqs))
	for i, req := range reqs {
		tr, err := e.Explain(ctx, req)
		out[i] = BatchResult[Trace]{Result: tr, Err: err}
	}
	return out
}
