// Package memory is a map-backed, concurrency-safe implementation of
// model.Storage. It is the backend used for tests, seeding, and any deployment
// that does not need durability. It enforces the same validation and
// typed-action rules as the SQLite reference backend, so the shared conformance
// suite (storage/storagetest) passes against both unchanged.
package memory

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/storagetime"
)

// Store is a map-backed model.Storage. The zero value is not usable; construct
// one with New. All reads and writes are guarded by a single RWMutex, which is
// ample for an in-memory backend and keeps the maps trivially consistent.
type Store struct {
	mu          sync.RWMutex
	accounts    map[string]model.Account
	memberships map[membershipKey]model.Membership
	objectTypes map[string]model.ObjectType
	permissions map[string]model.Permission
	principals  map[string]model.Principal
	roles       map[string]model.Role
	groups      map[string]model.Group
	grants      map[string]model.Grant
	// templates holds every template version, keyed by (name, version) (E5-S1).
	templates map[templateKey]model.Template
	// rules holds every named rule, keyed by name (E5-S2).
	rules map[string]model.Rule
	// The four shared-wiring maps (E1-S2). They are the in-memory form of the five
	// wiring tables, and there are four maps rather than five because a provider
	// entry's reference rows live INSIDE the entry — see "THE FOUR CASCADE EDGES"
	// below for why that is the cascade rather than a shortcut around it.
	wiringConnections        map[string]model.WiringConnection
	wiringProviders          map[string]model.WiringProvider
	wiringFieldTypes         map[wiringFieldKey]model.WiringFieldType
	wiringAttributeProviders map[string]model.WiringAttributeProvider
	// audit is the append-only audit trail (FR-25). It is an ordered slice rather
	// than a map because the trail is append-only and queried newest-first.
	audit []model.AuditEvent
}

// membershipKey is the composite identity of a membership edge.
type membershipKey struct {
	principalID string
	accountID   string
}

// templateKey is the composite identity of a stored template version.
type templateKey struct {
	name    string
	version int
}

// wiringFieldKey is the composite identity of a field-type declaration, matching
// the (object_type, field) primary key.
type wiringFieldKey struct {
	objectType string
	field      string
}

// New returns an empty, ready-to-use in-memory Store.
func New() *Store {
	return &Store{
		accounts:    make(map[string]model.Account),
		memberships: make(map[membershipKey]model.Membership),
		objectTypes: make(map[string]model.ObjectType),
		permissions: make(map[string]model.Permission),
		principals:  make(map[string]model.Principal),
		roles:       make(map[string]model.Role),
		groups:      make(map[string]model.Group),
		grants:      make(map[string]model.Grant),
		templates:   make(map[templateKey]model.Template),
		rules:       make(map[string]model.Rule),

		wiringConnections:        make(map[string]model.WiringConnection),
		wiringProviders:          make(map[string]model.WiringProvider),
		wiringFieldTypes:         make(map[wiringFieldKey]model.WiringFieldType),
		wiringAttributeProviders: make(map[string]model.WiringAttributeProvider),
	}
}

// Compile-time assertion that Store satisfies the storage boundary.
var _ model.Storage = (*Store)(nil)

// Setup is a no-op for the in-memory backend: New already allocated the maps.
func (s *Store) Setup(context.Context) error { return nil }

// Close is a no-op for the in-memory backend.
func (s *Store) Close() error { return nil }

func notFound(kind, id string) error {
	return aerr.WithContext(aerr.APERTURE_NOT_FOUND,
		kind+" not found",
		map[string]any{"kind": kind, "id": id})
}

// validateStamps refuses a CreatedAt/UpdatedAt pair that the storage layer could
// not represent, with the same APERTURE_INVALID_INPUT the SQLite backend returns.
//
// SQLite gets this check for free: its created_at/updated_at columns are INTEGER
// nanoseconds, so every write runs through storagetime.Encode. This backend keeps
// the time.Time as handed to it and would happily store year 3000 — so it applies
// the identical rule by hand. The storable range is a property of the storage
// CONTRACT, not of one dialect's encoding, and storage/storagetest allows no
// backend-conditional assertions: if only one backend refused an out-of-range
// CreatedAt, the two would have diverged.
func validateStamps(created, updated time.Time) error {
	if err := storagetime.Validate(created); err != nil {
		return err
	}
	return storagetime.Validate(updated)
}

// ---- Referential integrity ----
//
// The SQLite backend gets referential integrity from the schema: eleven foreign
// keys, seven ON DELETE RESTRICT and four ON DELETE CASCADE (see the
// "Referential integrity" header in storage/sqlite/schema.sql). This backend has
// no schema, so it enforces the SAME eleven edges by hand, in both directions:
//
//	apt_memberships.principal_id  -> apt_principals(id)      RESTRICT
//	apt_permissions.object_type   -> apt_object_types(name)  RESTRICT
//	apt_principal_roles.role_id   -> apt_roles(id)           RESTRICT
//	apt_role_permissions.permission_id -> apt_permissions(id) RESTRICT
//	apt_group_members.principal_id -> apt_principals(id)     RESTRICT
//	apt_grants.permission_id      -> apt_permissions(id)     RESTRICT
//	apt_wiring_providers.object_type -> apt_object_types(name) RESTRICT
//	apt_principal_roles.principal_id -> apt_principals(id)   CASCADE
//	apt_role_permissions.role_id  -> apt_roles(id)           CASCADE
//	apt_group_members.group_id    -> apt_groups(id)          CASCADE
//	apt_wiring_provider_references.object_type
//	                              -> apt_wiring_providers(object_type) CASCADE
//
// A write naming a parent that does not exist is refused; a delete with a live
// child is refused (RESTRICT) or takes the child with it (CASCADE). The refusal
// is APERTURE_STORAGE_CONSTRAINT, the same code SQLite's driver error maps to in
// wrapStorage, under the same "<op>: ..." message shape — storage/storagetest
// allows no backend-conditional assertions, so a caller must not be able to tell
// which backend refused it.
//
// THE FOUR CASCADE EDGES. In SQLite those four child tables are real tables and
// the cascade deletes rows. Here the same four relationships are stored INSIDE
// the owning record — a principal's role list is model.Principal.RoleIDs, a
// role's permission list is model.Role.PermissionIDs, a group's member list is
// model.Group.MemberPrincipalIDs, and a wiring provider entry's reference rows
// are model.WiringProvider.References — so removing the owner from its map
// removes the child rows in the same statement, atomically, with no window in
// which a half-deleted owner is visible. That is what makes the cascade real
// rather than implied: after DeletePrincipal("alice") there is no lingering
// alice->role assignment anywhere for a later check to find, exactly as in
// SQLite, and the role she held becomes deletable.
//
// The wiring edge is where this backend and the SQL ones are asymmetric ON
// PURPOSE. The two SQL backends must NOT write the reference cleanup by hand —
// the schema's ON DELETE CASCADE is the only implementation there, because two
// halves that cover for each other leave the conformance suite green when either
// one breaks. This backend has no schema to do it, so holding the references
// inside the entry IS the hand-written half, and it is written in the shape that
// cannot drift: there is no separate reference map to forget to clear.
//
// ORDERING. Every check below runs BEFORE any map is touched, while s.mu is
// held. A refused operation therefore mutates nothing, and no check can ever
// read a partially applied delete — the two hazards the SQL backends get from
// running inside a transaction.
//
// THE THREE COLUMNS THAT CARRY NO FOREIGN KEY IN SQLITE EITHER are enforced
// here on exactly the same terms, because SQLite enforces them in Go too (see
// storage/sqlite/integrity.go). They are not a memory-backend special case:
//
//	apt_memberships.account_id -> apt_accounts(id) OR model.AccountWildcard
//	apt_grants.account_id      -> apt_accounts(id) OR model.AccountWildcard
//	apt_grants.subject_id      -> apt_principals | apt_roles | apt_groups,
//	                              whichever subject_kind selects
//
// The two account_id columns legitimately carry model.AccountWildcard ("*"),
// which ValidateAccount refuses to create as an account row, so no SQL dialect
// can express them as a reference — the rule is "an apt_accounts row OR exactly
// the wildcard", and hasAccountRefLocked's first line is what keeps the wildcard
// grant and the wildcard membership working. The grant subject is POLYMORPHIC:
// subject_kind picks the table, so subjectTargetLocked does the dispatch.
//
// Both are enforced in BOTH directions here as well: deleting an account still
// stamped on a membership or a grant is refused, and so is deleting a principal,
// role, or group a grant still names as its subject — the RESTRICT half those
// columns could not declare.

// constraint renders a referential refusal with the code and shape the SQLite
// backend produces for the same violation: APERTURE_STORAGE_CONSTRAINT, message
// prefixed by the same operation name wrapStorage would have used. The detail
// names the schema edge, so a reader can find the constraint that objected.
func constraint(op, format string, args ...any) error {
	return aerr.Newf(aerr.APERTURE_STORAGE_CONSTRAINT, op+": "+format, args...)
}

// keepLowest folds a candidate id into a running minimum. The child-lookup
// helpers below scan maps, whose iteration order is random; reporting the
// lowest-sorted offender makes a refusal message deterministic across runs.
func keepLowest(best string, found bool, candidate string) (string, bool) {
	if !found || candidate < best {
		return candidate, true
	}
	return best, true
}

// -- parent-side lookups (a write may not name a parent that does not exist) --

func (s *Store) hasPrincipalLocked(id string) bool {
	_, ok := s.principals[id]
	return ok
}

func (s *Store) hasRoleLocked(id string) bool {
	_, ok := s.roles[id]
	return ok
}

func (s *Store) hasPermissionLocked(id string) bool {
	_, ok := s.permissions[id]
	return ok
}

// hasAccountRefLocked reports whether an account_id is a legal reference: an
// existing apt_accounts row, OR the reserved model.AccountWildcard sentinel.
//
// The wildcard line is not an optimization — it is the rule. "*" is deliberately
// not an account row (ValidateAccount refuses to create one), so a plain
// existence check here would refuse every wildcard grant and every wildcard
// membership, both of which are shipped features the cross-account super-admin
// rides on.
func (s *Store) hasAccountRefLocked(accountID string) bool {
	if accountID == model.AccountWildcard {
		return true
	}
	_, ok := s.accounts[accountID]
	return ok
}

// subjectTargetLocked resolves a grant's polymorphic subject: subject_kind picks
// which table subject_id must exist in. It reports the SQL table name and the
// human noun for the refusal message, whether the target exists, and whether the
// kind was one it knows — false only for a kind ValidateGrant already refused,
// which cannot reach a write.
func (s *Store) subjectTargetLocked(sub model.Subject) (table, noun string, exists, known bool) {
	switch sub.Kind {
	case model.SubjectPrincipal:
		_, ok := s.principals[sub.ID]
		return "apt_principals", "principal", ok, true
	case model.SubjectRole:
		_, ok := s.roles[sub.ID]
		return "apt_roles", "role", ok, true
	case model.SubjectGroup:
		_, ok := s.groups[sub.ID]
		return "apt_groups", "group", ok, true
	}
	return "", "", false, false
}

// -- child-side lookups (a RESTRICT parent may not be deleted while cited) --

// membershipOfPrincipalLocked reports an account the principal is still a member
// of (apt_memberships.principal_id).
func (s *Store) membershipOfPrincipalLocked(principalID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for k := range s.memberships {
		if k.principalID == principalID {
			best, found = keepLowest(best, found, k.accountID)
		}
	}
	return best, found
}

// groupWithMemberLocked reports a group the principal is still a member of
// (apt_group_members.principal_id).
func (s *Store) groupWithMemberLocked(principalID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, g := range s.groups {
		for _, m := range g.MemberPrincipalIDs {
			if m == principalID {
				best, found = keepLowest(best, found, id)
				break
			}
		}
	}
	return best, found
}

// principalWithRoleLocked reports a principal that still holds the role
// (apt_principal_roles.role_id).
func (s *Store) principalWithRoleLocked(roleID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, p := range s.principals {
		for _, r := range p.RoleIDs {
			if r == roleID {
				best, found = keepLowest(best, found, id)
				break
			}
		}
	}
	return best, found
}

// roleWithPermissionLocked reports a role that still bundles the permission
// (apt_role_permissions.permission_id).
func (s *Store) roleWithPermissionLocked(permissionID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, r := range s.roles {
		for _, p := range r.PermissionIDs {
			if p == permissionID {
				best, found = keepLowest(best, found, id)
				break
			}
		}
	}
	return best, found
}

// grantWithPermissionLocked reports a grant that still cites the permission
// (apt_grants.permission_id).
func (s *Store) grantWithPermissionLocked(permissionID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, g := range s.grants {
		if g.PermissionID == permissionID {
			best, found = keepLowest(best, found, id)
		}
	}
	return best, found
}

// membershipOfAccountLocked reports a principal still a member of the account
// (apt_memberships.account_id).
func (s *Store) membershipOfAccountLocked(accountID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for k := range s.memberships {
		if k.accountID == accountID {
			best, found = keepLowest(best, found, k.principalID)
		}
	}
	return best, found
}

// grantOfAccountLocked reports a grant still stamped with the account
// (apt_grants.account_id).
func (s *Store) grantOfAccountLocked(accountID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, g := range s.grants {
		if g.AccountID == accountID {
			best, found = keepLowest(best, found, id)
		}
	}
	return best, found
}

// grantWithSubjectLocked reports a grant that still names (kind, id) as its
// subject (apt_grants.subject_id, under the kind that selects this table).
func (s *Store) grantWithSubjectLocked(kind model.SubjectKind, subjectID string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, g := range s.grants {
		if g.Subject.Kind == kind && g.Subject.ID == subjectID {
			best, found = keepLowest(best, found, id)
		}
	}
	return best, found
}

// permissionOfObjectTypeLocked reports a permission still hanging off the object
// type (apt_permissions.object_type).
func (s *Store) permissionOfObjectTypeLocked(name string) (string, bool) {
	var (
		best  string
		found bool
	)
	for id, p := range s.permissions {
		if p.ObjectType == name {
			best, found = keepLowest(best, found, id)
		}
	}
	return best, found
}

// hasObjectTypeLocked reports whether an object_type is a legal reference: an
// existing apt_object_types row. It is the parent-side lookup for
// apt_wiring_providers.object_type.
func (s *Store) hasObjectTypeLocked(name string) bool {
	_, ok := s.objectTypes[name]
	return ok
}

// wiringProviderOfObjectTypeLocked finds a wiring provider entry serving name —
// the child-side lookup for apt_wiring_providers.object_type's RESTRICT. The
// entry's key IS the object type, so the answer is a single map probe rather than
// a scan, and there is at most one.
func (s *Store) wiringProviderOfObjectTypeLocked(name string) (string, bool) {
	if _, ok := s.wiringProviders[name]; ok {
		return name, true
	}
	return "", false
}

// ---- Account ----

func (s *Store) PutAccount(_ context.Context, a model.Account) error {
	if err := model.ValidateAccount(a); err != nil {
		return err
	}
	if err := validateStamps(a.CreatedAt, a.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
	return nil
}

func (s *Store) GetAccount(_ context.Context, id string) (model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return model.Account{}, notFound("account", id)
	}
	return a, nil
}

func (s *Store) ListAccounts(_ context.Context) ([]model.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out, nil
}

func (s *Store) DeleteAccount(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return notFound("account", id)
	}
	// apt_memberships.account_id and apt_grants.account_id would be ON DELETE
	// RESTRICT if they could be foreign keys at all — they cannot, because both
	// carry model.AccountWildcard — so the RESTRICT half is enforced by hand, in
	// both backends. Without it, deleting an account silently orphans every
	// membership and every grant stamped with it.
	if principalID, ok := s.membershipOfAccountLocked(id); ok {
		return constraint("delete account",
			"apt_memberships.account_id references apt_accounts(id): principal %q is still a member of account %q",
			principalID, id)
	}
	if grantID, ok := s.grantOfAccountLocked(id); ok {
		return constraint("delete account",
			"apt_grants.account_id references apt_accounts(id): grant %q is still stamped with account %q",
			grantID, id)
	}
	delete(s.accounts, id)
	return nil
}

// ---- Membership ----

func (s *Store) PutMembership(_ context.Context, m model.Membership) error {
	if err := model.ValidateMembership(m); err != nil {
		return err
	}
	if err := validateStamps(m.CreatedAt, m.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_memberships.account_id -> apt_accounts(id) OR model.AccountWildcard.
	// Checked before the principal edge because that is the order the SQLite
	// backend runs them in: its Go check precedes the INSERT the foreign key
	// fires on.
	if !s.hasAccountRefLocked(m.AccountID) {
		return constraint("put membership",
			"apt_memberships.account_id references apt_accounts(id): account %q does not exist",
			m.AccountID)
	}
	// apt_memberships.principal_id -> apt_principals(id).
	if !s.hasPrincipalLocked(m.PrincipalID) {
		return constraint("put membership",
			"apt_memberships.principal_id references apt_principals(id): principal %q does not exist",
			m.PrincipalID)
	}
	s.memberships[membershipKey{m.PrincipalID, m.AccountID}] = m
	return nil
}

func (s *Store) GetMembership(_ context.Context, principalID, accountID string) (model.Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.memberships[membershipKey{principalID, accountID}]
	if !ok {
		return model.Membership{}, notFound("membership", principalID+"@"+accountID)
	}
	return m, nil
}

func (s *Store) DeleteMembership(_ context.Context, principalID, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := membershipKey{principalID, accountID}
	if _, ok := s.memberships[key]; !ok {
		return notFound("membership", principalID+"@"+accountID)
	}
	delete(s.memberships, key)
	return nil
}

func (s *Store) MembershipsForPrincipal(_ context.Context, principalID string) ([]model.Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Membership, 0)
	for k, m := range s.memberships {
		if k.principalID == principalID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) MembershipsForAccount(_ context.Context, accountID string) ([]model.Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Membership, 0)
	for k, m := range s.memberships {
		if k.accountID == accountID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) IsMember(_ context.Context, principalID, accountID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.memberships[membershipKey{principalID, accountID}]
	return ok, nil
}

// ---- ObjectType ----

func (s *Store) PutObjectType(_ context.Context, ot model.ObjectType) error {
	if err := model.ValidateObjectType(ot); err != nil {
		return err
	}
	if err := validateStamps(ot.CreatedAt, ot.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ot.Actions = cloneStrings(ot.Actions)
	s.objectTypes[ot.Name] = ot
	return nil
}

func (s *Store) GetObjectType(_ context.Context, name string) (model.ObjectType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ot, ok := s.objectTypes[name]
	if !ok {
		return model.ObjectType{}, notFound("object type", name)
	}
	ot.Actions = cloneStrings(ot.Actions)
	return ot, nil
}

func (s *Store) ListObjectTypes(_ context.Context) ([]model.ObjectType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.ObjectType, 0, len(s.objectTypes))
	for _, ot := range s.objectTypes {
		ot.Actions = cloneStrings(ot.Actions)
		out = append(out, ot)
	}
	return out, nil
}

func (s *Store) DeleteObjectType(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objectTypes[name]; !ok {
		return notFound("object type", name)
	}
	// apt_permissions.object_type -> apt_object_types(name), ON DELETE RESTRICT.
	// The object type is what makes a permission's action verb legal, so a
	// permission may not outlive it.
	if permID, ok := s.permissionOfObjectTypeLocked(name); ok {
		return constraint("delete object type",
			"apt_permissions.object_type references apt_object_types(name): permission %q still hangs off object type %q",
			permID, name)
	}
	// apt_wiring_providers.object_type -> apt_object_types(name), ON DELETE
	// RESTRICT. A provider entry for a type the model no longer has is wiring
	// nothing can reach — every decision arrives at it through a permission, which
	// is keyed to an object type by the edge just above. Refusing the delete makes
	// the operator remove the wiring on purpose, instead of discovering afterwards
	// that a push-and-read-back round trip quietly lost an entry.
	if wiringType, ok := s.wiringProviderOfObjectTypeLocked(name); ok {
		return constraint("delete object type",
			"apt_wiring_providers.object_type references apt_object_types(name): wiring provider %q still serves object type %q",
			wiringType, name)
	}
	delete(s.objectTypes, name)
	return nil
}

// ---- Permission ----

func (s *Store) PutPermission(_ context.Context, p model.Permission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_permissions.object_type -> apt_object_types(name), write direction.
	//
	// This is the one edge whose write side is NOT_FOUND rather than
	// APERTURE_STORAGE_CONSTRAINT, in BOTH backends: the object type has to be
	// read anyway to validate the permission's action verb against it, so
	// sqlite.PutPermission fails on GetObjectType long before the foreign key
	// could object. Reporting a constraint here would be the divergence, not the
	// parity.
	ot, ok := s.objectTypes[p.ObjectType]
	if !ok {
		return notFound("object type", p.ObjectType)
	}
	if err := model.ValidatePermission(p, ot); err != nil {
		return err
	}
	if err := validateStamps(p.CreatedAt, p.UpdatedAt); err != nil {
		return err
	}
	s.permissions[p.ID] = p
	return nil
}

func (s *Store) GetPermission(_ context.Context, id string) (model.Permission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.permissions[id]
	if !ok {
		return model.Permission{}, notFound("permission", id)
	}
	return p, nil
}

func (s *Store) ListPermissions(_ context.Context) ([]model.Permission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Permission, 0, len(s.permissions))
	for _, p := range s.permissions {
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) DeletePermission(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.permissions[id]; !ok {
		return notFound("permission", id)
	}
	// apt_role_permissions.permission_id -> apt_permissions(id), ON DELETE
	// RESTRICT: many roles may cite one permission, and none of them owns it.
	if roleID, ok := s.roleWithPermissionLocked(id); ok {
		return constraint("delete permission",
			"apt_role_permissions.permission_id references apt_permissions(id): role %q still bundles permission %q",
			roleID, id)
	}
	// apt_grants.permission_id -> apt_permissions(id), ON DELETE RESTRICT: a
	// grant is authority, and authority citing a deleted permission is authority
	// nobody can read or revoke.
	if grantID, ok := s.grantWithPermissionLocked(id); ok {
		return constraint("delete permission",
			"apt_grants.permission_id references apt_permissions(id): grant %q still cites permission %q",
			grantID, id)
	}
	delete(s.permissions, id)
	return nil
}

// ---- Principal ----

func (s *Store) PutPrincipal(_ context.Context, p model.Principal) error {
	if err := model.ValidatePrincipal(p); err != nil {
		return err
	}
	if err := validateStamps(p.CreatedAt, p.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_principal_roles.role_id -> apt_roles(id): every role assignment this
	// principal carries is a join row, and a join row may not name a role that
	// does not exist. Checked for the WHOLE list before anything is stored, so a
	// principal is never written with half its assignments.
	for _, roleID := range p.RoleIDs {
		if !s.hasRoleLocked(roleID) {
			return constraint("put principal",
				"apt_principal_roles.role_id references apt_roles(id): role %q does not exist",
				roleID)
		}
	}
	p.RoleIDs = cloneStrings(p.RoleIDs)
	s.principals[p.ID] = p
	return nil
}

func (s *Store) GetPrincipal(_ context.Context, id string) (model.Principal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.principals[id]
	if !ok {
		return model.Principal{}, notFound("principal", id)
	}
	p.RoleIDs = cloneStrings(p.RoleIDs)
	return p, nil
}

func (s *Store) ListPrincipals(_ context.Context) ([]model.Principal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Principal, 0, len(s.principals))
	for _, p := range s.principals {
		p.RoleIDs = cloneStrings(p.RoleIDs)
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) DeletePrincipal(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.principals[id]; !ok {
		return notFound("principal", id)
	}
	// apt_memberships.principal_id -> apt_principals(id), ON DELETE RESTRICT: a
	// principal outlives its memberships, so it may not go while an edge stands.
	if accountID, ok := s.membershipOfPrincipalLocked(id); ok {
		return constraint("delete principal",
			"apt_memberships.principal_id references apt_principals(id): principal %q is still a member of account %q",
			id, accountID)
	}
	// apt_group_members.principal_id -> apt_principals(id), ON DELETE RESTRICT: a
	// principal exists independently of any group, so the group must let go first.
	if groupID, ok := s.groupWithMemberLocked(id); ok {
		return constraint("delete principal",
			"apt_group_members.principal_id references apt_principals(id): principal %q is still a member of group %q",
			id, groupID)
	}
	// apt_grants.subject_id -> apt_principals(id), for the grants whose
	// subject_kind is "principal". Polymorphic, so no foreign key expresses it in
	// either backend; the RESTRICT it would have carried is this check. A grant is
	// authority, and authority naming a subject that no longer exists is authority
	// nobody can read or revoke.
	if grantID, ok := s.grantWithSubjectLocked(model.SubjectPrincipal, id); ok {
		return constraint("delete principal",
			"apt_grants.subject_id references apt_principals(id) when subject_kind is %q: grant %q still names principal %q",
			string(model.SubjectPrincipal), grantID, id)
	}
	// apt_principal_roles.principal_id -> apt_principals(id), ON DELETE CASCADE:
	// the principal OWNS its role assignments, so they go with it. Here they live
	// in the record itself (p.RoleIDs), so this one delete removes both, leaving
	// nothing for a later check to trip over — the role she held is now deletable.
	delete(s.principals, id)
	return nil
}

// ---- Role ----

func (s *Store) PutRole(_ context.Context, r model.Role) error {
	if err := model.ValidateRole(r); err != nil {
		return err
	}
	if err := validateStamps(r.CreatedAt, r.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_role_permissions.permission_id -> apt_permissions(id): a role's bundle
	// is join rows, and none of them may name a permission that does not exist.
	for _, permID := range r.PermissionIDs {
		if !s.hasPermissionLocked(permID) {
			return constraint("put role",
				"apt_role_permissions.permission_id references apt_permissions(id): permission %q does not exist",
				permID)
		}
	}
	r.PermissionIDs = cloneStrings(r.PermissionIDs)
	s.roles[r.ID] = r
	return nil
}

func (s *Store) GetRole(_ context.Context, id string) (model.Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.roles[id]
	if !ok {
		return model.Role{}, notFound("role", id)
	}
	r.PermissionIDs = cloneStrings(r.PermissionIDs)
	return r, nil
}

func (s *Store) ListRoles(_ context.Context) ([]model.Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Role, 0, len(s.roles))
	for _, r := range s.roles {
		r.PermissionIDs = cloneStrings(r.PermissionIDs)
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) DeleteRole(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roles[id]; !ok {
		return notFound("role", id)
	}
	// apt_principal_roles.role_id -> apt_roles(id), ON DELETE RESTRICT: a role is
	// a shared entity a principal merely points at, so an assigned role stays.
	if principalID, ok := s.principalWithRoleLocked(id); ok {
		return constraint("delete role",
			"apt_principal_roles.role_id references apt_roles(id): principal %q still holds role %q",
			principalID, id)
	}
	// apt_grants.subject_id -> apt_roles(id), for the grants whose subject_kind
	// is "role". See DeletePrincipal.
	if grantID, ok := s.grantWithSubjectLocked(model.SubjectRole, id); ok {
		return constraint("delete role",
			"apt_grants.subject_id references apt_roles(id) when subject_kind is %q: grant %q still names role %q",
			string(model.SubjectRole), grantID, id)
	}
	// apt_role_permissions.role_id -> apt_roles(id), ON DELETE CASCADE: the role
	// OWNS its permission bundle (r.PermissionIDs here), so it goes with the role
	// in the same delete. The permissions themselves are untouched — only the
	// bundle rows were the role's — and each becomes deletable once nothing else
	// cites it.
	delete(s.roles, id)
	return nil
}

// ---- Group ----

func (s *Store) PutGroup(_ context.Context, g model.Group) error {
	if err := model.ValidateGroup(g); err != nil {
		return err
	}
	if err := validateStamps(g.CreatedAt, g.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_group_members.principal_id -> apt_principals(id): every member row must
	// name a principal that exists. The whole list is checked before the group is
	// stored, so a group is never written with a phantom member in it.
	for _, principalID := range g.MemberPrincipalIDs {
		if !s.hasPrincipalLocked(principalID) {
			return constraint("put group",
				"apt_group_members.principal_id references apt_principals(id): principal %q does not exist",
				principalID)
		}
	}
	g.MemberPrincipalIDs = cloneStrings(g.MemberPrincipalIDs)
	s.groups[g.ID] = g
	return nil
}

func (s *Store) GetGroup(_ context.Context, id string) (model.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return model.Group{}, notFound("group", id)
	}
	g.MemberPrincipalIDs = cloneStrings(g.MemberPrincipalIDs)
	return g, nil
}

func (s *Store) ListGroups(_ context.Context) ([]model.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Group, 0, len(s.groups))
	for _, g := range s.groups {
		g.MemberPrincipalIDs = cloneStrings(g.MemberPrincipalIDs)
		out = append(out, g)
	}
	return out, nil
}

func (s *Store) DeleteGroup(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[id]; !ok {
		return notFound("group", id)
	}
	// apt_grants.subject_id -> apt_groups(id), for the grants whose subject_kind
	// is "group". See DeletePrincipal. This is the one thing that DOES point at a
	// group, so the "nothing points AT a group" note below is about the schema's
	// foreign keys, not about this check.
	if grantID, ok := s.grantWithSubjectLocked(model.SubjectGroup, id); ok {
		return constraint("delete group",
			"apt_grants.subject_id references apt_groups(id) when subject_kind is %q: grant %q still names group %q",
			string(model.SubjectGroup), grantID, id)
	}
	// apt_group_members.group_id -> apt_groups(id), ON DELETE CASCADE: a member
	// row has no meaning without its group, so the group takes its membership
	// list (g.MemberPrincipalIDs) with it. Nothing points AT a group, so this
	// delete has no RESTRICT edge to check — and the principals that were members
	// survive, each becoming deletable once no other group or membership holds it.
	delete(s.groups, id)
	return nil
}

// ---- Grant ----

func (s *Store) PutGrant(_ context.Context, g model.Grant) error {
	if err := model.ValidateGrant(g); err != nil {
		return err
	}
	if err := validateStamps(g.CreatedAt, g.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A grant makes three references, checked in the order the SQLite backend
	// runs them: its two Go checks, then the INSERT its permission_id foreign key
	// fires on.
	//
	// apt_grants.account_id -> apt_accounts(id) OR model.AccountWildcard.
	if !s.hasAccountRefLocked(g.AccountID) {
		return constraint("put grant",
			"apt_grants.account_id references apt_accounts(id): account %q does not exist",
			g.AccountID)
	}
	// apt_grants.subject_id -> whichever table subject_kind selects.
	if table, noun, exists, known := s.subjectTargetLocked(g.Subject); known && !exists {
		return constraint("put grant",
			"apt_grants.subject_id references %s(id) when subject_kind is %q: %s %q does not exist",
			table, string(g.Subject.Kind), noun, g.Subject.ID)
	}
	// apt_grants.permission_id -> apt_permissions(id).
	if !s.hasPermissionLocked(g.PermissionID) {
		return constraint("put grant",
			"apt_grants.permission_id references apt_permissions(id): permission %q does not exist",
			g.PermissionID)
	}
	s.grants[g.ID] = g
	return nil
}

func (s *Store) GetGrant(_ context.Context, id string) (model.Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.grants[id]
	if !ok {
		return model.Grant{}, notFound("grant", id)
	}
	return g, nil
}

func (s *Store) ListGrants(_ context.Context, accountID string) ([]model.Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Grant, 0)
	for _, g := range s.grants {
		if g.AccountID == accountID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Store) ListGrantsPage(_ context.Context, accountID string, offset, limit int) ([]model.Grant, int, error) {
	offset, limit = model.ClampGrantPage(offset, limit)
	allAccounts := accountID == model.AllAccounts
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Collect every matching grant, then order deterministically before paginating
	// so pages are stable across calls (map iteration order is not).
	matched := make([]model.Grant, 0)
	for _, g := range s.grants {
		// AllAccounts spans every account (wildcard "*" rows included inline);
		// otherwise match the single account exactly, like ListGrants.
		if allAccounts || g.AccountID == accountID {
			matched = append(matched, g)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].AccountID != matched[j].AccountID {
			return matched[i].AccountID < matched[j].AccountID
		}
		return matched[i].ID < matched[j].ID
	})
	total := len(matched)
	// Apply the offset/limit window; an offset past the end yields an empty page.
	if offset >= total {
		return make([]model.Grant, 0), total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := make([]model.Grant, end-offset)
	copy(page, matched[offset:end])
	return page, total, nil
}

func (s *Store) DeleteGrant(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.grants[id]; !ok {
		return notFound("grant", id)
	}
	delete(s.grants, id)
	return nil
}

// ---- Decision-engine queries ----

func (s *Store) GrantsForSubjects(_ context.Context, accountID string, subjects []model.Subject) ([]model.Grant, error) {
	if len(subjects) == 0 {
		return []model.Grant{}, nil
	}
	want := make(map[model.Subject]struct{}, len(subjects))
	for _, sub := range subjects {
		want[sub] = struct{}{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Grant, 0)
	for _, g := range s.grants {
		// A grant matches when stamped to the active account OR to the all-accounts
		// wildcard; the wildcard is the one grant that crosses the account boundary.
		if g.AccountID != accountID && g.AccountID != model.AccountWildcard {
			continue
		}
		if _, ok := want[g.Subject]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Store) GroupsForPrincipal(_ context.Context, principalID string) ([]model.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Group, 0)
	for _, g := range s.groups {
		for _, m := range g.MemberPrincipalIDs {
			if m == principalID {
				gc := g
				gc.MemberPrincipalIDs = cloneStrings(g.MemberPrincipalIDs)
				out = append(out, gc)
				break
			}
		}
	}
	return out, nil
}

// ---- Template (named, versioned) ----

func (s *Store) PutTemplate(_ context.Context, t model.Template) error {
	if err := model.ValidateTemplate(t); err != nil {
		return err
	}
	if err := validateStamps(t.CreatedAt, t.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templates[templateKey{t.Name, t.Version}] = cloneTemplate(t)
	return nil
}

func (s *Store) GetTemplate(_ context.Context, name string, version int) (model.Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version > 0 {
		t, ok := s.templates[templateKey{name, version}]
		if !ok {
			return model.Template{}, notFound("template", name+":v"+itoa(version))
		}
		return cloneTemplate(t), nil
	}
	// version <= 0: select the latest (highest) version of name.
	best := -1
	var found model.Template
	for k, t := range s.templates {
		if k.name == name && k.version > best {
			best = k.version
			found = t
		}
	}
	if best < 0 {
		return model.Template{}, notFound("template", name)
	}
	return cloneTemplate(found), nil
}

func (s *Store) ListTemplates(_ context.Context) ([]model.Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Template, 0, len(s.templates))
	for _, t := range s.templates {
		out = append(out, cloneTemplate(t))
	}
	model.SortTemplates(out)
	return out, nil
}

func (s *Store) DeleteTemplate(_ context.Context, name string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version > 0 {
		key := templateKey{name, version}
		if _, ok := s.templates[key]; !ok {
			return notFound("template", name+":v"+itoa(version))
		}
		delete(s.templates, key)
		return nil
	}
	// version <= 0: delete every version of name.
	removed := 0
	for k := range s.templates {
		if k.name == name {
			delete(s.templates, k)
			removed++
		}
	}
	if removed == 0 {
		return notFound("template", name)
	}
	return nil
}

// ---- Rule (named) ----

func (s *Store) PutRule(_ context.Context, r model.Rule) error {
	if err := model.ValidateRule(r); err != nil {
		return err
	}
	if err := validateStamps(r.CreatedAt, r.UpdatedAt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[r.Name] = cloneRule(r)
	return nil
}

func (s *Store) GetRule(_ context.Context, name string) (model.Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[name]
	if !ok {
		return model.Rule{}, notFound("rule", name)
	}
	return cloneRule(r), nil
}

func (s *Store) ListRules(_ context.Context) ([]model.Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Rule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, cloneRule(r))
	}
	model.SortRules(out)
	return out, nil
}

func (s *Store) DeleteRule(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[name]; !ok {
		return notFound("rule", name)
	}
	delete(s.rules, name)
	return nil
}

// ---- Shared wiring (five tables, one all-or-nothing replace) ----
//
// Four maps for five tables: a provider entry's reference rows live inside the
// entry, which is this backend's hand-written form of the
// apt_wiring_provider_references CASCADE. See "THE FOUR CASCADE EDGES" in the
// referential-integrity header above — including why the SQL backends must NOT
// write that cleanup by hand and this one must.

// ReplaceWiring replaces the whole wiring set. The order of the three phases is
// the contract, not an implementation detail: it is the order the SQL backends
// produce by construction, and storagetest allows no backend-conditional
// assertion, so a caller must not be able to tell which backend refused a bad
// set.
//
//  1. STRUCTURAL validation of the whole set (model.ValidateWiringSet) —
//     APERTURE_INVALID_INPUT for an empty key, a negative max_size, a duplicate.
//  2. STAMP validation of every entity — APERTURE_INVALID_INPUT. The SQL
//     backends encode every stamp before they open a transaction for the same
//     reason: which row is refused must not depend on which one happened to be
//     written first.
//  3. REFERENTIAL checks — APERTURE_STORAGE_CONSTRAINT, which is what the SQL
//     foreign key raises at the INSERT.
//
// Only then is anything written, and the write builds fresh maps and swaps them
// in under the same lock, so a refusal at any phase leaves the old wiring exactly
// as it was. That is the all-or-nothing the push surface depends on.
func (s *Store) ReplaceWiring(_ context.Context, set model.WiringSet) error {
	if err := model.ValidateWiringSet(set); err != nil {
		return err
	}
	for _, c := range set.Connections {
		if err := validateStamps(c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
	}
	for _, p := range set.Providers {
		if err := validateStamps(p.CreatedAt, p.UpdatedAt); err != nil {
			return err
		}
	}
	for _, ft := range set.FieldTypes {
		if err := validateStamps(ft.CreatedAt, ft.UpdatedAt); err != nil {
			return err
		}
	}
	for _, ap := range set.AttributeProviders {
		if err := validateStamps(ap.CreatedAt, ap.UpdatedAt); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// apt_wiring_providers.object_type -> apt_object_types(name), write direction.
	for _, p := range set.Providers {
		if !s.hasObjectTypeLocked(p.ObjectType) {
			return constraint("replace wiring",
				"apt_wiring_providers.object_type references apt_object_types(name): object type %q does not exist",
				p.ObjectType)
		}
	}

	connections := make(map[string]model.WiringConnection, len(set.Connections))
	for _, c := range set.Connections {
		connections[c.Name] = c
	}
	providers := make(map[string]model.WiringProvider, len(set.Providers))
	for _, p := range set.Providers {
		providers[p.ObjectType] = cloneWiringProvider(p)
	}
	fieldTypes := make(map[wiringFieldKey]model.WiringFieldType, len(set.FieldTypes))
	for _, ft := range set.FieldTypes {
		fieldTypes[wiringFieldKey{ft.ObjectType, ft.Field}] = ft
	}
	attributes := make(map[string]model.WiringAttributeProvider, len(set.AttributeProviders))
	for _, ap := range set.AttributeProviders {
		attributes[ap.Subject] = cloneWiringAttributeProvider(ap)
	}
	s.wiringConnections = connections
	s.wiringProviders = providers
	s.wiringFieldTypes = fieldTypes
	s.wiringAttributeProviders = attributes
	return nil
}

// GetWiring returns the whole set from one lock acquisition, which is this
// backend's form of the SQL backends' single transaction: no push can land
// between two of the four sections.
func (s *Store) GetWiring(_ context.Context) (model.WiringSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := model.WiringSet{
		Connections:        s.wiringConnectionsLocked(),
		Providers:          s.wiringProvidersLocked(),
		FieldTypes:         s.wiringFieldTypesLocked(),
		AttributeProviders: s.wiringAttributeProvidersLocked(),
	}
	return set, nil
}

func (s *Store) ListWiringConnections(_ context.Context) ([]model.WiringConnection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wiringConnectionsLocked(), nil
}

func (s *Store) wiringConnectionsLocked() []model.WiringConnection {
	out := make([]model.WiringConnection, 0, len(s.wiringConnections))
	for _, c := range s.wiringConnections {
		out = append(out, c)
	}
	model.SortWiringConnections(out)
	return out
}

func (s *Store) GetWiringConnection(_ context.Context, name string) (model.WiringConnection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.wiringConnections[name]
	if !ok {
		return model.WiringConnection{}, notFound("wiring connection", name)
	}
	return c, nil
}

func (s *Store) ListWiringProviders(_ context.Context) ([]model.WiringProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wiringProvidersLocked(), nil
}

func (s *Store) wiringProvidersLocked() []model.WiringProvider {
	out := make([]model.WiringProvider, 0, len(s.wiringProviders))
	for _, p := range s.wiringProviders {
		out = append(out, cloneWiringProvider(p))
	}
	model.SortWiringProviders(out)
	return out
}

func (s *Store) GetWiringProvider(_ context.Context, objectType string) (model.WiringProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.wiringProviders[objectType]
	if !ok {
		return model.WiringProvider{}, notFound("wiring provider", objectType)
	}
	p = cloneWiringProvider(p)
	model.SortWiringReferences(p.References)
	return p, nil
}

func (s *Store) ListWiringFieldTypes(_ context.Context) ([]model.WiringFieldType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wiringFieldTypesLocked(), nil
}

func (s *Store) wiringFieldTypesLocked() []model.WiringFieldType {
	out := make([]model.WiringFieldType, 0, len(s.wiringFieldTypes))
	for _, ft := range s.wiringFieldTypes {
		out = append(out, ft)
	}
	model.SortWiringFieldTypes(out)
	return out
}

func (s *Store) GetWiringFieldType(_ context.Context, objectType, field string) (model.WiringFieldType, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ft, ok := s.wiringFieldTypes[wiringFieldKey{objectType, field}]
	if !ok {
		return model.WiringFieldType{}, notFound("wiring field type", objectType+"."+field)
	}
	return ft, nil
}

func (s *Store) ListWiringAttributeProviders(_ context.Context) ([]model.WiringAttributeProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wiringAttributeProvidersLocked(), nil
}

func (s *Store) wiringAttributeProvidersLocked() []model.WiringAttributeProvider {
	out := make([]model.WiringAttributeProvider, 0, len(s.wiringAttributeProviders))
	for _, ap := range s.wiringAttributeProviders {
		out = append(out, cloneWiringAttributeProvider(ap))
	}
	model.SortWiringAttributeProviders(out)
	return out
}

func (s *Store) GetWiringAttributeProvider(_ context.Context, subject string) (model.WiringAttributeProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ap, ok := s.wiringAttributeProviders[subject]
	if !ok {
		return model.WiringAttributeProvider{}, notFound("wiring attribute provider", subject)
	}
	return cloneWiringAttributeProvider(ap), nil
}

// cloneWiringProvider deep-copies an entry's reference rows so a stored entry
// cannot be mutated through a caller's slice (and vice versa).
func cloneWiringProvider(p model.WiringProvider) model.WiringProvider {
	if len(p.References) > 0 {
		rs := make([]model.WiringReference, len(p.References))
		copy(rs, p.References)
		p.References = rs
	} else {
		p.References = nil
	}
	return p
}

// cloneWiringAttributeProvider deep-copies an entry's declared key set.
//
// It goes through model.DeclaredKeys.Clone rather than cloneStrings on purpose:
// cloneStrings returns nil for an empty input, which would turn a DECLARED EMPTY
// key set into a NOT DECLARED one — the one collapse the DeclaredKeys type exists
// to prevent, and the reason the type carries an explicit Declared bit instead of
// relying on nil-versus-empty.
func cloneWiringAttributeProvider(ap model.WiringAttributeProvider) model.WiringAttributeProvider {
	ap.DeclaredKeys = ap.DeclaredKeys.Clone()
	return ap
}

// ---- Transactional apply ----

// Atomic stages the whole batch on a snapshot and commits it only when fn
// succeeds. The parent store is locked for the entire transaction (serializing
// transactions), and fn operates on a CHILD store holding copies of every map —
// so the child's own writes never touch the parent until commit, giving real
// rollback: if fn errors, the child is discarded and the parent is unchanged. A
// nested Atomic (s is already a child) flattens into the current staging buffer.
func (s *Store) Atomic(ctx context.Context, fn func(tx model.Storage) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.snapshotLocked()
	if err := fn(child); err != nil {
		// Discard the child entirely: the parent's maps were never touched.
		return err
	}
	s.commitFromLocked(child)
	return nil
}

// snapshotLocked builds a child Store with a deep-enough copy of every map so
// the child can be mutated without affecting the parent. The caller holds s.mu.
func (s *Store) snapshotLocked() *Store {
	c := New()
	for k, v := range s.accounts {
		c.accounts[k] = v
	}
	for k, v := range s.memberships {
		c.memberships[k] = v
	}
	for k, v := range s.objectTypes {
		v.Actions = cloneStrings(v.Actions)
		c.objectTypes[k] = v
	}
	for k, v := range s.permissions {
		c.permissions[k] = v
	}
	for k, v := range s.principals {
		v.RoleIDs = cloneStrings(v.RoleIDs)
		c.principals[k] = v
	}
	for k, v := range s.roles {
		v.PermissionIDs = cloneStrings(v.PermissionIDs)
		c.roles[k] = v
	}
	for k, v := range s.groups {
		v.MemberPrincipalIDs = cloneStrings(v.MemberPrincipalIDs)
		c.groups[k] = v
	}
	for k, v := range s.grants {
		c.grants[k] = v
	}
	for k, v := range s.templates {
		c.templates[k] = cloneTemplate(v)
	}
	for k, v := range s.rules {
		c.rules[k] = cloneRule(v)
	}
	for k, v := range s.wiringConnections {
		c.wiringConnections[k] = v
	}
	for k, v := range s.wiringProviders {
		c.wiringProviders[k] = cloneWiringProvider(v)
	}
	for k, v := range s.wiringFieldTypes {
		c.wiringFieldTypes[k] = v
	}
	for k, v := range s.wiringAttributeProviders {
		c.wiringAttributeProviders[k] = cloneWiringAttributeProvider(v)
	}
	c.audit = make([]model.AuditEvent, len(s.audit))
	copy(c.audit, s.audit)
	return c
}

// commitFromLocked replaces the parent's maps with the committed child's. The
// caller holds s.mu; the child is never used again so the maps can be adopted
// directly.
func (s *Store) commitFromLocked(c *Store) {
	s.accounts = c.accounts
	s.memberships = c.memberships
	s.objectTypes = c.objectTypes
	s.permissions = c.permissions
	s.principals = c.principals
	s.roles = c.roles
	s.groups = c.groups
	s.grants = c.grants
	s.templates = c.templates
	s.rules = c.rules
	s.wiringConnections = c.wiringConnections
	s.wiringProviders = c.wiringProviders
	s.wiringFieldTypes = c.wiringFieldTypes
	s.wiringAttributeProviders = c.wiringAttributeProviders
	s.audit = c.audit
}

// cloneTemplate deep-copies a template's slices so a stored template cannot be
// mutated by the caller (and vice versa).
func cloneTemplate(t model.Template) model.Template {
	if len(t.Params) > 0 {
		ps := make([]model.TemplateParam, len(t.Params))
		copy(ps, t.Params)
		t.Params = ps
	} else {
		t.Params = nil
	}
	if len(t.Grants) > 0 {
		gs := make([]model.TemplateGrant, len(t.Grants))
		copy(gs, t.Grants)
		t.Grants = gs
	} else {
		t.Grants = nil
	}
	return t
}

// cloneRule deep-copies a rule's AST bytes so a stored rule cannot be mutated by
// the caller (and vice versa).
func cloneRule(r model.Rule) model.Rule {
	if len(r.AST) > 0 {
		ast := make([]byte, len(r.AST))
		copy(ast, r.AST)
		r.AST = ast
	} else {
		r.AST = nil
	}
	return r
}

// itoa is a tiny strconv.Itoa alias kept local to avoid importing strconv for a
// single not-found message helper.
func itoa(n int) string {
	return strconv.Itoa(n)
}

// ---- Audit trail (append-only) ----

// AppendAudit validates the instant even though this backend keeps a time.Time
// rather than encoding it: the storable range is a property of the storage
// contract, not of one dialect's encoding, so every backend must refuse the same
// instants (storage/storagetest allows no backend-conditional assertions).
func (s *Store) AppendAudit(_ context.Context, ev model.AuditEvent) error {
	if err := storagetime.Validate(ev.Timestamp); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Details = cloneDetails(ev.Details)
	s.audit = append(s.audit, ev)
	return nil
}

func (s *Store) QueryAudit(_ context.Context, filter model.AuditFilter) ([]model.AuditEvent, error) {
	if err := storagetime.Validate(filter.Since); err != nil {
		return nil, err
	}
	if err := storagetime.Validate(filter.Until); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.AuditEvent, 0)
	for _, ev := range s.audit {
		if filter.Matches(ev) {
			ev.Details = cloneDetails(ev.Details)
			out = append(out, ev)
		}
	}
	sortAuditDesc(out)
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (s *Store) PruneAudit(_ context.Context, policy model.RetentionPolicy) (int, error) {
	if err := storagetime.Validate(policy.Before); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.audit)

	// Age bound: drop events strictly older than policy.Before.
	if !policy.Before.IsZero() {
		kept := s.audit[:0:0]
		for _, ev := range s.audit {
			if ev.Timestamp.Before(policy.Before) {
				continue
			}
			kept = append(kept, ev)
		}
		s.audit = kept
	}

	// Size bound: keep only the newest MaxCount events. Order the survivors
	// newest-first, truncate, then restore insertion (chronological) order so the
	// trail stays append-ordered.
	if policy.MaxCount > 0 && len(s.audit) > policy.MaxCount {
		ordered := make([]model.AuditEvent, len(s.audit))
		copy(ordered, s.audit)
		sortAuditDesc(ordered)
		ordered = ordered[:policy.MaxCount]
		keep := make(map[string]struct{}, len(ordered))
		for _, ev := range ordered {
			keep[ev.ID] = struct{}{}
		}
		kept := s.audit[:0:0]
		for _, ev := range s.audit {
			if _, ok := keep[ev.ID]; ok {
				kept = append(kept, ev)
			}
		}
		s.audit = kept
	}

	return before - len(s.audit), nil
}

// sortAuditDesc orders events newest-first by (timestamp, id) so the in-memory
// and SQLite backends return audit queries in one identical order.
func sortAuditDesc(evs []model.AuditEvent) {
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].Timestamp.Equal(evs[j].Timestamp) {
			return evs[i].Timestamp.After(evs[j].Timestamp)
		}
		return evs[i].ID > evs[j].ID
	})
}

// cloneDetails shallow-copies an audit details map so a stored event cannot be
// mutated by the caller (and vice versa). Returns nil for an empty map.
func cloneDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneStrings returns a defensive copy so callers cannot mutate stored slices
// (and stored slices cannot mutate caller-held ones). Returns nil for empty
// input to keep round-tripped values comparable.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
