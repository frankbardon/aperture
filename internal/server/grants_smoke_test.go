package server_test

import (
	"context"
	"encoding/json"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"

	"github.com/twitchtv/twirp"
)

// listGrants mirrors the grants.js read pattern: ListGrants for an account, then
// unmarshal each entity_json into a model.Grant the UI table renders from.
func listGrants(t *testing.T, ctx context.Context, c rpc.ApertureService, account string) []model.Grant {
	t.Helper()
	resp, err := c.ListGrants(ctx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Account: account}, AccountId: account})
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	out := make([]model.Grant, 0, len(resp.EntitiesJson))
	for _, s := range resp.EntitiesJson {
		var g model.Grant
		if err := json.Unmarshal([]byte(s), &g); err != nil {
			t.Fatalf("unmarshal grant: %v", err)
		}
		out = append(out, g)
	}
	return out
}

func grantInList(t *testing.T, ctx context.Context, c rpc.ApertureService, account, id string) bool {
	t.Helper()
	for _, g := range listGrants(t, ctx, c, account) {
		if g.ID == id {
			return true
		}
	}
	return false
}

// TestGrantsSmokeAllAccountsPagination is the UI proxy for the grants god-view:
// a system-admin (root) lists grants across EVERY account by sending an empty
// account_id, paging through the result via offset/limit and reading total to
// render prev/next; a non-admin (alice) is denied that path. The wildcard ("*")
// platform grant is returned inline in the all-accounts view.
func TestGrantsSmokeAllAccountsPagination(t *testing.T) {
	srv, store := newTestServer(t)
	c := client(srv)
	ctx := context.Background()

	// A second account plus concrete grants in both, so the all-accounts view
	// spans tenants. The base fixture already seeds the "*" g-root-admin grant.
	must(t, store.PutAccount(ctx, model.Account{ID: "beta", Name: "Beta"}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-read", ObjectType: "document", Action: "read"}))
	for _, g := range []model.Grant{
		{ID: "acme-1", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}, PermissionID: "perm-read", Object: "account:acme/document:1", Effect: model.EffectAllow},
		{ID: "acme-2", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}, PermissionID: "perm-read", Object: "account:acme/document:2", Effect: model.EffectAllow},
		{ID: "beta-1", AccountID: "beta", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}, PermissionID: "perm-read", Object: "account:beta/document:1", Effect: model.EffectAllow},
	} {
		must(t, store.PutGrant(ctx, g))
	}

	rootCtx := asPrincipal(context.Background(), t, "root")

	// System-admin all-accounts (empty account_id): total spans every account
	// (acme-1, acme-2, beta-1, g-root-admin = 4), wildcard grant returned inline.
	resp, err := c.ListGrants(rootCtx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Principal: "root"}, AccountId: model.AllAccounts, Limit: 100})
	if err != nil {
		t.Fatalf("system-admin all-accounts ListGrants: %v", err)
	}
	if resp.Total != 4 {
		t.Fatalf("all-accounts total = %d; want 4", resp.Total)
	}
	if len(resp.EntitiesJson) != 4 {
		t.Fatalf("all-accounts page = %d rows; want 4", len(resp.EntitiesJson))
	}
	var sawWildcard bool
	for _, s := range resp.EntitiesJson {
		var g model.Grant
		must(t, json.Unmarshal([]byte(s), &g))
		if g.AccountID == model.AccountWildcard {
			sawWildcard = true
		}
	}
	if !sawWildcard {
		t.Fatal("all-accounts view dropped the wildcard (\"*\") grant; it must be inline")
	}

	// Paging: a window of 2 returns a partial page and echoes offset/limit while
	// total reports the full count so the UI can compute next/prev.
	page1, err := c.ListGrants(rootCtx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Principal: "root"}, AccountId: model.AllAccounts, Offset: 0, Limit: 2})
	if err != nil {
		t.Fatalf("all-accounts page 1: %v", err)
	}
	if page1.Total != 4 || len(page1.EntitiesJson) != 2 || page1.Offset != 0 || page1.Limit != 2 {
		t.Fatalf("page 1 = %d rows, total %d, off %d, lim %d; want 2/4/0/2", len(page1.EntitiesJson), page1.Total, page1.Offset, page1.Limit)
	}
	page2, err := c.ListGrants(rootCtx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Principal: "root"}, AccountId: model.AllAccounts, Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("all-accounts page 2: %v", err)
	}
	if page2.Total != 4 || len(page2.EntitiesJson) != 2 {
		t.Fatalf("page 2 = %d rows, total %d; want 2/4", len(page2.EntitiesJson), page2.Total)
	}

	// Non-admin (alice) is denied the all-accounts path (twirp PermissionDenied,
	// APERTURE_AUTHZ_DENIED meta), even though alice may read her own account.
	aliceCtx := asPrincipal(context.Background(), t, "alice")
	_, err = c.ListGrants(aliceCtx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Principal: "alice"}, AccountId: model.AllAccounts, Limit: 100})
	if err == nil {
		t.Fatal("non-admin all-accounts ListGrants should be denied")
	}
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("want twirp PermissionDenied, got %v", err)
	}
	if code := te.Meta("code"); code != string(aerr.APERTURE_AUTHZ_DENIED) {
		t.Fatalf("want meta code %s, got %q", aerr.APERTURE_AUTHZ_DENIED, code)
	}

	// Single-account path stays backward compatible: alice lists her own account.
	own, err := c.ListGrants(aliceCtx, &rpc.ListGrantsRequest{Actor: &rpc.Actor{Principal: "alice", Account: acct}, AccountId: acct})
	if err != nil {
		t.Fatalf("single-account ListGrants(acme) for member: %v", err)
	}
	if own.Total != 2 || len(own.EntitiesJson) != 2 {
		t.Fatalf("acme page = %d rows, total %d; want 2/2 (acme-1, acme-2)", len(own.EntitiesJson), own.Total)
	}
}

// TestGrantsSmokeTemplateProvision is the UI proxy for the templates tab: an
// admin defines a template (PutTemplate, system tier), previews it client-side,
// then provisions a principal by applying it (ApplyTemplate, account tier,
// transactional). The expanded grant lands and is observable via the ListGrants
// the grants tab renders from.
func TestGrantsSmokeTemplateProvision(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	tmpl := model.Template{
		Name: "project-member", Version: 1,
		Description: "Provision a member onto a project.",
		Params: []model.TemplateParam{
			{Name: "account", Type: model.ParamSegment},
			{Name: "who", Type: model.ParamSegment},
		},
		Grants: []model.TemplateGrant{{
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "${who}"},
			PermissionID: "perm-admin",
			Object:       "account:${account}/document:*",
			Effect:       model.EffectAllow,
		}},
	}
	if _, err := c.PutTemplate(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, tmpl)}); err != nil {
		t.Fatalf("PutTemplate: %v", err)
	}

	// The list the templates tab renders from must show it.
	tl, err := c.ListTemplates(ctx, &rpc.ListRequest{})
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tl.EntitiesJson) == 0 {
		t.Fatal("template not listed after define")
	}

	// Apply (single) — the transactional provision the apply modal commits.
	resp, err := c.ApplyTemplate(ctx, &rpc.ApplyTemplateRequest{
		Actor:   actor,
		Name:    "project-member",
		Account: acct,
		Params:  map[string]string{"account": acct, "who": "alice"},
	})
	if err != nil {
		t.Fatalf("ApplyTemplate: %v", err)
	}
	if len(resp.EntitiesJson) != 1 {
		t.Fatalf("apply returned %d grants, want 1", len(resp.EntitiesJson))
	}
	var applied model.Grant
	if err := json.Unmarshal([]byte(resp.EntitiesJson[0]), &applied); err != nil {
		t.Fatalf("unmarshal applied grant: %v", err)
	}
	if applied.Subject.ID != "alice" || applied.Object != "account:acme/document:*" {
		t.Fatalf("applied grant not substituted: %+v", applied)
	}
	if !grantInList(t, ctx, c, acct, applied.ID) {
		t.Fatalf("provisioned grant %q not visible in the grants list", applied.ID)
	}

	// Bulk provision (bulk apply modal): client-expanded grants for several
	// principals written atomically via BulkPutGrants.
	bulk := []model.Grant{
		{ID: "bulk-carol-0", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "carol"}, PermissionID: "perm-admin", Object: "account:acme/document:*", Effect: model.EffectAllow},
		{ID: "bulk-dave-0", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "dave"}, PermissionID: "perm-admin", Object: "account:acme/document:*", Effect: model.EffectAllow},
	}
	if _, err := c.BulkPutGrants(ctx, &rpc.BulkGrantsRequest{
		Actor:      actor,
		GrantsJson: []string{mustJSON(t, bulk[0]), mustJSON(t, bulk[1])},
	}); err != nil {
		t.Fatalf("BulkPutGrants: %v", err)
	}
	if !grantInList(t, ctx, c, acct, "bulk-carol-0") || !grantInList(t, ctx, c, acct, "bulk-dave-0") {
		t.Fatal("bulk-provisioned grants not visible in the grants list")
	}
}

// TestGrantsSmokeDelegation is the UI proxy for the delegation tab: a designated
// delegator (alice) holding a may-delegate right plus a read authority over the
// atlas subtree bestows a more-specific, delegatable subset grant — it lands —
// while an escalating bestow (broader than alice's own authority) is rejected
// with APERTURE_DELEGATION_DENIED, the code the delegation error banner surfaces.
func TestGrantsSmokeDelegation(t *testing.T) {
	srv, store := newTestServer(t)
	c := client(srv)
	ctx := context.Background()

	// Seed the delegation prerequisites directly (as E6-S2 tests seed via store):
	// a document object type with a delegate verb, a delegatable read permission,
	// a may-delegate permission, the grantee, and alice's own authority.
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read", "write", "aperture.delegate"}}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "project", Actions: []string{"read", "write", "aperture.delegate"}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-read", ObjectType: "document", Action: "read", Delegatable: true}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-delegate", ObjectType: "document", Action: "aperture.delegate"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: acct}))
	// alice's authority: may-delegate over all of the account + read over the atlas subtree.
	must(t, store.PutGrant(ctx, model.Grant{ID: "g-alice-delegate", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}, PermissionID: "perm-delegate", Object: "account:acme/**", Effect: model.EffectAllow}))
	must(t, store.PutGrant(ctx, model.Grant{ID: "g-alice-read", AccountID: acct, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"}, PermissionID: "perm-read", Object: "account:acme/project:atlas/**", Effect: model.EffectAllow}))

	aliceCtx := asPrincipal(context.Background(), t, "alice")
	// alice is a delegator, not an admin, so she cannot read the grants list under
	// the read-visibility policy; verify what landed through an admin (root) view.
	rootCtx := asPrincipal(context.Background(), t, "root")

	// Valid bestow: a more-specific subset of alice's read authority.
	good := model.Grant{
		ID: "g-bob-atlas", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: "perm-read",
		Object:       "account:acme/project:atlas/document:42",
		Effect:       model.EffectAllow,
	}
	if _, err := c.Bestow(aliceCtx, &rpc.BestowRequest{GrantJson: mustJSON(t, good)}); err != nil {
		t.Fatalf("valid Bestow rejected: %v", err)
	}
	if !grantInList(t, rootCtx, c, acct, "g-bob-atlas") {
		t.Fatal("bestowed grant did not land in the grants list")
	}

	// Escalating bestow: broader than alice's own read authority — must be denied.
	esc := model.Grant{
		ID: "g-bob-escalate", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: "perm-read",
		Object:       "account:acme/**",
		Effect:       model.EffectAllow,
	}
	_, err := c.Bestow(aliceCtx, &rpc.BestowRequest{GrantJson: mustJSON(t, esc)})
	if err == nil {
		t.Fatal("escalating bestow should have been denied")
	}
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("want twirp PermissionDenied, got %v", err)
	}
	if code := te.Meta("code"); code != string(aerr.APERTURE_DELEGATION_DENIED) {
		t.Fatalf("want meta code %s, got %q", aerr.APERTURE_DELEGATION_DENIED, code)
	}
	if grantInList(t, rootCtx, c, acct, "g-bob-escalate") {
		t.Fatal("escalating grant must not have landed")
	}
}
