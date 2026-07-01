package service

import (
	"context"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
)

// This file extends the mutation facade with named-rule CRUD (E5-S2). Rules are
// GLOBAL schema, like object-types and templates, so defining or removing one is
// a SYSTEM-tier mutation; reading is ungated like every other entity read. The
// rule AST is persisted verbatim as its canonical JSON (the rules package's own
// serialization), so the node editor (E7) and the state file (export/import)
// share one format.

// PutRule upserts a named rule (system-admin tier). Before persisting, the AST is
// DEEP-validated by the rules engine (rules.ValidateAST): structural validity, the
// exposed variable roots, and a compile pass that surfaces type errors and unknown
// functions — so an invalid rule is rejected at save with its APERTURE_RULE_* code
// (shown on the editor canvas), never stored. The stricter model.ValidateRule
// (JSON-object shape) still runs inside the store as a final structural gate.
func (s *Service) PutRule(ctx context.Context, actor Actor, r model.Rule) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordMutation(ctx, actor, "PutRule", "rule:"+r.Name, err) }()
	if err = s.authorize(ctx, actor, authz.MutationPutRule, ""); err != nil {
		return err
	}
	if err = validateRule(r); err != nil {
		return err
	}
	s.stamp(&r.CreatedAt, &r.UpdatedAt)
	return s.store.PutRule(ctx, r)
}

// ValidateRule compiles/validates a rule WITHOUT persisting it, so the node editor
// (E7-S3) can check a rule server-side before saving. It runs the same deep
// validation PutRule applies (model shape + rules.ValidateAST), returning nil for
// a valid rule and the APERTURE_RULE_* coded error otherwise. It touches no
// storage and requires no admin tier — it is a pure, non-persisting check.
func (s *Service) ValidateRule(ctx context.Context, r model.Rule) error {
	return validateRule(r)
}

// validateRule is the shared definition-time rule gate: the model's structural
// check (non-empty name, AST is a JSON object) followed by the rules engine's
// deep compile-time validation of the AST.
func validateRule(r model.Rule) error {
	if err := model.ValidateRule(r); err != nil {
		return err
	}
	return rules.ValidateAST(r.AST)
}

// GetRule reads one rule by name. Reads require no admin tier.
func (s *Service) GetRule(ctx context.Context, name string) (model.Rule, error) {
	if err := s.requireStore(); err != nil {
		return model.Rule{}, err
	}
	return s.store.GetRule(ctx, name)
}

// ListRules lists every stored rule. Reads require no admin tier.
func (s *Service) ListRules(ctx context.Context) ([]model.Rule, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListRules(ctx)
}

// DeleteRule removes a rule by name (system-admin tier).
func (s *Service) DeleteRule(ctx context.Context, actor Actor, name string) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordMutation(ctx, actor, "DeleteRule", "rule:"+name, err) }()
	if err = s.authorize(ctx, actor, authz.MutationDeleteRule, ""); err != nil {
		return err
	}
	return s.store.DeleteRule(ctx, name)
}
