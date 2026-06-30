// Package scope holds Aperture's pluggable scope-strategy resolvers: the seam
// that decides HOW a grant enumerates the objects it covers (FR-7).
//
// In E1 a grant's object set was a single literal identity pattern. In E2 a
// grant declares an "object access mode" — a scope strategy — that the decision
// engine consults for object membership instead of (only) literal pattern
// matching. This package ships the three built-in strategies and a registry so
// host code can add its own:
//
//   - literal   — the E1 behaviour: the grant covers exactly the objects its
//     identity pattern matches. (Owned natively by the engine; the registry
//     covers the three non-trivial strategies below.)
//   - implicit  — every object of the permission's type within the grant's
//     pattern scope (unfettered, opt-out-able via exclusive).
//   - inclusive — opt-in: only the objects named by an explicit id-list (full
//     list-backed path) OR selected by a rule (E2-S3 seam).
//   - exclusive — opt-out: every object of the type within the pattern scope
//     EXCEPT those named by an id-list (full) or excluded by a rule (E2-S3 seam).
//
// Membership composes with the grant's identity pattern: the pattern always
// bounds the scope (and supplies the specificity the engine's deny-overrides
// tiebreak consumes), while the strategy decides membership WITHIN that bound.
// A resolver never computes specificity — that stays the pattern's job in the
// engine, so the E1 resolution semantics are untouched.
//
// Dependencies are deliberately minimal: scope imports only identity and errors,
// never model, so it stays a leaf the engine adapts model.Grant/Permission into.
// The two not-yet-built dependencies are isolated behind seam interfaces with
// inert defaults:
//
//   - ObjectLister enumerates "all objects of a type" for implicit/exclusive
//     Members. E2-S2's object provider supplies the real one; until then the
//     default returns APERTURE_SCOPE_LISTER_UNCONFIGURED. Contains never needs
//     it (implicit/exclusive membership is computable without enumeration).
//   - RuleEvaluator evaluates a rule-backed inclusive/exclusive path. E2-S3
//     wires it; until then the default returns APERTURE_SCOPE_RULE_UNCONFIGURED.
//
// Resolver evaluation runs inside the engine's Check hot path, so construction
// is cheap (small value structs, list membership by linear scan over the
// typically-small id-list, no map allocation) and holds no cache — nothing here
// precludes one (E4-S4).
package scope

import (
	"context"
	"strings"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Built-in strategy keys. literal is owned natively by the engine and is not in
// the default registry; the other three are the resolvers this package ships.
const (
	// StrategyLiteral is the E1 default: cover exactly the pattern's matches.
	StrategyLiteral = "literal"
	// StrategyImplicit covers every object of the type within the pattern scope.
	StrategyImplicit = "implicit"
	// StrategyInclusive covers only an explicit id-list (or a rule selection).
	StrategyInclusive = "inclusive"
	// StrategyExclusive covers all-of-type minus an id-list (or rule exclusion).
	StrategyExclusive = "exclusive"
)

// DefaultMaxMembers bounds Members enumeration when a caller does not impose its
// own limit, so a resolver can never materialise an unbounded object set.
const DefaultMaxMembers = 1000

// ScopeResolver decides a grant's object membership for one strategy. It is
// constructed per evaluation from a GrantContext (the grant's pattern, the
// permission's object type, the parsed scope Spec, and the principal/action
// context) plus the runtime Deps (lister, rule evaluator).
//
// Contains answers the hot-path question — "is this concrete object a member of
// the grant's object set?" — and never needs to enumerate. Members performs a
// bounded enumeration for Enumerate-style callers (E2-S4); strategies that
// enumerate "all objects of the type" depend on the ObjectLister and surface
// APERTURE_SCOPE_LISTER_UNCONFIGURED until E2-S2 supplies one.
type ScopeResolver interface {
	// Contains reports whether object is a member of the grant's object set.
	Contains(ctx context.Context, object identity.Identity) (bool, error)
	// Members returns the member identities that also match pattern, bounded by
	// DefaultMaxMembers. It may require an ObjectLister; when none is configured
	// it returns APERTURE_SCOPE_LISTER_UNCONFIGURED.
	Members(ctx context.Context, pattern identity.Pattern) ([]identity.Identity, error)
}

// Spec is the typed form of a permission's opaque scope-strategy reference. The
// engine parses Permission.ScopeStrategy into a Spec with ParseSpec; the model
// keeps the reference as an opaque string, so this typing introduces no schema
// change.
//
// Reference grammar: "strategy" optionally followed by ';'-separated params,
// each "name=value". Identity strings never contain ';', '=', or ',', so those
// separators never collide with the id-list values:
//
//	implicit
//	inclusive;ids=account:acme/document:42,account:acme/document:99
//	exclusive;ids=account:acme/document:7
//	inclusive;rule=quarantine-rule
type Spec struct {
	// Strategy is the resolver key (e.g. "inclusive"). Empty parses to literal.
	Strategy string
	// IDs is the explicit object-identity list for the inclusive/exclusive
	// list-backed path. Entries are canonical identity strings.
	IDs []string
	// Rule is the rule reference for the inclusive/exclusive rule-backed path
	// (E2-S3). Empty when the strategy uses the list path.
	Rule string
}

// ParseSpec parses a permission's opaque scope-strategy reference into a Spec.
// An empty reference (or the explicit "literal") yields the literal strategy, so
// E1 grants — which carry no strategy — keep their literal behaviour. It
// validates structure only (known params, non-empty values, no duplicates); it
// does not consult the registry, so a custom strategy key parses cleanly and is
// resolved by whatever the host registered. Malformed references yield
// APERTURE_SCOPE_INVALID.
func ParseSpec(ref string) (Spec, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Spec{Strategy: StrategyLiteral}, nil
	}
	parts := strings.Split(ref, ";")
	strategy := strings.TrimSpace(parts[0])
	if strategy == "" {
		return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"scope reference has an empty strategy key",
			map[string]any{"ref": ref})
	}
	spec := Spec{Strategy: strategy}
	seen := make(map[string]struct{}, len(parts)-1)
	for _, raw := range parts[1:] {
		param := strings.TrimSpace(raw)
		if param == "" {
			return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
				"scope reference has an empty parameter",
				map[string]any{"ref": ref})
		}
		eq := strings.IndexByte(param, '=')
		if eq < 0 {
			return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
				"scope parameter is missing its '=' separator",
				map[string]any{"ref": ref, "param": param})
		}
		name, value := strings.TrimSpace(param[:eq]), strings.TrimSpace(param[eq+1:])
		if value == "" {
			return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
				"scope parameter has an empty value",
				map[string]any{"ref": ref, "param": name})
		}
		if _, dup := seen[name]; dup {
			return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
				"scope reference repeats a parameter",
				map[string]any{"ref": ref, "param": name})
		}
		seen[name] = struct{}{}
		switch name {
		case "ids":
			ids := strings.Split(value, ",")
			out := make([]string, 0, len(ids))
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
						"scope id-list has an empty entry",
						map[string]any{"ref": ref})
				}
				out = append(out, id)
			}
			spec.IDs = out
		case "rule":
			spec.Rule = value
		default:
			return Spec{}, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
				"scope reference has an unknown parameter",
				map[string]any{"ref": ref, "param": name})
		}
	}
	return spec, nil
}

// GrantContext is the per-evaluation context a resolver is built from. Pattern
// is the grant's already-parsed object pattern (the engine parses it once for
// both specificity and scope); ObjectType is the permission's object type;
// Spec is the parsed scope reference; Principal and Action carry the request
// context the rule path needs.
type GrantContext struct {
	Pattern    identity.Pattern
	ObjectType string
	Spec       Spec
	Principal  string
	Action     string
}

// ObjectLister enumerates the object identities of a type, bounded. It is the
// minimal seam implicit/exclusive Members depend on; E2-S2's object provider
// supplies the real implementation. limit <= 0 means DefaultMaxMembers.
type ObjectLister interface {
	// List returns up to limit object identities of objectType that match
	// pattern. The pattern bounds the enumeration to the grant's scope.
	List(ctx context.Context, objectType string, pattern identity.Pattern, limit int) ([]identity.Identity, error)
}

// noLister is the default ObjectLister: enumeration is unavailable until E2-S2.
type noLister struct{}

func (noLister) List(context.Context, string, identity.Pattern, int) ([]identity.Identity, error) {
	return nil, aerr.New(aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED,
		"scope: object enumeration requires an ObjectLister (arrives in E2-S2)")
}

// RuleEvaluator decides rule-backed scope membership. It is the seam the
// rule-driven inclusive/exclusive path consults; E2-S3 supplies the Pulse-backed
// implementation. Selected reports whether object is selected by rule for the
// given principal/action context.
type RuleEvaluator interface {
	Selected(ctx context.Context, rule string, object identity.Identity, principal, action string) (bool, error)
}

// noRules is the default RuleEvaluator: the rule path is unwired until E2-S3.
type noRules struct{}

func (noRules) Selected(context.Context, string, identity.Identity, string, string) (bool, error) {
	return false, aerr.New(aerr.APERTURE_SCOPE_RULE_UNCONFIGURED,
		"scope: rule-backed membership requires a RuleEvaluator (arrives in E2-S3)")
}

// Deps are the runtime dependencies a resolver may consult. The zero value is
// usable: a nil Lister/Rules defaults to the inert no-op that reports the
// dependency is unconfigured.
type Deps struct {
	Lister ObjectLister
	Rules  RuleEvaluator
}

func (d Deps) lister() ObjectLister {
	if d.Lister != nil {
		return d.Lister
	}
	return noLister{}
}

func (d Deps) rules() RuleEvaluator {
	if d.Rules != nil {
		return d.Rules
	}
	return noRules{}
}

// Factory constructs a ScopeResolver for one grant evaluation. It validates the
// Spec for its strategy and captures the context and deps the resolver needs.
type Factory func(gc GrantContext, deps Deps) (ScopeResolver, error)

// Registry maps strategy keys to factories. It is safe for concurrent use, so a
// host can register custom strategies at startup and the engine can resolve from
// it on the hot path. Use NewRegistry for an empty one or DefaultRegistry for
// one preloaded with the three built-ins.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// DefaultRegistry returns a registry preloaded with the built-in implicit,
// inclusive, and exclusive strategies. literal is handled natively by the engine
// and is intentionally not registered here.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.MustRegister(StrategyImplicit, newImplicitResolver)
	r.MustRegister(StrategyInclusive, newInclusiveResolver)
	r.MustRegister(StrategyExclusive, newExclusiveResolver)
	return r
}

// Register adds a strategy factory under key. It rejects an empty key, a nil
// factory, or a duplicate registration with APERTURE_SCOPE_INVALID.
func (r *Registry) Register(key string, f Factory) error {
	if key == "" {
		return aerr.New(aerr.APERTURE_SCOPE_INVALID, "scope: cannot register an empty strategy key")
	}
	if f == nil {
		return aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"scope: cannot register a nil factory", map[string]any{"strategy": key})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[key]; dup {
		return aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"scope: strategy is already registered", map[string]any{"strategy": key})
	}
	r.factories[key] = f
	return nil
}

// MustRegister is Register that panics on error; for package init and host
// startup wiring where a registration failure is a programming error.
func (r *Registry) MustRegister(key string, f Factory) {
	if err := r.Register(key, f); err != nil {
		panic(err)
	}
}

// Has reports whether key is registered.
func (r *Registry) Has(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[key]
	return ok
}

// Keys returns the registered strategy keys (unordered). Useful for host
// introspection and tests.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	return out
}

// Resolve builds the resolver for gc.Spec.Strategy, validating the spec via the
// strategy's factory. An unregistered strategy yields
// APERTURE_SCOPE_UNKNOWN_STRATEGY; a spec the strategy rejects yields
// APERTURE_SCOPE_INVALID.
func (r *Registry) Resolve(gc GrantContext, deps Deps) (ScopeResolver, error) {
	key := gc.Spec.Strategy
	r.mu.RLock()
	f, ok := r.factories[key]
	r.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_SCOPE_UNKNOWN_STRATEGY,
			"scope: no resolver registered for strategy",
			map[string]any{"strategy": key})
	}
	return f(gc, deps)
}

// boundLimit normalises a caller limit to a positive bound.
func boundLimit(limit int) int {
	if limit <= 0 || limit > DefaultMaxMembers {
		return DefaultMaxMembers
	}
	return limit
}

// terminalOfType reports whether object's terminal segment is of objectType —
// the engine's notion of "object is of the type" for implicit/exclusive
// membership. An empty objectType (a permission with no type) matches nothing.
func terminalOfType(object identity.Identity, objectType string) bool {
	if objectType == "" {
		return false
	}
	segs := object.Segments()
	if len(segs) == 0 {
		return false
	}
	return segs[len(segs)-1].Type == objectType
}
