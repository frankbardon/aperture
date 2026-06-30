// Package memory is a map-backed, concurrency-safe implementation of
// model.Storage. It is the backend used for tests, seeding, and any deployment
// that does not need durability. It enforces the same validation and
// typed-action rules as the SQLite reference backend, so the shared conformance
// suite (storage/storagetest) passes against both unchanged.
package memory

import (
	"context"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
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
}

// membershipKey is the composite identity of a membership edge.
type membershipKey struct {
	principalID string
	accountID   string
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

// ---- Account ----

func (s *Store) PutAccount(_ context.Context, a model.Account) error {
	if err := model.ValidateAccount(a); err != nil {
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
	delete(s.accounts, id)
	return nil
}

// ---- Membership ----

func (s *Store) PutMembership(_ context.Context, m model.Membership) error {
	if err := model.ValidateMembership(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	delete(s.objectTypes, name)
	return nil
}

// ---- Permission ----

func (s *Store) PutPermission(_ context.Context, p model.Permission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ot, ok := s.objectTypes[p.ObjectType]
	if !ok {
		return notFound("object type", p.ObjectType)
	}
	if err := model.ValidatePermission(p, ot); err != nil {
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
	delete(s.permissions, id)
	return nil
}

// ---- Principal ----

func (s *Store) PutPrincipal(_ context.Context, p model.Principal) error {
	if err := model.ValidatePrincipal(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	delete(s.principals, id)
	return nil
}

// ---- Role ----

func (s *Store) PutRole(_ context.Context, r model.Role) error {
	if err := model.ValidateRole(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	delete(s.roles, id)
	return nil
}

// ---- Group ----

func (s *Store) PutGroup(_ context.Context, g model.Group) error {
	if err := model.ValidateGroup(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	delete(s.groups, id)
	return nil
}

// ---- Grant ----

func (s *Store) PutGrant(_ context.Context, g model.Grant) error {
	if err := model.ValidateGrant(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if g.AccountID != accountID {
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
