package identity

import (
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// segKind classifies a pattern segment for matching.
type segKind uint8

const (
	// kindLiteral is a fixed "type:id" segment; either component may itself be
	// the single-component wildcard "*".
	kindLiteral segKind = iota
	// kindSingle is a bare "*" segment that matches exactly one segment of any
	// type and id.
	kindSingle
	// kindDouble is a "**" segment that matches one-or-more segments
	// recursively.
	kindDouble
)

// patSeg is one precompiled pattern segment. Wildcards and id/type sets are
// resolved at parse time so Match never re-parses on the hot path. For
// kindLiteral, typeWild and idWild record whether the respective component is
// "*", and typeSet / idSet hold the members when the component is an explicit
// "{a,b,c}" set (nil when it is a plain literal or a wildcard).
type patSeg struct {
	kind     segKind
	typ      string
	id       string
	typeWild bool
	idWild   bool
	typeSet  []string
	idSet    []string
}

// Pattern is a precompiled identity matcher. It is a path of segments where any
// segment may be a literal "type:id" (with "*" allowed for either component, or
// an explicit "{a,b,c}" set enumerating allowed values for a component), a bare
// "*" (matches exactly one segment), or "**" (matches one-or-more segments
// recursively). Build one with ParsePattern; match with Matches.
type Pattern struct {
	segments []patSeg
	// raw caches the canonical source so String round-trips losslessly.
	raw string
}

// Len reports the number of pattern segments. A "**" counts as one segment even
// though it matches a variable number of identity segments.
func (p Pattern) Len() int { return len(p.segments) }

// String returns the pattern's canonical source string.
func (p Pattern) String() string { return p.raw }

// ParsePattern parses a pattern string. Beyond the concrete-identity grammar it
// accepts the wildcard sentinels: a bare "*" segment, a "**" segment, and "*"
// in place of a literal type or id component (e.g. "project:*"). A component may
// also be an explicit set "{a,b,c}" that matches any of the listed values, so a
// single pattern (and thus a single grant) can scope to several ids without a
// wildcard — e.g. "brand:{1,5,23}". It rejects the same structural faults as
// Parse — empty input, empty segment, a literal segment missing its ':',
// empty/illegal components — plus an empty set "{}", with an
// APERTURE_IDENTITY_INVALID coded error.
func ParsePattern(s string) (Pattern, error) {
	if s == "" {
		return Pattern{}, aerr.New(aerr.APERTURE_IDENTITY_INVALID, "pattern is empty")
	}
	parts := strings.Split(s, string(segSep))
	segs := make([]patSeg, 0, len(parts))
	for i, part := range parts {
		seg, err := parsePatternSegment(part, i)
		if err != nil {
			return Pattern{}, err
		}
		segs = append(segs, seg)
	}
	return Pattern{segments: segs, raw: s}, nil
}

// MustParsePattern is ParsePattern that panics on error. For test fixtures and
// constants only.
func MustParsePattern(s string) Pattern {
	p, err := ParsePattern(s)
	if err != nil {
		panic(err)
	}
	return p
}

func parsePatternSegment(part string, index int) (patSeg, error) {
	switch part {
	case "":
		return patSeg{}, aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"pattern contains an empty segment",
			map[string]any{"index": index})
	case wildcard:
		return patSeg{kind: kindSingle}, nil
	case doubleWildcard:
		return patSeg{kind: kindDouble}, nil
	}
	colon := strings.IndexByte(part, kindSep)
	if colon < 0 {
		return patSeg{}, aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"pattern segment is missing its ':' type separator",
			map[string]any{"index": index, "segment": part})
	}
	typ, id := part[:colon], part[colon+1:]
	typeWild, typeSet, err := parseComponent(typ, index, "type")
	if err != nil {
		return patSeg{}, err
	}
	idWild, idSet, err := parseComponent(id, index, "id")
	if err != nil {
		return patSeg{}, err
	}
	return patSeg{
		kind: kindLiteral, typ: typ, id: id,
		typeWild: typeWild, idWild: idWild, typeSet: typeSet, idSet: idSet,
	}, nil
}

// parseComponent validates a literal pattern component. It accepts the lone "*"
// wildcard (reported via wild) and an explicit "{a,b,c}" set (returned via set,
// nil for a plain literal). Exactly one of wild / set / plain-literal holds.
func parseComponent(v string, index int, which string) (wild bool, set []string, err error) {
	if v == wildcard {
		return true, nil, nil
	}
	if len(v) >= 2 && v[0] == '{' && v[len(v)-1] == '}' {
		set, err := parseSet(v, index, which)
		return false, set, err
	}
	if err := validateComponent(v, index, which); err != nil {
		return false, nil, err
	}
	return false, nil, nil
}

// parseSet parses a "{a,b,c}" component into its members. Each member is a plain
// literal component (no wildcard, no nested set), validated the same way a
// literal component is, so illegal characters and the '*' sentinel are rejected.
// The set must be non-empty; duplicate members are dropped, preserving order.
func parseSet(v string, index int, which string) ([]string, error) {
	inner := v[1 : len(v)-1]
	if inner == "" {
		return nil, aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"pattern component set is empty", map[string]any{"index": index, which: v})
	}
	members := strings.Split(inner, ",")
	seen := make(map[string]struct{}, len(members))
	out := make([]string, 0, len(members))
	for _, m := range members {
		if err := validateComponent(m, index, which); err != nil {
			return nil, err
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

// matches reports whether this pattern segment matches a concrete identity
// segment. Only meaningful for kindLiteral and kindSingle.
func (ps patSeg) matchesSegment(s Segment) bool {
	switch ps.kind {
	case kindSingle:
		return true
	case kindLiteral:
		return compMatches(ps.typeWild, ps.typeSet, ps.typ, s.Type) &&
			compMatches(ps.idWild, ps.idSet, ps.id, s.ID)
	default:
		return false
	}
}

// compMatches reports whether a pattern component matches the concrete value v.
// A wildcard component matches anything; a set matches any listed member; a plain
// literal matches its exact value.
func compMatches(wild bool, set []string, lit, v string) bool {
	if wild {
		return true
	}
	if set != nil {
		return slices.Contains(set, v)
	}
	return lit == v
}

// Matches reports whether the pattern matches the given concrete identity.
//
// Semantics:
//   - a literal segment matches one identity segment with the same type and id
//     (a "*" component matches any value for that component);
//   - "*" matches exactly one identity segment (any type, any id);
//   - "**" matches one-or-more identity segments recursively.
//
// Matching is anchored at both ends: every identity segment must be consumed.
// The implementation slices into the precompiled segments and never allocates.
func (p Pattern) Matches(id Identity) bool {
	return matchSegments(p.segments, id.segments)
}

// Match is the package-level form of Pattern.Matches.
func Match(p Pattern, id Identity) bool { return p.Matches(id) }

// Expand returns the finite set of concrete identities a pattern matches when it
// is fully enumerable — every segment is a literal "type:id" whose components are
// each a plain literal or an explicit "{a,b,c}" set, with NO "*" or "**" wildcard
// anywhere. It reports ok=false for any pattern containing a wildcard, whose
// language is unbounded (or provider-dependent) and cannot be listed here.
//
// The result is the cross-product of each segment's component members, in a
// deterministic order (segment order, then the member order each set was written
// in). It lets a set-scoped grant like "brand:{1,5,23}" enumerate to its concrete
// objects without a provider.
func (p Pattern) Expand() ([]Identity, bool) {
	for _, s := range p.segments {
		if s.kind != kindLiteral || s.typeWild || s.idWild {
			return nil, false
		}
	}
	combos := []Identity{{}}
	for _, s := range p.segments {
		types := compValues(s.typeSet, s.typ)
		ids := compValues(s.idSet, s.id)
		next := make([]Identity, 0, len(combos)*len(types)*len(ids))
		for _, base := range combos {
			for _, t := range types {
				for _, idv := range ids {
					segs := make([]Segment, 0, len(base.segments)+1)
					segs = append(segs, base.segments...)
					segs = append(segs, Segment{Type: t, ID: idv})
					next = append(next, Identity{segments: segs})
				}
			}
		}
		combos = next
	}
	return combos, true
}

// matchSegments is the recursive glob matcher. pat and ids are slices into the
// precompiled pattern and the identity; slicing does not allocate, and the
// recursion depth is bounded by the (small) path length.
func matchSegments(pat []patSeg, ids []Segment) bool {
	for len(pat) > 0 {
		p := pat[0]
		if p.kind == kindDouble {
			// "**" must consume at least one identity segment, then the rest of
			// the pattern must match some suffix. Try the shortest consumption
			// first (greedy-from-the-left via backtracking).
			rest := pat[1:]
			if len(rest) == 0 {
				// Trailing "**": match iff there is at least one segment left.
				return len(ids) >= 1
			}
			for i := 1; i <= len(ids); i++ {
				if matchSegments(rest, ids[i:]) {
					return true
				}
			}
			return false
		}
		// Single-segment pattern element: needs exactly one identity segment.
		if len(ids) == 0 || !p.matchesSegment(ids[0]) {
			return false
		}
		pat, ids = pat[1:], ids[1:]
	}
	// Pattern exhausted: match iff the identity is fully consumed.
	return len(ids) == 0
}
