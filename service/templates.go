package service

import (
	"context"
	"strconv"

	"github.com/frankbardon/aperture/authz"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// This file extends the mutation facade with the E5-S1 provisioning surface:
// parameterized template CRUD, transactional template apply, and bulk
// grant/revoke. Three layering rules hold:
//
//   - Template DEFINITION (PutTemplate / DeleteTemplate) is SYSTEM-tier: defining
//     the provisioning schema is global administration, like object-types and
//     roles (the authz Template rows already map to TierSystem).
//   - Template APPLY and bulk grant/revoke WRITE GRANTS into an account, so they
//     are ACCOUNT-tier — gated exactly like a raw PutGrant/DeleteGrant, in the
//     TARGET account, so an account-admin can provision its own account and only
//     its own.
//   - Apply and bulk are TRANSACTIONAL: every expanded grant is written inside one
//     storage.Atomic, so a partial failure rolls back the WHOLE operation — no
//     grant persists if any fails — and the apply is audited as ONE logical event.

// ---- Template definition (system tier) ----

// PutTemplate upserts a versioned template (system-admin tier). The template is
// validated structurally before persisting (ValidateTemplate); a bad template is
// rejected here so it can never reach apply.
func (s *Service) PutTemplate(ctx context.Context, actor Actor, t model.Template) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() {
		s.recordMutation(ctx, actor, "PutTemplate", "template:"+t.Name+":v"+strconv.Itoa(t.Version), err)
	}()
	if err = s.authorize(ctx, actor, authz.MutationPutTemplate, ""); err != nil {
		return err
	}
	s.stamp(&t.CreatedAt, &t.UpdatedAt)
	return s.store.PutTemplate(ctx, t)
}

// GetTemplate reads one template version (latest when version <= 0). Reads
// require no admin tier.
func (s *Service) GetTemplate(ctx context.Context, name string, version int) (model.Template, error) {
	if err := s.requireStore(); err != nil {
		return model.Template{}, err
	}
	return s.store.GetTemplate(ctx, name, version)
}

// ListTemplates lists every stored template version. Reads require no admin tier.
func (s *Service) ListTemplates(ctx context.Context) ([]model.Template, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.store.ListTemplates(ctx)
}

// DeleteTemplate removes a template version (system-admin tier). A version <= 0
// deletes every version of the name.
func (s *Service) DeleteTemplate(ctx context.Context, actor Actor, name string, version int) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() {
		s.recordMutation(ctx, actor, "DeleteTemplate", "template:"+name+":v"+strconv.Itoa(version), err)
	}()
	if err = s.authorize(ctx, actor, authz.MutationDeleteTemplate, ""); err != nil {
		return err
	}
	return s.store.DeleteTemplate(ctx, name, version)
}

// ---- Template apply (account tier, transactional, single audit event) ----

// ApplyTemplate resolves a template's parameters, expands it to concrete grants,
// and applies them TRANSACTIONALLY into app.Account: either every grant persists
// or — on any failure — none does. It is account-tier (the actor must hold
// account-admin in app.Account, exactly as a raw PutGrant) and is audited as ONE
// logical event carrying the template name+version and resolved parameters, not
// one event per expanded grant. It returns the grants that were applied.
func (s *Service) ApplyTemplate(ctx context.Context, actor Actor, app model.TemplateApplication) (applied []model.Grant, err error) {
	if err = s.requireMutator(); err != nil {
		return nil, err
	}
	// version recorded in the audit event is the resolved version (after latest
	// selection), captured below; default to the requested one for an early failure.
	version := app.Version
	defer func() {
		ids := grantIDsOf(applied)
		s.recordTemplateApply(ctx, actor, app, version, ids, err)
	}()

	if actor.Principal == "" {
		return nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: a mutation requires an authenticated principal")
	}
	if app.Account == "" {
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "service: template apply requires a target account")
	}
	// Account-tier authority in the target account, the same gate a raw grant write
	// passes.
	if err = s.gate.Authorize(ctx, actor.gateActor(), authz.MutationPutGrant, app.Account); err != nil {
		return nil, err
	}

	tmpl, err := s.store.GetTemplate(ctx, app.Name, app.Version)
	if err != nil {
		return nil, err // NOT_FOUND when the template/version is unknown.
	}
	version = tmpl.Version

	grants, err := model.ExpandTemplate(tmpl, app, s.clock())
	if err != nil {
		return nil, err // template-param / validation failure, nothing written.
	}

	// Apply transactionally: a partial failure rolls the WHOLE batch back.
	err = s.store.Atomic(ctx, func(tx model.Storage) error {
		for _, g := range grants {
			if e := tx.PutGrant(ctx, g); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	applied = grants
	return applied, nil
}

// ---- Bulk grant / revoke (account tier, transactional) ----

// BulkPutGrants provisions many grants in one transactional call: either all are
// written or — on any failure — none. Each distinct target account is
// account-tier authorized (an actor may only write grants into accounts it
// administers), then the whole batch is applied inside one storage.Atomic.
func (s *Service) BulkPutGrants(ctx context.Context, actor Actor, grants []model.Grant) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	account := commonAccount(grants)
	defer func() { s.recordBulk(ctx, actor, account, "BulkPutGrants", grantIDsOf(grants), err) }()

	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: a mutation requires an authenticated principal")
	}
	if len(grants) == 0 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "service: bulk put requires at least one grant")
	}
	// Authorize every distinct target account before writing any grant.
	for _, acct := range distinctAccounts(grants) {
		if acct == "" {
			return aerr.New(aerr.APERTURE_INVALID_INPUT, "service: a grant is not stamped with an account")
		}
		if err = s.gate.Authorize(ctx, actor.gateActor(), authz.MutationPutGrant, acct); err != nil {
			return err
		}
	}
	staged := make([]model.Grant, len(grants))
	for i, g := range grants {
		s.stamp(&g.CreatedAt, &g.UpdatedAt)
		staged[i] = g
	}
	return s.store.Atomic(ctx, func(tx model.Storage) error {
		for _, g := range staged {
			if e := tx.PutGrant(ctx, g); e != nil {
				return e
			}
		}
		return nil
	})
}

// BulkDeleteGrants deprovisions many grants in one transactional call: either all
// are removed or — on any failure (e.g. an unknown id) — none. Each grant's
// account governs the account-tier authority check, so every grant is loaded to
// resolve its account; each distinct account is authorized before any delete.
func (s *Service) BulkDeleteGrants(ctx context.Context, actor Actor, grantIDs []string) (err error) {
	if err = s.requireMutator(); err != nil {
		return err
	}
	defer func() { s.recordBulk(ctx, actor, "", "BulkDeleteGrants", grantIDs, err) }()

	if actor.Principal == "" {
		return aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: a mutation requires an authenticated principal")
	}
	if len(grantIDs) == 0 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "service: bulk delete requires at least one grant id")
	}
	// Resolve each grant's account (NOT_FOUND fails the whole batch) and authorize
	// every distinct account before deleting anything.
	accounts := map[string]struct{}{}
	for _, id := range grantIDs {
		g, gerr := s.store.GetGrant(ctx, id)
		if gerr != nil {
			return gerr
		}
		accounts[g.AccountID] = struct{}{}
	}
	for acct := range accounts {
		if err = s.gate.Authorize(ctx, actor.gateActor(), authz.MutationDeleteGrant, acct); err != nil {
			return err
		}
	}
	return s.store.Atomic(ctx, func(tx model.Storage) error {
		for _, id := range grantIDs {
			if e := tx.DeleteGrant(ctx, id); e != nil {
				return e
			}
		}
		return nil
	})
}

// ---- helpers ----

func grantIDsOf(grants []model.Grant) []string {
	if len(grants) == 0 {
		return nil
	}
	out := make([]string, len(grants))
	for i, g := range grants {
		out[i] = g.ID
	}
	return out
}

func distinctAccounts(grants []model.Grant) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, g := range grants {
		if _, ok := seen[g.AccountID]; !ok {
			seen[g.AccountID] = struct{}{}
			out = append(out, g.AccountID)
		}
	}
	return out
}

// commonAccount returns the single account a batch targets, or "" when the batch
// is empty or spans more than one account (the audit event's Account field is a
// best-effort summary; per-account authorization still runs over every account).
func commonAccount(grants []model.Grant) string {
	accts := distinctAccounts(grants)
	if len(accts) == 1 {
		return accts[0]
	}
	return ""
}

// itoa is strconv.Itoa, used by the audit helpers in audit.go.
func itoa(n int) string { return strconv.Itoa(n) }
