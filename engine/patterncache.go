package engine

import (
	"sync"

	"github.com/frankbardon/aperture/identity"
)

// patternCache memoises the parse of a grant's object pattern. The literal and
// scope coverers parse Grant.Object on EVERY candidate of EVERY Check; for a
// principal resolving dozens of grants that is the dominant per-Check allocator
// (each ParsePattern splits the string and builds a fresh segment slice). The
// set of distinct grant object strings is small and bounded by the model, while
// a parsed Pattern is immutable and a pure function of its source — so caching
// the parse is allocation-free on the steady-state hot path and changes no
// decision semantics: a cache hit yields the identical Pattern a fresh parse
// would, and Specificity over it is unchanged.
//
// It is safe for concurrent use (sync.Map gives lock-free reads), and shared by
// an engine and any WithStore clone of it — a parsed pattern is read-only, so
// sharing is sound. Parse errors are NOT cached: a corrupt stored pattern is the
// rare, already-validated-on-write path, and surfacing it every time keeps the
// error contract intact without polluting the cache.
type patternCache struct {
	m sync.Map // string -> identity.Pattern
}

func newPatternCache() *patternCache { return &patternCache{} }

// pattern returns the parsed form of s, parsing and caching it on first sight.
// The returned Pattern is identical to identity.ParsePattern(s) and must be
// treated as read-only.
func (c *patternCache) pattern(s string) (identity.Pattern, error) {
	if v, ok := c.m.Load(s); ok {
		return v.(identity.Pattern), nil
	}
	p, err := identity.ParsePattern(s)
	if err != nil {
		return identity.Pattern{}, err
	}
	c.m.Store(s, p)
	return p, nil
}
