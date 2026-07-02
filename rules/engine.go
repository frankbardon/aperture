package rules

import (
	"context"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Rule is a named rule definition: a reference label plus the AST that decides
// it. Rule is the unit a RuleSource resolves and the editor/state-file persist,
// so it serializes to stable JSON alongside its AST.
type Rule struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	AST         *Node  `json:"ast"`
}

// RuleSource resolves a scope strategy's opaque rule reference to its definition.
// A host backs it with whatever store holds rules (a config map, a database, the
// state file); MapSource is the in-memory default. A reference with no matching
// rule yields APERTURE_RULE_NOT_FOUND.
type RuleSource interface {
	Lookup(ctx context.Context, ref string) (*Rule, error)
}

// MapSource is an in-memory RuleSource keyed by rule reference. It is the simple
// default for tests and static configurations.
type MapSource map[string]*Rule

// Lookup resolves ref, returning APERTURE_RULE_NOT_FOUND when it is absent.
func (m MapSource) Lookup(_ context.Context, ref string) (*Rule, error) {
	r, ok := m[ref]
	if !ok || r == nil {
		return nil, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: no rule registered for reference", map[string]any{"rule": ref})
	}
	return r, nil
}

// MetadataFetcher supplies an object's metadata for the evaluation context. Its
// signature matches *provider.Registry.Fetch (provider.Metadata is map[string]any),
// so a *provider.Registry is wired directly as the fetcher without this package
// importing provider. The returned map is treated as read-only.
type MetadataFetcher interface {
	Fetch(ctx context.Context, id identity.Identity) (map[string]any, error)
}

// PrincipalResolver supplies a principal's attribute bag for the evaluation
// context, keyed by principal id. The default exposes only {"id": principal}; a
// host wires a richer resolver (roles, account, clearance) when its rules need
// principal attributes. The returned map is treated as read-only.
type PrincipalResolver interface {
	Attributes(ctx context.Context, principal string) (map[string]any, error)
}

// Engine is the rules engine: it resolves a rule reference, compiles-and-caches
// it, builds the evaluation context from object metadata and principal/action,
// and evaluates. It satisfies scope.RuleEvaluator (see scope.go), so the
// inclusive/exclusive scope resolvers get their rule-backed variant by wiring an
// Engine as scope.Deps{Rules: engine}.
//
// Engine is safe for concurrent use: the compiler options are read-only and the
// compiled-rule cache is concurrency-safe.
type Engine struct {
	source    RuleSource
	fetcher   MetadataFetcher
	principal PrincipalResolver
	compiler  *Compiler
	cache     *compiledCache
}

// Option configures an Engine.
type Option func(*engineConfig)

type engineConfig struct {
	compilerOpts []CompilerOption
	ttl          time.Duration
	clock        Clock
	principal    PrincipalResolver
}

// WithFunction registers a pure host function callable from rules (expr-lang's
// Function seam). See Function.
func WithFunction(name string, fn func(args ...any) (any, error)) Option {
	return func(c *engineConfig) { c.compilerOpts = append(c.compilerOpts, Function(name, fn)) }
}

// WithCacheTTL bounds how long a compiled rule stays cached. TTL <= 0 (the
// default) keeps compiled rules until explicitly invalidated.
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *engineConfig) { c.ttl = ttl }
}

// WithClock injects the clock the cache TTL reads, so tests drive expiry without
// sleeping. Defaults to the real clock.
func WithClock(clk Clock) Option {
	return func(c *engineConfig) { c.clock = clk }
}

// WithPrincipalResolver supplies principal attributes to the evaluation context.
// Without it, principal exposes only its id.
func WithPrincipalResolver(r PrincipalResolver) Option {
	return func(c *engineConfig) { c.principal = r }
}

// NewEngine builds an Engine over a rule source and a metadata fetcher. A nil
// fetcher means object metadata is empty (for rules that read only the
// principal/action context). Pass a *provider.Registry as the fetcher to read
// real object metadata.
func NewEngine(source RuleSource, fetcher MetadataFetcher, opts ...Option) *Engine {
	cfg := &engineConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	var principal PrincipalResolver = idOnlyPrincipal{}
	if cfg.principal != nil {
		principal = cfg.principal
	}
	return &Engine{
		source:    source,
		fetcher:   fetcher,
		principal: principal,
		compiler:  NewCompiler(cfg.compilerOpts...),
		cache:     newCompiledCache(cfg.ttl, cfg.clock),
	}
}

// Selected reports whether object is selected by the named rule for the given
// principal/action context. It is the scope.RuleEvaluator implementation the
// rule-backed inclusive/exclusive scope resolvers consult: an inclusive grant
// covers an object Selected reports true for, an exclusive grant excludes one.
//
// The flow is: resolve the rule reference, compile-and-cache its AST, fetch the
// object's metadata, build the context, and evaluate. Any failure is an
// APERTURE_* coded error and the resolver treats it as a non-decision — there is
// no select-on-error.
func (e *Engine) Selected(ctx context.Context, rule string, object identity.Identity, principal, action string) (bool, error) {
	r, err := e.source.Lookup(ctx, rule)
	if err != nil {
		return false, err
	}
	if r == nil || r.AST == nil {
		return false, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: reference resolved to an empty rule", map[string]any{"rule": rule})
	}
	compiled, err := e.compile(r.AST)
	if err != nil {
		return false, err
	}
	metadata, err := e.metadata(ctx, object)
	if err != nil {
		return false, err
	}
	principalAttrs, err := e.principal.Attributes(ctx, principal)
	if err != nil {
		return false, err
	}
	in := Input{
		Object:    metadata,
		Principal: principalAttrs,
		Account:   map[string]any{},
		Action:    action,
	}
	return compiled.Eval(ctx, in)
}

// Compile validates, compiles, and caches an AST, returning the reusable program.
// A second call with an AST that renders to the same expression returns the
// cached program without recompiling. It is exported so a host can warm the cache
// or validate a rule (e.g. the node editor's save path) ahead of evaluation.
func (e *Engine) Compile(n *Node) (*Compiled, error) {
	return e.compile(n)
}

func (e *Engine) compile(n *Node) (*Compiled, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	src, err := n.Expr()
	if err != nil {
		return nil, err
	}
	hash := hashSource(src)
	if c, ok := e.cache.get(hash); ok {
		return c, nil
	}
	compiled, err := e.compiler.compileSource(src)
	if err != nil {
		return nil, err
	}
	e.cache.put(compiled)
	return compiled, nil
}

// metadata fetches object's metadata, or returns an empty map when no fetcher is
// configured (rules that read only principal/action still evaluate).
func (e *Engine) metadata(ctx context.Context, object identity.Identity) (map[string]any, error) {
	if e.fetcher == nil {
		return map[string]any{}, nil
	}
	md, err := e.fetcher.Fetch(ctx, object)
	if err != nil {
		return nil, err
	}
	if md == nil {
		md = map[string]any{}
	}
	return md, nil
}

// CacheStats exposes the compiled-rule cache counters for observability and the
// latency benchmark (E4-S4).
func (e *Engine) CacheStats() CacheStats { return e.cache.stats() }

// InvalidateAll clears the compiled-rule cache. A host calls it after a rule's
// definition changes underneath a cached compilation.
func (e *Engine) InvalidateAll() { e.cache.clear() }

// Invalidate drops the cached compilation of a single rule AST, reporting whether
// one was cached. A host calls it when exactly one rule's definition changed, so
// the next evaluation recompiles only that rule.
func (e *Engine) Invalidate(n *Node) (bool, error) {
	if err := n.Validate(); err != nil {
		return false, err
	}
	src, err := n.Expr()
	if err != nil {
		return false, err
	}
	return e.cache.invalidate(hashSource(src)), nil
}

// idOnlyPrincipal is the default PrincipalResolver: it exposes only the
// principal's id, so a rule can reference principal.id without any host wiring.
type idOnlyPrincipal struct{}

func (idOnlyPrincipal) Attributes(_ context.Context, principal string) (map[string]any, error) {
	return map[string]any{"id": principal}, nil
}
