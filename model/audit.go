package model

import "time"

// AuditEvent is one append-only entry in Aperture's audit trail (FR-25). The
// trail records safety-critical events ALWAYS (every mutation, every
// impersonation event, every delegation) and decision-checks SAMPLED, so the
// security record is complete where it matters without sinking the decision hot
// path.
//
// AuditEvent is a PUBLIC contract: the E6-S4 audit viewer reads exactly this
// shape, so the field set is additive-only — never repurpose or remove a field.
// A record made under impersonation carries BOTH the real actor (who truly
// acted) and the effective subject (whose authority was borrowed), so an
// impersonated action is never mis-attributed to the target alone.
type AuditEvent struct {
	// ID is the entry's unique identifier (assigned by the recorder).
	ID string
	// Timestamp is when the event occurred (the recorder's clock).
	Timestamp time.Time
	// EventType is the broad category the audit query filters on.
	EventType AuditEventType
	// Action is the specific operation, e.g. "PutGrant", "Check", "Bestow",
	// "ImpersonationStart". Free-form within an EventType.
	Action string
	// Actor is the principal that REALLY acted — under impersonation this is the
	// operator (the real actor), never the borrowed target.
	Actor string
	// EffectiveSubject is the target whose authority was used under impersonation.
	// Empty on the ordinary, non-impersonated path.
	EffectiveSubject string
	// ImpersonationMode is "augment" or "become" when the event happened under an
	// impersonation session; empty otherwise.
	ImpersonationMode string
	// Account is the active account the event was scoped to.
	Account string
	// Target is the entity, object, or resource the event concerns (e.g.
	// "grant:g1", an object identity, or a principal id).
	Target string
	// Outcome is the result: allow/deny for a decision, success/failure for a
	// mutation, impersonation, or delegation event.
	Outcome AuditOutcome
	// Reason is a human-readable explanation (the deciding grants, or the failure
	// cause).
	Reason string
	// Details is an optional structured blob backends persist as JSON. Use it for
	// event-specific context (e.g. an enumeration count) the flat columns do not
	// capture.
	Details map[string]any
}

// AuditEventType is the broad category of an audited event. It is the field the
// audit query filters on and the E6-S4 viewer groups by.
type AuditEventType string

const (
	// AuditMutation is a model mutation: entity CRUD or a raw grant write/delete.
	AuditMutation AuditEventType = "mutation"
	// AuditDecision is a decision check (Check/Enumerate/Explain). Decisions are
	// sampled, not always recorded.
	AuditDecision AuditEventType = "decision"
	// AuditImpersonation is an impersonation lifecycle event (start/stop).
	AuditImpersonation AuditEventType = "impersonation"
	// AuditDelegation is a delegation event (bestow/revoke).
	AuditDelegation AuditEventType = "delegation"
)

// AuditOutcome is the result of an audited event.
type AuditOutcome string

const (
	// OutcomeAllow is a decision that permitted.
	OutcomeAllow AuditOutcome = "allow"
	// OutcomeDeny is a decision that denied.
	OutcomeDeny AuditOutcome = "deny"
	// OutcomeSuccess is a mutation/impersonation/delegation that succeeded.
	OutcomeSuccess AuditOutcome = "success"
	// OutcomeFailure is a mutation/impersonation/delegation that failed (a
	// validation, authorization, or storage error).
	OutcomeFailure AuditOutcome = "failure"
)

// AuditFilter is the queryable audit API (FR-25): every field is an optional
// narrowing predicate, ANDed together. A zero AuditFilter matches everything.
// Results are returned newest-first.
type AuditFilter struct {
	// Actor narrows to events whose real actor equals this principal id.
	Actor string
	// Account narrows to events scoped to this account.
	Account string
	// EventType narrows to one category (mutation, decision, ...).
	EventType AuditEventType
	// Outcome narrows to one outcome (allow, deny, success, failure).
	Outcome AuditOutcome
	// Since, when non-zero, is the inclusive lower time bound.
	Since time.Time
	// Until, when non-zero, is the exclusive upper time bound.
	Until time.Time
	// Limit caps the number of returned events; <= 0 means no cap.
	Limit int
}

// matches reports whether ev satisfies every set predicate of f. It is the
// in-memory backend's filter and the reference semantics the SQLite backend's
// SQL mirrors.
func (f AuditFilter) matches(ev AuditEvent) bool {
	if f.Actor != "" && ev.Actor != f.Actor {
		return false
	}
	if f.Account != "" && ev.Account != f.Account {
		return false
	}
	if f.EventType != "" && ev.EventType != f.EventType {
		return false
	}
	if f.Outcome != "" && ev.Outcome != f.Outcome {
		return false
	}
	if !f.Since.IsZero() && ev.Timestamp.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !ev.Timestamp.Before(f.Until) {
		return false
	}
	return true
}

// Matches is the exported form of the filter predicate, so the in-memory backend
// (a separate package) can reuse the canonical matching semantics.
func (f AuditFilter) Matches(ev AuditEvent) bool { return f.matches(ev) }

// RetentionPolicy configures audit pruning (FR-25): a bounded trail by age,
// size, or both. The audit layer computes Before from a configured max-age and
// its clock, keeping the storage method deterministic.
type RetentionPolicy struct {
	// Before, when non-zero, deletes every event strictly older than this instant.
	Before time.Time
	// MaxCount, when > 0, keeps only the newest MaxCount events and deletes the
	// rest. Applied after the age bound.
	MaxCount int
}
