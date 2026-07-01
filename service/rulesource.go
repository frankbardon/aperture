package service

import (
	"context"
	"encoding/json"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
)

// This file bridges the persisted rule store (model.Storage / model.Rule, whose
// AST is opaque JSON) to the rules engine's RuleSource (rules.Rule, whose AST is
// a decoded *rules.Node). It is the seam that makes a SAVED rule take effect in
// the rule-backed scope strategies (E2-S1) immediately: the same store the editor
// writes through PutRule is the source the decision engine resolves against, so
// there is no second rule store to keep in sync.

// StorageRuleSource resolves a rule reference by reading the persisted rule from a
// model.Storage and decoding its AST into a rules.Node. Wiring it as the rules
// engine's source (rules.NewEngine(storageRuleSource, fetcher)) is what closes the
// E7 loop: a rule saved via the editor is exactly what a rule-backed grant
// resolves against on the next decision.
type StorageRuleSource struct {
	store model.Storage
}

// NewStorageRuleSource adapts store to a rules.RuleSource. It is used by the serve
// wiring (and the tests) to back the decision engine's rule resolution with the
// same store the editor persists to.
func NewStorageRuleSource(store model.Storage) *StorageRuleSource {
	return &StorageRuleSource{store: store}
}

// Lookup resolves ref to its stored rule, returning APERTURE_RULE_NOT_FOUND when
// absent (the store's GetRule surfaces a not-found) and APERTURE_RULE_INVALID when
// the persisted AST cannot be decoded into a rules.Node.
func (s *StorageRuleSource) Lookup(ctx context.Context, ref string) (*rules.Rule, error) {
	r, err := s.store.GetRule(ctx, ref)
	if err != nil {
		return nil, err
	}
	return decodeStoredRule(r)
}

// decodeStoredRule converts a persisted model.Rule (opaque JSON AST) into the
// rules.Rule the engine evaluates (decoded *rules.Node).
func decodeStoredRule(r model.Rule) (*rules.Rule, error) {
	var n rules.Node
	if err := json.Unmarshal(r.AST, &n); err != nil {
		return nil, aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"service: stored rule AST is not a valid rules.Node", map[string]any{"rule": r.Name})
	}
	return &rules.Rule{Name: r.Name, Description: r.Description, AST: &n}, nil
}

// overlayRuleSource layers hypothetical (unsaved) rules over a base rule source
// for the READ-ONLY what-if path: a rule whose name matches an overlay entry
// resolves to the overlay AST (the edit being previewed), shadowing the stored
// rule; every other reference falls through to the base. It is never persisted
// and its map is built per simulation.
type overlayRuleSource struct {
	base  rules.RuleSource
	rules map[string]*rules.Rule
}

// newOverlayRuleSource decodes overlay model.Rules into rules.Rules and layers
// them over base. A rule that fails to decode is skipped rather than failing the
// whole preview — the client-side validate + server ValidateRule already gate a
// broken AST before it reaches here.
func newOverlayRuleSource(base rules.RuleSource, overlay []model.Rule) *overlayRuleSource {
	m := make(map[string]*rules.Rule, len(overlay))
	for _, r := range overlay {
		if dr, err := decodeStoredRule(r); err == nil {
			m[r.Name] = dr
		}
	}
	return &overlayRuleSource{base: base, rules: m}
}

// Lookup resolves ref from the overlay first, then the base. With no base and no
// overlay match it reports APERTURE_RULE_NOT_FOUND.
func (o *overlayRuleSource) Lookup(ctx context.Context, ref string) (*rules.Rule, error) {
	if r, ok := o.rules[ref]; ok {
		return r, nil
	}
	if o.base == nil {
		return nil, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"service: no rule registered for reference", map[string]any{"rule": ref})
	}
	return o.base.Lookup(ctx, ref)
}
