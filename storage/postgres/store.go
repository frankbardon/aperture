package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/storagetime"
)

// This file is the Postgres statement set: every method of model.Storage. It is
// deliberately a LINE-BY-LINE TWIN of storage/sqlite/sqlite.go rather than an
// idiomatic rewrite. CI cannot run this backend (no service containers), so the
// only cheap defence against the two backends drifting apart is that a reader
// can hold the two files side by side. Every divergence below is a dialect
// requirement, and every one of them is written down where it occurs.
//
// The four dialect divergences, in full:
//
//  1. PLACEHOLDERS are $1..$n rather than ?. Postgres has no positional-?
//     syntax, so the numbering is written out; where a predicate is assembled at
//     run time (ListGrantsPage, GrantsForSubjects, QueryAudit) the number is
//     carried in a counter beside the argument slice, so the two can never
//     disagree.
//
//  2. UPSERTS are ON CONFLICT ... DO UPDATE everywhere, including on the four
//     leaf tables SQLite still writes with INSERT OR REPLACE. REPLACE is
//     delete-then-insert, which fires the children's ON DELETE actions — the bug
//     that once wiped a group's members on re-save — and Postgres has no such
//     statement to port anyway. Using one form for all nine upserts removes the
//     "is this table a leaf?" judgement from every future edit.
//
//  3. TEXT ORDERING is pinned with COLLATE "C". This is the one divergence that
//     is invisible until it bites. SQLite compares TEXT byte-wise (BINARY);
//     Postgres compares it in the DATABASE's collation, which on a stock Linux
//     cluster is a glibc locale like en_US.UTF-8 that weights punctuation below
//     alphanumerics on the first pass. Under that collation 'g1' sorts BEFORE
//     'g-star' and under SQLite's it sorts after — so two backends the
//     conformance suite asserts are identical would return list pages in
//     different orders, on ids that Aperture's own fixtures already contain.
//     COLLATE "C" is byte-wise, which is exactly SQLite's rule, so every
//     ORDER BY over a TEXT column carries it. Integer columns (occurred_at, seq,
//     version) never do: their ordering is arithmetic and needs no collation.
//     Note that this is not reproducible on every server — macOS's libc and ICU
//     both order these two ids the way C does — which is precisely why it is
//     pinned in the statements rather than discovered in production.
//
//  4. TRANSACTIONS are begun with an EXPLICIT sql.LevelReadCommitted rather than
//     BeginTx(ctx, nil). nil takes the server's default_transaction_isolation,
//     which a host is free to set cluster-wide; a store that silently ran at
//     REPEATABLE READ would read its own integrity checks against a stale
//     snapshot. Setup pins the same level for a stronger version of the same
//     reason, measured in E4-S2 (see its doc comment).
//
// The schema qualifier: every table identifier below is written
// apt_schema.apt_<thing>, the same seam schema.sql uses, and is substituted
// through s.q before the statement is executed. Writing the statements bare
// would work today — the qualifier is empty until E4-S4 configures it — and
// would leave that story rewriting every statement in this file. schema.sql's
// header says so explicitly: the seam belongs in "every statement in the
// statement set built on top". The substitution is a strings.Replace on a short
// string, once per statement, against a network round trip.

var _ model.Storage = (*Store)(nil)

// q substitutes this Store's schema qualifier into a statement. See the
// package doc for why the qualifier can only ever be textual.
func (s *Store) q(stmt string) string { return qualify(stmt, s.qualifier) }

// ---- timestamp + error helpers ----

// The unit for every stored instant is int64 NANOSECONDS since the Unix epoch,
// UTC, encoded and decoded by storage/storagetime. That package owns the zero
// mapping (time.Time{} <-> 0) and the representable-range rejection, and is the
// only place in the storage layer where a time.Time becomes an integer — never
// call UnixNano here (storage/storagetime/exclusivity_test.go parses this file
// with go/ast and fails the build if you do). Every timestamp column in
// schema.sql is BIGINT carrying that same int64.

// encodeStamps encodes an entity's CreatedAt/UpdatedAt pair for the INSERT.
// An instant outside the storable range comes back as APERTURE_INVALID_INPUT
// from storagetime and is returned before any statement runs, so an
// unrepresentable timestamp is refused rather than written as an overflow.
func encodeStamps(created, updated time.Time) (int64, int64, error) {
	c, err := storagetime.Encode(created)
	if err != nil {
		return 0, 0, err
	}
	u, err := storagetime.Encode(updated)
	if err != nil {
		return 0, 0, err
	}
	return c, u, nil
}

// decodeStamps is the read-side counterpart. Decode cannot fail — every int64 is
// a representable instant — so scanners assign the pair directly.
func decodeStamps(created, updated int64) (time.Time, time.Time) {
	return storagetime.Decode(created), storagetime.Decode(updated)
}

// encodeBool stores a Go bool as the schema's 0/1 BIGINT convention.
// apt_permissions.delegatable is deliberately NOT a Postgres BOOLEAN: a boolean
// column would make the Go scan target differ per backend, which is exactly the
// per-dialect carve-out storagetest forbids. See schema.sql, DIVERGENCE 1.
func encodeBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func notFound(kind, id string) error {
	return aerr.WithContext(aerr.APERTURE_NOT_FOUND,
		kind+" not found",
		map[string]any{"kind": kind, "id": id})
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

type scanner interface {
	Scan(dest ...any) error
}

// ---- Account ----

func (s *Store) PutAccount(ctx context.Context, a model.Account) error {
	if err := model.ValidateAccount(a); err != nil {
		return err
	}
	created, updated, err := encodeStamps(a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return err
	}
	// ON CONFLICT DO UPDATE, never a delete-then-insert: an account is a PARENT
	// row (memberships and grants point at it), and removing the existing row
	// first would fire the children's ON DELETE actions. See the "Referential
	// integrity" note in schema.sql.
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_accounts (id, name, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`),
		a.ID, a.Name, a.Description, created, updated)
	if err != nil {
		return wrapStorage("put account", err)
	}
	return nil
}

func (s *Store) GetAccount(ctx context.Context, id string) (model.Account, error) {
	row := s.exec.QueryRowContext(ctx, s.q(
		`SELECT id, name, description, created_at, updated_at FROM apt_schema.apt_accounts WHERE id = $1`), id)
	a, err := scanAccount(row)
	if isNoRows(err) {
		return model.Account{}, notFound("account", id)
	}
	return a, err
}

func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(
		`SELECT id, name, description, created_at, updated_at FROM apt_schema.apt_accounts ORDER BY id COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list accounts", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("list accounts", err)
	}
	return out, nil
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete account", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM apt_schema.apt_accounts WHERE id = $1`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("account", id)
		}
		// apt_memberships.account_id and apt_grants.account_id would be
		// ON DELETE RESTRICT if they could be foreign keys at all. They cannot —
		// both carry model.AccountWildcard — so the RESTRICT half is enforced
		// here instead, inside the same transaction: refusing rolls the delete
		// back, so an account with live children is never even briefly gone.
		return s.checkAccountUncited(ctx, tx, "delete account", id)
	})
}

func scanAccount(sc scanner) (model.Account, error) {
	var (
		a                model.Account
		created, updated int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &a.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Account{}, err
		}
		return model.Account{}, wrapStorage("scan account", err)
	}
	a.CreatedAt, a.UpdatedAt = decodeStamps(created, updated)
	return a, nil
}

// ---- Membership ----

func (s *Store) PutMembership(ctx context.Context, m model.Membership) error {
	if err := model.ValidateMembership(m); err != nil {
		return err
	}
	created, updated, err := encodeStamps(m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return err
	}
	// The principal_id edge is a real foreign key and fires on the INSERT below.
	// account_id is not and cannot be one — a membership stamped "*" enrolls its
	// principal in every account — so it is checked here, in the same transaction
	// as the write. See integrity.go.
	return s.inTx(ctx, "put membership", func(tx sqlExec) error {
		if err := s.checkAccountRef(ctx, tx, "put membership", "apt_memberships.account_id", m.AccountID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO apt_schema.apt_memberships (principal_id, account_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (principal_id, account_id) DO UPDATE SET
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`),
			m.PrincipalID, m.AccountID, created, updated)
		return err
	})
}

func (s *Store) GetMembership(ctx context.Context, principalID, accountID string) (model.Membership, error) {
	row := s.exec.QueryRowContext(ctx,
		s.q(membershipSelect+` WHERE principal_id = $1 AND account_id = $2`), principalID, accountID)
	m, err := scanMembership(row)
	if isNoRows(err) {
		return model.Membership{}, notFound("membership", principalID+"@"+accountID)
	}
	return m, err
}

func (s *Store) DeleteMembership(ctx context.Context, principalID, accountID string) error {
	res, err := s.exec.ExecContext(ctx, s.q(
		`DELETE FROM apt_schema.apt_memberships WHERE principal_id = $1 AND account_id = $2`), principalID, accountID)
	if err != nil {
		return wrapStorage("delete membership", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("membership", principalID+"@"+accountID)
	}
	return nil
}

func (s *Store) MembershipsForPrincipal(ctx context.Context, principalID string) ([]model.Membership, error) {
	rows, err := s.exec.QueryContext(ctx,
		s.q(membershipSelect+` WHERE principal_id = $1 ORDER BY account_id COLLATE "C"`), principalID)
	if err != nil {
		return nil, wrapStorage("memberships for principal", err)
	}
	return collectMemberships(rows)
}

func (s *Store) MembershipsForAccount(ctx context.Context, accountID string) ([]model.Membership, error) {
	rows, err := s.exec.QueryContext(ctx,
		s.q(membershipSelect+` WHERE account_id = $1 ORDER BY principal_id COLLATE "C"`), accountID)
	if err != nil {
		return nil, wrapStorage("memberships for account", err)
	}
	return collectMemberships(rows)
}

func (s *Store) IsMember(ctx context.Context, principalID, accountID string) (bool, error) {
	row := s.exec.QueryRowContext(ctx, s.q(
		`SELECT 1 FROM apt_schema.apt_memberships WHERE principal_id = $1 AND account_id = $2`), principalID, accountID)
	var one int
	switch err := row.Scan(&one); {
	case isNoRows(err):
		return false, nil
	case err != nil:
		return false, wrapStorage("is member", err)
	default:
		return true, nil
	}
}

const membershipSelect = `SELECT principal_id, account_id, created_at, updated_at FROM apt_schema.apt_memberships`

func collectMemberships(rows *sql.Rows) ([]model.Membership, error) {
	defer func() { _ = rows.Close() }()
	out := make([]model.Membership, 0)
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan memberships", err)
	}
	return out, nil
}

func scanMembership(sc scanner) (model.Membership, error) {
	var (
		m                model.Membership
		created, updated int64
	)
	if err := sc.Scan(&m.PrincipalID, &m.AccountID, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Membership{}, err
		}
		return model.Membership{}, wrapStorage("scan membership", err)
	}
	m.CreatedAt, m.UpdatedAt = decodeStamps(created, updated)
	return m, nil
}

// ---- ObjectType ----

func (s *Store) PutObjectType(ctx context.Context, ot model.ObjectType) error {
	if err := model.ValidateObjectType(ot); err != nil {
		return err
	}
	actions, err := json.Marshal(ot.Actions)
	if err != nil {
		return wrapStorage("marshal actions", err)
	}
	created, updated, err := encodeStamps(ot.CreatedAt, ot.UpdatedAt)
	if err != nil {
		return err
	}
	// Upsert in place: apt_permissions.object_type points here under RESTRICT, so
	// deleting-and-reinserting the row would refuse any object type that already
	// has permissions. See PutAccount for the full reasoning.
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_object_types (name, apt_actions, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET
			apt_actions = excluded.apt_actions,
			description = excluded.description,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`),
		ot.Name, string(actions), ot.Description, created, updated)
	if err != nil {
		return wrapStorage("put object type", err)
	}
	return nil
}

func (s *Store) GetObjectType(ctx context.Context, name string) (model.ObjectType, error) {
	row := s.exec.QueryRowContext(ctx, s.q(
		`SELECT name, apt_actions, description, created_at, updated_at FROM apt_schema.apt_object_types WHERE name = $1`), name)
	ot, err := scanObjectType(row)
	if isNoRows(err) {
		return model.ObjectType{}, notFound("object type", name)
	}
	return ot, err
}

func (s *Store) ListObjectTypes(ctx context.Context) ([]model.ObjectType, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(
		`SELECT name, apt_actions, description, created_at, updated_at FROM apt_schema.apt_object_types ORDER BY name COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list object types", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.ObjectType, 0)
	for rows.Next() {
		ot, err := scanObjectType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ot)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("list object types", err)
	}
	return out, nil
}

func (s *Store) DeleteObjectType(ctx context.Context, name string) error {
	return s.deleteByID(ctx, "object type", "apt_schema.apt_object_types", "name", name)
}

func scanObjectType(sc scanner) (model.ObjectType, error) {
	var (
		ot               model.ObjectType
		actions          string
		created, updated int64
	)
	if err := sc.Scan(&ot.Name, &actions, &ot.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.ObjectType{}, err
		}
		return model.ObjectType{}, wrapStorage("scan object type", err)
	}
	if err := json.Unmarshal([]byte(actions), &ot.Actions); err != nil {
		return model.ObjectType{}, wrapStorage("unmarshal actions", err)
	}
	ot.CreatedAt, ot.UpdatedAt = decodeStamps(created, updated)
	return ot, nil
}

// ---- Permission ----

func (s *Store) PutPermission(ctx context.Context, p model.Permission) error {
	ot, err := s.GetObjectType(ctx, p.ObjectType)
	if err != nil {
		return err // NOT_FOUND when the object type is unknown
	}
	if err := model.ValidatePermission(p, ot); err != nil {
		return err
	}
	created, updated, err := encodeStamps(p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return err
	}
	// Upsert in place: role assignments and grants point at a permission under
	// RESTRICT. See PutAccount for the full reasoning.
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_permissions (id, object_type, apt_action, scope_strategy, delegatable, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			object_type = excluded.object_type,
			apt_action = excluded.apt_action,
			scope_strategy = excluded.scope_strategy,
			delegatable = excluded.delegatable,
			description = excluded.description,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`),
		p.ID, p.ObjectType, p.Action, p.ScopeStrategy, encodeBool(p.Delegatable), p.Description, created, updated)
	if err != nil {
		return wrapStorage("put permission", err)
	}
	return nil
}

func (s *Store) GetPermission(ctx context.Context, id string) (model.Permission, error) {
	row := s.exec.QueryRowContext(ctx, s.q(permissionSelect+` WHERE id = $1`), id)
	p, err := scanPermission(row)
	if isNoRows(err) {
		return model.Permission{}, notFound("permission", id)
	}
	return p, err
}

func (s *Store) ListPermissions(ctx context.Context) ([]model.Permission, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(permissionSelect+` ORDER BY id COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list permissions", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.Permission, 0)
	for rows.Next() {
		p, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("list permissions", err)
	}
	return out, nil
}

func (s *Store) DeletePermission(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "permission", "apt_schema.apt_permissions", "id", id)
}

const permissionSelect = `SELECT id, object_type, apt_action, scope_strategy, delegatable, description, created_at, updated_at FROM apt_schema.apt_permissions`

func scanPermission(sc scanner) (model.Permission, error) {
	var (
		p                model.Permission
		delegatable      int64
		created, updated int64
	)
	if err := sc.Scan(&p.ID, &p.ObjectType, &p.Action, &p.ScopeStrategy, &delegatable, &p.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Permission{}, err
		}
		return model.Permission{}, wrapStorage("scan permission", err)
	}
	p.Delegatable = delegatable != 0
	p.CreatedAt, p.UpdatedAt = decodeStamps(created, updated)
	return p, nil
}

// ---- Principal ----

func (s *Store) PutPrincipal(ctx context.Context, p model.Principal) error {
	if err := model.ValidatePrincipal(p); err != nil {
		return err
	}
	created, updated, err := encodeStamps(p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return err
	}
	return s.inTx(ctx, "put principal", func(tx sqlExec) error {
		// Upsert in place. Removing the principal row first would CASCADE its role
		// assignments away (harmless — they are rewritten two statements below)
		// but also trip the RESTRICT edges from apt_memberships and
		// apt_group_members, so re-saving a principal who is in an account or a
		// group would be refused. See PutAccount.
		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO apt_schema.apt_principals (id, kind, apt_identity, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET
				kind = excluded.kind,
				apt_identity = excluded.apt_identity,
				display_name = excluded.display_name,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`),
			p.ID, string(p.Kind), p.Identity, p.DisplayName, created, updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(
			`DELETE FROM apt_schema.apt_principal_roles WHERE principal_id = $1`), p.ID); err != nil {
			return err
		}
		for i, roleID := range p.RoleIDs {
			if _, err := tx.ExecContext(ctx, s.q(
				`INSERT INTO apt_schema.apt_principal_roles (principal_id, role_id, seq) VALUES ($1, $2, $3)`),
				p.ID, roleID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetPrincipal(ctx context.Context, id string) (model.Principal, error) {
	row := s.exec.QueryRowContext(ctx, s.q(principalSelect+` WHERE id = $1`), id)
	p, err := scanPrincipal(row)
	if isNoRows(err) {
		return model.Principal{}, notFound("principal", id)
	}
	if err != nil {
		return model.Principal{}, err
	}
	p.RoleIDs, err = s.childIDs(ctx, principalRolesSelect, id)
	if err != nil {
		return model.Principal{}, err
	}
	return p, nil
}

func (s *Store) ListPrincipals(ctx context.Context) ([]model.Principal, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(principalSelect+` ORDER BY id COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list principals", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0)
	out := make([]model.Principal, 0)
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
		ids = append(ids, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("list principals", err)
	}
	for i := range out {
		out[i].RoleIDs, err = s.childIDs(ctx, principalRolesSelect, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DeletePrincipal(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete principal", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM apt_schema.apt_principals WHERE id = $1`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("principal", id)
		}
		// apt_grants.subject_id -> apt_principals(id), for the grants whose
		// subject_kind is "principal". Polymorphic, so no foreign key expresses
		// it; the RESTRICT it would have carried is this check. See integrity.go.
		if err := s.checkSubjectUncited(ctx, tx, "delete principal", model.SubjectPrincipal, id); err != nil {
			return err
		}
		// apt_principal_roles.principal_id is ON DELETE CASCADE, so the role
		// assignments went with the row above. There is no hand-written cleanup
		// here on purpose -- see the note on DeleteGroup.
		return nil
	})
}

const (
	principalSelect      = `SELECT id, kind, apt_identity, display_name, created_at, updated_at FROM apt_schema.apt_principals`
	principalRolesSelect = `SELECT role_id FROM apt_schema.apt_principal_roles WHERE principal_id = $1 ORDER BY seq`
)

func scanPrincipal(sc scanner) (model.Principal, error) {
	var (
		p                model.Principal
		kind             string
		created, updated int64
	)
	if err := sc.Scan(&p.ID, &kind, &p.Identity, &p.DisplayName, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Principal{}, err
		}
		return model.Principal{}, wrapStorage("scan principal", err)
	}
	p.Kind = model.PrincipalKind(kind)
	p.CreatedAt, p.UpdatedAt = decodeStamps(created, updated)
	return p, nil
}

// ---- Role ----

func (s *Store) PutRole(ctx context.Context, r model.Role) error {
	if err := model.ValidateRole(r); err != nil {
		return err
	}
	created, updated, err := encodeStamps(r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return err
	}
	return s.inTx(ctx, "put role", func(tx sqlExec) error {
		// Upsert in place: apt_principal_roles.role_id points here under RESTRICT,
		// so re-saving an assigned role would be refused. See PutAccount.
		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO apt_schema.apt_roles (id, name, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				name = excluded.name,
				description = excluded.description,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`),
			r.ID, r.Name, r.Description, created, updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(
			`DELETE FROM apt_schema.apt_role_permissions WHERE role_id = $1`), r.ID); err != nil {
			return err
		}
		for i, permID := range r.PermissionIDs {
			if _, err := tx.ExecContext(ctx, s.q(
				`INSERT INTO apt_schema.apt_role_permissions (role_id, permission_id, seq) VALUES ($1, $2, $3)`),
				r.ID, permID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetRole(ctx context.Context, id string) (model.Role, error) {
	row := s.exec.QueryRowContext(ctx, s.q(roleSelect+` WHERE id = $1`), id)
	r, err := scanRole(row)
	if isNoRows(err) {
		return model.Role{}, notFound("role", id)
	}
	if err != nil {
		return model.Role{}, err
	}
	r.PermissionIDs, err = s.childIDs(ctx, rolePermissionsSelect, id)
	if err != nil {
		return model.Role{}, err
	}
	return r, nil
}

func (s *Store) ListRoles(ctx context.Context) ([]model.Role, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(roleSelect+` ORDER BY id COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list roles", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.Role, 0)
	ids := make([]string, 0)
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("list roles", err)
	}
	for i := range out {
		out[i].PermissionIDs, err = s.childIDs(ctx, rolePermissionsSelect, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete role", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM apt_schema.apt_roles WHERE id = $1`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("role", id)
		}
		// apt_grants.subject_id -> apt_roles(id), for the grants whose
		// subject_kind is "role". See integrity.go.
		if err := s.checkSubjectUncited(ctx, tx, "delete role", model.SubjectRole, id); err != nil {
			return err
		}
		// apt_role_permissions.role_id is ON DELETE CASCADE, so the permission
		// bundle went with the row above. See the note on DeleteGroup.
		return nil
	})
}

const (
	roleSelect            = `SELECT id, name, description, created_at, updated_at FROM apt_schema.apt_roles`
	rolePermissionsSelect = `SELECT permission_id FROM apt_schema.apt_role_permissions WHERE role_id = $1 ORDER BY seq`
)

func scanRole(sc scanner) (model.Role, error) {
	var (
		r                model.Role
		created, updated int64
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Role{}, err
		}
		return model.Role{}, wrapStorage("scan role", err)
	}
	r.CreatedAt, r.UpdatedAt = decodeStamps(created, updated)
	return r, nil
}

// ---- Group ----

func (s *Store) PutGroup(ctx context.Context, g model.Group) error {
	if err := model.ValidateGroup(g); err != nil {
		return err
	}
	created, updated, err := encodeStamps(g.CreatedAt, g.UpdatedAt)
	if err != nil {
		return err
	}
	return s.inTx(ctx, "put group", func(tx sqlExec) error {
		// Upsert in place: a group owns its member rows under CASCADE, and
		// removing the group row first would delete them out from under the
		// rewrite below on a path that only LOOKS harmless. Consistency with the
		// other four entity upserts is worth more than the coincidence. See
		// PutAccount.
		if _, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO apt_schema.apt_groups (id, name, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				name = excluded.name,
				description = excluded.description,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`),
			g.ID, g.Name, g.Description, created, updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(
			`DELETE FROM apt_schema.apt_group_members WHERE group_id = $1`), g.ID); err != nil {
			return err
		}
		for i, principalID := range g.MemberPrincipalIDs {
			if _, err := tx.ExecContext(ctx, s.q(
				`INSERT INTO apt_schema.apt_group_members (group_id, principal_id, seq) VALUES ($1, $2, $3)`),
				g.ID, principalID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetGroup(ctx context.Context, id string) (model.Group, error) {
	row := s.exec.QueryRowContext(ctx, s.q(groupSelect+` WHERE id = $1`), id)
	g, err := scanGroup(row)
	if isNoRows(err) {
		return model.Group{}, notFound("group", id)
	}
	if err != nil {
		return model.Group{}, err
	}
	g.MemberPrincipalIDs, err = s.childIDs(ctx, groupMembersSelect, id)
	if err != nil {
		return model.Group{}, err
	}
	return g, nil
}

func (s *Store) ListGroups(ctx context.Context) ([]model.Group, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(groupSelect+` ORDER BY id COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list groups", err)
	}
	return s.collectGroups(ctx, rows, "list groups")
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete group", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM apt_schema.apt_groups WHERE id = $1`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("group", id)
		}
		// apt_grants.subject_id -> apt_groups(id), for the grants whose
		// subject_kind is "group". See integrity.go.
		if err := s.checkSubjectUncited(ctx, tx, "delete group", model.SubjectGroup, id); err != nil {
			return err
		}
		// apt_group_members.group_id is ON DELETE CASCADE, so the member rows went
		// with the row above.
		//
		// This used to be a hand-written DELETE alongside the key, and the two
		// covered for each other: breaking EITHER half alone left the conformance
		// suite green, so neither was actually proven. The Go half came out once
		// the schema half was demonstrated to carry the behaviour on its own. If
		// you add the DELETE back you re-create that blind spot -- the cascade is
		// the schema's job, and storagetest's
		// ReferentialCascadeRemovesTheJoinRowsWithTheirOwner is what holds it to it.
		return nil
	})
}

func (s *Store) GroupsForPrincipal(ctx context.Context, principalID string) ([]model.Group, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(`
		SELECT g.id, g.name, g.description, g.created_at, g.updated_at
		FROM apt_schema.apt_groups g
		JOIN apt_schema.apt_group_members m ON m.group_id = g.id
		WHERE m.principal_id = $1
		ORDER BY g.id COLLATE "C"`), principalID)
	if err != nil {
		return nil, wrapStorage("groups for principal", err)
	}
	return s.collectGroups(ctx, rows, "groups for principal")
}

// collectGroups drains a group query and fills each group's member list. The
// member fetch is a second pass rather than a join so the seq ordering the join
// table carries is preserved exactly, the same way sqlite does it.
func (s *Store) collectGroups(ctx context.Context, rows *sql.Rows, op string) ([]model.Group, error) {
	defer func() { _ = rows.Close() }()
	out := make([]model.Group, 0)
	ids := make([]string, 0)
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
		ids = append(ids, g.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage(op, err)
	}
	for i := range out {
		members, err := s.childIDs(ctx, groupMembersSelect, ids[i])
		if err != nil {
			return nil, err
		}
		out[i].MemberPrincipalIDs = members
	}
	return out, nil
}

const (
	groupSelect        = `SELECT id, name, description, created_at, updated_at FROM apt_schema.apt_groups`
	groupMembersSelect = `SELECT principal_id FROM apt_schema.apt_group_members WHERE group_id = $1 ORDER BY seq`
)

func scanGroup(sc scanner) (model.Group, error) {
	var (
		g                model.Group
		created, updated int64
	)
	if err := sc.Scan(&g.ID, &g.Name, &g.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Group{}, err
		}
		return model.Group{}, wrapStorage("scan group", err)
	}
	g.CreatedAt, g.UpdatedAt = decodeStamps(created, updated)
	return g, nil
}

// ---- Grant ----

func (s *Store) PutGrant(ctx context.Context, g model.Grant) error {
	if err := model.ValidateGrant(g); err != nil {
		return err
	}
	created, updated, err := encodeStamps(g.CreatedAt, g.UpdatedAt)
	if err != nil {
		return err
	}
	// A grant makes three references. permission_id is a real foreign key and
	// fires on the INSERT below. The other two cannot be foreign keys and are
	// checked here, in the same transaction: account_id may carry the wildcard,
	// and (subject_kind, subject_id) is polymorphic. See integrity.go.
	return s.inTx(ctx, "put grant", func(tx sqlExec) error {
		if err := s.checkAccountRef(ctx, tx, "put grant", "apt_grants.account_id", g.AccountID); err != nil {
			return err
		}
		if err := s.checkGrantSubject(ctx, tx, "put grant", g.Subject); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO apt_schema.apt_grants (id, account_id, subject_kind, subject_id, permission_id, apt_object, effect, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (id) DO UPDATE SET
				account_id = excluded.account_id,
				subject_kind = excluded.subject_kind,
				subject_id = excluded.subject_id,
				permission_id = excluded.permission_id,
				apt_object = excluded.apt_object,
				effect = excluded.effect,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at`),
			g.ID, g.AccountID, string(g.Subject.Kind), g.Subject.ID, g.PermissionID, g.Object, string(g.Effect),
			created, updated)
		return err
	})
}

func (s *Store) GetGrant(ctx context.Context, id string) (model.Grant, error) {
	row := s.exec.QueryRowContext(ctx, s.q(grantSelect+` WHERE id = $1`), id)
	g, err := scanGrant(row)
	if isNoRows(err) {
		return model.Grant{}, notFound("grant", id)
	}
	return g, err
}

func (s *Store) ListGrants(ctx context.Context, accountID string) ([]model.Grant, error) {
	rows, err := s.exec.QueryContext(ctx,
		s.q(grantSelect+` WHERE account_id = $1 ORDER BY id COLLATE "C"`), accountID)
	if err != nil {
		return nil, wrapStorage("list grants", err)
	}
	return collectGrants(rows)
}

func (s *Store) ListGrantsPage(ctx context.Context, accountID string, offset, limit int) ([]model.Grant, int, error) {
	offset, limit = model.ClampGrantPage(offset, limit)
	allAccounts := accountID == model.AllAccounts

	// The count and the page share the same predicate so the total is the exact
	// pre-pagination match count for the page that follows it. AllAccounts drops
	// the WHERE clause entirely (wildcard "*" rows are counted/returned inline as
	// the ordinary rows they are); otherwise both are scoped to the one account.
	//
	// The placeholder numbers differ between the two statements — the page's
	// LIMIT/OFFSET follow the optional account_id — so they are built separately
	// rather than sharing one `where` string as sqlite's ? placeholders allow.
	var (
		countQuery = `SELECT COUNT(*) FROM apt_schema.apt_grants`
		pageQuery  = grantSelect
		countArgs  []any
		pageArgs   []any
	)
	if allAccounts {
		pageQuery += ` ORDER BY account_id COLLATE "C", id COLLATE "C" LIMIT $1 OFFSET $2`
		pageArgs = append(pageArgs, limit, offset)
	} else {
		countQuery += ` WHERE account_id = $1`
		pageQuery += ` WHERE account_id = $1 ORDER BY account_id COLLATE "C", id COLLATE "C" LIMIT $2 OFFSET $3`
		countArgs = append(countArgs, accountID)
		pageArgs = append(pageArgs, accountID, limit, offset)
	}

	var total int
	if err := s.exec.QueryRowContext(ctx, s.q(countQuery), countArgs...).Scan(&total); err != nil {
		return nil, 0, wrapStorage("count grants", err)
	}

	// Deterministic ordering (account_id, id) so pages are stable and line up
	// with the in-memory backend's ordering.
	rows, err := s.exec.QueryContext(ctx, s.q(pageQuery), pageArgs...)
	if err != nil {
		return nil, 0, wrapStorage("list grants page", err)
	}
	page, err := collectGrants(rows)
	if err != nil {
		return nil, 0, err
	}
	return page, total, nil
}

func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "grant", "apt_schema.apt_grants", "id", id)
}

func (s *Store) GrantsForSubjects(ctx context.Context, accountID string, subjects []model.Subject) ([]model.Grant, error) {
	if len(subjects) == 0 {
		return []model.Grant{}, nil
	}
	// Build a parameterized predicate: account_id = $1 AND (subject matches any).
	// The placeholder number is carried in n alongside the argument slice, so the
	// two cannot drift apart as the subject list grows.
	var b strings.Builder
	b.WriteString(grantSelect)
	// Match grants stamped to the active account OR to the all-accounts wildcard;
	// the wildcard is the one grant that crosses the account boundary.
	b.WriteString(` WHERE (account_id = $1 OR account_id = $2) AND (`)
	args := make([]any, 0, 2+2*len(subjects))
	args = append(args, accountID, model.AccountWildcard)
	n := 2
	for i, sub := range subjects {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString("(subject_kind = $" + strconv.Itoa(n+1) + " AND subject_id = $" + strconv.Itoa(n+2) + ")")
		args = append(args, string(sub.Kind), sub.ID)
		n += 2
	}
	b.WriteString(`) ORDER BY id COLLATE "C"`)
	rows, err := s.exec.QueryContext(ctx, s.q(b.String()), args...)
	if err != nil {
		return nil, wrapStorage("grants for subjects", err)
	}
	return collectGrants(rows)
}

const grantSelect = `SELECT id, account_id, subject_kind, subject_id, permission_id, apt_object, effect, created_at, updated_at FROM apt_schema.apt_grants`

func collectGrants(rows *sql.Rows) ([]model.Grant, error) {
	defer func() { _ = rows.Close() }()
	out := make([]model.Grant, 0)
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan grants", err)
	}
	return out, nil
}

func scanGrant(sc scanner) (model.Grant, error) {
	var (
		g                       model.Grant
		kind, effect            string
		created, updated        int64
		subjectID, permissionID string
	)
	if err := sc.Scan(&g.ID, &g.AccountID, &kind, &subjectID, &permissionID, &g.Object, &effect, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Grant{}, err
		}
		return model.Grant{}, wrapStorage("scan grant", err)
	}
	g.Subject = model.Subject{Kind: model.SubjectKind(kind), ID: subjectID}
	g.PermissionID = permissionID
	g.Effect = model.Effect(effect)
	g.CreatedAt, g.UpdatedAt = decodeStamps(created, updated)
	return g, nil
}

// ---- Template (named, versioned) ----

const templateSelect = `SELECT name, version, description, params, apt_grants, created_at, updated_at FROM apt_schema.apt_templates`

func (s *Store) PutTemplate(ctx context.Context, t model.Template) error {
	if err := model.ValidateTemplate(t); err != nil {
		return err
	}
	params, err := json.Marshal(t.Params)
	if err != nil {
		return wrapStorage("marshal template params", err)
	}
	grants, err := json.Marshal(t.Grants)
	if err != nil {
		return wrapStorage("marshal template grants", err)
	}
	created, updated, err := encodeStamps(t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_templates (name, version, description, params, apt_grants, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name, version) DO UPDATE SET
			description = excluded.description,
			params = excluded.params,
			apt_grants = excluded.apt_grants,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`),
		t.Name, t.Version, t.Description, string(params), string(grants),
		created, updated)
	if err != nil {
		return wrapStorage("put template", err)
	}
	return nil
}

func (s *Store) GetTemplate(ctx context.Context, name string, version int) (model.Template, error) {
	if version > 0 {
		row := s.exec.QueryRowContext(ctx, s.q(templateSelect+` WHERE name = $1 AND version = $2`), name, version)
		t, err := scanTemplate(row)
		if isNoRows(err) {
			return model.Template{}, notFound("template", name+":v"+itoa(version))
		}
		return t, err
	}
	// version <= 0: latest (highest) version of name. version is a BIGINT, so the
	// ordering is arithmetic and carries no collation.
	row := s.exec.QueryRowContext(ctx, s.q(templateSelect+` WHERE name = $1 ORDER BY version DESC LIMIT 1`), name)
	t, err := scanTemplate(row)
	if isNoRows(err) {
		return model.Template{}, notFound("template", name)
	}
	return t, err
}

func (s *Store) ListTemplates(ctx context.Context) ([]model.Template, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(templateSelect+` ORDER BY name COLLATE "C", version`))
	if err != nil {
		return nil, wrapStorage("list templates", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.Template, 0)
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan templates", err)
	}
	return out, nil
}

func (s *Store) DeleteTemplate(ctx context.Context, name string, version int) error {
	if version > 0 {
		res, err := s.exec.ExecContext(ctx, s.q(
			`DELETE FROM apt_schema.apt_templates WHERE name = $1 AND version = $2`), name, version)
		if err != nil {
			return wrapStorage("delete template", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("template", name+":v"+itoa(version))
		}
		return nil
	}
	res, err := s.exec.ExecContext(ctx, s.q(`DELETE FROM apt_schema.apt_templates WHERE name = $1`), name)
	if err != nil {
		return wrapStorage("delete template", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("template", name)
	}
	return nil
}

func scanTemplate(sc scanner) (model.Template, error) {
	var (
		t                model.Template
		params, grants   string
		created, updated int64
	)
	if err := sc.Scan(&t.Name, &t.Version, &t.Description, &params, &grants, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Template{}, err
		}
		return model.Template{}, wrapStorage("scan template", err)
	}
	if params != "" {
		if err := json.Unmarshal([]byte(params), &t.Params); err != nil {
			return model.Template{}, wrapStorage("unmarshal template params", err)
		}
	}
	if grants != "" {
		if err := json.Unmarshal([]byte(grants), &t.Grants); err != nil {
			return model.Template{}, wrapStorage("unmarshal template grants", err)
		}
	}
	t.CreatedAt, t.UpdatedAt = decodeStamps(created, updated)
	return t, nil
}

// itoa is strconv.Itoa, kept local for the not-found message helpers.
func itoa(n int) string { return strconv.Itoa(n) }

// ---- Rule (named) ----

const ruleSelect = `SELECT name, description, ast, created_at, updated_at FROM apt_schema.apt_rules`

func (s *Store) PutRule(ctx context.Context, r model.Rule) error {
	if err := model.ValidateRule(r); err != nil {
		return err
	}
	created, updated, err := encodeStamps(r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_rules (name, description, ast, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET
			description = excluded.description,
			ast = excluded.ast,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`),
		r.Name, r.Description, string(r.AST), created, updated)
	if err != nil {
		return wrapStorage("put rule", err)
	}
	return nil
}

func (s *Store) GetRule(ctx context.Context, name string) (model.Rule, error) {
	row := s.exec.QueryRowContext(ctx, s.q(ruleSelect+` WHERE name = $1`), name)
	r, err := scanRule2(row)
	if isNoRows(err) {
		return model.Rule{}, notFound("rule", name)
	}
	return r, err
}

func (s *Store) ListRules(ctx context.Context) ([]model.Rule, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(ruleSelect+` ORDER BY name COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list rules", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.Rule, 0)
	for rows.Next() {
		r, err := scanRule2(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan rules", err)
	}
	return out, nil
}

func (s *Store) DeleteRule(ctx context.Context, name string) error {
	return s.deleteByID(ctx, "rule", "apt_schema.apt_rules", "name", name)
}

// scanRule2 scans a rules row. It is named scanRule2 to avoid colliding with the
// RBAC role scanner (scanRole); rules and roles are distinct entities. The name
// matches storage/sqlite's for the same reason it does there.
func scanRule2(sc scanner) (model.Rule, error) {
	var (
		r                model.Rule
		ast              string
		created, updated int64
	)
	if err := sc.Scan(&r.Name, &r.Description, &ast, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Rule{}, err
		}
		return model.Rule{}, wrapStorage("scan rule", err)
	}
	r.AST = json.RawMessage(ast)
	r.CreatedAt, r.UpdatedAt = decodeStamps(created, updated)
	return r, nil
}

// ---- Shared wiring (five tables, one all-or-nothing replace) ----
//
// The write surface is ReplaceWiring and nothing else, for the reason
// model.Storage's doc comment gives: wiring is only meaningful whole. What that
// means HERE is that the four DELETEs and every INSERT run in one transaction, so
// a foreign-key refusal on the last provider entry leaves the tables exactly as
// they were rather than half-replaced.
//
// THE CASCADE IS NOT REPEATED HERE. apt_wiring_provider_references is emptied by
// the DELETE on apt_wiring_providers, through the schema's ON DELETE CASCADE, and
// there is deliberately no DELETE FROM apt_wiring_provider_references anywhere in
// this file. That is the rule the other three cascading edges follow: when a Go
// method repeats the cleanup the schema already performs, the two halves cover
// for each other, breaking either one alone leaves the conformance suite green,
// and neither is ever proven.
//
// The dialect divergences here are the usual ones from this file's header: $n
// placeholders, s.q around every statement, and COLLATE "C" on every TEXT
// ORDER BY. The ORDER BY clauses matter more than they look — the read back of a
// pushed wiring set has to be byte-stable, and an object type or a field name
// ordered by a glibc locale would put the same rows in a different order than
// SQLite does.

const (
	wiringConnectionSelect = `SELECT name, created_at, updated_at FROM apt_schema.apt_wiring_connections`
	wiringProviderSelect   = `SELECT object_type, kind, apt_connection, get_one, get_all, id_column, ttl, max_size, created_at, updated_at FROM apt_schema.apt_wiring_providers`
	wiringReferenceSelect  = `SELECT object_type, field, target_type FROM apt_schema.apt_wiring_provider_references`
	wiringFieldTypeSelect  = `SELECT object_type, field, declared_type, created_at, updated_at FROM apt_schema.apt_wiring_field_types`
	wiringAttributeSelect  = `SELECT subject, kind, apt_connection, get_one, get_all, id_column, ttl, max_size, declared_keys, created_at, updated_at FROM apt_schema.apt_wiring_attribute_providers`
)

// stampPair is one entity's encoded CreatedAt/UpdatedAt pair.
type stampPair struct{ created, updated int64 }

// wiringStamps is the whole set's encoded stamps, section by section.
// ReplaceWiring encodes every one of them BEFORE it opens a transaction, so an
// unrepresentable instant is refused with APERTURE_INVALID_INPUT and nothing is
// written — rather than being discovered part-way through the insert loop, where
// the rollback would still be correct but the error a caller sees would depend on
// which section happened to be written first.
type wiringStamps struct {
	connections []stampPair
	providers   []stampPair
	fieldTypes  []stampPair
	attributes  []stampPair
}

// encodeWiringStamps encodes the whole set's stamps in section order, mirroring
// the order ReplaceWiring writes them.
func encodeWiringStamps(set model.WiringSet) (wiringStamps, error) {
	var out wiringStamps
	encode := func(created, updated time.Time) (stampPair, error) {
		c, u, err := encodeStamps(created, updated)
		return stampPair{c, u}, err
	}
	out.connections = make([]stampPair, len(set.Connections))
	for i, c := range set.Connections {
		p, err := encode(c.CreatedAt, c.UpdatedAt)
		if err != nil {
			return wiringStamps{}, err
		}
		out.connections[i] = p
	}
	out.providers = make([]stampPair, len(set.Providers))
	for i, p := range set.Providers {
		sp, err := encode(p.CreatedAt, p.UpdatedAt)
		if err != nil {
			return wiringStamps{}, err
		}
		out.providers[i] = sp
	}
	out.fieldTypes = make([]stampPair, len(set.FieldTypes))
	for i, ft := range set.FieldTypes {
		p, err := encode(ft.CreatedAt, ft.UpdatedAt)
		if err != nil {
			return wiringStamps{}, err
		}
		out.fieldTypes[i] = p
	}
	out.attributes = make([]stampPair, len(set.AttributeProviders))
	for i, ap := range set.AttributeProviders {
		p, err := encode(ap.CreatedAt, ap.UpdatedAt)
		if err != nil {
			return wiringStamps{}, err
		}
		out.attributes[i] = p
	}
	return out, nil
}

func (s *Store) ReplaceWiring(ctx context.Context, set model.WiringSet) error {
	if err := model.ValidateWiringSet(set); err != nil {
		return err
	}
	stamps, err := encodeWiringStamps(set)
	if err != nil {
		return err
	}
	// declared_keys is rendered by model.DeclaredKeys.Encode rather than spelled
	// out here: "" (not declared) and "[]" (declared empty) are different answers,
	// and a twin pair of encoders that drifted would not produce a visibly wrong
	// row — it would produce a slot that silently stopped being enforced.
	declared := make([]string, len(set.AttributeProviders))
	for i, ap := range set.AttributeProviders {
		raw, err := ap.DeclaredKeys.Encode()
		if err != nil {
			return err
		}
		declared[i] = raw
	}
	return s.inTx(ctx, "replace wiring", func(tx sqlExec) error {
		// The DELETE on apt_wiring_providers takes the reference rows with it
		// through ON DELETE CASCADE. Do not add a fifth DELETE for them.
		for _, table := range []string{
			"apt_schema.apt_wiring_connections",
			"apt_schema.apt_wiring_providers",
			"apt_schema.apt_wiring_field_types",
			"apt_schema.apt_wiring_attribute_providers",
		} {
			if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM `+table)); err != nil {
				return err
			}
		}
		for i, c := range set.Connections {
			if _, err := tx.ExecContext(ctx, s.q(`
				INSERT INTO apt_schema.apt_wiring_connections (name, created_at, updated_at)
				VALUES ($1, $2, $3)`),
				c.Name, stamps.connections[i].created, stamps.connections[i].updated); err != nil {
				return err
			}
		}
		for i, p := range set.Providers {
			if _, err := tx.ExecContext(ctx, s.q(`
				INSERT INTO apt_schema.apt_wiring_providers
					(object_type, kind, apt_connection, get_one, get_all, id_column, ttl, max_size, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`),
				p.ObjectType, p.Kind, p.Connection, p.GetOne, p.GetAll, p.IDColumn, p.TTL, p.MaxSize,
				stamps.providers[i].created, stamps.providers[i].updated); err != nil {
				return err
			}
			for _, r := range p.References {
				if _, err := tx.ExecContext(ctx, s.q(`
					INSERT INTO apt_schema.apt_wiring_provider_references (object_type, field, target_type)
					VALUES ($1, $2, $3)`),
					p.ObjectType, r.Field, r.TargetType); err != nil {
					return err
				}
			}
		}
		for i, ft := range set.FieldTypes {
			if _, err := tx.ExecContext(ctx, s.q(`
				INSERT INTO apt_schema.apt_wiring_field_types (object_type, field, declared_type, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5)`),
				ft.ObjectType, ft.Field, ft.DeclaredType,
				stamps.fieldTypes[i].created, stamps.fieldTypes[i].updated); err != nil {
				return err
			}
		}
		for i, ap := range set.AttributeProviders {
			if _, err := tx.ExecContext(ctx, s.q(`
				INSERT INTO apt_schema.apt_wiring_attribute_providers
					(subject, kind, apt_connection, get_one, get_all, id_column, ttl, max_size, declared_keys, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`),
				ap.Subject, ap.Kind, ap.Connection, ap.GetOne, ap.GetAll, ap.IDColumn, ap.TTL, ap.MaxSize,
				declared[i], stamps.attributes[i].created, stamps.attributes[i].updated); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetWiring reads the four sections inside ONE transaction. Without it a boot
// could read a provider list from before a push and a field-type list from after
// it, and build a registry that never existed in the database at any instant.
func (s *Store) GetWiring(ctx context.Context) (model.WiringSet, error) {
	var set model.WiringSet
	err := s.inTx(ctx, "get wiring", func(tx sqlExec) error {
		var err error
		if set.Connections, err = s.listWiringConnections(ctx, tx); err != nil {
			return err
		}
		if set.Providers, err = s.listWiringProviders(ctx, tx); err != nil {
			return err
		}
		if set.FieldTypes, err = s.listWiringFieldTypes(ctx, tx); err != nil {
			return err
		}
		if set.AttributeProviders, err = s.listWiringAttributeProviders(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return model.WiringSet{}, err
	}
	return set, nil
}

func (s *Store) ListWiringConnections(ctx context.Context) ([]model.WiringConnection, error) {
	return s.listWiringConnections(ctx, s.exec)
}

// listWiringConnections takes the exec explicitly so GetWiring can run it inside
// its own transaction while the List* method runs it against the pool. It is a
// METHOD here where storage/sqlite's is a free function, because the statement
// has to go through s.q for the schema qualifier. Every wiring read below follows
// the same shape for the same reason.
func (s *Store) listWiringConnections(ctx context.Context, exec sqlExec) ([]model.WiringConnection, error) {
	rows, err := exec.QueryContext(ctx, s.q(wiringConnectionSelect+` ORDER BY name COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list wiring connections", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.WiringConnection, 0)
	for rows.Next() {
		c, err := scanWiringConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan wiring connections", err)
	}
	return out, nil
}

func (s *Store) GetWiringConnection(ctx context.Context, name string) (model.WiringConnection, error) {
	row := s.exec.QueryRowContext(ctx, s.q(wiringConnectionSelect+` WHERE name = $1`), name)
	c, err := scanWiringConnection(row)
	if isNoRows(err) {
		return model.WiringConnection{}, notFound("wiring connection", name)
	}
	return c, err
}

func scanWiringConnection(sc scanner) (model.WiringConnection, error) {
	var (
		c                model.WiringConnection
		created, updated int64
	)
	if err := sc.Scan(&c.Name, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.WiringConnection{}, err
		}
		return model.WiringConnection{}, wrapStorage("scan wiring connection", err)
	}
	c.CreatedAt, c.UpdatedAt = decodeStamps(created, updated)
	return c, nil
}

func (s *Store) ListWiringProviders(ctx context.Context) ([]model.WiringProvider, error) {
	return s.listWiringProviders(ctx, s.exec)
}

// listWiringProviders reads the entries and their reference rows with TWO
// queries, not one per entry: the references are grouped in Go by owner. A
// per-entry query would be a round trip per object type on a boot path whose
// whole job is to read everything once.
func (s *Store) listWiringProviders(ctx context.Context, exec sqlExec) ([]model.WiringProvider, error) {
	refs, err := s.wiringReferencesByOwner(ctx, exec, "")
	if err != nil {
		return nil, err
	}
	rows, err := exec.QueryContext(ctx, s.q(wiringProviderSelect+` ORDER BY object_type COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list wiring providers", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.WiringProvider, 0)
	for rows.Next() {
		p, err := scanWiringProvider(rows)
		if err != nil {
			return nil, err
		}
		p.References = refs[p.ObjectType]
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan wiring providers", err)
	}
	return out, nil
}

func (s *Store) GetWiringProvider(ctx context.Context, objectType string) (model.WiringProvider, error) {
	row := s.exec.QueryRowContext(ctx, s.q(wiringProviderSelect+` WHERE object_type = $1`), objectType)
	p, err := scanWiringProvider(row)
	if isNoRows(err) {
		return model.WiringProvider{}, notFound("wiring provider", objectType)
	}
	if err != nil {
		return model.WiringProvider{}, err
	}
	refs, err := s.wiringReferencesByOwner(ctx, s.exec, objectType)
	if err != nil {
		return model.WiringProvider{}, err
	}
	p.References = refs[objectType]
	return p, nil
}

func scanWiringProvider(sc scanner) (model.WiringProvider, error) {
	var (
		p                model.WiringProvider
		created, updated int64
	)
	if err := sc.Scan(&p.ObjectType, &p.Kind, &p.Connection, &p.GetOne, &p.GetAll,
		&p.IDColumn, &p.TTL, &p.MaxSize, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.WiringProvider{}, err
		}
		return model.WiringProvider{}, wrapStorage("scan wiring provider", err)
	}
	p.CreatedAt, p.UpdatedAt = decodeStamps(created, updated)
	return p, nil
}

// wiringReferencesByOwner groups the reference rows by owning object type. An
// empty objectType reads them all. The ORDER BY is what makes a read back
// byte-stable: a references: map has no order of its own, so field order IS the
// canonical one.
func (s *Store) wiringReferencesByOwner(ctx context.Context, exec sqlExec, objectType string) (map[string][]model.WiringReference, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if objectType == "" {
		rows, err = exec.QueryContext(ctx,
			s.q(wiringReferenceSelect+` ORDER BY object_type COLLATE "C", field COLLATE "C"`))
	} else {
		rows, err = exec.QueryContext(ctx,
			s.q(wiringReferenceSelect+` WHERE object_type = $1 ORDER BY field COLLATE "C"`), objectType)
	}
	if err != nil {
		return nil, wrapStorage("list wiring provider references", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]model.WiringReference)
	for rows.Next() {
		var owner string
		var r model.WiringReference
		if err := rows.Scan(&owner, &r.Field, &r.TargetType); err != nil {
			return nil, wrapStorage("scan wiring provider reference", err)
		}
		out[owner] = append(out[owner], r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan wiring provider references", err)
	}
	return out, nil
}

func (s *Store) ListWiringFieldTypes(ctx context.Context) ([]model.WiringFieldType, error) {
	return s.listWiringFieldTypes(ctx, s.exec)
}

func (s *Store) listWiringFieldTypes(ctx context.Context, exec sqlExec) ([]model.WiringFieldType, error) {
	rows, err := exec.QueryContext(ctx,
		s.q(wiringFieldTypeSelect+` ORDER BY object_type COLLATE "C", field COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list wiring field types", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.WiringFieldType, 0)
	for rows.Next() {
		ft, err := scanWiringFieldType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ft)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan wiring field types", err)
	}
	return out, nil
}

func (s *Store) GetWiringFieldType(ctx context.Context, objectType, field string) (model.WiringFieldType, error) {
	row := s.exec.QueryRowContext(ctx,
		s.q(wiringFieldTypeSelect+` WHERE object_type = $1 AND field = $2`), objectType, field)
	ft, err := scanWiringFieldType(row)
	if isNoRows(err) {
		return model.WiringFieldType{}, notFound("wiring field type", objectType+"."+field)
	}
	return ft, err
}

func scanWiringFieldType(sc scanner) (model.WiringFieldType, error) {
	var (
		ft               model.WiringFieldType
		created, updated int64
	)
	if err := sc.Scan(&ft.ObjectType, &ft.Field, &ft.DeclaredType, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.WiringFieldType{}, err
		}
		return model.WiringFieldType{}, wrapStorage("scan wiring field type", err)
	}
	ft.CreatedAt, ft.UpdatedAt = decodeStamps(created, updated)
	return ft, nil
}

func (s *Store) ListWiringAttributeProviders(ctx context.Context) ([]model.WiringAttributeProvider, error) {
	return s.listWiringAttributeProviders(ctx, s.exec)
}

func (s *Store) listWiringAttributeProviders(ctx context.Context, exec sqlExec) ([]model.WiringAttributeProvider, error) {
	rows, err := exec.QueryContext(ctx, s.q(wiringAttributeSelect+` ORDER BY subject COLLATE "C"`))
	if err != nil {
		return nil, wrapStorage("list wiring attribute providers", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.WiringAttributeProvider, 0)
	for rows.Next() {
		ap, err := scanWiringAttributeProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan wiring attribute providers", err)
	}
	return out, nil
}

func (s *Store) GetWiringAttributeProvider(ctx context.Context, subject string) (model.WiringAttributeProvider, error) {
	row := s.exec.QueryRowContext(ctx, s.q(wiringAttributeSelect+` WHERE subject = $1`), subject)
	ap, err := scanWiringAttributeProvider(row)
	if isNoRows(err) {
		return model.WiringAttributeProvider{}, notFound("wiring attribute provider", subject)
	}
	return ap, err
}

func scanWiringAttributeProvider(sc scanner) (model.WiringAttributeProvider, error) {
	var (
		ap               model.WiringAttributeProvider
		declared         string
		created, updated int64
	)
	if err := sc.Scan(&ap.Subject, &ap.Kind, &ap.Connection, &ap.GetOne, &ap.GetAll,
		&ap.IDColumn, &ap.TTL, &ap.MaxSize, &declared, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.WiringAttributeProvider{}, err
		}
		return model.WiringAttributeProvider{}, wrapStorage("scan wiring attribute provider", err)
	}
	keys, err := model.ParseDeclaredKeys(declared)
	if err != nil {
		// Already Aperture-coded by the model, so wrapping it here would re-stamp
		// APERTURE_INVALID_INPUT as APERTURE_STORAGE and bury its fixups.
		return model.WiringAttributeProvider{}, err
	}
	ap.DeclaredKeys = keys
	ap.CreatedAt, ap.UpdatedAt = decodeStamps(created, updated)
	return ap, nil
}

// ---- Audit trail (append-only) ----

const auditColumns = `id, occurred_at, event_type, apt_action, actor, effective_subject, impersonation_mode, account, target, outcome, reason, details`

func (s *Store) AppendAudit(ctx context.Context, ev model.AuditEvent) error {
	details := ""
	if len(ev.Details) > 0 {
		b, err := json.Marshal(ev.Details)
		if err != nil {
			return wrapStorage("marshal audit details", err)
		}
		details = string(b)
	}
	ts, err := storagetime.Encode(ev.Timestamp)
	if err != nil {
		return err
	}
	_, err = s.exec.ExecContext(ctx, s.q(`
		INSERT INTO apt_schema.apt_audit_log (`+auditColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			occurred_at = excluded.occurred_at,
			event_type = excluded.event_type,
			apt_action = excluded.apt_action,
			actor = excluded.actor,
			effective_subject = excluded.effective_subject,
			impersonation_mode = excluded.impersonation_mode,
			account = excluded.account,
			target = excluded.target,
			outcome = excluded.outcome,
			reason = excluded.reason,
			details = excluded.details`),
		ev.ID, ts, string(ev.EventType), ev.Action, ev.Actor,
		ev.EffectiveSubject, ev.ImpersonationMode, ev.Account, ev.Target, string(ev.Outcome),
		ev.Reason, details)
	if err != nil {
		return wrapStorage("append audit", err)
	}
	return nil
}

func (s *Store) QueryAudit(ctx context.Context, filter model.AuditFilter) ([]model.AuditEvent, error) {
	var b strings.Builder
	b.WriteString(`SELECT `)
	b.WriteString(auditColumns)
	b.WriteString(` FROM apt_schema.apt_audit_log`)
	var (
		where []string
		args  []any
	)
	// next returns the placeholder for the argument about to be appended, so the
	// numbering follows the slice rather than a hand-maintained count.
	next := func() string { return "$" + strconv.Itoa(len(args)+1) }
	if filter.Actor != "" {
		where = append(where, "actor = "+next())
		args = append(args, filter.Actor)
	}
	if filter.Account != "" {
		where = append(where, "account = "+next())
		args = append(args, filter.Account)
	}
	if filter.EventType != "" {
		where = append(where, "event_type = "+next())
		args = append(args, string(filter.EventType))
	}
	if filter.Outcome != "" {
		where = append(where, "outcome = "+next())
		args = append(args, string(filter.Outcome))
	}
	if !filter.Since.IsZero() {
		since, err := storagetime.Encode(filter.Since)
		if err != nil {
			return nil, err
		}
		where = append(where, "occurred_at >= "+next())
		args = append(args, since)
	}
	if !filter.Until.IsZero() {
		until, err := storagetime.Encode(filter.Until)
		if err != nil {
			return nil, err
		}
		where = append(where, "occurred_at < "+next())
		args = append(args, until)
	}
	if len(where) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(where, " AND "))
	}
	b.WriteString(` ORDER BY occurred_at DESC, id COLLATE "C" DESC`)
	if filter.Limit > 0 {
		b.WriteString(" LIMIT " + next())
		args = append(args, filter.Limit)
	}
	rows, err := s.exec.QueryContext(ctx, s.q(b.String()), args...)
	if err != nil {
		return nil, wrapStorage("query audit", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]model.AuditEvent, 0)
	for rows.Next() {
		ev, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("scan audit", err)
	}
	return out, nil
}

func (s *Store) PruneAudit(ctx context.Context, policy model.RetentionPolicy) (int, error) {
	var removed int
	// Age bound: delete events strictly older than policy.Before.
	if !policy.Before.IsZero() {
		before, err := storagetime.Encode(policy.Before)
		if err != nil {
			return removed, err
		}
		res, err := s.exec.ExecContext(ctx, s.q(
			`DELETE FROM apt_schema.apt_audit_log WHERE occurred_at < $1`), before)
		if err != nil {
			return removed, wrapStorage("prune audit by age", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}
	// Size bound: keep only the newest MaxCount events. The ordering inside the
	// subquery is the same one QueryAudit returns, so "newest" means the same
	// thing to both.
	if policy.MaxCount > 0 {
		res, err := s.exec.ExecContext(ctx, s.q(`
			DELETE FROM apt_schema.apt_audit_log WHERE id NOT IN (
				SELECT id FROM apt_schema.apt_audit_log ORDER BY occurred_at DESC, id COLLATE "C" DESC LIMIT $1
			)`), policy.MaxCount)
		if err != nil {
			return removed, wrapStorage("prune audit by size", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}
	return removed, nil
}

func scanAudit(sc scanner) (model.AuditEvent, error) {
	var (
		ev                          model.AuditEvent
		occurredAt                  int64
		eventType, outcome, details string
	)
	if err := sc.Scan(&ev.ID, &occurredAt, &eventType, &ev.Action, &ev.Actor,
		&ev.EffectiveSubject, &ev.ImpersonationMode, &ev.Account, &ev.Target, &outcome,
		&ev.Reason, &details); err != nil {
		return model.AuditEvent{}, wrapStorage("scan audit", err)
	}
	ev.Timestamp = storagetime.Decode(occurredAt)
	ev.EventType = model.AuditEventType(eventType)
	ev.Outcome = model.AuditOutcome(outcome)
	if details != "" {
		if err := json.Unmarshal([]byte(details), &ev.Details); err != nil {
			return model.AuditEvent{}, wrapStorage("unmarshal audit details", err)
		}
	}
	return ev, nil
}

// ---- shared helpers ----

// childIDs runs a single-column query returning the ordered list of ids. It
// returns nil (not an empty slice) when there are no rows so round-tripped
// values compare equal to caller-supplied nil/empty slices.
func (s *Store) childIDs(ctx context.Context, query, arg string) ([]string, error) {
	rows, err := s.exec.QueryContext(ctx, s.q(query), arg)
	if err != nil {
		return nil, wrapStorage("query child ids", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrapStorage("scan child id", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStorage("query child ids", err)
	}
	return out, nil
}

// deleteByID deletes one row, returning APERTURE_NOT_FOUND when nothing matched.
// table and col are internal literals, never caller input.
func (s *Store) deleteByID(ctx context.Context, kind, table, col, id string) error {
	res, err := s.exec.ExecContext(ctx, s.q(`DELETE FROM `+table+` WHERE `+col+` = $1`), id)
	if err != nil {
		return wrapStorage("delete "+kind, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(kind, id)
	}
	return nil
}

// txOptions is what every transaction this backend begins runs at. See
// divergence 4 in this file's header: the level is stated rather than inherited
// so a cluster-wide default_transaction_isolation cannot change what Aperture's
// integrity checks read.
var txOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error. A coded error returned by fn (e.g. NOT_FOUND) passes through verbatim;
// raw driver errors are wrapped as APERTURE_STORAGE (or
// APERTURE_STORAGE_CONSTRAINT) under op.
//
// When this Store is already transaction-scoped (pool == nil — it is running
// inside an Atomic), there is no new transaction to begin: fn runs against the
// current exec (the enclosing *sql.Tx) so the multi-statement write joins the
// surrounding transaction and an outer rollback still covers it. That is not
// only correctness: taking a second connection here while the first is held
// would consume the pool a nesting level at a time.
//
// The `if aerr.CodeOf(err) != ""` guard is load-bearing and is NOT something
// aerr.Wrap does for us. Wrap builds a fresh CodedError with whatever code it is
// handed and CodeOf reports the OUTERMOST one, so wrapping without the guard
// buries APERTURE_STORAGE_CONSTRAINT — and the fixups that go with it — under
// APERTURE_STORAGE.
func (s *Store) inTx(ctx context.Context, op string, fn func(tx sqlExec) error) error {
	if s.pool == nil {
		if err := fn(s.exec); err != nil {
			if aerr.CodeOf(err) != "" {
				return err
			}
			return wrapStorage(op, err)
		}
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, txOptions)
	if err != nil {
		return wrapStorage(op, err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		if aerr.CodeOf(err) != "" {
			return err
		}
		return wrapStorage(op, err)
	}
	if err := tx.Commit(); err != nil {
		return wrapStorage(op, err)
	}
	return nil
}

// Atomic runs fn inside a single Postgres transaction against a
// transaction-scoped Store, committing when fn returns nil and rolling the WHOLE
// batch back when fn returns an error — so no write performed inside fn persists
// if any step fails.
//
// A nested Atomic (this Store is already transaction-scoped) FLATTENS into the
// current transaction. That is copied from storage/sqlite deliberately, and it
// matters more here than it does there. SQLite's pool is capped at one
// connection, so a second BeginTx would block immediately and obviously; this
// pool holds twenty, so a non-flattening implementation would appear to work and
// then deadlock in production the moment the nesting depth reached the number of
// free connections — each level holding a connection while waiting for the next.
// Flattening means a nesting of any depth costs exactly one connection, and an
// outer rollback still covers everything.
func (s *Store) Atomic(ctx context.Context, fn func(tx model.Storage) error) error {
	if s.pool == nil {
		// Already inside a transaction: reuse it (flatten).
		return fn(s)
	}
	tx, err := s.pool.BeginTx(ctx, txOptions)
	if err != nil {
		return wrapStorage("atomic", err)
	}
	// The child carries the schema configuration forward: a transaction-scoped
	// Store must address the same tables as the root it came from. Both fields
	// travel, not just the qualifier — a child that inherited the qualifier and
	// not the name would build the right statements and then, if Setup were ever
	// called on it, resolve the wrong schema.
	child := &Store{pool: nil, exec: tx, schema: s.schema, qualifier: s.qualifier}
	if err := fn(child); err != nil {
		_ = tx.Rollback()
		if aerr.CodeOf(err) != "" {
			return err
		}
		return wrapStorage("atomic", err)
	}
	if err := tx.Commit(); err != nil {
		return wrapStorage("atomic", err)
	}
	return nil
}
