package rules

import "github.com/frankbardon/aperture/scope"

// Compile-time proof that *Engine is the rule-backed scope evaluator E2-S1 left
// as a seam. The engine wiring (E2-S4) passes an *Engine as
// scope.Deps{Rules: engine} / engine.ScopeDeps{Rules: engine} so the
// inclusive/exclusive resolvers get their rule-backed variant.
var _ scope.RuleEvaluator = (*Engine)(nil)
