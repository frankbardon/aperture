package service

import (
	"context"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/model"
)

// This file extends the mutation facade with named-rule CRUD (E5-S2). Rules are
// GLOBAL schema, like object-types and templates, so defining or removing one is
// a SYSTEM-tier mutation; reading is ungated like every other entity read. The
// rule AST is persisted verbatim as its canonical JSON (the rules package's own
// serialization), so the node editor (E7) and the state file (export/import)
// share one format.

// PutRule upserts a named rule (system-admin tier). The rule is validated
// structurally before persisting (ValidateRule); the AST's deep validity is the
// rules engine's concern, enforced by the import path and the editor's save.
func (s *Service) PutRule(ctx context.Context, actor Actor, r model.Rule) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordMutation(ctx, actor, "PutRule", "rule:"+r.Name, err) }()
	if err = s.authorize(ctx, actor, authz.MutationPutRule, ""); err != nil {
		return err
	}
	s.stamp(&r.CreatedAt, &r.UpdatedAt)
	return s.store.PutRule(ctx, r)
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
