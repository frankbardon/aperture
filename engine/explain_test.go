package engine

import (
	"context"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// findEval returns the GrantEvaluation for grantID in the trace, or fails.
func findEval(t *testing.T, tr Trace, grantID string) GrantEvaluation {
	t.Helper()
	for _, ev := range tr.Considered {
		if ev.GrantID == grantID {
			return ev
		}
	}
	t.Fatalf("trace does not consider grant %q (considered: %d)", grantID, len(tr.Considered))
	return GrantEvaluation{}
}

// --- Acceptance: explain trace for the carve-out case ---

func TestExplain_CarveOut(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	// Deny everything in acme, carve out the atlas project as allowed (literal).
	f.grant("deny-acme", acctAcme, subjPrincipal("alice"), model.EffectDeny, permRead, "account:acme/**")
	f.grant("allow-atlas", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "account:acme/project:atlas/**")

	tr, err := f.eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/project:atlas/document:42",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !tr.Decision.Allow {
		t.Fatalf("carve-out object should be allowed; trace says deny\n%s", tr.String())
	}
	if len(tr.Decision.DecidingGrantIDs) != 1 || tr.Decision.DecidingGrantIDs[0] != "allow-atlas" {
		t.Fatalf("deciding = %v, want [allow-atlas]\n%s", tr.Decision.DecidingGrantIDs, tr.String())
	}

	// Both grants are recorded, both cover the object, allow-atlas is more
	// specific and is the deciding one.
	deny := findEval(t, tr, "deny-acme")
	allow := findEval(t, tr, "allow-atlas")
	if !deny.Covered || !allow.Covered {
		t.Fatalf("both grants should cover the atlas object: deny.Covered=%v allow.Covered=%v", deny.Covered, allow.Covered)
	}
	if allow.Specificity <= deny.Specificity {
		t.Fatalf("allow-atlas (%d) should be strictly more specific than deny-acme (%d)", allow.Specificity, deny.Specificity)
	}
	if !allow.Deciding || deny.Deciding {
		t.Fatalf("only allow-atlas should be deciding: allow.Deciding=%v deny.Deciding=%v", allow.Deciding, deny.Deciding)
	}
	if tr.MaxSpecificity != allow.Specificity {
		t.Fatalf("MaxSpecificity = %d, want %d", tr.MaxSpecificity, allow.Specificity)
	}

	// The rendered report names the deciding grant and the verdict.
	report := tr.String()
	if !strings.Contains(report, "allow-atlas") || !strings.Contains(report, "ALLOW") {
		t.Fatalf("rendered trace should name the deciding grant and verdict:\n%s", report)
	}
}

// An action mismatch is recorded as considered-but-not-matched, not silently
// dropped, so the trace shows what was ruled out.
func TestExplain_RecordsActionMismatch(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g-write", acctAcme, subjPrincipal("alice"), model.EffectAllow, permWrite, "document:42")

	tr, err := f.eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "document:42",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	ev := findEval(t, tr, "g-write")
	if ev.ActionMatched {
		t.Fatalf("write grant must not match a read request")
	}
	if tr.Decision.Allow {
		t.Fatalf("action mismatch should yield default deny")
	}
}

func TestExplain_DefaultDeny(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	tr, err := f.eng.Explain(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "document:42",
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if tr.Decision.Allow || len(tr.Considered) != 0 {
		t.Fatalf("no grants should be a default deny with nothing considered, got allow=%v considered=%d",
			tr.Decision.Allow, len(tr.Considered))
	}
}

// --- Wiring: scope + provider + rules end to end through Explain ---

// TestExplain_ScopeAndRule wires the full E2 stack — provider registry (metadata
// fetcher + object lister) and rules engine (rule evaluator) into a scoped engine
// — and explains a rule-backed inclusive grant. The trace must show the rule
// strategy covering the object whose metadata the rule selects.
func TestExplain_ScopeAndRule(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-rule", ObjectType: "document", Action: "read", ScopeStrategy: "inclusive;rule=sensitive",
	}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-rule", AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-rule", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	// Provider supplies object metadata the rule reads; one secret, one public.
	prov := metaProvider{md: map[string]provider.Metadata{
		"account:acme/document:secret": {"level": "secret"},
		"account:acme/document:public": {"level": "public"},
	}}
	reg := provider.NewRegistry()
	reg.MustRegister("document", prov)

	// Rule: object.level == "secret".
	rule := &rules.Rule{Name: "sensitive", AST: rules.Compare(rules.OpEq, rules.Var("object.level"), rules.Lit("secret"))}
	rulesEng := rules.NewEngine(rules.MapSource{"sensitive": rule}, reg)

	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg, Rules: rulesEng}))

	// The rule selects the secret document → covered → allowed.
	tr, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:secret"})
	if err != nil {
		t.Fatalf("Explain(secret): %v", err)
	}
	ev := findEval(t, tr, "g-rule")
	if ev.Strategy != "inclusive" {
		t.Fatalf("strategy = %q, want inclusive", ev.Strategy)
	}
	if !ev.Covered || !tr.Decision.Allow {
		t.Fatalf("rule should select the secret document: covered=%v allow=%v\n%s", ev.Covered, tr.Decision.Allow, tr.String())
	}

	// The rule does not select the public document → not covered → default deny.
	tr2, err := eng.Explain(ctx, Request{Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:public"})
	if err != nil {
		t.Fatalf("Explain(public): %v", err)
	}
	if tr2.Decision.Allow {
		t.Fatalf("rule should not select the public document\n%s", tr2.String())
	}
}

// metaProvider is an ObjectProvider that serves fixed metadata per identity.
type metaProvider struct{ md map[string]provider.Metadata }

func (p metaProvider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	if md, ok := p.md[id.String()]; ok {
		return md, nil
	}
	return nil, aerr.New(aerr.APERTURE_NOT_FOUND, "meta provider: absent object")
}

func (p metaProvider) List(_ context.Context) ([]provider.Object, error) {
	out := make([]provider.Object, 0, len(p.md))
	for s, md := range p.md {
		id, err := identity.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, provider.Object{ID: id, Metadata: md})
	}
	return out, nil
}

func (p metaProvider) Query(ctx context.Context, f provider.Filter) ([]provider.Object, error) {
	all, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Object, 0, len(all))
	for _, o := range all {
		if f.Pattern != nil && !f.Pattern.Matches(o.ID) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}
