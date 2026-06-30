package service

import (
	"context"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// This file adds the READ-ONLY what-if surface to the decision facade. Simulate
// answers "what WOULD the decision be if these hypothetical principals / groups /
// permissions / grants / memberships existed?" without ever persisting any of
// them. It is the seam the MCP Simulate tool (E4-S3) and the what-if simulator UI
// (E6-S4) drive.
//
// Mechanism: an in-memory overlay (overlayStore) wraps the live storage and
// layers the hypothetical entities on top for the handful of reads the engine's
// decision path performs. The engine never writes, and the overlay's mutators are
// inert (APERTURE_UNIMPLEMENTED), so a simulation can NEVER leak a write into the
// backing store — the read-only guarantee is structural, not merely conventional.
// The transient engine is e.WithStore(overlay): same coverer, membership policy,
// and clock as the live engine, just a different read source.

// Overlay is the set of hypothetical entities a Simulate run layers over the live
// model. Every field is additive and optional: an overlay entity with the same id
// as a stored one shadows it (so a what-if can model an edited grant or a
// re-roled principal), and ids absent from the overlay fall through to storage.
// Nothing here is ever written.
type Overlay struct {
	// Principals are hypothetical (or shadowing) principals. A principal here with
	// the same id as a stored one replaces it for the simulation — the way to model
	// "what if alice had role X".
	Principals []model.Principal
	// Groups are hypothetical groups; a principal's group membership in the
	// simulation is the union of stored groups and these.
	Groups []model.Group
	// Permissions are hypothetical (or shadowing) permissions a hypothetical grant
	// may reference.
	Permissions []model.Permission
	// Grants are the hypothetical grants — the common what-if input ("what if I
	// bestowed this grant?"). They are account-scoped exactly like stored grants.
	Grants []model.Grant
	// Memberships are hypothetical account memberships, consulted only when the
	// engine enforces membership.
	Memberships []model.Membership
}

// Simulate renders the decision for q as it WOULD be under the hypothetical
// overlay, with the same fail-closed contract as Check: an input-validation error
// is returned; any other engine error folds into a deny Result. Nothing is
// written and nothing is audited — a simulation is not a real decision. It
// requires the entity surface (WithStorage) so it has a base to overlay.
func (s *Service) Simulate(ctx context.Context, ov Overlay, q Query) (Result, error) {
	eng, err := s.simEngine(ov)
	if err != nil {
		return Result{}, err
	}
	dec, derr := eng.Check(ctx, q.request())
	return renderCheck(dec, derr)
}

// SimulateExplain returns the full decision Trace for q under the hypothetical
// overlay — the most useful what-if output, since it shows WHICH hypothetical
// grant decided the verdict and why. Engine errors are returned verbatim (Explain
// is a diagnostic). Nothing is written or audited.
func (s *Service) SimulateExplain(ctx context.Context, ov Overlay, q Query) (engine.Trace, error) {
	eng, err := s.simEngine(ov)
	if err != nil {
		return engine.Trace{}, err
	}
	return eng.Explain(ctx, q.request())
}

// simEngine builds the transient, read-only what-if engine: the live engine
// re-pointed at an overlay store. It requires the entity surface so there is a
// base store to overlay.
func (s *Service) simEngine(ov Overlay) (*engine.Engine, error) {
	if err := s.requireStore(); err != nil {
		return nil, err
	}
	return s.eng.WithStore(newOverlayStore(s.store, ov)), nil
}

// overlayStore is a read-only model.Storage that wraps a base store and layers an
// Overlay's hypothetical entities over the reads the decision engine performs
// (GetPrincipal, GroupsForPrincipal, GrantsForSubjects, GetPermission, IsMember).
// All other reads delegate to the base via the embedded interface; every WRITE is
// overridden to be inert, so the engine — or a buggy caller — can never persist a
// simulated entity through this store.
type overlayStore struct {
	// base provides the embedded read defaults (Get*/List* the overlay does not
	// specialise). It is NEVER written through: every mutator is overridden below.
	base model.Storage

	principals  map[string]model.Principal
	permissions map[string]model.Permission
	groups      []model.Group
	grants      []model.Grant
	memberships map[string]bool // key: principalID + "\x00" + accountID
}

func newOverlayStore(base model.Storage, ov Overlay) *overlayStore {
	o := &overlayStore{
		base:        base,
		principals:  make(map[string]model.Principal, len(ov.Principals)),
		permissions: make(map[string]model.Permission, len(ov.Permissions)),
		groups:      ov.Groups,
		grants:      ov.Grants,
		memberships: make(map[string]bool, len(ov.Memberships)),
	}
	for _, p := range ov.Principals {
		o.principals[p.ID] = p
	}
	for _, p := range ov.Permissions {
		o.permissions[p.ID] = p
	}
	for _, m := range ov.Memberships {
		o.memberships[m.PrincipalID+"\x00"+m.AccountID] = true
	}
	return o
}

// --- Overlaid reads (the engine's decision path) ----------------------------

// GetPrincipal returns the overlay principal when present (shadowing storage),
// else the stored one.
func (o *overlayStore) GetPrincipal(ctx context.Context, id string) (model.Principal, error) {
	if p, ok := o.principals[id]; ok {
		return p, nil
	}
	return o.base.GetPrincipal(ctx, id)
}

// GroupsForPrincipal returns the union of stored groups and overlay groups that
// list principalID as a member, de-duplicated by group id (an overlay group
// shadows a stored one of the same id).
func (o *overlayStore) GroupsForPrincipal(ctx context.Context, principalID string) ([]model.Group, error) {
	base, err := o.base.GroupsForPrincipal(ctx, principalID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(base)+len(o.groups))
	out := make([]model.Group, 0, len(base)+len(o.groups))
	for _, g := range o.groups {
		if !groupHasMember(g, principalID) {
			continue
		}
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	for _, g := range base {
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	return out, nil
}

func groupHasMember(g model.Group, principalID string) bool {
	for _, m := range g.MemberPrincipalIDs {
		if m == principalID {
			return true
		}
	}
	return false
}

// GrantsForSubjects returns the account-scoped grants for subjects from the base
// store plus every overlay grant stamped to accountID whose subject is in
// subjects. Overlay grants are appended (deny-overrides resolution in the engine
// handles precedence), so a hypothetical deny correctly carves out a stored allow.
func (o *overlayStore) GrantsForSubjects(ctx context.Context, accountID string, subjects []model.Subject) ([]model.Grant, error) {
	base, err := o.base.GrantsForSubjects(ctx, accountID, subjects)
	if err != nil {
		return nil, err
	}
	if len(o.grants) == 0 {
		return base, nil
	}
	want := make(map[model.Subject]bool, len(subjects))
	for _, s := range subjects {
		want[s] = true
	}
	out := base
	for _, g := range o.grants {
		if g.AccountID != accountID {
			continue
		}
		if want[g.Subject] {
			out = append(out, g)
		}
	}
	return out, nil
}

// GetPermission returns the overlay permission when present, else the stored one.
func (o *overlayStore) GetPermission(ctx context.Context, id string) (model.Permission, error) {
	if p, ok := o.permissions[id]; ok {
		return p, nil
	}
	return o.base.GetPermission(ctx, id)
}

// IsMember reports membership as the union of stored memberships and overlay
// memberships, so a what-if can admit a principal to an account it is not yet in.
func (o *overlayStore) IsMember(ctx context.Context, principalID, accountID string) (bool, error) {
	if o.memberships[principalID+"\x00"+accountID] {
		return true, nil
	}
	return o.base.IsMember(ctx, principalID, accountID)
}

// --- Delegated reads --------------------------------------------------------
//
// Reads the overlay does not specialise delegate straight to the base store.

func (o *overlayStore) GetAccount(ctx context.Context, id string) (model.Account, error) {
	return o.base.GetAccount(ctx, id)
}
func (o *overlayStore) ListAccounts(ctx context.Context) ([]model.Account, error) {
	return o.base.ListAccounts(ctx)
}
func (o *overlayStore) GetMembership(ctx context.Context, principalID, accountID string) (model.Membership, error) {
	return o.base.GetMembership(ctx, principalID, accountID)
}
func (o *overlayStore) MembershipsForPrincipal(ctx context.Context, principalID string) ([]model.Membership, error) {
	return o.base.MembershipsForPrincipal(ctx, principalID)
}
func (o *overlayStore) MembershipsForAccount(ctx context.Context, accountID string) ([]model.Membership, error) {
	return o.base.MembershipsForAccount(ctx, accountID)
}
func (o *overlayStore) GetObjectType(ctx context.Context, name string) (model.ObjectType, error) {
	return o.base.GetObjectType(ctx, name)
}
func (o *overlayStore) ListObjectTypes(ctx context.Context) ([]model.ObjectType, error) {
	return o.base.ListObjectTypes(ctx)
}
func (o *overlayStore) ListPermissions(ctx context.Context) ([]model.Permission, error) {
	return o.base.ListPermissions(ctx)
}
func (o *overlayStore) ListPrincipals(ctx context.Context) ([]model.Principal, error) {
	return o.base.ListPrincipals(ctx)
}
func (o *overlayStore) GetRole(ctx context.Context, id string) (model.Role, error) {
	return o.base.GetRole(ctx, id)
}
func (o *overlayStore) ListRoles(ctx context.Context) ([]model.Role, error) {
	return o.base.ListRoles(ctx)
}
func (o *overlayStore) GetGroup(ctx context.Context, id string) (model.Group, error) {
	return o.base.GetGroup(ctx, id)
}
func (o *overlayStore) ListGroups(ctx context.Context) ([]model.Group, error) {
	return o.base.ListGroups(ctx)
}
func (o *overlayStore) GetGrant(ctx context.Context, id string) (model.Grant, error) {
	return o.base.GetGrant(ctx, id)
}
func (o *overlayStore) ListGrants(ctx context.Context, accountID string) ([]model.Grant, error) {
	return o.base.ListGrants(ctx, accountID)
}
func (o *overlayStore) QueryAudit(ctx context.Context, filter model.AuditFilter) ([]model.AuditEvent, error) {
	return o.base.QueryAudit(ctx, filter)
}
func (o *overlayStore) GetTemplate(ctx context.Context, name string, version int) (model.Template, error) {
	return o.base.GetTemplate(ctx, name, version)
}
func (o *overlayStore) ListTemplates(ctx context.Context) ([]model.Template, error) {
	return o.base.ListTemplates(ctx)
}

// --- Inert writes -----------------------------------------------------------
//
// Every mutator is overridden to fail with APERTURE_UNIMPLEMENTED. The decision
// engine never calls these; overriding them makes the no-write guarantee
// STRUCTURAL — a simulation physically cannot persist through this store.

func errReadOnly() error {
	return aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: simulate overlay store is read-only")
}

func (o *overlayStore) Setup(context.Context) error { return errReadOnly() }
func (o *overlayStore) Close() error                { return nil }

func (o *overlayStore) PutAccount(context.Context, model.Account) error       { return errReadOnly() }
func (o *overlayStore) DeleteAccount(context.Context, string) error           { return errReadOnly() }
func (o *overlayStore) PutMembership(context.Context, model.Membership) error { return errReadOnly() }
func (o *overlayStore) DeleteMembership(context.Context, string, string) error {
	return errReadOnly()
}
func (o *overlayStore) PutObjectType(context.Context, model.ObjectType) error { return errReadOnly() }
func (o *overlayStore) DeleteObjectType(context.Context, string) error        { return errReadOnly() }
func (o *overlayStore) PutPermission(context.Context, model.Permission) error { return errReadOnly() }
func (o *overlayStore) DeletePermission(context.Context, string) error        { return errReadOnly() }
func (o *overlayStore) PutPrincipal(context.Context, model.Principal) error   { return errReadOnly() }
func (o *overlayStore) DeletePrincipal(context.Context, string) error         { return errReadOnly() }
func (o *overlayStore) PutRole(context.Context, model.Role) error             { return errReadOnly() }
func (o *overlayStore) DeleteRole(context.Context, string) error              { return errReadOnly() }
func (o *overlayStore) PutGroup(context.Context, model.Group) error           { return errReadOnly() }
func (o *overlayStore) DeleteGroup(context.Context, string) error             { return errReadOnly() }
func (o *overlayStore) PutGrant(context.Context, model.Grant) error           { return errReadOnly() }
func (o *overlayStore) DeleteGrant(context.Context, string) error             { return errReadOnly() }
func (o *overlayStore) AppendAudit(context.Context, model.AuditEvent) error   { return errReadOnly() }
func (o *overlayStore) PruneAudit(context.Context, model.RetentionPolicy) (int, error) {
	return 0, errReadOnly()
}
func (o *overlayStore) PutTemplate(context.Context, model.Template) error { return errReadOnly() }
func (o *overlayStore) DeleteTemplate(context.Context, string, int) error { return errReadOnly() }
func (o *overlayStore) Atomic(context.Context, func(tx model.Storage) error) error {
	return errReadOnly()
}

// overlayStore must satisfy model.Storage in full.
var _ model.Storage = (*overlayStore)(nil)
