package service

import (
	"context"
	"encoding/json"

	"github.com/frankbardon/aperture/authz"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
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
//
// The round-trip guarantee above is byte-stability of the FILE, and it holds
// unconditionally. What it is NOT is a promise that every export can be
// re-imported: Export always emits every account and principal it can read, so
// an unmodified export of a deployment that manages them will be REFUSED whole
// by a deployment that does not (see requireImportable). Moving state into a
// locked deployment therefore means either deleting the locked sections from the
// file or lifting the switch for the duration — the export side has no
// "skip the unmanaged entities" mode, deliberately, because an export that
// silently omitted part of the model would be a worse lie than a refused import.

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

// requireImportable refuses a whole document that carries a section for an
// entity kind this deployment does not manage, with the same
// APERTURE_ENTITY_UNMANAGED code the facade's per-entity gate uses.
//
// This check exists because Import is the ONE write path that does not go
// through Service.PutAccount / PutPrincipal / PutMembership. It hands the
// document to seed.Document.Apply, which reaches model.Storage directly, so
// without this an operator who has locked accounts could still create a hundred
// of them with a single Import call. Anything that adds a new bulk-apply entry
// point to the facade needs the same explicit check — the facade gate cannot be
// inherited by a path that never calls it.
//
// Refusal is WHOLE-DOCUMENT and happens BEFORE the transaction opens, so a
// refused import does no storage work at all. Partial application — dropping the
// locked sections and applying the rest — is deliberately not offered: Import's
// contract is that a file either lands or does not, and silently importing three
// quarters of a state file would be the worst of both answers.
//
// It runs before authorize for the same reason requireManaged does: deployment
// posture and access control are different questions, and only one of them was
// asked. The message names the SECTION a human has to remove and the switch that
// would allow it — never an entity id, so a refusal discloses nothing about the
// document's contents or about any account.
//
// DELIBERATE HOLE, not an oversight: the boot-time seed loader
// (`aperture serve --seed`, seed.Document.Apply called straight against storage)
// is NOT gated by this or anything else. Whoever runs the process also sets
// APERTURE_MANAGE_* on it, so boot-time provisioning sits at the same trust level
// as the switch itself; gating it would mean a deployment could not seed the very
// entities it then declares unmanaged. Import is different because it is a
// runtime RPC an authenticated caller reaches over the wire.
// requireDeclaredRuleKeys applies DECLARED-KEY enforcement to a document's rules.
//
// Import is a rule-WRITING path, and for a while it was the hole in the gate. The
// enforcement's whole argument is that the keys a LOCAL attribute layer adds are
// inert BY CONSTRUCTION, because no rule naming one can be saved — and that only
// holds if every way of saving a rule checks. PutRule and ValidateRule did; Import
// handed the document to Apply with nothing in between, so a rule naming a key only
// one instance's local layer serves could be written through this method and would
// then decide on that instance and read a MISSING PATH on every other one. A missing
// path neither denies nor errors: it makes every predicate over it false, so an
// inclusive grant denies and an EXCLUSIVE grant stops excluding, with nothing in any
// verdict, trace or note to say why. See rules/declared.go for the full argument.
//
// It refuses the document WHOLE and before the transaction opens, for the reason
// requireImportable does: a half-applied import is worse than a refused one, and a
// caller who has to work out which half landed has no way to find out.
//
// An AST this cannot parse is passed over rather than reported. seed.Document.Apply
// validates every rule's structure immediately afterwards, with an
// APERTURE_RULE_INVALID that names the rule; classifying it here as well would give
// one bad file two different refusals depending on which check happened to run first.
func (s *Service) requireDeclaredRuleKeys(doc *seed.Document) error {
	if !s.declaredKeys.Enforcing() {
		// The opt-out, taken once here rather than left to each
		// CheckDeclaredAttributeKeys call: a deployment that declares nothing walks
		// no AST at all, so importing into it costs exactly what it did before
		// declaring existed.
		return nil
	}
	for _, r := range doc.Rules {
		var n rules.Node
		if err := json.Unmarshal(r.AST, &n); err != nil {
			continue
		}
		if err := rules.CheckDeclaredAttributeKeys(&n, s.declaredKeys); err != nil {
			// The code is PASSED THROUGH rather than wrapped: Wrap re-stamps, and
			// APERTURE_RULE_UNDECLARED_ATTRIBUTE's fixups are the ones that name the
			// key and the attribute_providers: entry to go and edit. A generic import
			// code here would cost the operator exactly the remedy.
			return aerr.WithContext(aerr.CodeOf(err),
				"service: import refused — rule "+r.Name+" reads an attribute key this deployment's "+
					"wiring does not declare; nothing from the file was applied: "+err.Error(),
				map[string]any{
					"rule": r.Name,
					"fix": "add the key to declared_keys: on the attribute_providers: entry that serves it, " +
						"or stop reading it in rule " + r.Name,
				})
		}
	}
	return nil
}

func (s *Service) requireImportable(doc *seed.Document) error {
	// The seed Document's YAML/JSON keys for the three gated kinds are exactly
	// the managedKind plurals (accounts:, principals:, memberships:), so one
	// value names the kind, the env var, and the section to point a human at.
	for _, sec := range []struct {
		kind    managedKind
		present bool
	}{
		{kindAccount, len(doc.Accounts) > 0},
		{kindPrincipal, len(doc.Principals) > 0},
		{kindMembership, len(doc.Memberships) > 0},
	} {
		if !sec.present || sec.kind.of(s.managed).Enabled() {
			continue
		}
		return aerr.WithContext(aerr.APERTURE_ENTITY_UNMANAGED,
			"service: import refused — the document carries a "+sec.kind.plural+
				": section and this deployment does not manage "+sec.kind.plural+
				"; nothing from the file was applied",
			map[string]any{
				"entity":  sec.kind.noun,
				"section": sec.kind.plural,
				"env":     sec.kind.env,
				"posture": sec.kind.of(s.managed).String(),
				"fix": "remove the " + sec.kind.plural + ": section from the document, or set " +
					sec.kind.env + "=true and restart aperture; it is read once at startup",
			})
	}
	return nil
}

// Import applies doc as an idempotent, transactional upsert (system-admin tier).
// Every entity is written inside one storage.Atomic, so a validation failure
// anywhere rolls the WHOLE import back — a partial or invalid file is rejected
// with its APERTURE_* coded error and nothing persists. Import is additive: it
// upserts every entity in the file and does not delete entities absent from it,
// so re-importing the same file changes nothing.
//
// A document carrying a section for an entity kind this deployment does not
// manage is refused whole, before the transaction opens — see requireImportable,
// which is also where the reason Import needs its own gate is written down.
func (s *Service) Import(ctx context.Context, actor Actor, doc *seed.Document) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordMutation(ctx, actor, "Import", "state", err) }()
	if doc == nil {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "service: import requires a document")
	}
	if err = s.requireImportable(doc); err != nil {
		return err
	}
	if err = s.requireDeclaredRuleKeys(doc); err != nil {
		return err
	}
	if err = s.authorize(ctx, actor, authz.MutationImport, ""); err != nil {
		return err
	}
	return s.store.Atomic(ctx, func(tx model.Storage) error {
		return doc.Apply(ctx, tx)
	})
}
