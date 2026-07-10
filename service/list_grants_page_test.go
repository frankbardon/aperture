package service

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// The all-accounts (empty account_id) path is system-admin only. readScopeFixture
// seeds three grants: g-root (stamped "*"), g-vmgr (visa), g-visa-data (visa).

func TestListGrantsPage_SystemAdminAllAccounts(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	root := Actor{Principal: "root"}

	page, total, err := s.ListGrantsPage(ctx, root, model.AllAccounts, 0, 100)
	if err != nil {
		t.Fatalf("system-admin ListGrantsPage(all): %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d; want 3 (every account's grants, wildcard inline)", total)
	}
	if len(page) != 3 {
		t.Fatalf("page len = %d; want 3", len(page))
	}
	// The wildcard-stamped platform grant is returned inline, not filtered out.
	var sawWildcard bool
	for _, g := range page {
		if g.AccountID == model.AccountWildcard {
			sawWildcard = true
		}
	}
	if !sawWildcard {
		t.Fatalf("all-accounts page dropped the wildcard (\"*\") grant; it must be returned inline")
	}
}

func TestListGrantsPage_AccountAdminAllAccountsDenied(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	vmgr := Actor{Principal: "vmgr"} // account-admin of visa, not a system-admin

	_, _, err := s.ListGrantsPage(ctx, vmgr, model.AllAccounts, 0, 100)
	if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("account-admin ListGrantsPage(all) = %v; want APERTURE_AUTHZ_DENIED", aerr.CodeOf(err))
	}
}

func TestListGrantsPage_NonAdminAllAccountsDenied(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	nobody := Actor{Principal: "nobody"}

	if _, _, err := s.ListGrantsPage(ctx, nobody, model.AllAccounts, 0, 100); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-admin ListGrantsPage(all) = %v; want APERTURE_AUTHZ_DENIED", aerr.CodeOf(err))
	}
}

func TestListGrantsPage_SingleAccountUnchanged(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()

	// System-admin may read any single account.
	page, total, err := s.ListGrantsPage(ctx, Actor{Principal: "root"}, "visa", 0, 100)
	if err != nil {
		t.Fatalf("system-admin ListGrantsPage(visa): %v", err)
	}
	if total != 2 || len(page) != 2 {
		t.Fatalf("visa page = %d rows, total %d; want 2/2 (g-vmgr + g-visa-data)", len(page), total)
	}

	// Account-admin may read its own account.
	if _, _, err := s.ListGrantsPage(ctx, Actor{Principal: "vmgr"}, "visa", 0, 100); err != nil {
		t.Fatalf("account-admin ListGrantsPage(visa): %v", err)
	}
	// But not another account, nor the wildcard account — same gate as ListGrants.
	if _, _, err := s.ListGrantsPage(ctx, Actor{Principal: "vmgr"}, "dish", 0, 100); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("account-admin ListGrantsPage(dish) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}
	if _, _, err := s.ListGrantsPage(ctx, Actor{Principal: "vmgr"}, model.AccountWildcard, 0, 100); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("account-admin ListGrantsPage(*) = %v; want AUTHZ_DENIED", aerr.CodeOf(err))
	}
}

func TestListGrantsPage_PageBoundaries(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	root := Actor{Principal: "root"}

	// A window smaller than the total returns a partial page but the full total.
	page, total, err := s.ListGrantsPage(ctx, root, model.AllAccounts, 0, 2)
	if err != nil {
		t.Fatalf("ListGrantsPage(all, 0, 2): %v", err)
	}
	if total != 3 || len(page) != 2 {
		t.Fatalf("first page = %d rows, total %d; want 2 rows / total 3", len(page), total)
	}

	// Offset past the end returns an empty page while total still reports every match.
	page, total, err = s.ListGrantsPage(ctx, root, model.AllAccounts, 999, 100)
	if err != nil {
		t.Fatalf("ListGrantsPage(all, 999, 100): %v", err)
	}
	if total != 3 || len(page) != 0 {
		t.Fatalf("past-end page = %d rows, total %d; want 0 rows / total 3", len(page), total)
	}

	// A limit over the server cap is clamped by the store (no error); total is unaffected.
	page, total, err = s.ListGrantsPage(ctx, root, model.AllAccounts, 0, model.MaxGrantPageSize+50)
	if err != nil {
		t.Fatalf("ListGrantsPage(all, over-cap): %v", err)
	}
	if total != 3 || len(page) != 3 {
		t.Fatalf("over-cap page = %d rows, total %d; want 3/3 (clamped to cap, still fits)", len(page), total)
	}
}

func TestListGrantsPage_NegativePageParamsRejected(t *testing.T) {
	s := readScopeFixture(t)
	ctx := context.Background()
	root := Actor{Principal: "root"}

	if _, _, err := s.ListGrantsPage(ctx, root, model.AllAccounts, -1, 100); aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("negative offset = %v; want APERTURE_INVALID_INPUT", aerr.CodeOf(err))
	}
	if _, _, err := s.ListGrantsPage(ctx, root, model.AllAccounts, 0, -5); aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("negative limit = %v; want APERTURE_INVALID_INPUT", aerr.CodeOf(err))
	}
}
