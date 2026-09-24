package sqlite

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// ---- The two sentinel-carrying columns, enforced in Go ----
//
// Eleven relationship columns carry a real foreign key (see the "Referential
// integrity" header in schema.sql). Three do not, and cannot, because the value
// they hold is not always a row reference. This file is where those three are
// enforced instead — in the application layer, with the same code and the same
// message shape a foreign key produces, so a caller cannot tell which mechanism
// refused it and storage/memory can mirror it exactly.
//
// CHECK 1 — apt_memberships.account_id and apt_grants.account_id.
// Both legitimately hold model.AccountWildcard ("*"): a grant stamped "*"
// applies in EVERY account, and a membership stamped "*" enrolls its principal
// in every account (engine.requireMembership falls back to IsMember(p, "*")
// before denying). ValidateAccount REFUSES to create "*" as an account row, so
// the parent a foreign key would reference is forbidden by the model, and SQL
// has no conditional foreign key — in SQLite or in Postgres. The rule is
// therefore: account_id must name an apt_accounts row OR be exactly
// model.AccountWildcard. A naive existence check would refuse "*" and break both
// shipped features; that is the trap, and checkAccountRef's first line is the
// whole of the defence.
//
// CHECK 2 — apt_grants.(subject_kind, subject_id).
// The pair is POLYMORPHIC: subject_kind picks which table subject_id must exist
// in — apt_principals, apt_roles, or apt_groups. No single-table foreign key
// expresses that, so subjectTable does the dispatch and the check runs against
// exactly one table per grant.
//
// BOTH DIRECTIONS, for both checks. A write naming a parent that does not exist
// is refused, AND a delete that would strand a child is refused — the half
// ON DELETE RESTRICT would have given for free. Without the delete side,
// DeleteAccount still orphans memberships and grants and DeletePrincipal still
// orphans the grants that name it.
//
// ATOMICITY. Every check runs inside the same transaction as the statement it
// guards (s.inTx, which flattens into an enclosing Atomic), so a refusal leaves
// nothing written and no check can read a partially applied delete.
//
// NO SCHEMA CHANGE. None of this touches apt_grants or
// idx_apt_grants_account_subject (account_id, subject_kind, subject_id), which
// the engine's hot-path GrantsForSubjects query rides on.

// constraint renders an application-layer referential refusal with the code and
// shape wrapStorage produces for a foreign-key violation: APERTURE_STORAGE_CONSTRAINT,
// message prefixed by the operation name. The detail names the edge that
// objected, in the same "<child column> references <parent>(<column>)" form the
// schema uses, so a reader can find it.
func constraint(op, format string, args ...any) error {
	return aerr.Newf(aerr.APERTURE_STORAGE_CONSTRAINT, op+": "+format, args...)
}

// rowExists reports whether a single-column existence query matched.
func rowExists(ctx context.Context, exec sqlExec, query string, args ...any) (bool, error) {
	var one int
	switch err := exec.QueryRowContext(ctx, query, args...).Scan(&one); {
	case isNoRows(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// firstID returns the lowest-sorted id a child query matches, if any. The query
// must ORDER BY the selected column so a refusal message names the same offender
// on every run — storage/memory reports the lowest-sorted child too.
func firstID(ctx context.Context, exec sqlExec, query string, args ...any) (string, bool, error) {
	var id string
	switch err := exec.QueryRowContext(ctx, query, args...).Scan(&id); {
	case isNoRows(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return id, true, nil
}

// subjectTable maps a grant's subject_kind onto the table subject_id must exist
// in, plus the human noun for the message. ok is false only for a kind
// ValidateGrant already refused, which cannot reach a write.
func subjectTable(kind model.SubjectKind) (table, noun string, ok bool) {
	switch kind {
	case model.SubjectPrincipal:
		return "apt_principals", "principal", true
	case model.SubjectRole:
		return "apt_roles", "role", true
	case model.SubjectGroup:
		return "apt_groups", "group", true
	}
	return "", "", false
}

// -- write side --

// checkAccountRef is CHECK 1's write half: account_id must name an apt_accounts
// row or be exactly model.AccountWildcard. column is the qualified column name
// for the message ("apt_grants.account_id").
func checkAccountRef(ctx context.Context, exec sqlExec, op, column, accountID string) error {
	// The wildcard is a sentinel, not a reference. This line is what keeps the
	// wildcard grant and the wildcard membership working.
	if accountID == model.AccountWildcard {
		return nil
	}
	ok, err := rowExists(ctx, exec, `SELECT 1 FROM apt_accounts WHERE id = ?`, accountID)
	if err != nil {
		return err
	}
	if !ok {
		return constraint(op,
			"%s references apt_accounts(id): account %q does not exist",
			column, accountID)
	}
	return nil
}

// checkGrantSubject is CHECK 2's write half: subject_id must exist in the table
// subject_kind selects.
func checkGrantSubject(ctx context.Context, exec sqlExec, op string, sub model.Subject) error {
	table, noun, ok := subjectTable(sub.Kind)
	if !ok {
		// Unreachable: ValidateGrant rejects any other kind before a write starts.
		return nil
	}
	found, err := rowExists(ctx, exec, `SELECT 1 FROM `+table+` WHERE id = ?`, sub.ID)
	if err != nil {
		return err
	}
	if !found {
		return constraint(op,
			"apt_grants.subject_id references %s(id) when subject_kind is %q: %s %q does not exist",
			table, string(sub.Kind), noun, sub.ID)
	}
	return nil
}

// -- delete side --

// checkAccountUncited is CHECK 1's delete half: an account may not be deleted
// while a membership or a grant is still stamped with it. This is what
// ON DELETE RESTRICT would have given for free on the two columns that cannot
// carry a foreign key.
func checkAccountUncited(ctx context.Context, exec sqlExec, op, accountID string) error {
	principalID, ok, err := firstID(ctx, exec,
		`SELECT principal_id FROM apt_memberships WHERE account_id = ? ORDER BY principal_id LIMIT 1`, accountID)
	if err != nil {
		return err
	}
	if ok {
		return constraint(op,
			"apt_memberships.account_id references apt_accounts(id): principal %q is still a member of account %q",
			principalID, accountID)
	}
	grantID, ok, err := firstID(ctx, exec,
		`SELECT id FROM apt_grants WHERE account_id = ? ORDER BY id LIMIT 1`, accountID)
	if err != nil {
		return err
	}
	if ok {
		return constraint(op,
			"apt_grants.account_id references apt_accounts(id): grant %q is still stamped with account %q",
			grantID, accountID)
	}
	return nil
}

// checkSubjectUncited is CHECK 2's delete half: a principal, role, or group may
// not be deleted while a grant still names it as its subject. A grant is
// authority, and authority naming a subject that no longer exists is authority
// nobody can read or revoke — the same reason apt_grants.permission_id is
// RESTRICT in the schema.
func checkSubjectUncited(ctx context.Context, exec sqlExec, op string, kind model.SubjectKind, id string) error {
	table, noun, ok := subjectTable(kind)
	if !ok {
		return nil
	}
	grantID, found, err := firstID(ctx, exec,
		`SELECT id FROM apt_grants WHERE subject_kind = ? AND subject_id = ? ORDER BY id LIMIT 1`,
		string(kind), id)
	if err != nil {
		return err
	}
	if found {
		return constraint(op,
			"apt_grants.subject_id references %s(id) when subject_kind is %q: grant %q still names %s %q",
			table, string(kind), grantID, noun, id)
	}
	return nil
}
