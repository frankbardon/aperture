-- Aperture SQLite schema. Hand-written, embedded, no ORM/migration tool.
-- Every statement is CREATE ... IF NOT EXISTS so Setup is idempotent.
--
-- Design notes:
--   * Tables are explicit and extensible: timestamps live on every entity for
--     forward-compatibility with audit (E4-S2); membership is normalized into
--     join tables so a future Postgres port maps over cleanly.
--   * Object-type action verb sets are stored as a JSON text column (a value
--     list, not a relationship); membership edges are real join tables.
--   * Grants carry account_id (the cross-account isolation stamp) and are
--     indexed by (account_id, subject_kind, subject_id) for the decision
--     engine's hot-path GrantsForSubjects query.

CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

-- Memberships are edges keyed by the (principal_id, account_id) pair: a
-- principal is a member of an account at most once. Indexed both ways so
-- "accounts for a principal" and "members of an account" are both cheap.
CREATE TABLE IF NOT EXISTS memberships (
    principal_id TEXT NOT NULL,
    account_id   TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (principal_id, account_id)
);

CREATE INDEX IF NOT EXISTS idx_memberships_account
    ON memberships (account_id);

CREATE TABLE IF NOT EXISTS object_types (
    name        TEXT PRIMARY KEY,
    actions     TEXT NOT NULL,          -- JSON array of verb strings
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS permissions (
    id             TEXT PRIMARY KEY,
    object_type    TEXT NOT NULL,
    action         TEXT NOT NULL,
    scope_strategy TEXT NOT NULL DEFAULT '',
    delegatable    INTEGER NOT NULL DEFAULT 0,  -- 0/1: may this permission be bestowed (E3-S2)
    description    TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS principals (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    identity     TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS principal_roles (
    principal_id TEXT NOT NULL,
    role_id      TEXT NOT NULL,
    seq          INTEGER NOT NULL,       -- preserves caller-supplied order
    PRIMARY KEY (principal_id, role_id)
);

CREATE TABLE IF NOT EXISTS roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id       TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS groups (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id     TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    PRIMARY KEY (group_id, principal_id)
);

CREATE INDEX IF NOT EXISTS idx_group_members_principal
    ON group_members (principal_id);

CREATE TABLE IF NOT EXISTS grants (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL,
    subject_kind  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    permission_id TEXT NOT NULL,
    object        TEXT NOT NULL,         -- identity pattern, string form
    effect        TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_grants_account_subject
    ON grants (account_id, subject_kind, subject_id);

-- Provisioning templates (E5-S1, FR-18/FR-19): named, versioned bundles of
-- parameterized grants. Identity is the (name, version) pair so multiple
-- versions of a name coexist; apply selects the latest by default. The typed
-- parameter declarations and the parameterized grant templates ride as JSON
-- value columns (a value list, not a relationship), mirroring how object-type
-- verb sets are stored.
CREATE TABLE IF NOT EXISTS templates (
    name        TEXT NOT NULL,
    version     INTEGER NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '[]',   -- JSON array of {Name,Type,Description}
    grants      TEXT NOT NULL DEFAULT '[]',   -- JSON array of template grants
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (name, version)
);

-- Named rules (E5-S2): the persisted home for the rule AST a scope strategy
-- resolves. The AST rides as a JSON text column carrying the rules package's
-- canonical serialization verbatim (a rules.Node), so a round-trip is
-- byte-stable and the editor's format is preserved exactly. Identity is the
-- rule name; PutRule upserts on it.
CREATE TABLE IF NOT EXISTS rules (
    name        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    ast         TEXT NOT NULL,                -- rules.Node canonical JSON
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

-- Append-only audit trail (E4-S2, FR-25). Writes are inserts only; deletes
-- happen exclusively through bulk retention pruning. The timestamp is stored as
-- integer Unix nanoseconds so range filters and newest-first ordering compare
-- numerically (RFC3339 text would mis-sort variable-length fractional seconds).
-- A record made under impersonation carries both the real actor (actor) and the
-- borrowed target (effective_subject + impersonation_mode). The details column
-- is an optional JSON blob for event-specific context.
CREATE TABLE IF NOT EXISTS audit_log (
    id                 TEXT PRIMARY KEY,
    ts_nanos           INTEGER NOT NULL,
    event_type         TEXT NOT NULL,
    action             TEXT NOT NULL DEFAULT '',
    actor              TEXT NOT NULL DEFAULT '',
    effective_subject  TEXT NOT NULL DEFAULT '',
    impersonation_mode TEXT NOT NULL DEFAULT '',
    account            TEXT NOT NULL DEFAULT '',
    target             TEXT NOT NULL DEFAULT '',
    outcome            TEXT NOT NULL DEFAULT '',
    reason             TEXT NOT NULL DEFAULT '',
    details            TEXT NOT NULL DEFAULT ''     -- JSON object, '' when none
);

CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts_nanos);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log (actor);
CREATE INDEX IF NOT EXISTS idx_audit_account ON audit_log (account);
CREATE INDEX IF NOT EXISTS idx_audit_event_type ON audit_log (event_type);
