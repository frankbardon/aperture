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

// TestCRUDSmokeRole is the happy-path UI proxy for the roles screen: it drives
// the exact Twirp RPCs the crud.js component issues — create (PutRole) → list
// (ListRoles) → edit (PutRole) → delete (DeleteRole) — as the seeded admin, and
// asserts each step is observable via the list the UI renders from.
func TestCRUDSmokeRole(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	// Create.
	role := model.Role{ID: "editor2", Name: "Editor two", Description: "Writes docs.", PermissionIDs: []string{"perm-admin"}}
	if _, err := c.PutRole(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, role)}); err != nil {
		t.Fatalf("PutRole (create): %v", err)
	}

	// List — the UI renders from ListRoles; the new role must appear.
	got := findRole(t, ctx, c, "editor2")
	if got.Name != "Editor two" {
		t.Fatalf("created role name = %q, want %q", got.Name, "Editor two")
	}

	// Edit — the UI re-Puts the same id with changed fields.
	got.Name = "Editor renamed"
	got.PermissionIDs = nil
	if _, err := c.PutRole(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, got)}); err != nil {
		t.Fatalf("PutRole (edit): %v", err)
	}
	if after := findRole(t, ctx, c, "editor2"); after.Name != "Editor renamed" {
		t.Fatalf("edited role name = %q, want %q", after.Name, "Editor renamed")
	}

	// Delete — then confirm it is gone from the list.
	if _, err := c.DeleteRole(ctx, &rpc.DeleteRequest{Actor: actor, Id: "editor2"}); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if roleInList(t, ctx, c, "editor2") {
		t.Fatal("role editor2 still present after delete")
	}
}

// TestCRUDSmokeObjectTypeMultipleActions guards the object-type screen's Actions
// field end to end: the crud.js taglist widget lets an admin type several
// comma-separated verbs, which must persist as a multi-element string array. A
// widget regression (deriving the input value from the parsed array on every
// keystroke) erased the comma and capped the field at one action; this asserts
// the server contract the fixed widget feeds — many actions in, many actions out.
func TestCRUDSmokeObjectTypeMultipleActions(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	ot := model.ObjectType{
		Name:        "folder",
		Description: "A container of documents.",
		Actions:     []string{"read", "write", "delete", "share"},
	}
	if _, err := c.PutObjectType(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, ot)}); err != nil {
		t.Fatalf("PutObjectType: %v", err)
	}

	resp, err := c.GetObjectType(ctx, &rpc.GetRequest{Actor: actor, Id: "folder"})
	if err != nil {
		t.Fatalf("GetObjectType: %v", err)
	}
	var got model.ObjectType
	if err := json.Unmarshal([]byte(resp.EntityJson), &got); err != nil {
		t.Fatalf("decode object type: %v", err)
	}
	want := []string{"read", "write", "delete", "share"}
	if len(got.Actions) != len(want) {
		t.Fatalf("persisted actions = %v, want %v", got.Actions, want)
	}
	for i, a := range want {
		if got.Actions[i] != a {
			t.Fatalf("action[%d] = %q, want %q (full: %v)", i, got.Actions[i], a, got.Actions)
		}
	}
}

// TestCRUDSmokePrincipal is the happy-path UI proxy for the principals screen:
// create → list → edit → delete a principal as the admin.
func TestCRUDSmokePrincipal(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")
	actor := &rpc.Actor{Principal: "root", Account: acct}

	p := model.Principal{ID: "dave", Kind: model.PrincipalUser, Identity: "user:dave", DisplayName: "Dave"}
	if _, err := c.PutPrincipal(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, p)}); err != nil {
		t.Fatalf("PutPrincipal (create): %v", err)
	}

	got := findPrincipal(t, ctx, c, "dave")
	if got.Identity != "user:dave" {
		t.Fatalf("created principal identity = %q, want %q", got.Identity, "user:dave")
	}

	got.DisplayName = "Dave renamed"
	got.RoleIDs = []string{}
	if _, err := c.PutPrincipal(ctx, &rpc.EntityRequest{Actor: actor, EntityJson: mustJSON(t, got)}); err != nil {
		t.Fatalf("PutPrincipal (edit): %v", err)
	}
	if after := findPrincipal(t, ctx, c, "dave"); after.DisplayName != "Dave renamed" {
		t.Fatalf("edited principal display name = %q, want %q", after.DisplayName, "Dave renamed")
	}

	if _, err := c.DeletePrincipal(ctx, &rpc.DeleteRequest{Actor: actor, Id: "dave"}); err != nil {
		t.Fatalf("DeletePrincipal: %v", err)
	}
	if principalInList(t, ctx, c, "dave") {
		t.Fatal("principal dave still present after delete")
	}
}

// TestCRUDSmokeNonAdminDenied confirms the read-only path the UI hides behind the
// tier probe: a non-admin (alice) create is refused with PermissionDenied and the
// APERTURE_AUTHZ_DENIED code the crud.js error banner surfaces via meta["code"].
func TestCRUDSmokeNonAdminDenied(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "alice")

	_, err := c.PutRole(ctx, &rpc.EntityRequest{
		Actor:      &rpc.Actor{Principal: "alice", Account: acct},
		EntityJson: mustJSON(t, model.Role{ID: "nope", Name: "Nope"}),
	})
	if err == nil {
		t.Fatal("want denial for non-admin create, got nil")
	}
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("want twirp PermissionDenied, got %v", err)
	}
	if code := te.Meta("code"); code != string(aerr.APERTURE_AUTHZ_DENIED) {
		t.Fatalf("want meta code %s, got %q", aerr.APERTURE_AUTHZ_DENIED, code)
	}
}

// ---- helpers that mirror the UI's "list then find" read pattern ----

func listRoles(t *testing.T, ctx context.Context, c rpc.ApertureService) []model.Role {
	t.Helper()
	resp, err := c.ListRoles(ctx, &rpc.Empty{})
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	out := make([]model.Role, 0, len(resp.EntitiesJson))
	for _, s := range resp.EntitiesJson {
		var r model.Role
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatalf("unmarshal role: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func findRole(t *testing.T, ctx context.Context, c rpc.ApertureService, id string) model.Role {
	t.Helper()
	for _, r := range listRoles(t, ctx, c) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("role %q not found in list", id)
	return model.Role{}
}

func roleInList(t *testing.T, ctx context.Context, c rpc.ApertureService, id string) bool {
	t.Helper()
	for _, r := range listRoles(t, ctx, c) {
		if r.ID == id {
			return true
		}
	}
	return false
}

func listPrincipals(t *testing.T, ctx context.Context, c rpc.ApertureService) []model.Principal {
	t.Helper()
	resp, err := c.ListPrincipals(ctx, &rpc.Empty{})
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	out := make([]model.Principal, 0, len(resp.EntitiesJson))
	for _, s := range resp.EntitiesJson {
		var p model.Principal
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatalf("unmarshal principal: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func findPrincipal(t *testing.T, ctx context.Context, c rpc.ApertureService, id string) model.Principal {
	t.Helper()
	for _, p := range listPrincipals(t, ctx, c) {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("principal %q not found in list", id)
	return model.Principal{}
}

func principalInList(t *testing.T, ctx context.Context, c rpc.ApertureService, id string) bool {
	t.Helper()
	for _, p := range listPrincipals(t, ctx, c) {
		if p.ID == id {
			return true
		}
	}
	return false
}
