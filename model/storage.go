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
	// Setup creates or migrates the backend's schema. It is idempotent and must
	// be called once before any other method.
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
}
