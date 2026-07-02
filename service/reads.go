package service

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// This file gates entity READS by the caller's authority, mirroring the tier
// checks the mutations already enforce. A system-admin reads everything; any
// other authenticated principal is scoped to the accounts it administers, so one
// customer's admin can never enumerate another customer's grants, principals, or
// account. The shared schema catalogs (object types, permissions, roles, groups,
// rules, templates) stay readable by any authenticated principal — they are the
// vocabulary an account-admin needs to author grants, not per-account data.

// readScope resolves what an actor may READ. sys=true means unrestricted (a
// system-admin, or a local/unauthenticated context). Otherwise accounts is the
// set of account ids the actor may see: the accounts it is a MEMBER of plus the
// accounts it ADMINISTERS. Membership is the broad "people on this account see
// the account's data" rule; account-admin additionally covers accounts an admin
// governs without belonging to.
//
// Scoping is an authorization-surface concern: with no gate wired (a local CLI
// or MCP facade) or no identified caller, reads are unrestricted. The HTTP
// surface rejects an anonymous request before the service is reached, so a
// non-empty principal here always denotes a real HTTP caller to scope.
func (s *Service) readScope(ctx context.Context, actor Actor) (sys bool, accounts map[string]struct{}, err error) {
	if s.gate == nil || actor.Principal == "" {
		return true, nil, nil
	}
	// System (platform) authority is anchored at the wildcard account, so resolve
	// it there regardless of any account the caller happens to be viewing — a read
	// carries no active account, and the engine rejects an empty one.
	sysActor := Actor{Principal: actor.Principal, Account: model.AccountWildcard}
	if s.gate.RequireSystemAdmin(ctx, sysActor.gateActor()) == nil {
		return true, nil, nil
	}
	accounts = make(map[string]struct{})
	// Accounts the actor belongs to. The "*" wildcard membership is a platform
	// concern (its holders are system-admins, handled above), so it is not treated
	// as a concrete visible account here.
	memberships, err := s.store.MembershipsForPrincipal(ctx, actor.Principal)
	if err != nil {
		return false, nil, err
	}
	for _, m := range memberships {
		if m.AccountID != model.AccountWildcard {
			accounts[m.AccountID] = struct{}{}
		}
	}
	// Plus accounts the actor administers (which it need not be a member of).
	// RequireAccountAdmin resolves in each TARGET account, so the actor's own
	// account field is irrelevant.
	all, err := s.store.ListAccounts(ctx)
	if err != nil {
		return false, nil, err
	}
	for _, a := range all {
		if _, seen := accounts[a.ID]; seen {
			continue
		}
		if s.gate.RequireAccountAdmin(ctx, actor.gateActor(), a.ID) == nil {
			accounts[a.ID] = struct{}{}
		}
	}
	return false, accounts, nil
}

// authorizeAccountRead reports (nil) when the actor may read data scoped to
// accountID, else APERTURE_AUTHZ_DENIED. A system-admin may read any account; an
// account-admin only the accounts it administers. Platform ("*") data is never
// visible to a non-system-admin.
func (s *Service) authorizeAccountRead(sys bool, accounts map[string]struct{}, accountID string) error {
	if sys {
		return nil
	}
	if accountID != model.AccountWildcard {
		if _, ok := accounts[accountID]; ok {
			return nil
		}
	}
	return aerr.WithContext(aerr.APERTURE_AUTHZ_DENIED,
		"service: not authorized to read data for this account",
		map[string]any{"account": accountID})
}
