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
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Store is a SQLite-backed model.Storage. Construct one with Open (durable file)
// or OpenMemory (ephemeral, for tests). Call Setup once before use.
type Store struct {
	db *sql.DB
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
	return &Store{db: db}, nil
}

// OpenMemory opens a private in-memory SQLite database. Useful for tests and
// ephemeral seeding. The database lives only as long as the Store.
func OpenMemory() (*Store, error) {
	return Open("file::memory:?cache=shared")
}

// Setup creates the schema. It is idempotent (every statement is IF NOT EXISTS).
func (s *Store) Setup(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return aerr.Wrap(aerr.APERTURE_STORAGE, "apply schema", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
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

func wrapStorage(op string, err error) error {
	return aerr.Wrap(aerr.APERTURE_STORAGE, op, err)
}

func notFound(kind, id string) error {
	return aerr.WithContext(aerr.APERTURE_NOT_FOUND,
		kind+" not found",
		map[string]any{"kind": kind, "id": id})
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// ---- ObjectType ----

func (s *Store) PutObjectType(ctx context.Context, ot model.ObjectType) error {
	if err := model.ValidateObjectType(ot); err != nil {
		return err
	}
	actions, err := json.Marshal(ot.Actions)
	if err != nil {
		return wrapStorage("marshal actions", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO object_types (name, actions, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		ot.Name, string(actions), ot.Description, encodeTime(ot.CreatedAt), encodeTime(ot.UpdatedAt))
	if err != nil {
		return wrapStorage("put object type", err)
	}
	return nil
}

func (s *Store) GetObjectType(ctx context.Context, name string) (model.ObjectType, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, actions, description, created_at, updated_at FROM object_types WHERE name = ?`, name)
	ot, err := scanObjectType(row)
	if isNoRows(err) {
		return model.ObjectType{}, notFound("object type", name)
	}
	return ot, err
}

func (s *Store) ListObjectTypes(ctx context.Context) ([]model.ObjectType, error) {
	rows, err := s.db.QueryContext(ctx,
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
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO permissions (id, object_type, action, scope_strategy, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ObjectType, p.Action, p.ScopeStrategy, p.Description, encodeTime(p.CreatedAt), encodeTime(p.UpdatedAt))
	if err != nil {
		return wrapStorage("put permission", err)
	}
	return nil
}

func (s *Store) GetPermission(ctx context.Context, id string) (model.Permission, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, object_type, action, scope_strategy, description, created_at, updated_at FROM permissions WHERE id = ?`, id)
	p, err := scanPermission(row)
	if isNoRows(err) {
		return model.Permission{}, notFound("permission", id)
	}
	return p, err
}

func (s *Store) ListPermissions(ctx context.Context) ([]model.Permission, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, object_type, action, scope_strategy, description, created_at, updated_at FROM permissions ORDER BY id`)
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
		created, updated string
	)
	if err := sc.Scan(&p.ID, &p.ObjectType, &p.Action, &p.ScopeStrategy, &p.Description, &created, &updated); err != nil {
		if isNoRows(err) {
			return model.Permission{}, err
		}
		return model.Permission{}, wrapStorage("scan permission", err)
	}
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
	return s.inTx(ctx, "put principal", func(tx *sql.Tx) error {
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
	row := s.db.QueryRowContext(ctx,
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
	rows, err := s.db.QueryContext(ctx,
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
	return s.inTx(ctx, "delete principal", func(tx *sql.Tx) error {
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
	return s.inTx(ctx, "put role", func(tx *sql.Tx) error {
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
	row := s.db.QueryRowContext(ctx,
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
	rows, err := s.db.QueryContext(ctx,
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
	return s.inTx(ctx, "delete role", func(tx *sql.Tx) error {
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
	return s.inTx(ctx, "put group", func(tx *sql.Tx) error {
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
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM groups WHERE id = ?`, id)
	return scanGroup(row)
}

func (s *Store) ListGroups(ctx context.Context) ([]model.Group, error) {
	rows, err := s.db.QueryContext(ctx,
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
	return s.inTx(ctx, "delete group", func(tx *sql.Tx) error {
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
	rows, err := s.db.QueryContext(ctx, `
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
	_, err := s.db.ExecContext(ctx, `
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
	row := s.db.QueryRowContext(ctx, grantSelect+` WHERE id = ?`, id)
	g, err := scanGrant(row)
	if isNoRows(err) {
		return model.Grant{}, notFound("grant", id)
	}
	return g, err
}

func (s *Store) ListGrants(ctx context.Context, accountID string) ([]model.Grant, error) {
	rows, err := s.db.QueryContext(ctx, grantSelect+` WHERE account_id = ? ORDER BY id`, accountID)
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
	rows, err := s.db.QueryContext(ctx, b.String(), args...)
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

// ---- shared helpers ----

// childIDs runs a single-column query returning the ordered list of ids. It
// returns nil (not an empty slice) when there are no rows so round-tripped
// values compare equal to caller-supplied nil/empty slices.
func (s *Store) childIDs(ctx context.Context, query, arg string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ?`, id)
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
func (s *Store) inTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
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
