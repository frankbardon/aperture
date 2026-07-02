package identity

// Contains reports whether the outer pattern's language is a superset of the
// inner pattern's: every concrete identity inner matches, outer matches too.
// Equivalently, inner is "equal-or-more-specific / contained within" outer.
//
// It is the containment test the delegation subset rule (E3-S2) consumes: a
// bestowed grant whose object pattern is Contains-ed by an effective grant's
// object pattern stays within that grant's authority.
//
// The check is SOUND and conservative: it never reports containment that does
// not hold, but — because it is a structural test rather than a full language
// inclusion decision over the one-or-more "**" wildcard — it may return false
// for some genuinely-contained pairs. Delegation fails closed, so a false
// negative rejects a bestow that could in principle have been allowed; it never
// permits an escalation. Reflexive (a pattern contains itself) and transitive in
// practice for the patterns Aperture grants use.
func Contains(outer, inner Pattern) bool {
	return containsSegs(outer.segments, inner.segments)
}

// Contains is the method form of the package-level Contains, with the receiver
// as the outer (broader) pattern.
func (p Pattern) Contains(inner Pattern) bool { return Contains(p, inner) }

// containsSegs decides whether the outer segment sequence subsumes the inner
// one. Every pattern segment matches at least one identity segment (a literal
// and a single "*" match exactly one; "**" matches one-or-more), so an empty
// outer can only subsume an empty inner, and a non-empty outer never subsumes an
// empty inner.
func containsSegs(outer, inner []patSeg) bool {
	for {
		if len(outer) == 0 {
			return len(inner) == 0
		}
		o0 := outer[0]
		if o0.kind == kindDouble {
			rest := outer[1:]
			// A trailing "**" matches one-or-more of anything, so it subsumes any
			// non-empty inner remainder (and nothing else).
			if len(rest) == 0 {
				return len(inner) >= 1
			}
			// "**" absorbs a non-empty prefix of inner (each inner segment expands to
			// at least one concrete segment, so any prefix is a valid one-or-more
			// span), then the rest of outer must subsume the remaining suffix. Try
			// every split point; this is the standard glob-subsumption backtrack.
			for k := 1; k <= len(inner); k++ {
				if containsSegs(rest, inner[k:]) {
					return true
				}
			}
			return false
		}
		// o0 is a single-width segment (literal or single "*").
		if len(inner) == 0 {
			return false
		}
		i0 := inner[0]
		if i0.kind == kindDouble {
			// Inner "**" expands to arbitrarily many segments; a single-width outer
			// segment can match only one, so it cannot subsume it. Reject (sound).
			return false
		}
		if !segSubsumes(o0, i0) {
			return false
		}
		outer, inner = outer[1:], inner[1:]
	}
}

// segSubsumes reports whether single-width outer segment o matches every
// identity segment inner segment i can match. Neither argument is "**"
// (handled in containsSegs).
func segSubsumes(o, i patSeg) bool {
	if o.kind == kindSingle {
		// "*" matches any one segment, so it subsumes any single-width inner.
		return true
	}
	// o is a literal (possibly with wildcard components).
	if i.kind == kindSingle {
		// A bare "*" matches any type and id; only a fully-wild literal covers that.
		return o.typeWild && o.idWild
	}
	// Both literals: o subsumes i component-wise.
	return compSubsumes(o.typeWild, o.typeSet, o.typ, i.typeWild, i.typeSet, i.typ) &&
		compSubsumes(o.idWild, o.idSet, o.id, i.idWild, i.idSet, i.id)
}

// compSubsumes reports whether an outer component's value language is a superset
// of an inner component's. A wild outer covers anything; a wild inner is broader
// than any fixed outer and is never subsumed. Otherwise both sides reduce to a
// finite value set (a plain literal is a singleton), and outer subsumes inner iff
// every inner value is an outer value — so "{1,5,23}" subsumes "5" and "{1,5}",
// but not "3" or "{5,9}".
func compSubsumes(oWild bool, oSet []string, oLit string, iWild bool, iSet []string, iLit string) bool {
	if oWild {
		return true
	}
	if iWild {
		return false
	}
	outer := make(map[string]struct{}, len(oSet)+1)
	for _, v := range compValues(oSet, oLit) {
		outer[v] = struct{}{}
	}
	for _, v := range compValues(iSet, iLit) {
		if _, ok := outer[v]; !ok {
			return false
		}
	}
	return true
}

// compValues returns a component's allowed values: the set members when it is a
// "{a,b,c}" set, or the single literal otherwise. Not valid for a wildcard
// component (callers check wild first).
func compValues(set []string, lit string) []string {
	if set != nil {
		return set
	}
	return []string{lit}
}
