package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"github.com/twitchtv/twirp"
)

// newRulesTestServer boots the full Twirp surface with the rules engine wired
// over a STORAGE-backed rule source, so rule-backed scope strategies resolve the
// same rules the editor saves through PutRule (the E7-S3 loop). It seeds "root"
// (system-admin), "alice" (non-admin member), a rule-backed permission, and a
// grant that lets alice read documents ONLY when the "vip" rule selects them.
func newRulesTestServer(t *testing.T) (*httptest.Server, model.Storage) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))

	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "root", Kind: model.PrincipalUser, Identity: "user:root"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "root", AccountID: acct}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))

	// Admin authority (system + account tier) modelled in-scheme for root.
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "system", Actions: []string{authz.AdminAction}}))
	must(t, store.PutPermission(ctx, model.Permission{ID: "perm-admin", ObjectType: "system", Action: authz.AdminAction}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-root-admin", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "root"},
		PermissionID: "perm-admin", Object: "**", Effect: model.EffectAllow,
	}))

	// A rule-backed grant: alice may read any document the "vip" rule selects.
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(t, store.PutPermission(ctx, model.Permission{
		ID: "perm-doc-read", ObjectType: "document", Action: "read",
		ScopeStrategy: "inclusive;rule=vip",
	}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-alice-vip", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "perm-doc-read", Object: "**", Effect: model.EffectAllow,
	}))

	ruleSource := service.NewStorageRuleSource(store)
	ruleEngine := rules.NewEngine(ruleSource, nil)
	eng := engine.New(store, engine.WithScopeResolution(nil, engine.ScopeDeps{Rules: ruleEngine}))
	svc := service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithRuleSource(ruleSource, nil),
	)
	handler := server.Authenticate(auth.NewDev(), server.New(svc))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, store
}

// ruleJSON marshals a model.Rule (name + AST) to the rule_json wire field.
func ruleJSON(t *testing.T, name string, ast *rules.Node) string {
	t.Helper()
	raw, err := json.Marshal(ast)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}
	return mustJSON(t, model.Rule{Name: name, AST: raw})
}

// TestRulesEditorSmoke drives the exact RPC path the node editor uses: an invalid
// rule is rejected with an APERTURE_RULE_* code; the fixed rule saves and reads
// back; the saved rule immediately governs a rule-backed grant's decision; a live
// what-if previews an UNSAVED edit without persisting it; and a non-admin PutRule
// is refused with 403.
func TestRulesEditorSmoke(t *testing.T) {
	srv, store := newRulesTestServer(t)
	c := client(srv)
	rootCtx := asPrincipal(context.Background(), t, "root")

	doc := "account:acme/document:1"
	query := &rpc.CheckRequest{Account: acct, Principal: "alice", Action: "read", Object: doc}

	// 1. Save an INVALID rule (references an unknown variable root) -> reject with
	// an APERTURE_RULE_* code, and nothing persisted.
	badAST := rules.Compare(rules.OpEq, rules.Var("bogus.field"), rules.Lit("x"))
	_, err := c.PutRule(rootCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", badAST),
	})
	te, ok := err.(twirp.Error)
	if !ok {
		t.Fatalf("PutRule(invalid): want twirp error, got %v", err)
	}
	if code := te.Meta("code"); !strings.HasPrefix(code, "APERTURE_RULE_") {
		t.Fatalf("PutRule(invalid): want APERTURE_RULE_* code, got %q (%v)", code, err)
	}
	if _, gErr := store.GetRule(context.Background(), "vip"); gErr == nil {
		t.Fatalf("invalid rule must not persist")
	}

	// ValidateRule is a non-persisting check with the same verdict.
	if _, vErr := c.ValidateRule(rootCtx, &rpc.RuleRequest{RuleJson: ruleJSON(t, "vip", badAST)}); vErr == nil {
		t.Fatalf("ValidateRule(invalid): want error, got nil")
	}

	// 2. Fix -> save a VALID rule that selects alice, and read it back.
	goodAST := rules.Compare(rules.OpEq, rules.Var("principal.id"), rules.Lit("alice"))
	if _, vErr := c.ValidateRule(rootCtx, &rpc.RuleRequest{RuleJson: ruleJSON(t, "vip", goodAST)}); vErr != nil {
		t.Fatalf("ValidateRule(valid): %v", vErr)
	}
	if _, pErr := c.PutRule(rootCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", goodAST),
	}); pErr != nil {
		t.Fatalf("PutRule(valid): %v", pErr)
	}
	got, err := c.GetRule(rootCtx, &rpc.GetRequest{Id: "vip"})
	if err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	var back model.Rule
	if err := json.Unmarshal([]byte(got.RuleJson), &back); err != nil {
		t.Fatalf("unmarshal rule: %v", err)
	}
	if back.Name != "vip" {
		t.Fatalf("GetRule round-trip mismatch: %+v", back)
	}

	// 3. The saved rule immediately governs the rule-backed grant: alice is now
	// allowed to read the document (the rule selects her).
	dec, err := c.Check(rootCtx, query)
	if err != nil {
		t.Fatalf("Check after save: %v", err)
	}
	if !dec.Allow {
		t.Fatalf("saved rule should allow alice; got deny: %s", dec.Reason)
	}

	// Re-save the rule so it selects nobody: the decision flips to deny, proving a
	// saved edit takes effect against the live rule-backed grant.
	denyAST := rules.Compare(rules.OpEq, rules.Var("principal.id"), rules.Lit("nobody"))
	if _, pErr := c.PutRule(rootCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", denyAST),
	}); pErr != nil {
		t.Fatalf("PutRule(deny edit): %v", pErr)
	}
	dec, err = c.Check(rootCtx, query)
	if err != nil {
		t.Fatalf("Check after deny edit: %v", err)
	}
	if dec.Allow {
		t.Fatalf("edited rule should deny alice; got allow")
	}

	// 4. Live what-if: preview an UNSAVED edit (selects alice again) over the live
	// model. SimulateExplain must show an allow WITHOUT persisting — the next real
	// Check still denies because the saved rule is unchanged.
	sim, err := c.SimulateExplain(rootCtx, &rpc.SimulateRequest{
		Query:     query,
		RulesJson: []string{ruleJSON(t, "vip", goodAST)},
	})
	if err != nil {
		t.Fatalf("SimulateExplain: %v", err)
	}
	var trace engine.Trace
	if err := json.Unmarshal([]byte(sim.TraceJson), &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if !trace.Decision.Allow {
		t.Fatalf("what-if preview of the edited rule should allow; got deny: %s", trace.Decision.Reason)
	}
	// The preview persisted nothing: the stored rule is still the deny edit.
	dec, err = c.Check(rootCtx, query)
	if err != nil {
		t.Fatalf("Check after simulate: %v", err)
	}
	if dec.Allow {
		t.Fatalf("simulate must not persist; live Check should still deny")
	}
	if stored, gErr := store.GetRule(context.Background(), "vip"); gErr != nil {
		t.Fatalf("GetRule after simulate: %v", gErr)
	} else if strings.Contains(string(stored.AST), "alice") {
		t.Fatalf("simulate leaked the previewed rule into storage: %s", stored.AST)
	}

	// 5. A non-admin (alice) cannot edit rules: 403 PermissionDenied.
	aliceCtx := asPrincipal(context.Background(), t, "alice")
	_, err = c.PutRule(aliceCtx, &rpc.RuleRequest{
		Actor:    &rpc.Actor{Account: acct},
		RuleJson: ruleJSON(t, "vip", goodAST),
	})
	te, ok = err.(twirp.Error)
	if !ok || te.Code() != twirp.PermissionDenied {
		t.Fatalf("non-admin PutRule: want PermissionDenied, got %v", err)
	}
	if code := te.Meta("code"); code != string(aerr.APERTURE_AUTHZ_DENIED) {
		t.Fatalf("non-admin PutRule: want APERTURE_AUTHZ_DENIED, got %q", code)
	}
}
