package identity

// Specificity scoring weights. Paths are short (segment and component counts
// stay well under these magnitudes), so a single weighted int yields a stable,
// deterministic total order without overflow:
//
//   - literal components dominate (a fixed type or id is the strongest signal);
//   - a longer pinned path breaks ties among equal literal counts;
//   - each "**" applies a penalty because it matches a variable-length span and
//     is therefore the least specific construct.
const (
	literalWeight    = 10000
	segmentWeight    = 100
	doubleStarWeight = 1000
)

// Specificity returns a deterministic score for a pattern: the more specific a
// pattern is, the higher its score. It is derived from the number of literal
// components (a "type:id" segment contributes up to two), the total segment
// count, and a penalty for each recursive "**" wildcard.
//
// For two patterns that both match a given identity, the one a human would call
// "more specific" — more fixed components, fewer / shallower wildcards — scores
// strictly higher. This is the tiebreak E1-S4's deny-overrides resolution
// consumes.
func Specificity(p Pattern) int {
	literals := 0
	doubleStars := 0
	for _, s := range p.segments {
		switch s.kind {
		case kindLiteral:
			if !s.typeWild {
				literals++
			}
			if !s.idWild {
				literals++
			}
		case kindSingle:
			// Both components wildcard: contributes only its pinned position.
		case kindDouble:
			doubleStars++
		}
	}
	return literals*literalWeight +
		len(p.segments)*segmentWeight -
		doubleStars*doubleStarWeight
}

// Compare orders two patterns by specificity. It returns +1 when a is more
// specific than b, -1 when a is less specific, and 0 when they rank equally.
// The ordering is total and deterministic, so it is safe to use as a sort key.
func Compare(a, b Pattern) int {
	sa, sb := Specificity(a), Specificity(b)
	switch {
	case sa > sb:
		return 1
	case sa < sb:
		return -1
	default:
		return 0
	}
}

// MoreSpecific reports whether a ranks strictly above b.
func MoreSpecific(a, b Pattern) bool { return Compare(a, b) > 0 }
