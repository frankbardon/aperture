package identity

import (
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

// patSeg is one precompiled pattern segment. Wildcards are resolved at parse
// time so Match never re-parses on the hot path. For kindLiteral, typeWild and
// idWild record whether the respective component is "*".
type patSeg struct {
	kind     segKind
	typ      string
	id       string
	typeWild bool
	idWild   bool
}

// Pattern is a precompiled identity matcher. It is a path of segments where any
// segment may be a literal "type:id" (with "*" allowed for either component), a
// bare "*" (matches exactly one segment), or "**" (matches one-or-more segments
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
// in place of a literal type or id component (e.g. "project:*"). It rejects the
// same structural faults as Parse — empty input, empty segment, a literal
// segment missing its ':', empty/illegal components — with an
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
	typeWild, err := parseComponent(typ, index, "type")
	if err != nil {
		return patSeg{}, err
	}
	idWild, err := parseComponent(id, index, "id")
	if err != nil {
		return patSeg{}, err
	}
	return patSeg{kind: kindLiteral, typ: typ, id: id, typeWild: typeWild, idWild: idWild}, nil
}

// parseComponent validates a literal pattern component, allowing the lone "*"
// wildcard. It reports whether the component is that wildcard.
func parseComponent(v string, index int, which string) (bool, error) {
	if v == wildcard {
		return true, nil
	}
	if err := validateComponent(v, index, which); err != nil {
		return false, err
	}
	return false, nil
}

// matches reports whether this pattern segment matches a concrete identity
// segment. Only meaningful for kindLiteral and kindSingle.
func (ps patSeg) matchesSegment(s Segment) bool {
	switch ps.kind {
	case kindSingle:
		return true
	case kindLiteral:
		if !ps.typeWild && ps.typ != s.Type {
			return false
		}
		if !ps.idWild && ps.id != s.ID {
			return false
		}
		return true
	default:
		return false
	}
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
