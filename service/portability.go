package service

import (
	"context"

	"github.com/frankbardon/aperture/authz"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
)

// This file adds the E5-S2 declarative export/import surface to the facade:
//
//   - Export is a SYSTEM-tier READ. It serializes the COMPLETE source-of-truth
//     model — every RBAC entity, grants, templates, and rules (AST) — into one
//     declarative seed.Document, EXCLUDING live host domain-object metadata (the
//     provider cache is derived, never source of truth). Because it can read the
//     whole system it is gated at the system tier (RequireSystemAdmin), but it
//     writes nothing, so it is not an audited mutation.
//
//   - Import is a SYSTEM-tier MUTATION. It applies a declarative Document as an
//     idempotent upsert, TRANSACTIONALLY (one storage.Atomic): either the whole
//     file applies or — on any validation failure — nothing does, so a bad file
//     never half-applies. Re-importing the same file is a no-op at the model
//     level, and an export→import→export round-trip is byte-stable.
//
// Both wire to the same seed.Document the E1-S5 loader used, generalized to the
// whole model — the export file is a strict superset of the seed format.

// Export reads the whole model out of storage as a declarative Document. It is a
// system-tier read: actor must be an authenticated system-admin. It never
// mutates, so it is not recorded as an audit mutation.
func (s *Service) Export(ctx context.Context, actor Actor) (*seed.Document, error) {
	if err := s.requireMutator(); err != nil {
		return nil, err
	}
	if actor.Principal == "" {
		return nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: export requires an authenticated principal")
	}
	if err := s.gate.RequireSystemAdmin(ctx, actor.gateActor()); err != nil {
		return nil, err
	}
	return seed.Export(ctx, s.store)
}

// Import applies doc as an idempotent, transactional upsert (system-admin tier).
// Every entity is written inside one storage.Atomic, so a validation failure
// anywhere rolls the WHOLE import back — a partial or invalid file is rejected
// with its APERTURE_* coded error and nothing persists. Import is additive: it
// upserts every entity in the file and does not delete entities absent from it,
// so re-importing the same file changes nothing.
func (s *Service) Import(ctx context.Context, actor Actor, doc *seed.Document) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordMutation(ctx, actor, "Import", "state", err) }()
	if doc == nil {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "service: import requires a document")
	}
	if err = s.authorize(ctx, actor, authz.MutationImport, ""); err != nil {
		return err
	}
	return s.store.Atomic(ctx, func(tx model.Storage) error {
		return doc.Apply(ctx, tx)
	})
}
