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
