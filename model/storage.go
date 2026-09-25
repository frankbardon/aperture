package model

import "context"

// Storage is Aperture's persistence boundary: the single seam every backend
// implements. The in-memory backend (storage/memory) and the SQLite reference
// backend (storage/sqlite) both satisfy it, and it is deliberately free of any
// backend-specific concept so a future Postgres backend slots in unchanged.
//
// Shape and contract:
//
//   - Put* is an upsert keyed on the entity's id (or, for object types, name):
//     it creates the entity when absent and replaces it when present. Put*
//     validates its argument and, for permissions, enforces typed-action
//     validation against the referenced object type.
//   - Get* returns APERTURE_NOT_FOUND when the id is unknown.
//   - List* returns every entity of the kind (grants are listed per account).
//   - Delete* returns APERTURE_NOT_FOUND when the id is unknown.
//   - Underlying backend failures surface as APERTURE_STORAGE.
//
// Account stamping is enforced through the grant queries: every grant carries an
// AccountID, and GrantsForSubjects / ListGrants are account-scoped so a grant
// stamped to one account can never surface in another. This is the data-layer
// half of the cross-account isolation guarantee completed in E3-S1.
//
// All methods are safe for concurrent use by multiple goroutines.
type Storage interface {
	// Setup CREATES the backend's schema when it is absent, and must be called
	// once before any other method. It is idempotent: against a database that
	// already carries the current schema it creates nothing and changes nothing.
	//
	// It does NOT migrate. Aperture ships no migration tool, no schema
	// versioning, and no user_version pragma, so a schema change is a hard break
	// and an old database does not upgrade. A backend that can recognise a
	// database written by an older build refuses it here with
	// APERTURE_STORAGE_SCHEMA_INCOMPATIBLE — at startup, rather than misreading
	// it at the first query — and the remedy is to move the old database aside
	// and re-seed a fresh one. See docs/src/concepts/storage.md.
	Setup(ctx context.Context) error
	// Close releases backend resources. It is safe to call once.
	Close() error

	// ---- Account ----

	PutAccount(ctx context.Context, a Account) error
	GetAccount(ctx context.Context, id string) (Account, error)
	ListAccounts(ctx context.Context) ([]Account, error)
	DeleteAccount(ctx context.Context, id string) error

	// ---- Membership (keyed by the (principalID, accountID) pair) ----

	// PutMembership upserts the edge linking principalID to accountID. The pair is
	// the membership's identity, so re-putting the same pair replaces it.
	PutMembership(ctx context.Context, m Membership) error
	// GetMembership returns the edge for the pair, or APERTURE_NOT_FOUND when the
	// principal is not a member of the account.
	GetMembership(ctx context.Context, principalID, accountID string) (Membership, error)
	// DeleteMembership removes the edge, returning APERTURE_NOT_FOUND when absent.
	DeleteMembership(ctx context.Context, principalID, accountID string) error
	// MembershipsForPrincipal returns every account the principal belongs to.
	MembershipsForPrincipal(ctx context.Context, principalID string) ([]Membership, error)
	// MembershipsForAccount returns every principal that belongs to the account.
	MembershipsForAccount(ctx context.Context, accountID string) ([]Membership, error)
	// IsMember reports whether principalID is a member of accountID. It is the
	// decision engine's membership-enforcement query: a tight existence check that
	// avoids materializing the full membership list on the hot path.
	IsMember(ctx context.Context, principalID, accountID string) (bool, error)

	// ---- ObjectType (keyed by Name) ----

	PutObjectType(ctx context.Context, ot ObjectType) error
	GetObjectType(ctx context.Context, name string) (ObjectType, error)
	ListObjectTypes(ctx context.Context) ([]ObjectType, error)
	DeleteObjectType(ctx context.Context, name string) error

	// ---- Permission ----

	// PutPermission validates the permission's action against its object type's
	// declared verb set (APERTURE_ACTION_UNDECLARED on failure) and returns
	// APERTURE_NOT_FOUND when the referenced object type does not exist.
	PutPermission(ctx context.Context, p Permission) error
	GetPermission(ctx context.Context, id string) (Permission, error)
	ListPermissions(ctx context.Context) ([]Permission, error)
	DeletePermission(ctx context.Context, id string) error

	// ---- Principal ----

	PutPrincipal(ctx context.Context, p Principal) error
	GetPrincipal(ctx context.Context, id string) (Principal, error)
	ListPrincipals(ctx context.Context) ([]Principal, error)
	DeletePrincipal(ctx context.Context, id string) error

	// ---- Role ----

	PutRole(ctx context.Context, r Role) error
	GetRole(ctx context.Context, id string) (Role, error)
	ListRoles(ctx context.Context) ([]Role, error)
	DeleteRole(ctx context.Context, id string) error

	// ---- Group ----

	PutGroup(ctx context.Context, g Group) error
	GetGroup(ctx context.Context, id string) (Group, error)
	ListGroups(ctx context.Context) ([]Group, error)
	DeleteGroup(ctx context.Context, id string) error

	// ---- Grant ----

	// PutGrant validates the grant (including that Object parses as an identity
	// pattern and that AccountID is present) before persisting it.
	PutGrant(ctx context.Context, g Grant) error
	GetGrant(ctx context.Context, id string) (Grant, error)
	// ListGrants returns every grant stamped to accountID.
	ListGrants(ctx context.Context, accountID string) ([]Grant, error)
	// ListGrantsPage returns a single deterministic page of grants plus the total
	// number of matching grants (the pre-pagination count), so a caller can render
	// a page and drive prev/next without a second query.
	//
	// Scope: when accountID is AllAccounts (the empty string) the page spans EVERY
	// account, and wildcard-stamped ("*") grants are returned inline as the
	// ordinary rows they are — they are NOT filtered out. Any other accountID
	// scopes the page to that single account, matching ListGrants' account
	// stamping exactly (a "*" listing returns only the wildcard grants).
	//
	// Pagination: offset and limit are normalized through ClampGrantPage — a
	// non-positive limit becomes DefaultGrantPageSize, a limit above
	// MaxGrantPageSize is clamped to the cap, and a negative offset floors at
	// zero — so no single call can return more than MaxGrantPageSize rows. The
	// returned total is the full match count BEFORE offset/limit are applied, so
	// it does not shrink as the caller pages through.
	//
	// Ordering is deterministic and stable across pages: by AccountID, then by
	// grant id. Both backends return the same order so pages line up.
	ListGrantsPage(ctx context.Context, accountID string, offset, limit int) (grants []Grant, total int, err error)
	DeleteGrant(ctx context.Context, id string) error

	// ---- Decision-engine queries ----

	// GrantsForSubjects returns every grant stamped to accountID whose subject is
	// in subjects. It is the decision engine's hot-path query: the engine expands
	// a principal into its subject set (the principal, its roles, its groups) and
	// asks for exactly the grants that bind to that set, account-scoped so no
	// cross-account grant is ever returned. An empty subjects slice returns no
	// grants.
	GrantsForSubjects(ctx context.Context, accountID string, subjects []Subject) ([]Grant, error)

	// GroupsForPrincipal returns every group that lists principalID as a member.
	// The engine uses it to build the group half of a principal's subject set.
	GroupsForPrincipal(ctx context.Context, principalID string) ([]Group, error)

	// ---- Template (named, versioned; E5-S1) ----

	// PutTemplate upserts a template keyed on the (Name, Version) pair: putting a
	// new version under an existing name keeps the older versions. It validates the
	// template (ValidateTemplate) before persisting.
	PutTemplate(ctx context.Context, t Template) error
	// GetTemplate returns the template for (name, version). When version <= 0 it
	// returns the latest (highest) version of name. Returns APERTURE_NOT_FOUND when
	// no matching version exists.
	GetTemplate(ctx context.Context, name string, version int) (Template, error)
	// ListTemplates returns every stored template version, ordered by name then
	// ascending version.
	ListTemplates(ctx context.Context) ([]Template, error)
	// DeleteTemplate removes a template. When version <= 0 it deletes every version
	// of name; otherwise it deletes only the named version. Returns
	// APERTURE_NOT_FOUND when nothing matched.
	DeleteTemplate(ctx context.Context, name string, version int) error

	// ---- Rule (named; E5-S2) ----

	// PutRule upserts a rule keyed on Name: it creates the rule when absent and
	// replaces it when present. It validates the rule (ValidateRule) before
	// persisting — the AST is stored verbatim as its canonical JSON.
	PutRule(ctx context.Context, r Rule) error
	// GetRule returns the rule named name, or APERTURE_NOT_FOUND when it is unknown.
	GetRule(ctx context.Context, name string) (Rule, error)
	// ListRules returns every stored rule, ordered by name.
	ListRules(ctx context.Context) ([]Rule, error)
	// DeleteRule removes the rule named name, returning APERTURE_NOT_FOUND when it
	// is unknown.
	DeleteRule(ctx context.Context, name string) error

	// ---- Shared wiring (E1-S2) ----
	//
	// Wiring is not model state. Every entity above is who exists and who may do
	// what; the five wiring tables are where a decision's object metadata and
	// attribute bags are read FROM — the database-backed home for a seed
	// document's connections:, providers:, field_types: and attribute_providers:
	// sections, so a second instance can boot with no seed file and decide
	// identically. See model/wiring.go and skills/storage-schema.md.
	//
	// The write surface is ONE method, and deliberately not a Put/Delete pair per
	// entity. Wiring is only meaningful whole: a provider entry naming a
	// connection the manifest does not list is not half-valid wiring, and an
	// instance booting against a set written half-way would build a registry
	// missing exactly the entries whose write failed, while reporting nothing. So
	// the caller hands over the whole set and gets all of it or none of it.

	// GetWiring returns the WHOLE wiring set from one consistent snapshot: the
	// four sections read together, inside a transaction on the backends that have
	// one, so no boot can see a set that changed underneath it half-way through.
	// Every section comes back in canonical order (WiringSet.Sort), and each
	// provider entry carries its own reference rows.
	//
	// A database nothing has been pushed to answers with a zero WiringSet, which
	// WiringSet.IsEmpty reports — the state that tells a booting instance to fall
	// back to its local seed file. That is NOT an error.
	GetWiring(ctx context.Context) (WiringSet, error)
	// ReplaceWiring replaces the whole wiring set with set, atomically: every row
	// of all five tables is removed and set is written in its place, and a failure
	// at any point leaves the tables EXACTLY as they were. Pushing a zero
	// WiringSet therefore clears the wiring entirely.
	//
	// It validates the whole set (ValidateWiringSet) before writing anything, so a
	// malformed entry — an empty key, a negative max_size, a duplicate object type
	// — is APERTURE_INVALID_INPUT with nothing written. A provider entry naming an
	// object type the model does not have is refused with
	// APERTURE_STORAGE_CONSTRAINT: apt_wiring_providers.object_type is a real
	// foreign key, because a provider for a type no permission can name is wiring
	// nothing can reach.
	//
	// It does NOT check that a Kind is implemented, that a Connection appears in
	// the manifest, or that a TTL parses. Those belong to the layer that BUILDS
	// the wiring, which owns the vocabulary; see model/wiring.go.
	ReplaceWiring(ctx context.Context, set WiringSet) error
	// ListWiringConnections returns the connection manifest, ordered by name.
	ListWiringConnections(ctx context.Context) ([]WiringConnection, error)
	// GetWiringConnection returns one manifest entry, or APERTURE_NOT_FOUND.
	GetWiringConnection(ctx context.Context, name string) (WiringConnection, error)
	// ListWiringProviders returns every provider entry with its reference rows,
	// ordered by object type (and each entry's references by field).
	ListWiringProviders(ctx context.Context) ([]WiringProvider, error)
	// GetWiringProvider returns one provider entry with its reference rows, or
	// APERTURE_NOT_FOUND.
	GetWiringProvider(ctx context.Context, objectType string) (WiringProvider, error)
	// ListWiringFieldTypes returns every field-type declaration, ordered by object
	// type then field.
	ListWiringFieldTypes(ctx context.Context) ([]WiringFieldType, error)
	// GetWiringFieldType returns one field-type declaration, or
	// APERTURE_NOT_FOUND.
	GetWiringFieldType(ctx context.Context, objectType, field string) (WiringFieldType, error)
	// ListWiringAttributeProviders returns every attribute-provider entry, ordered
	// by slot.
	ListWiringAttributeProviders(ctx context.Context) ([]WiringAttributeProvider, error)
	// GetWiringAttributeProvider returns one attribute-provider entry, or
	// APERTURE_NOT_FOUND.
	GetWiringAttributeProvider(ctx context.Context, subject string) (WiringAttributeProvider, error)

	// ---- Transactional apply (E5-S1) ----

	// Atomic runs fn inside a transaction against a tx-scoped Storage, committing
	// when fn returns nil and rolling the WHOLE batch back when fn returns an error
	// — no write fn performed persists if any step fails. Both backends give real
	// atomicity (SQLite via BEGIN/COMMIT/ROLLBACK, the in-memory backend via a
	// staged snapshot committed only on success). It is the primitive the bulk
	// grant/revoke endpoints and template apply build on. fn MUST use the tx handed
	// to it (not the outer Storage); a nested Atomic flattens into the current
	// transaction so an outer rollback still covers everything.
	Atomic(ctx context.Context, fn func(tx Storage) error) error

	// ---- Audit trail (append-only, FR-25) ----

	// AppendAudit appends ev to the audit trail. The trail is append-only: the
	// only writes are Append (one event), and the only deletes are bulk retention
	// pruning (PruneAudit) — there is no update or single-event delete, so a
	// recorded event cannot be silently altered. Underlying backend failures
	// surface as APERTURE_STORAGE.
	AppendAudit(ctx context.Context, ev AuditEvent) error
	// QueryAudit returns the audit events matching filter, newest-first (by
	// timestamp, then id, descending). A zero filter returns the whole trail
	// (subject to its Limit). It is the queryable API the E6-S4 viewer builds on.
	QueryAudit(ctx context.Context, filter AuditFilter) ([]AuditEvent, error)
	// PruneAudit deletes the events the retention policy no longer keeps (older
	// than policy.Before, then any beyond policy.MaxCount newest) and returns the
	// number of events removed.
	PruneAudit(ctx context.Context, policy RetentionPolicy) (int, error)
}
