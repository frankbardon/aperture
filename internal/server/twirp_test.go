package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"github.com/twitchtv/twirp"
)

const acct = "acme"

// newTestServer boots the full Twirp surface over an admin-seeded in-memory
// store, wrapped in the dev authenticator middleware (bearer == principal id).
// "root" holds an all-covering ** admin grant (both tiers); "alice" is an
// authenticated non-admin.
func newTestServer(t *testing.T) (*httptest.Server, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))

	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "root", Kind: model.PrincipalUser, Identity: "user:root"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "root", AccountID: acct}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	// Admin authority modelled in-scheme: an aperture.admin permission on the
	// reserved "system" object type, granted to root over the all-covering **.
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-admin", ObjectType: "system", Action: authz.AdminAction}))
	// Stamped to the "*" wildcard account so root is a platform system-admin
	// (resolves in every account, like bera.yaml's platform-admins), not an
	// admin confined to one account.
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-root-admin", AccountID: model.AccountWildcard,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "root"},
		PermissionID: "perm-admin", Object: "**", Effect: model.EffectAllow,
	}))

	eng := engine.New(store)
	svc := service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithDelegation(delegation.New(store, eng)),
		service.WithImpersonation(impersonation.New(store, eng)),
	)
	handler := server.Authenticate(auth.NewDev(), server.New(svc))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, store
}

// client returns a Twirp JSON client whose calls carry the given bearer (the
// principal id, per the dev authenticator). An empty bearer is anonymous.
func client(srv *httptest.Server) rpc.ApertureService {
	return rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient)
}

func asPrincipal(ctx context.Context, t *testing.T, id string) context.Context {
	t.Helper()
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+id)
	ctx, err := twirp.WithHTTPRequestHeaders(ctx, h)
	if err != nil {
		t.Fatalf("set headers: %v", err)
	}
	return ctx
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// TestTwirpMutationRoundTrip exercises an authed admin write + read-back over the
// wire: root puts a new principal (system tier) and a new grant (account tier),
// then reads the principal back.
func TestTwirpMutationRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")

	// System-tier write: create a new principal.
	carol := model.Principal{ID: "carol", Kind: model.PrincipalUser, Identity: "user:carol"}
	if _, err := c.PutPrincipal(ctx, &rpc.EntityRequest{
		Actor:      &rpc.Actor{Principal: "root", Account: acct},
		EntityJson: mustJSON(t, carol),
	}); err != nil {
		t.Fatalf("PutPrincipal as root: %v", err)
	}

	// Read it back.
	got, err := c.GetPrincipal(ctx, &rpc.GetRequest{Id: "carol"})
	if err != nil {
		t.Fatalf("GetPrincipal: %v", err)
	}
	var back model.Principal
	if err := json.Unmarshal([]byte(got.EntityJson), &back); err != nil {
		t.Fatalf("unmarshal principal: %v", err)
	}
	if back.ID != "carol" || back.Identity != "user:carol" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}

	// Account-tier write: root holds the ** grant so is also account-admin.
	g := model.Grant{
		ID: "g-new", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "carol"},
		PermissionID: "perm-admin", Object: "account:acme/document:1", Effect: model.EffectAllow,
	}
	if _, err := c.PutGrant(ctx, &rpc.EntityRequest{
		Actor:      &rpc.Actor{Principal: "root", Account: acct},
		EntityJson: mustJSON(t, g),
	}); err != nil {
		t.Fatalf("PutGrant as root: %v", err)
	}
	if _, err := c.GetGrant(ctx, &rpc.GetRequest{Id: "g-new"}); err != nil {
		t.Fatalf("GetGrant: %v", err)
	}
}

// TestTwirpTemplateProvisioning exercises the E5-S1 surface end to end over the
// wire: root defines a template (system tier), applies it transactionally into
// the account (account tier), and the expanded grants are readable.
func TestTwirpTemplateProvisioning(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "root")

	tmpl := model.Template{
		Name: "member", Version: 1,
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
	if _, err := c.PutTemplate(ctx, &rpc.EntityRequest{
		Actor:      &rpc.Actor{Principal: "root", Account: acct},
		EntityJson: mustJSON(t, tmpl),
	}); err != nil {
		t.Fatalf("PutTemplate: %v", err)
	}

	resp, err := c.ApplyTemplate(ctx, &rpc.ApplyTemplateRequest{
		Actor:   &rpc.Actor{Principal: "root", Account: acct},
		Name:    "member",
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
	if _, err := c.GetGrant(ctx, &rpc.GetRequest{Id: applied.ID}); err != nil {
		t.Fatalf("applied grant not readable: %v", err)
	}

	// Bulk revoke deprovisions it.
	if _, err := c.BulkDeleteGrants(ctx, &rpc.BulkDeleteGrantsRequest{
		Actor:    &rpc.Actor{Principal: "root", Account: acct},
		GrantIds: []string{applied.ID},
	}); err != nil {
		t.Fatalf("BulkDeleteGrants: %v", err)
	}
	if _, err := c.GetGrant(ctx, &rpc.GetRequest{Id: applied.ID}); err == nil {
		t.Fatal("grant should have been bulk-revoked")
	}

	// A non-admin apply is refused (account-tier gate).
	aliceCtx := asPrincipal(context.Background(), t, "alice")
	if _, err := c.ApplyTemplate(aliceCtx, &rpc.ApplyTemplateRequest{
		Actor:   &rpc.Actor{Principal: "alice", Account: acct},
		Name:    "member",
		Account: acct,
		Params:  map[string]string{"account": acct, "who": "alice"},
	}); err == nil {
		t.Fatal("non-admin apply should be denied")
	}
}

// TestTwirpQueryOpen confirms the decision API needs no auth: Check works
// anonymously and returns the admin decision (root is allowed aperture.admin).
func TestTwirpQueryOpen(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)

	dec, err := c.Check(context.Background(), &rpc.CheckRequest{
		Account: acct, Principal: "root", Action: authz.AdminAction, Object: "system:schema",
	})
	if err != nil {
		t.Fatalf("anonymous Check: %v", err)
	}
	if !dec.Allow {
		t.Fatalf("want allow for root admin check, got deny: %s", dec.Reason)
	}
}

// TestTwirpUnauthenticated confirms a mutation with NO credential is refused
// with twirp Unauthenticated (HTTP 401).
func TestTwirpUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)

	_, err := c.PutPrincipal(context.Background(), &rpc.EntityRequest{
		EntityJson: mustJSON(t, model.Principal{ID: "x", Kind: model.PrincipalUser, Identity: "user:x"}),
	})
	if err == nil {
		t.Fatal("want error for anonymous mutation, got nil")
	}
	if te, ok := err.(twirp.Error); !ok || te.Code() != twirp.Unauthenticated {
		t.Fatalf("want twirp Unauthenticated, got %v", err)
	}

	// And confirm the raw HTTP status is 401.
	body := `{"entity_json":"{}"}`
	resp, err := http.Post(srv.URL+rpc.ApertureServicePathPrefix+"PutPrincipal", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("raw post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want HTTP 401, got %d", resp.StatusCode)
	}
}

// TestTwirpNonAdminDenied confirms an authenticated NON-admin (alice) is refused
// a mutation with twirp PermissionDenied (HTTP 403), and that the coded error is
// surfaced via meta["code"].
func TestTwirpNonAdminDenied(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)
	ctx := asPrincipal(context.Background(), t, "alice")

	_, err := c.PutPrincipal(ctx, &rpc.EntityRequest{
		Actor:      &rpc.Actor{Principal: "alice", Account: acct},
		EntityJson: mustJSON(t, model.Principal{ID: "y", Kind: model.PrincipalUser, Identity: "user:y"}),
	})
	if err == nil {
		t.Fatal("want denial for non-admin mutation, got nil")
	}
	te, ok := err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("want twirp PermissionDenied, got %v", err)
	}
	if code := te.Meta("code"); code != string(aerr.APERTURE_AUTHZ_DENIED) {
		t.Fatalf("want meta code %s, got %q", aerr.APERTURE_AUTHZ_DENIED, code)
	}
}

// TestTwirpExplain confirms Explain returns a JSON trace whose verdict matches.
func TestTwirpExplain(t *testing.T) {
	srv, _ := newTestServer(t)
	c := client(srv)

	res, err := c.Explain(context.Background(), &rpc.CheckRequest{
		Account: acct, Principal: "root", Action: authz.AdminAction, Object: "system:schema",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var tr engine.Trace
	if err := json.Unmarshal([]byte(res.TraceJson), &tr); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("want allow trace, got: %s", tr.Decision.Reason)
	}
}
