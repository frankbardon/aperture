package postgres

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// ---- The three columns SQL cannot constrain, enforced in Go ----
//
// Eleven relationship columns carry a real foreign key (see the "Referential
// integrity" header in schema.sql). Three do not, and cannot, because the value
// they hold is not always a row reference. This file is where those three are
// enforced instead — in the application layer, with the same code and the same
// message shape a foreign key produces, so a caller cannot tell which mechanism
// refused it and every backend refuses identically.
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
// ON DELETE RESTRICT would have given for free.
//
// ATOMICITY. Every check runs inside the same transaction as the statement it
// guards (s.inTx, which flattens into an enclosing Atomic), so a refusal leaves
// nothing written and no check can read a partially applied delete.
//
// # Why this is a copy of storage/sqlite/integrity.go, and what keeps it honest
//
// The REFUSAL WORDING here is not prose: storagetest asserts, per backend, that
// each of these three refusals names the edge that objected
// ("apt_grants.subject_id ... apt_roles", "apt_memberships.account_id",
// "apt_grants.account_id"). A backend that refused with the right code and the
// wrong words would fail the shared contract, which is the point — a caller must
// not be able to tell which backend, or which mechanism, said no.
//
// So this file duplicates storage/sqlite/integrity.go's format strings verbatim,
// and the obvious objection is the right one: that is a third hand-written copy
// of the same sentences (storage/memory has the fourth), and nothing yet stopped
// them drifting. Lifting the wording into one shared package would be the better
// answer and is NOT what this story does, for a reason that is about scope
// rather than taste: doing it means editing storage/sqlite and storage/memory,
// which this story is explicitly forbidden to touch, and a "shared" package with
// exactly one caller is worse than the duplication it claims to fix.
//
// What lands instead is the parity gate that never existed:
// integrity_parity_test.go parses BOTH files with go/ast and diffs the refusal
// format strings function by function, in the house style (parse the source,
// never grep). The duplication stays; the drift does not. When a later story is
// free to touch all three backends, that gate is also the thing that makes
// collapsing them into one helper a safe, mechanical change.

// constraint renders an application-layer referential refusal with the code and
// shape wrapStorage produces for a foreign-key violation:
// APERTURE_STORAGE_CONSTRAINT, message prefixed by the operation name. The
// detail names the edge that objected, in the same "<child column> references
// <parent>(<column>)" form the schema uses, so a reader can find it.
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
// on every run — storage/memory reports the lowest-sorted child too. The
// ordering carries COLLATE "C" for the same reason every other ORDER BY in this
// backend does: "lowest-sorted" has to mean the byte order SQLite uses, not the
// database's locale.
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
//
// The table name returned is UNQUALIFIED: it is both the message's word for the
// edge and half of a table identifier, and only the identifier gets the schema
// qualifier. A qualified name in the refusal would make the message vary with
// deployment configuration, and storagetest reads it.
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
func (s *Store) checkAccountRef(ctx context.Context, exec sqlExec, op, column, accountID string) error {
	// The wildcard is a sentinel, not a reference. This line is what keeps the
	// wildcard grant and the wildcard membership working.
	if accountID == model.AccountWildcard {
		return nil
	}
	ok, err := rowExists(ctx, exec, s.q(`SELECT 1 FROM apt_schema.apt_accounts WHERE id = $1`), accountID)
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
func (s *Store) checkGrantSubject(ctx context.Context, exec sqlExec, op string, sub model.Subject) error {
	table, noun, ok := subjectTable(sub.Kind)
	if !ok {
		// Unreachable: ValidateGrant rejects any other kind before a write starts.
		return nil
	}
	found, err := rowExists(ctx, exec,
		s.q(`SELECT 1 FROM apt_schema.`+table+` WHERE id = $1`), sub.ID)
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
func (s *Store) checkAccountUncited(ctx context.Context, exec sqlExec, op, accountID string) error {
	principalID, ok, err := firstID(ctx, exec, s.q(
		`SELECT principal_id FROM apt_schema.apt_memberships WHERE account_id = $1 ORDER BY principal_id COLLATE "C" LIMIT 1`), accountID)
	if err != nil {
		return err
	}
	if ok {
		return constraint(op,
			"apt_memberships.account_id references apt_accounts(id): principal %q is still a member of account %q",
			principalID, accountID)
	}
	grantID, ok, err := firstID(ctx, exec, s.q(
		`SELECT id FROM apt_schema.apt_grants WHERE account_id = $1 ORDER BY id COLLATE "C" LIMIT 1`), accountID)
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
func (s *Store) checkSubjectUncited(ctx context.Context, exec sqlExec, op string, kind model.SubjectKind, id string) error {
	table, noun, ok := subjectTable(kind)
	if !ok {
		return nil
	}
	grantID, found, err := firstID(ctx, exec, s.q(
		`SELECT id FROM apt_schema.apt_grants WHERE subject_kind = $1 AND subject_id = $2 ORDER BY id COLLATE "C" LIMIT 1`),
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
