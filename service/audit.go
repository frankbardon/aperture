package service

import (
	"context"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// This file wires the audit trail (E4-S2) into the facade — the SINGLE place
// every surface's mutations and decisions flow through, so audit is recorded
// once, here, rather than duplicated per surface.
//
// Recording discipline (see the audit package):
//
//   - Mutations, impersonation, and delegation are ALWAYS recorded, synchronously
//     (they are not the hot path and the security record must be reliable).
//   - Decision checks are SAMPLED and recorded asynchronously off the Check
//     critical path, so audit never regresses the decision NFR (E4-S4).
//
// A facade built without WithAudit records nothing — s.audit is nil and every
// hook below short-circuits — so the existing no-audit construction (and its
// tests) are unaffected.

// WithAudit wires an audit recorder into the facade. The caller owns the
// recorder's lifecycle and MUST Close it on shutdown to flush buffered decision
// events. Pass an audit.Recorder configured with the desired sample rate,
// buffer, clock, and sink (typically the same model.Storage the facade writes
// to).
func WithAudit(rec *audit.Recorder) Option {
	return func(s *Service) { s.audit = rec }
}

// QueryAudit returns the append-only audit events matching filter, newest-first
// (the shape the E6-S4 audit viewer renders). It is a GATED READ, not a mutation
// — it records nothing and is not itself audited. Authorization: a system-admin
// may read the WHOLE trail; an account-admin may read only events scoped to
// their OWN account, so the query MUST name that account. Any other actor — or an
// account-admin querying with no account or a different account — is denied.
//
// The read comes from storage directly (not the async recorder), so it reflects
// every durably-persisted event regardless of whether decision audit is wired.
func (s *Service) QueryAudit(ctx context.Context, actor Actor, filter model.AuditFilter) ([]model.AuditEvent, error) {
	if err := s.requireMutator(); err != nil {
		return nil, err
	}
	if actor.Principal == "" {
		return nil, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"service: an audit query requires an authenticated principal")
	}
	if err := s.authorizeAuditRead(ctx, actor, filter.Account); err != nil {
		return nil, err
	}
	return s.store.QueryAudit(ctx, filter)
}

// authorizeAuditRead gates a QueryAudit: a system-admin reads everything; failing
// that, an account-admin may read events scoped to their own account (the filter
// must name it). The system-admin denial is returned when neither holds, so the
// caller sees why the broad read was refused.
func (s *Service) authorizeAuditRead(ctx context.Context, actor Actor, account string) error {
	sysErr := s.gate.RequireSystemAdmin(ctx, actor.gateActor())
	if sysErr == nil {
		return nil
	}
	if account != "" {
		if acctErr := s.gate.RequireAccountAdmin(ctx, actor.gateActor(), account); acctErr == nil {
			return nil
		}
	}
	return sysErr
}

// enrichImpersonation stamps the real actor, effective subject, and mode onto ev
// when ctx carries an active impersonation decorator, so EVERY audited event
// made under impersonation shows who REALLY acted (the operator) alongside whose
// authority was borrowed (the target).
func (s *Service) enrichImpersonation(ctx context.Context, ev *model.AuditEvent) {
	if ic, ok := engine.ImpersonationFromContext(ctx); ok && ic.Mode != engine.ModeNone {
		ev.Actor = ic.RealActor
		ev.EffectiveSubject = ic.EffectiveSubject
		ev.ImpersonationMode = string(ic.Mode)
	}
}

// recordMutation records an always-on mutation event synchronously. The outcome
// and reason are derived from err, so an authorization denial or storage fault
// is audited just as a success is. It is a no-op when audit is not wired.
func (s *Service) recordMutation(ctx context.Context, actor Actor, action, target string, err error) {
	if s.audit == nil {
		return
	}
	ev := model.AuditEvent{
		EventType: model.AuditMutation,
		Action:    action,
		Actor:     actor.Principal,
		Account:   actor.Account,
		Target:    target,
		Outcome:   outcomeOf(err),
		Reason:    reasonOf(err),
	}
	s.enrichImpersonation(ctx, &ev)
	_ = s.audit.Record(ctx, ev)
}

// recordDelegation records an always-on delegation event (bestow/revoke). The
// delegator is the real actor.
func (s *Service) recordDelegation(ctx context.Context, delegator, account, action, target string, err error) {
	if s.audit == nil {
		return
	}
	ev := model.AuditEvent{
		EventType: model.AuditDelegation,
		Action:    action,
		Actor:     delegator,
		Account:   account,
		Target:    target,
		Outcome:   outcomeOf(err),
		Reason:    reasonOf(err),
	}
	s.enrichImpersonation(ctx, &ev)
	_ = s.audit.Record(ctx, ev)
}

// recordImpersonation records an always-on impersonation lifecycle event. The
// operator is the real actor and the target is the effective subject, set
// explicitly (an ImpersonationStart establishes the session — there is not yet a
// decorator on ctx to read it from).
func (s *Service) recordImpersonation(ctx context.Context, operator, target, account string, mode engine.Mode, action string, err error) {
	if s.audit == nil {
		return
	}
	ev := model.AuditEvent{
		EventType:         model.AuditImpersonation,
		Action:            action,
		Actor:             operator,
		EffectiveSubject:  target,
		ImpersonationMode: string(mode),
		Account:           account,
		Target:            "principal:" + target,
		Outcome:           outcomeOf(err),
		Reason:            reasonOf(err),
	}
	_ = s.audit.Record(ctx, ev)
}

// recordTemplateApply records a template apply as ONE logical mutation event
// (not one per expanded grant): the template name+version, the target account,
// the resolved parameters, and the ids of the grants the apply expanded to,
// carried in Details. The per-grant writes happen inside the apply's storage
// transaction and emit no separate top-level audit event, so the apply is atomic
// in the audit trail as well as in storage.
func (s *Service) recordTemplateApply(ctx context.Context, actor Actor, app model.TemplateApplication, version int, grantIDs []string, err error) {
	if s.audit == nil {
		return
	}
	details := map[string]any{
		"template": app.Name,
		"version":  version,
	}
	if len(app.Params) > 0 {
		details["params"] = app.Params
	}
	if len(grantIDs) > 0 {
		details["grants"] = grantIDs
		details["grant_count"] = len(grantIDs)
	}
	ev := model.AuditEvent{
		EventType: model.AuditMutation,
		Action:    "ApplyTemplate",
		Actor:     actor.Principal,
		Account:   app.Account,
		Target:    "template:" + app.Name + ":v" + itoa(version),
		Outcome:   outcomeOf(err),
		Reason:    reasonOf(err),
		Details:   details,
	}
	s.enrichImpersonation(ctx, &ev)
	_ = s.audit.Record(ctx, ev)
}

// recordBulk records a bulk grant/revoke as ONE logical mutation event covering
// the whole batch, with the affected target ids in Details.
func (s *Service) recordBulk(ctx context.Context, actor Actor, account, action string, targets []string, err error) {
	if s.audit == nil {
		return
	}
	details := map[string]any{"count": len(targets)}
	if len(targets) > 0 {
		details["targets"] = targets
	}
	ev := model.AuditEvent{
		EventType: model.AuditMutation,
		Action:    action,
		Actor:     actor.Principal,
		Account:   account,
		Target:    "grants:bulk",
		Outcome:   outcomeOf(err),
		Reason:    reasonOf(err),
		Details:   details,
	}
	s.enrichImpersonation(ctx, &ev)
	_ = s.audit.Record(ctx, ev)
}

// recordDecision records a SAMPLED decision-check event asynchronously, off the
// Check critical path. The event is built lazily (only when the sampler keeps
// it) so an un-sampled decision pays nothing but the sampler call. Input-
// validation errors are caller bugs, not decisions, so they are not audited.
func (s *Service) recordDecision(ctx context.Context, action, account, principal, target string, allow bool, reason string, details map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordDecision(ctx, func() model.AuditEvent {
		ev := model.AuditEvent{
			EventType: model.AuditDecision,
			Action:    action,
			Actor:     principal,
			Account:   account,
			Target:    target,
			Outcome:   decisionOutcome(allow),
			Reason:    reason,
			Details:   details,
		}
		s.enrichImpersonation(ctx, &ev)
		return ev
	})
}

func outcomeOf(err error) model.AuditOutcome {
	if err != nil {
		return model.OutcomeFailure
	}
	return model.OutcomeSuccess
}

func decisionOutcome(allow bool) model.AuditOutcome {
	if allow {
		return model.OutcomeAllow
	}
	return model.OutcomeDeny
}

func reasonOf(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
