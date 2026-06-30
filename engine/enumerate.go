package engine

import (
	"context"
	"sort"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
)

// DefaultEnumerateLimit bounds Enumerate's result when the caller imposes no
// positive limit, so an enumeration can never materialise an unbounded set even
// if a provider lister would. It matches the scope/provider enumeration bound.
const DefaultEnumerateLimit = 1000

// EnumerateRequest is the input to Enumerate: the principal asking, the action,
// and the object PATTERN that bounds the search, scoped to an account. Pattern
// is an identity pattern (e.g. "account:acme/**" or "account:acme/document:*")
// that both bounds the candidate set and is intersected with each grant's own
// scope. Limit caps the number of returned ids; <= 0 means DefaultEnumerateLimit.
type EnumerateRequest struct {
	// Account is the active account the enumeration is scoped to. Mandatory.
	Account string
	// Principal is the id of the principal whose access is enumerated. Mandatory.
	Principal string
	// Action is the verb being enumerated (e.g. "read"). Mandatory.
	Action string
	// Pattern is the identity pattern bounding the search. Mandatory.
	Pattern string
	// Limit caps the number of returned object ids. <= 0 means the default bound.
	Limit int
}

// Enumerate returns the object ids under Pattern that Principal may take Action
// on, in the active account — the inverse of Check (FR-10). The result respects
// deny-overrides and specificity exactly as Check does: every returned id is one
// Check would allow, so a denied object is NEVER returned.
//
// Algorithm: the candidate set is the union of every ALLOW grant's covered
// objects (a scope resolver's bounded Members for implicit/inclusive/exclusive,
// or the grant's own concrete identity for literal), intersected with Pattern.
// Each candidate is then run through the same deny-overrides/specificity
// decision, so a candidate carved out by a more-specific or equal-specificity
// deny is dropped. Because any allowable object must be covered by at least one
// allow grant, gathering candidates from allow grants alone is complete; deny
// grants only ever subtract.
//
// Enumerate is the most cache-sensitive op, so it is deliberately bounded: each
// resolver's Members is itself limited, and the overall result is capped by
// Limit (default DefaultEnumerateLimit). Object order is deterministic (sorted
// by canonical id). An operational failure (storage, an unresolvable strategy,
// or an unconfigured lister an implicit/exclusive grant needs) is returned as an
// APERTURE_* coded error and the caller treats it as a non-result.
func (e *Engine) Enumerate(ctx context.Context, req EnumerateRequest) ([]string, error) {
	if err := validateEnumerateRequest(req); err != nil {
		return nil, err
	}
	query, err := identity.ParsePattern(req.Pattern)
	if err != nil {
		return nil, err
	}

	member, err := e.requireMembership(ctx, req.Account, req.Principal)
	if err != nil {
		return nil, err
	}
	if !member {
		// Fail-closed: a non-member may act on nothing in this account.
		return []string{}, nil
	}

	subjects, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return nil, err
	}
	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for subjects", err)
	}

	limit := boundEnumerateLimit(req.Limit)
	permCache := make(map[string]*model.Permission, len(grants))

	// The decision context reused per candidate. Object is filled per candidate.
	decReq := Request{Account: req.Account, Principal: req.Principal, Action: req.Action}

	// Gather candidate ids from the ALLOW grants whose action matches.
	seen := make(map[string]struct{})
	candidates := make([]identity.Identity, 0)
	for _, g := range grants {
		if g.Effect != model.EffectAllow {
			continue
		}
		ok, err := e.actionMatches(ctx, g, req.Action, permCache)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		perm := permCache[g.PermissionID]
		members, err := e.coverer.members(ctx, decReq, g, perm, query)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			s := m.String()
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			candidates = append(candidates, m)
		}
	}

	// Deterministic output: decide candidates in canonical-id order.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].String() < candidates[j].String()
	})

	out := make([]string, 0, len(candidates))
	for _, obj := range candidates {
		decReq.Object = obj.String()
		dec, err := e.evaluate(ctx, decReq, obj, grants, permCache)
		if err != nil {
			return nil, err
		}
		if !dec.Allow {
			continue
		}
		out = append(out, obj.String())
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// boundEnumerateLimit normalises a caller limit to a positive bound.
func boundEnumerateLimit(limit int) int {
	if limit <= 0 || limit > DefaultEnumerateLimit {
		return DefaultEnumerateLimit
	}
	return limit
}

// validateEnumerateRequest rejects a request missing any required field before
// any storage work happens.
func validateEnumerateRequest(req EnumerateRequest) error {
	switch {
	case req.Account == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate account is empty")
	case req.Principal == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate principal is empty")
	case req.Action == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate action is empty")
	case req.Pattern == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: enumerate pattern is empty")
	}
	return nil
}
