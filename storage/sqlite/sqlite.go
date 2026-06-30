// Package sqlite is the SQLite-first reference implementation of model.Storage.
// It uses modernc.org/sqlite — a pure-Go driver, so CGO stays off — with a
// hand-written, embedded schema (schema.sql) and no ORM or migration tool.
//
// The same validation and typed-action rules the in-memory backend enforces are
// enforced here, so the shared conformance suite (storage/storagetest) passes
// against both. Membership (principal→roles, role→permissions, group→members)
// is normalized into join tables; object-type verb sets are stored as a JSON
// value column. Grants carry their account stamp and are indexed for the
// decision engine's account-scoped GrantsForSubjects query.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// sqlExec is the subset of database/sql both *sql.DB and *sql.Tx satisfy. The
// Store runs every query through it so the SAME methods serve un-transacted
// calls (exec is the pool) and calls inside an Atomic transaction (exec is the
// *sql.Tx), without duplicating the query bodies.
type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is a SQLite-backed model.Storage. Construct one with Open (durable file)
// or OpenMemory (ephemeral, for tests). Call Setup once before use.
//
// A Store is either ROOT (pool != nil) — owning the connection pool and able to
// begin transactions — or TRANSACTION-SCOPED (pool == nil) — a transient handle
// whose exec is a *sql.Tx, produced by Atomic and never exposed beyond fn.
type Store struct {
	pool *sql.DB // the connection pool; nil for a transaction-scoped Store
	exec sqlExec // *sql.DB or *sql.Tx — what queries run against
}

var _ model.Storage = (*Store)(nil)

// Open opens (or creates) a SQLite database at dsn and returns a Store. dsn is a
// modernc.org/sqlite data source name, e.g. a file path or
// "file:aperture.db?_pragma=busy_timeout(5000)". The connection pool is capped
// at one connection so writes serialize cleanly under SQLite's single-writer
// model.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "open sqlite database", err)
	}
	// SQLite is a single-writer engine; one connection avoids "database is
	// locked" contention and keeps an in-memory DB on a single underlying handle.
	db.SetMaxOpenConns(1)
	return &Store{pool: db, exec: db}, nil
}

// OpenMemory opens a private in-memory SQLite database. Useful for tests and
// ephemeral seeding. The database lives only as long as the Store.
func OpenMemory() (*Store, error) {
	return Open("file::memory:?cache=shared")
}

// Setup creates the schema. It is idempotent (every statement is IF NOT EXISTS).
func (s *Store) Setup(ctx context.Context) error {
	if _, err := s.exec.ExecContext(ctx, schema); err != nil {
		return aerr.Wrap(aerr.APERTURE_STORAGE, "apply schema", err)
	}
	return nil
}

// Close releases the underlying database handle. It is a no-op on a
// transaction-scoped Store (which does not own the pool).
func (s *Store) Close() error {
	if s.pool == nil {
		return nil
	}
	if err := s.pool.Close(); err != nil {
		return aerr.Wrap(aerr.APERTURE_STORAGE, "close sqlite database", err)
	}
	return nil
}

// ---- timestamp + error helpers ----

// encodeTime renders a time as RFC3339Nano, or "" for the zero value so zero
// round-trips to zero rather than the year-0001 sentinel.
func encodeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, aerr.Wrap(aerr.APERTURE_STORAGE, "decode timestamp", err)
	}
	return t.UTC(), nil
}

// encodeBool stores a Go bool as SQLite's integer 0/1 convention.
func encodeBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func wrapStorage(op string, err error) error {
	return aerr.Wrap(aerr.APERTURE_STORAGE, op, err)
}

func notFound(kind, id string) error {
	return aerr.WithContext(aerr.APERTURE_NOT_FOUND,
		kind+" not found",
		map[string]any{"kind": kind, "id": id})
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ---- Account ----

func (s *Store) PutAccount(ctx context.Context, a model.Account) error {
	if err := model.ValidateAccount(a); err != nil {
		return err
	}
	_, err := s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO accounts (id, name, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Description, encodeTime(a.CreatedAt), encodeTime(a.UpdatedAt))
	if err != nil {
		return wrapStorage("put account", err)
	}
	return nil
}

func (s *Store) GetAccount(ctx context.Context, id string) (model.Account, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if isNoRows(err) {
		return model.Account{}, notFound("account", id)
	}
	return a, err
}

func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, wrapStorage("list accounts", err)
	}
	defer rows.Close()
	out := make([]model.Account, 0)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "account", "accounts", "id", id)
}

func scanAccount(sc scanner) (model.Account, error) {
	var (
		a                model.Account
		created, updated string
	)
	if err := sc.Scan(&a.ID, &a.Name, &a.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Account{}, err
		}
		return model.Account{}, wrapStorage("scan account", err)
	}
	var err error
	if a.CreatedAt, err = decodeTime(created); err != nil {
		return model.Account{}, err
	}
	if a.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Account{}, err
	}
	return a, nil
}

// ---- Membership ----

func (s *Store) PutMembership(ctx context.Context, m model.Membership) error {
	if err := model.ValidateMembership(m); err != nil {
		return err
	}
	_, err := s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO memberships (principal_id, account_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)`,
		m.PrincipalID, m.AccountID, encodeTime(m.CreatedAt), encodeTime(m.UpdatedAt))
	if err != nil {
		return wrapStorage("put membership", err)
	}
	return nil
}

func (s *Store) GetMembership(ctx context.Context, principalID, accountID string) (model.Membership, error) {
	row := s.exec.QueryRowContext(ctx,
		membershipSelect+` WHERE principal_id = ? AND account_id = ?`, principalID, accountID)
	m, err := scanMembership(row)
	if isNoRows(err) {
		return model.Membership{}, notFound("membership", principalID+"@"+accountID)
	}
	return m, err
}

func (s *Store) DeleteMembership(ctx context.Context, principalID, accountID string) error {
	res, err := s.exec.ExecContext(ctx,
		`DELETE FROM memberships WHERE principal_id = ? AND account_id = ?`, principalID, accountID)
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
		membershipSelect+` WHERE principal_id = ? ORDER BY account_id`, principalID)
	if err != nil {
		return nil, wrapStorage("memberships for principal", err)
	}
	return collectMemberships(rows)
}

func (s *Store) MembershipsForAccount(ctx context.Context, accountID string) ([]model.Membership, error) {
	rows, err := s.exec.QueryContext(ctx,
		membershipSelect+` WHERE account_id = ? ORDER BY principal_id`, accountID)
	if err != nil {
		return nil, wrapStorage("memberships for account", err)
	}
	return collectMemberships(rows)
}

func (s *Store) IsMember(ctx context.Context, principalID, accountID string) (bool, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT 1 FROM memberships WHERE principal_id = ? AND account_id = ?`, principalID, accountID)
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

const membershipSelect = `SELECT principal_id, account_id, created_at, updated_at FROM memberships`

func collectMemberships(rows *sql.Rows) ([]model.Membership, error) {
	defer rows.Close()
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
		created, updated string
	)
	if err := sc.Scan(&m.PrincipalID, &m.AccountID, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Membership{}, err
		}
		return model.Membership{}, wrapStorage("scan membership", err)
	}
	var err error
	if m.CreatedAt, err = decodeTime(created); err != nil {
		return model.Membership{}, err
	}
	if m.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Membership{}, err
	}
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
	_, err = s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO object_types (name, actions, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		ot.Name, string(actions), ot.Description, encodeTime(ot.CreatedAt), encodeTime(ot.UpdatedAt))
	if err != nil {
		return wrapStorage("put object type", err)
	}
	return nil
}

func (s *Store) GetObjectType(ctx context.Context, name string) (model.ObjectType, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT name, actions, description, created_at, updated_at FROM object_types WHERE name = ?`, name)
	ot, err := scanObjectType(row)
	if isNoRows(err) {
		return model.ObjectType{}, notFound("object type", name)
	}
	return ot, err
}

func (s *Store) ListObjectTypes(ctx context.Context) ([]model.ObjectType, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT name, actions, description, created_at, updated_at FROM object_types ORDER BY name`)
	if err != nil {
		return nil, wrapStorage("list object types", err)
	}
	defer rows.Close()
	out := make([]model.ObjectType, 0)
	for rows.Next() {
		ot, err := scanObjectType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ot)
	}
	return out, rows.Err()
}

func (s *Store) DeleteObjectType(ctx context.Context, name string) error {
	return s.deleteByID(ctx, "object type", "object_types", "name", name)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanObjectType(sc scanner) (model.ObjectType, error) {
	var (
		ot               model.ObjectType
		actions          string
		created, updated string
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
	var err error
	if ot.CreatedAt, err = decodeTime(created); err != nil {
		return model.ObjectType{}, err
	}
	if ot.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.ObjectType{}, err
	}
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
	_, err = s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO permissions (id, object_type, action, scope_strategy, delegatable, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ObjectType, p.Action, p.ScopeStrategy, encodeBool(p.Delegatable), p.Description, encodeTime(p.CreatedAt), encodeTime(p.UpdatedAt))
	if err != nil {
		return wrapStorage("put permission", err)
	}
	return nil
}

func (s *Store) GetPermission(ctx context.Context, id string) (model.Permission, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT id, object_type, action, scope_strategy, delegatable, description, created_at, updated_at FROM permissions WHERE id = ?`, id)
	p, err := scanPermission(row)
	if isNoRows(err) {
		return model.Permission{}, notFound("permission", id)
	}
	return p, err
}

func (s *Store) ListPermissions(ctx context.Context) ([]model.Permission, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT id, object_type, action, scope_strategy, delegatable, description, created_at, updated_at FROM permissions ORDER BY id`)
	if err != nil {
		return nil, wrapStorage("list permissions", err)
	}
	defer rows.Close()
	out := make([]model.Permission, 0)
	for rows.Next() {
		p, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePermission(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "permission", "permissions", "id", id)
}

func scanPermission(sc scanner) (model.Permission, error) {
	var (
		p                model.Permission
		delegatable      int64
		created, updated string
	)
	if err := sc.Scan(&p.ID, &p.ObjectType, &p.Action, &p.ScopeStrategy, &delegatable, &p.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Permission{}, err
		}
		return model.Permission{}, wrapStorage("scan permission", err)
	}
	p.Delegatable = delegatable != 0
	var err error
	if p.CreatedAt, err = decodeTime(created); err != nil {
		return model.Permission{}, err
	}
	if p.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Permission{}, err
	}
	return p, nil
}

// ---- Principal ----

func (s *Store) PutPrincipal(ctx context.Context, p model.Principal) error {
	if err := model.ValidatePrincipal(p); err != nil {
		return err
	}
	return s.inTx(ctx, "put principal", func(tx sqlExec) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO principals (id, kind, identity, display_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, string(p.Kind), p.Identity, p.DisplayName, encodeTime(p.CreatedAt), encodeTime(p.UpdatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM principal_roles WHERE principal_id = ?`, p.ID); err != nil {
			return err
		}
		for i, roleID := range p.RoleIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO principal_roles (principal_id, role_id, seq) VALUES (?, ?, ?)`,
				p.ID, roleID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetPrincipal(ctx context.Context, id string) (model.Principal, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT id, kind, identity, display_name, created_at, updated_at FROM principals WHERE id = ?`, id)
	p, err := scanPrincipal(row)
	if isNoRows(err) {
		return model.Principal{}, notFound("principal", id)
	}
	if err != nil {
		return model.Principal{}, err
	}
	p.RoleIDs, err = s.childIDs(ctx, `SELECT role_id FROM principal_roles WHERE principal_id = ? ORDER BY seq`, id)
	if err != nil {
		return model.Principal{}, err
	}
	return p, nil
}

func (s *Store) ListPrincipals(ctx context.Context) ([]model.Principal, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT id, kind, identity, display_name, created_at, updated_at FROM principals ORDER BY id`)
	if err != nil {
		return nil, wrapStorage("list principals", err)
	}
	defer rows.Close()
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
		out[i].RoleIDs, err = s.childIDs(ctx,
			`SELECT role_id FROM principal_roles WHERE principal_id = ? ORDER BY seq`, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DeletePrincipal(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete principal", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM principals WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("principal", id)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM principal_roles WHERE principal_id = ?`, id)
		return err
	})
}

func scanPrincipal(sc scanner) (model.Principal, error) {
	var (
		p                model.Principal
		kind             string
		created, updated string
	)
	if err := sc.Scan(&p.ID, &kind, &p.Identity, &p.DisplayName, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Principal{}, err
		}
		return model.Principal{}, wrapStorage("scan principal", err)
	}
	p.Kind = model.PrincipalKind(kind)
	var err error
	if p.CreatedAt, err = decodeTime(created); err != nil {
		return model.Principal{}, err
	}
	if p.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Principal{}, err
	}
	return p, nil
}

// ---- Role ----

func (s *Store) PutRole(ctx context.Context, r model.Role) error {
	if err := model.ValidateRole(r); err != nil {
		return err
	}
	return s.inTx(ctx, "put role", func(tx sqlExec) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO roles (id, name, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.Description, encodeTime(r.CreatedAt), encodeTime(r.UpdatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, r.ID); err != nil {
			return err
		}
		for i, permID := range r.PermissionIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO role_permissions (role_id, permission_id, seq) VALUES (?, ?, ?)`,
				r.ID, permID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetRole(ctx context.Context, id string) (model.Role, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM roles WHERE id = ?`, id)
	r, err := scanRole(row)
	if isNoRows(err) {
		return model.Role{}, notFound("role", id)
	}
	if err != nil {
		return model.Role{}, err
	}
	r.PermissionIDs, err = s.childIDs(ctx, `SELECT permission_id FROM role_permissions WHERE role_id = ? ORDER BY seq`, id)
	if err != nil {
		return model.Role{}, err
	}
	return r, nil
}

func (s *Store) ListRoles(ctx context.Context) ([]model.Role, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM roles ORDER BY id`)
	if err != nil {
		return nil, wrapStorage("list roles", err)
	}
	defer rows.Close()
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
		out[i].PermissionIDs, err = s.childIDs(ctx,
			`SELECT permission_id FROM role_permissions WHERE role_id = ? ORDER BY seq`, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete role", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM roles WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("role", id)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, id)
		return err
	})
}

func scanRole(sc scanner) (model.Role, error) {
	var (
		r                model.Role
		created, updated string
	)
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Role{}, err
		}
		return model.Role{}, wrapStorage("scan role", err)
	}
	var err error
	if r.CreatedAt, err = decodeTime(created); err != nil {
		return model.Role{}, err
	}
	if r.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Role{}, err
	}
	return r, nil
}

// ---- Group ----

func (s *Store) PutGroup(ctx context.Context, g model.Group) error {
	if err := model.ValidateGroup(g); err != nil {
		return err
	}
	return s.inTx(ctx, "put group", func(tx sqlExec) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO groups (id, name, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			g.ID, g.Name, g.Description, encodeTime(g.CreatedAt), encodeTime(g.UpdatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = ?`, g.ID); err != nil {
			return err
		}
		for i, principalID := range g.MemberPrincipalIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO group_members (group_id, principal_id, seq) VALUES (?, ?, ?)`,
				g.ID, principalID, i); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetGroup(ctx context.Context, id string) (model.Group, error) {
	g, err := s.getGroupRow(ctx, id)
	if isNoRows(err) {
		return model.Group{}, notFound("group", id)
	}
	if err != nil {
		return model.Group{}, err
	}
	g.MemberPrincipalIDs, err = s.childIDs(ctx, `SELECT principal_id FROM group_members WHERE group_id = ? ORDER BY seq`, id)
	if err != nil {
		return model.Group{}, err
	}
	return g, nil
}

func (s *Store) getGroupRow(ctx context.Context, id string) (model.Group, error) {
	row := s.exec.QueryRowContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM groups WHERE id = ?`, id)
	return scanGroup(row)
}

func (s *Store) ListGroups(ctx context.Context) ([]model.Group, error) {
	rows, err := s.exec.QueryContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, wrapStorage("list groups", err)
	}
	defer rows.Close()
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
		return nil, wrapStorage("list groups", err)
	}
	for i := range out {
		out[i].MemberPrincipalIDs, err = s.childIDs(ctx,
			`SELECT principal_id FROM group_members WHERE group_id = ? ORDER BY seq`, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete group", func(tx sqlExec) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("group", id)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id = ?`, id)
		return err
	})
}

func scanGroup(sc scanner) (model.Group, error) {
	var (
		g                model.Group
		created, updated string
	)
	if err := sc.Scan(&g.ID, &g.Name, &g.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Group{}, err
		}
		return model.Group{}, wrapStorage("scan group", err)
	}
	var err error
	if g.CreatedAt, err = decodeTime(created); err != nil {
		return model.Group{}, err
	}
	if g.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Group{}, err
	}
	return g, nil
}

func (s *Store) GroupsForPrincipal(ctx context.Context, principalID string) ([]model.Group, error) {
	rows, err := s.exec.QueryContext(ctx, `
		SELECT g.id, g.name, g.description, g.created_at, g.updated_at
		FROM groups g
		JOIN group_members m ON m.group_id = g.id
		WHERE m.principal_id = ?
		ORDER BY g.id`, principalID)
	if err != nil {
		return nil, wrapStorage("groups for principal", err)
	}
	defer rows.Close()
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
		return nil, wrapStorage("groups for principal", err)
	}
	for i := range out {
		out[i].MemberPrincipalIDs, err = s.childIDs(ctx,
			`SELECT principal_id FROM group_members WHERE group_id = ? ORDER BY seq`, ids[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- Grant ----

func (s *Store) PutGrant(ctx context.Context, g model.Grant) error {
	if err := model.ValidateGrant(g); err != nil {
		return err
	}
	_, err := s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO grants (id, account_id, subject_kind, subject_id, permission_id, object, effect, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.AccountID, string(g.Subject.Kind), g.Subject.ID, g.PermissionID, g.Object, string(g.Effect),
		encodeTime(g.CreatedAt), encodeTime(g.UpdatedAt))
	if err != nil {
		return wrapStorage("put grant", err)
	}
	return nil
}

func (s *Store) GetGrant(ctx context.Context, id string) (model.Grant, error) {
	row := s.exec.QueryRowContext(ctx, grantSelect+` WHERE id = ?`, id)
	g, err := scanGrant(row)
	if isNoRows(err) {
		return model.Grant{}, notFound("grant", id)
	}
	return g, err
}

func (s *Store) ListGrants(ctx context.Context, accountID string) ([]model.Grant, error) {
	rows, err := s.exec.QueryContext(ctx, grantSelect+` WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, wrapStorage("list grants", err)
	}
	return collectGrants(rows)
}

func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "grant", "grants", "id", id)
}

func (s *Store) GrantsForSubjects(ctx context.Context, accountID string, subjects []model.Subject) ([]model.Grant, error) {
	if len(subjects) == 0 {
		return []model.Grant{}, nil
	}
	// Build a parameterized predicate: account_id = ? AND (subject matches any).
	var b strings.Builder
	b.WriteString(grantSelect)
	b.WriteString(` WHERE account_id = ? AND (`)
	args := make([]any, 0, 1+2*len(subjects))
	args = append(args, accountID)
	for i, sub := range subjects {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString("(subject_kind = ? AND subject_id = ?)")
		args = append(args, string(sub.Kind), sub.ID)
	}
	b.WriteString(`) ORDER BY id`)
	rows, err := s.exec.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, wrapStorage("grants for subjects", err)
	}
	return collectGrants(rows)
}

const grantSelect = `SELECT id, account_id, subject_kind, subject_id, permission_id, object, effect, created_at, updated_at FROM grants`

func collectGrants(rows *sql.Rows) ([]model.Grant, error) {
	defer rows.Close()
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
		created, updated        string
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
	var err error
	if g.CreatedAt, err = decodeTime(created); err != nil {
		return model.Grant{}, err
	}
	if g.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Grant{}, err
	}
	return g, nil
}

// ---- Template (named, versioned) ----

const templateSelect = `SELECT name, version, description, params, grants, created_at, updated_at FROM templates`

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
	_, err = s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO templates (name, version, description, params, grants, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Version, t.Description, string(params), string(grants),
		encodeTime(t.CreatedAt), encodeTime(t.UpdatedAt))
	if err != nil {
		return wrapStorage("put template", err)
	}
	return nil
}

func (s *Store) GetTemplate(ctx context.Context, name string, version int) (model.Template, error) {
	if version > 0 {
		row := s.exec.QueryRowContext(ctx, templateSelect+` WHERE name = ? AND version = ?`, name, version)
		t, err := scanTemplate(row)
		if isNoRows(err) {
			return model.Template{}, notFound("template", name+":v"+itoa(version))
		}
		return t, err
	}
	// version <= 0: latest (highest) version of name.
	row := s.exec.QueryRowContext(ctx, templateSelect+` WHERE name = ? ORDER BY version DESC LIMIT 1`, name)
	t, err := scanTemplate(row)
	if isNoRows(err) {
		return model.Template{}, notFound("template", name)
	}
	return t, err
}

func (s *Store) ListTemplates(ctx context.Context) ([]model.Template, error) {
	rows, err := s.exec.QueryContext(ctx, templateSelect+` ORDER BY name, version`)
	if err != nil {
		return nil, wrapStorage("list templates", err)
	}
	defer rows.Close()
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
		res, err := s.exec.ExecContext(ctx, `DELETE FROM templates WHERE name = ? AND version = ?`, name, version)
		if err != nil {
			return wrapStorage("delete template", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("template", name+":v"+itoa(version))
		}
		return nil
	}
	res, err := s.exec.ExecContext(ctx, `DELETE FROM templates WHERE name = ?`, name)
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
		created, updated string
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
	var err error
	if t.CreatedAt, err = decodeTime(created); err != nil {
		return model.Template{}, err
	}
	if t.UpdatedAt, err = decodeTime(updated); err != nil {
		return model.Template{}, err
	}
	return t, nil
}

// itoa is strconv.Itoa, kept local for the not-found message helpers.
func itoa(n int) string { return strconv.Itoa(n) }

// ---- Audit trail (append-only) ----

const auditColumns = `id, ts_nanos, event_type, action, actor, effective_subject, impersonation_mode, account, target, outcome, reason, details`

func (s *Store) AppendAudit(ctx context.Context, ev model.AuditEvent) error {
	details := ""
	if len(ev.Details) > 0 {
		b, err := json.Marshal(ev.Details)
		if err != nil {
			return wrapStorage("marshal audit details", err)
		}
		details = string(b)
	}
	_, err := s.exec.ExecContext(ctx, `
		INSERT OR REPLACE INTO audit_log (`+auditColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Timestamp.UTC().UnixNano(), string(ev.EventType), ev.Action, ev.Actor,
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
	b.WriteString(` FROM audit_log`)
	var (
		where []string
		args  []any
	)
	if filter.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, filter.Actor)
	}
	if filter.Account != "" {
		where = append(where, "account = ?")
		args = append(args, filter.Account)
	}
	if filter.EventType != "" {
		where = append(where, "event_type = ?")
		args = append(args, string(filter.EventType))
	}
	if filter.Outcome != "" {
		where = append(where, "outcome = ?")
		args = append(args, string(filter.Outcome))
	}
	if !filter.Since.IsZero() {
		where = append(where, "ts_nanos >= ?")
		args = append(args, filter.Since.UTC().UnixNano())
	}
	if !filter.Until.IsZero() {
		where = append(where, "ts_nanos < ?")
		args = append(args, filter.Until.UTC().UnixNano())
	}
	if len(where) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(where, " AND "))
	}
	b.WriteString(" ORDER BY ts_nanos DESC, id DESC")
	if filter.Limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.Limit)
	}
	rows, err := s.exec.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, wrapStorage("query audit", err)
	}
	defer rows.Close()
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
		res, err := s.exec.ExecContext(ctx,
			`DELETE FROM audit_log WHERE ts_nanos < ?`, policy.Before.UTC().UnixNano())
		if err != nil {
			return removed, wrapStorage("prune audit by age", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}
	// Size bound: keep only the newest MaxCount events.
	if policy.MaxCount > 0 {
		res, err := s.exec.ExecContext(ctx, `
			DELETE FROM audit_log WHERE id NOT IN (
				SELECT id FROM audit_log ORDER BY ts_nanos DESC, id DESC LIMIT ?
			)`, policy.MaxCount)
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
		tsNanos                     int64
		eventType, outcome, details string
	)
	if err := sc.Scan(&ev.ID, &tsNanos, &eventType, &ev.Action, &ev.Actor,
		&ev.EffectiveSubject, &ev.ImpersonationMode, &ev.Account, &ev.Target, &outcome,
		&ev.Reason, &details); err != nil {
		return model.AuditEvent{}, wrapStorage("scan audit", err)
	}
	ev.Timestamp = time.Unix(0, tsNanos).UTC()
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
	rows, err := s.exec.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, wrapStorage("query child ids", err)
	}
	defer rows.Close()
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
func (s *Store) deleteByID(ctx context.Context, kind, table, col, id string) error {
	res, err := s.exec.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ?`, id)
	if err != nil {
		return wrapStorage("delete "+kind, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(kind, id)
	}
	return nil
}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error. A coded error returned by fn (e.g. NOT_FOUND) passes through verbatim;
// raw driver errors are wrapped as APERTURE_STORAGE under op.
//
// When this Store is already transaction-scoped (pool == nil — it is running
// inside an Atomic), there is no new transaction to begin: fn runs against the
// current exec (the enclosing *sql.Tx) so the multi-statement write joins the
// surrounding transaction and an outer rollback still covers it.
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
	tx, err := s.pool.BeginTx(ctx, nil)
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

// Atomic runs fn inside a single SQLite transaction against a transaction-scoped
// Store, committing when fn returns nil and rolling the WHOLE batch back when fn
// returns an error — so no write performed inside fn persists if any step fails.
// A nested Atomic (this Store is already transaction-scoped) flattens into the
// current transaction so an outer rollback still covers everything.
func (s *Store) Atomic(ctx context.Context, fn func(tx model.Storage) error) error {
	if s.pool == nil {
		// Already inside a transaction: reuse it (flatten).
		return fn(s)
	}
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return wrapStorage("atomic", err)
	}
	child := &Store{pool: nil, exec: tx}
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
