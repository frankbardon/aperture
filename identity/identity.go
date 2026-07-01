// Package identity defines Aperture's core primitive: a uniform, hierarchical,
// traversable object identity used for BOTH principals and protected objects.
// Every downstream decision resolves permissions against this type.
//
// An identity is an ordered path of typed segments. Each segment is a
// "type:id" pair, and segments are joined by '/':
//
//	account:acme/project:atlas/document:42
//
// The grammar is pure and allocation-light because it sits on the Check hot
// path (NFR p99 < 1ms): parsing splits and validates without regular
// expressions, and the resulting value carries its segments precompiled so
// matching never re-parses.
//
// This package is self-contained — it has no storage, engine, or transport
// dependencies. The only Aperture coupling is the root errors/ package, so
// every malformed value surfaces as an APERTURE_IDENTITY_INVALID coded error.
//
// Patterns (with '*' and '**' wildcards) and matching live in pattern.go;
// specificity ranking lives in specificity.go.
package identity

import (
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

const (
	// segSep separates segments within an identity path.
	segSep = '/'
	// kindSep separates a segment's type from its id.
	kindSep = ':'
	// wildcard is the single-segment / single-component wildcard sentinel.
	wildcard = "*"
	// doubleWildcard is the recursive (one-or-more) segment wildcard sentinel.
	doubleWildcard = "**"
)

// Segment is one "type:id" pair in an identity path. Both components are
// non-empty in a valid concrete identity.
type Segment struct {
	Type string
	ID   string
}

// String renders the segment in canonical "type:id" form.
func (s Segment) String() string {
	return s.Type + string(kindSep) + s.ID
}

// Identity is an ordered path of typed segments. The zero value is an empty,
// invalid identity; construct one via Parse or New. Segments are stored
// verbatim so that String round-trips a parsed value losslessly.
type Identity struct {
	segments []Segment
}

// Segments returns the identity's segments. The returned slice is a copy, so
// callers cannot mutate the identity's internal state.
func (id Identity) Segments() []Segment {
	out := make([]Segment, len(id.segments))
	copy(out, id.segments)
	return out
}

// Len reports the number of segments (the depth of the path).
func (id Identity) Len() int { return len(id.segments) }

// String renders the identity in canonical form: each segment as "type:id",
// joined by '/'. Parse(s).String() == s for every valid s.
func (id Identity) String() string {
	switch len(id.segments) {
	case 0:
		return ""
	case 1:
		return id.segments[0].String()
	}
	var b strings.Builder
	// Pre-size: each segment is type + ':' + id, plus a '/' between them.
	n := len(id.segments) - 1
	for _, s := range id.segments {
		n += len(s.Type) + 1 + len(s.ID)
	}
	b.Grow(n)
	for i, s := range id.segments {
		if i > 0 {
			b.WriteByte(segSep)
		}
		b.WriteString(s.Type)
		b.WriteByte(kindSep)
		b.WriteString(s.ID)
	}
	return b.String()
}

// Parse parses a canonical identity string into an Identity. It rejects an
// empty string, an empty segment (leading, trailing, or doubled '/'), a
// segment missing its ':' separator, an empty type or id, and any illegal
// character — each with an APERTURE_IDENTITY_INVALID coded error. Wildcards
// ('*', '**') are not valid in a concrete identity; use ParsePattern for those.
func Parse(s string) (Identity, error) {
	if s == "" {
		return Identity{}, aerr.New(aerr.APERTURE_IDENTITY_INVALID, "identity is empty")
	}
	parts := strings.Split(s, string(segSep))
	segs := make([]Segment, 0, len(parts))
	for i, part := range parts {
		seg, err := parseSegment(part, i)
		if err != nil {
			return Identity{}, err
		}
		segs = append(segs, seg)
	}
	return Identity{segments: segs}, nil
}

// MustParse is Parse that panics on error. Intended for package-level test
// fixtures and constants, never for caller-supplied input.
func MustParse(s string) Identity {
	id, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// New builds an Identity from already-split segments, validating each. It is
// the allocation-free-friendly constructor for callers that already hold the
// type/id pairs and want to skip string splitting.
func New(segments ...Segment) (Identity, error) {
	if len(segments) == 0 {
		return Identity{}, aerr.New(aerr.APERTURE_IDENTITY_INVALID, "identity has no segments")
	}
	segs := make([]Segment, len(segments))
	for i, s := range segments {
		if err := validateComponent(s.Type, i, "type"); err != nil {
			return Identity{}, err
		}
		if err := validateComponent(s.ID, i, "id"); err != nil {
			return Identity{}, err
		}
		segs[i] = s
	}
	return Identity{segments: segs}, nil
}

// parseSegment splits a "type:id" segment and validates both components. index
// is the zero-based position used in error context.
func parseSegment(part string, index int) (Segment, error) {
	if part == "" {
		return Segment{}, aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"identity contains an empty segment",
			map[string]any{"index": index})
	}
	colon := strings.IndexByte(part, kindSep)
	if colon < 0 {
		return Segment{}, aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"segment is missing its ':' type separator",
			map[string]any{"index": index, "segment": part})
	}
	typ, id := part[:colon], part[colon+1:]
	if err := validateComponent(typ, index, "type"); err != nil {
		return Segment{}, err
	}
	if err := validateComponent(id, index, "id"); err != nil {
		return Segment{}, err
	}
	return Segment{Type: typ, ID: id}, nil
}

// validateComponent enforces the literal component grammar: non-empty, no
// structural delimiters, no wildcard sentinel, and only allowed characters.
func validateComponent(v string, index int, which string) error {
	if v == "" {
		return aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
			"segment "+which+" is empty",
			map[string]any{"index": index})
	}
	for _, r := range v {
		if !isAllowedRune(r) {
			return aerr.WithContext(aerr.APERTURE_IDENTITY_INVALID,
				"segment "+which+" contains an illegal character",
				map[string]any{"index": index, which: v, "char": string(r)})
		}
	}
	return nil
}

// isAllowedRune reports whether r is legal inside a literal type or id
// component. The allowed set is ASCII letters, digits, and -._~@+ — enough for
// slugs, UUIDs, and qualified ids while excluding the structural delimiters
// '/' and ':', whitespace, and the '*' wildcard sentinel.
func isAllowedRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '-', '.', '_', '~', '@', '+':
		return true
	}
	return false
}
