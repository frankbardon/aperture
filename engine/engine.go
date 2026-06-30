// Package engine is Aperture's Policy Decision Point (PDP). It answers the
// single authorization question — "may this principal perform this action on
// this object, in this account?" — by resolving the grants that bind to the
// principal's effective subject set with deny-overrides plus a specificity
// tiebreak.
//
// Resolution rule (FR-6, FR-10, FR-11):
//
//   - A grant is a candidate when its permission's action matches the requested
//     action AND its object pattern covers the requested object.
//   - The decision is the effect of the most specific candidate. Specificity is
//     scored by the identity package (more fixed components, fewer/shallower
//     wildcards rank higher).
//   - A strictly more-specific allow carves an exception out of a broader deny
//     (e.g. deny account:acme/** + allow account:acme/project:atlas/** ⇒ atlas
//     is allowed, the rest of acme denied).
//   - At equal top specificity deny wins. This is both the deny-overrides rule
//     and the deterministic final tiebreak, so the result never depends on grant
//     insertion order.
//   - With no candidate the decision is DENY. Default-deny is the floor.
//
// Account isolation: the engine only ever asks storage for grants stamped to the
// request's active account (model.Storage.GrantsForSubjects is account-scoped),
// so a grant bestowed in another account can never influence a decision here.
//
// The "does this grant cover this object?" test is isolated behind the coverer
// seam. In E1 it is a literal identity-pattern match; in E2 a scope resolver
// will produce a grant's object set and swap in behind the same seam without
// touching the resolution logic.
//
// The hot path is allocation-conscious (NFR p99 < 1ms) but deliberately holds no
// cache yet — caching lands in E4-S4. Nothing here precludes it: Check is a pure
// function of storage state plus the request.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
)

// Request is the input to Check: the principal asking, the action and object in
// question, and the active account the decision is scoped to. It is a value type
// so the hot path passes it without indirection.
//
// Principal is a principal id (the key storage and the subject set are keyed on),
// not the principal's identity string. Object is an object-identity string in
// canonical form (e.g. "account:acme/project:atlas/document:42"). Account is the
// active account id; grants stamped to any other account are never consulted.
type Request struct {
	// Account is the active account the decision is scoped to. Mandatory.
	Account string
	// Principal is the id of the principal requesting access. Mandatory.
	Principal string
	// Action is the verb being attempted (e.g. "read"). Mandatory.
	Action string
	// Object is the canonical object-identity string. Mandatory.
	Object string
}

// Decision is the PDP's answer. Allow is the verdict; Reason is a human-readable
// explanation that names the deciding grant(s) (the seed Explain builds on in
// E2-S4); DecidingGrantIDs are the ids of the grant(s) that produced the verdict,
// in deterministic order (empty on a default-deny).
type Decision struct {
	// Allow is the verdict: true permits, false denies.
	Allow bool
	// Reason explains the verdict and identifies the deciding grant(s).
	Reason string
	// DecidingGrantIDs are the grant ids that produced the verdict, sorted for
	// determinism. Empty when the decision is a default-deny.
	DecidingGrantIDs []string
}

// coverer answers "does this grant cover this object, and at what specificity?".
// It is the seam scope strategies plug behind in E2: today a grant's object set
// is a single literal pattern, tomorrow it is whatever a scope resolver yields,
// and the resolution logic in Check is unaffected either way.
type coverer interface {
	// cover reports whether g applies to object and, when it does, the
	// specificity at which it applies (higher = more specific). It returns an
	// error only on a malformed grant pattern (an internal inconsistency, since
	// PutGrant validates the pattern at write time).
	cover(g model.Grant, object identity.Identity) (covered bool, specificity int, err error)
}

// literalCoverer is the E1 coverer: it treats Grant.Object as a literal identity
// pattern and matches it against the requested object with the identity matcher.
type literalCoverer struct{}

func (literalCoverer) cover(g model.Grant, object identity.Identity) (bool, int, error) {
	pat, err := identity.ParsePattern(g.Object)
	if err != nil {
		// PutGrant validates the pattern, so reaching here means the stored grant
		// is corrupt. Surface it rather than silently dropping the grant.
		return false, 0, aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"grant %s has an unparseable object pattern %q", g.ID, g.Object)
	}
	if !pat.Matches(object) {
		return false, 0, nil
	}
	return true, identity.Specificity(pat), nil
}

// Engine is the Policy Decision Point. It is stateless beyond its storage handle
// and coverer, and safe for concurrent use to whatever degree the underlying
// Storage is.
type Engine struct {
	store   model.Storage
	coverer coverer
}

// New returns an Engine backed by store. The coverer defaults to the E1 literal
// identity-pattern matcher.
func New(store model.Storage) *Engine {
	return &Engine{store: store, coverer: literalCoverer{}}
}

// candidate is a grant that matched the request, tagged with the specificity at
// which it applies.
type candidate struct {
	grant       model.Grant
	specificity int
}

// Check resolves a single authorization decision. It never returns an
// allow-on-error: any operational failure (bad request, storage error, missing
// principal) is returned as an APERTURE_* coded error and the caller treats it as
// a non-decision, while a well-formed request with no matching grant yields a
// clean default-deny.
func (e *Engine) Check(ctx context.Context, req Request) (Decision, error) {
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}

	object, err := identity.Parse(req.Object)
	if err != nil {
		return Decision{}, err
	}

	subjects, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return Decision{}, err
	}

	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return Decision{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for subjects", err)
	}

	// Resolve action matches against permissions, caching each lookup so a
	// subject with many grants on the same permission pays one query.
	permCache := make(map[string]*model.Permission, len(grants))
	candidates := make([]candidate, 0, len(grants))
	for _, g := range grants {
		ok, err := e.actionMatches(ctx, g, req.Action, permCache)
		if err != nil {
			return Decision{}, err
		}
		if !ok {
			continue
		}
		covered, spec, err := e.coverer.cover(g, object)
		if err != nil {
			return Decision{}, err
		}
		if !covered {
			continue
		}
		candidates = append(candidates, candidate{grant: g, specificity: spec})
	}

	return decide(req, candidates), nil
}

// subjectSet expands a principal into the set of subjects whose grants apply to
// it: the principal itself, every role it is assigned, and every group it belongs
// to. The expansion is the union {principal} ∪ roles ∪ groups.
func (e *Engine) subjectSet(ctx context.Context, principalID string) ([]model.Subject, error) {
	p, err := e.store.GetPrincipal(ctx, principalID)
	if err != nil {
		// A missing principal surfaces as APERTURE_NOT_FOUND verbatim; the caller
		// decides whether to render it as a hard error or a fail-closed deny.
		return nil, err
	}

	subjects := make([]model.Subject, 0, 1+len(p.RoleIDs)+2)
	subjects = append(subjects, model.Subject{Kind: model.SubjectPrincipal, ID: principalID})
	for _, roleID := range p.RoleIDs {
		subjects = append(subjects, model.Subject{Kind: model.SubjectRole, ID: roleID})
	}

	groups, err := e.store.GroupsForPrincipal(ctx, principalID)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load groups for principal", err)
	}
	for _, g := range groups {
		subjects = append(subjects, model.Subject{Kind: model.SubjectGroup, ID: g.ID})
	}
	return subjects, nil
}

// actionMatches reports whether the grant's permission names the requested
// action. A grant whose permission no longer exists (a dangling reference left by
// a deleted permission) is inert and reported as a non-match rather than an
// error, so a stale grant cannot crash the hot path. permCache memoises lookups
// across a single Check.
func (e *Engine) actionMatches(ctx context.Context, g model.Grant, action string, permCache map[string]*model.Permission) (bool, error) {
	perm, ok := permCache[g.PermissionID]
	if !ok {
		p, err := e.store.GetPermission(ctx, g.PermissionID)
		if err != nil {
			if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
				permCache[g.PermissionID] = nil
				return false, nil
			}
			return false, aerr.Wrap(aerr.APERTURE_STORAGE,
				"engine: failed to load permission for grant", err)
		}
		perm = &p
		permCache[g.PermissionID] = perm
	}
	if perm == nil {
		return false, nil
	}
	return perm.Action == action, nil
}

// decide applies deny-overrides with specificity tiebreak to the matched
// candidates. The verdict is the effect of the most specific candidate; at equal
// top specificity deny wins. The result is independent of candidate order.
func decide(req Request, candidates []candidate) Decision {
	if len(candidates) == 0 {
		return Decision{
			Allow: false,
			Reason: fmt.Sprintf(
				"default deny: no grant matched action %q on %q for principal %q in account %q",
				req.Action, req.Object, req.Principal, req.Account),
		}
	}

	// Find the top specificity, and whether any candidate at that level denies.
	maxSpec := candidates[0].specificity
	for _, c := range candidates[1:] {
		if c.specificity > maxSpec {
			maxSpec = c.specificity
		}
	}
	denyWins := false
	for _, c := range candidates {
		if c.specificity == maxSpec && c.grant.Effect == model.EffectDeny {
			denyWins = true
			break
		}
	}

	winning := model.EffectAllow
	if denyWins {
		winning = model.EffectDeny
	}

	// Collect the deciding grants: those at the top specificity carrying the
	// winning effect. Sort by id for a deterministic reason and id list.
	deciding := make([]model.Grant, 0, len(candidates))
	for _, c := range candidates {
		if c.specificity == maxSpec && c.grant.Effect == winning {
			deciding = append(deciding, c.grant)
		}
	}
	sort.Slice(deciding, func(i, j int) bool { return deciding[i].ID < deciding[j].ID })

	ids := make([]string, len(deciding))
	for i, g := range deciding {
		ids[i] = g.ID
	}

	return Decision{
		Allow:            winning == model.EffectAllow,
		Reason:           reasonFor(winning, deciding, len(candidates), maxSpec),
		DecidingGrantIDs: ids,
	}
}

// reasonFor renders a decision explanation that names the deciding grant(s) and
// records the specificity and the number of candidates considered — the seed
// E2-S4's Explain builds on.
func reasonFor(effect model.Effect, deciding []model.Grant, totalMatched, spec int) string {
	verb := "allowed"
	if effect == model.EffectDeny {
		verb = "denied"
	}
	parts := make([]string, len(deciding))
	for i, g := range deciding {
		parts[i] = fmt.Sprintf("%s (%s %s)", g.ID, g.Effect, g.Object)
	}
	return fmt.Sprintf(
		"%s by grant %s at specificity %d; %d matching grant(s) considered",
		verb, strings.Join(parts, ", "), spec, totalMatched)
}

// validateRequest rejects a Request missing any required field before any
// storage work happens.
func validateRequest(req Request) error {
	switch {
	case req.Account == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request account is empty")
	case req.Principal == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request principal is empty")
	case req.Action == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request action is empty")
	case req.Object == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request object is empty")
	}
	return nil
}
