package impersonation

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/storage/memory"
)

const (
	acctAcme  = "acme"
	acctOther = "other"

	permRead    = "perm-read"
	permAugment = "perm-augment"
	permBecome  = "perm-become"

	// Objects the operator's own authority vs the target's authority cover, so
	// augment (union) and become (target-only) are distinguishable.
	objOpOnly  = "account:acme/space:op/document:1"
	objTgtOnly = "account:acme/space:tgt/document:2"
)

// clock is a mutable, injected time source shared by the Service and the Engine
// so a test can advance time deterministically across the impersonation time-box.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// fixture wires an in-memory store with:
//   - a "document" object type (read) and a "user" object type that declares the
//     reserved impersonation verbs, plus a permission on each;
//   - operator "op" and target "tgt", both members of acme; "stranger", a member
//     of the "other" account only;
//   - op holding read over its OWN object and tgt holding read over a DIFFERENT
//     object, so augment vs become resolve to different permission sets.
//
// Impersonation rights are seeded per-test via grantRight, so each test controls
// exactly which gating right the operator holds.
type fixture struct {
	t     *testing.T
	store *memory.Store
	clk   *clock
	eng   *engine.Engine
	svc   *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.PutAccount(ctx, model.Account{ID: acctAcme, Name: "Acme"}))
	must(store.PutAccount(ctx, model.Account{ID: acctOther, Name: "Other"}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "user", Actions: []string{AugmentAction, BecomeAction}}))
	must(store.PutPermission(ctx, model.Permission{ID: permRead, ObjectType: "document", Action: "read"}))
	must(store.PutPermission(ctx, model.Permission{ID: permAugment, ObjectType: "user", Action: AugmentAction}))
	must(store.PutPermission(ctx, model.Permission{ID: permBecome, ObjectType: "user", Action: BecomeAction}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "op", Kind: model.PrincipalUser, Identity: "user:op"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "tgt", Kind: model.PrincipalUser, Identity: "user:tgt"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "stranger", Kind: model.PrincipalUser, Identity: "user:stranger"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "op", AccountID: acctAcme}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "tgt", AccountID: acctAcme}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "stranger", AccountID: acctOther}))

	clk := &clock{t: time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)}
	eng := engine.New(store, engine.WithClock(clk.now))
	svc := New(store, eng, WithClock(clk.now), WithTTL(15*time.Minute))

	f := &fixture{t: t, store: store, clk: clk, eng: eng, svc: svc}
	// Operator's OWN authority and the target's authority — disjoint objects.
	f.grant("g-op-read", acctAcme, "op", permRead, objOpOnly, model.EffectAllow)
	f.grant("g-tgt-read", acctAcme, "tgt", permRead, objTgtOnly, model.EffectAllow)
	return f
}

func (f *fixture) grant(id, account, principal, perm, object string, effect model.Effect) {
	f.t.Helper()
	g := model.Grant{
		ID:           id,
		AccountID:    account,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: principal},
		PermissionID: perm,
		Object:       object,
		Effect:       effect,
	}
	if err := f.store.PutGrant(context.Background(), g); err != nil {
		f.t.Fatalf("seed grant %s: %v", id, err)
	}
}

// grantRight gives op an impersonation right (augment or become) over object
// pattern (covering some target identity).
func (f *fixture) grantRight(id, perm, object string) {
	f.grant(id, acctAcme, "op", perm, object, model.EffectAllow)
}

func (f *fixture) checkAs(account, principal, action, object string, ic engine.ImpersonationContext) engine.Decision {
	f.t.Helper()
	dec, err := f.eng.CheckAs(context.Background(), engine.Request{
		Account: account, Principal: principal, Action: action, Object: object,
	}, ic)
	if err != nil {
		f.t.Fatalf("checkAs: %v", err)
	}
	return dec
}

func mustCode(t *testing.T, err error, want aerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", want)
	}
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("error code = %s, want %s (err: %v)", got, want, err)
	}
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("not a coded error: %v", err)
	}
	r, _ := ce.Context["reason"].(string)
	return r
}

// TestAugmentAddsTargetToOperator: augment resolves over the UNION — the operator
// gains the target's permissions ADDED to its own, still under its own identity.
func TestAugmentAddsTargetToOperator(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-augment", permAugment, "user:*")
	ctx := context.Background()

	// Baseline (no impersonation): op may read its own object, not the target's.
	if !f.checkAs(acctAcme, "op", "read", objOpOnly, engine.ImpersonationContext{}).Allow {
		t.Fatalf("op should read its own object without impersonation")
	}
	if f.checkAs(acctAcme, "op", "read", objTgtOnly, engine.ImpersonationContext{}).Allow {
		t.Fatalf("op should NOT read target's object without impersonation")
	}

	sess, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeAugment)
	if err != nil {
		t.Fatalf("start augment: %v", err)
	}
	ic := sess.Context()

	// Augment = own + target's: BOTH objects are now readable under op's identity.
	own := f.checkAs(acctAcme, "op", "read", objOpOnly, ic)
	if !own.Allow {
		t.Fatalf("augment must keep the operator's OWN permissions")
	}
	tgt := f.checkAs(acctAcme, "op", "read", objTgtOnly, ic)
	if !tgt.Allow {
		t.Fatalf("augment must ADD the target's permissions")
	}
	// Real + effective actor propagation on every impersonated decision.
	for _, d := range []engine.Decision{own, tgt} {
		if d.Impersonation == nil {
			t.Fatalf("decision missing impersonation context")
		}
		if d.Impersonation.RealActor != "op" || d.Impersonation.EffectiveSubject != "tgt" || d.Impersonation.Mode != engine.ModeAugment {
			t.Fatalf("bad impersonation context: %+v", d.Impersonation)
		}
	}
}

// TestBecomeAssumesTargetOnly: become resolves over the TARGET'S subject set
// alone — the operator's own permissions do NOT apply, distinguishing it from
// augment.
func TestBecomeAssumesTargetOnly(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-become", permBecome, "user:*")
	ctx := context.Background()

	sess, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome)
	if err != nil {
		t.Fatalf("start become: %v", err)
	}
	ic := sess.Context()

	// Become = target only: target's object readable, operator's OWN object is NOT.
	if !f.checkAs(acctAcme, "op", "read", objTgtOnly, ic).Allow {
		t.Fatalf("become must grant the target's permissions")
	}
	if f.checkAs(acctAcme, "op", "read", objOpOnly, ic).Allow {
		t.Fatalf("become must DROP the operator's own permissions (full assumption)")
	}
	dec := f.checkAs(acctAcme, "op", "read", objTgtOnly, ic)
	if dec.Impersonation == nil || dec.Impersonation.Mode != engine.ModeBecome ||
		dec.Impersonation.RealActor != "op" || dec.Impersonation.EffectiveSubject != "tgt" {
		t.Fatalf("become decision must record the real actor + mode: %+v", dec.Impersonation)
	}
}

// TestTimeBoxExpiry: a session expires automatically; past expiry the decision
// fails closed with NO elevation (the operator's own authority only).
func TestTimeBoxExpiry(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-become", permBecome, "user:*")
	ctx := context.Background()

	sess, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ic := sess.Context()

	// In-window: become grants the target's object.
	if !f.checkAs(acctAcme, "op", "read", objTgtOnly, ic).Allow {
		t.Fatalf("precondition: in-window become should allow")
	}

	// Advance past the 15m time-box. The SAME presented context now confers no
	// elevation: the decision falls back to op's own authority and denies.
	f.clk.advance(16 * time.Minute)
	dec := f.checkAs(acctAcme, "op", "read", objTgtOnly, ic)
	if dec.Allow {
		t.Fatalf("expired session must not elevate")
	}
	if dec.Impersonation != nil {
		t.Fatalf("expired session must not record an active impersonation: %+v", dec.Impersonation)
	}
	// Session.Live is the hard-error guard for surfaces.
	if _, err := sess.Live(f.clk.now()); err == nil {
		t.Fatalf("Live should reject an expired session")
	} else {
		mustCode(t, err, aerr.APERTURE_IMPERSONATION_EXPIRED)
	}
}

// TestBecomeGatingStrictlyStronger: the become right is strictly stronger than
// augment. An operator holding only the augment right CANNOT become; the become
// right holder CAN augment (it implies augment).
func TestBecomeGatingStrictlyStronger(t *testing.T) {
	ctx := context.Background()

	// (a) augment right only: augment succeeds, become is denied.
	f := newFixture(t)
	f.grantRight("g-op-augment", permAugment, "user:*")
	if _, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeAugment); err != nil {
		t.Fatalf("augment right should permit augment: %v", err)
	}
	_, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome)
	mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
	if r := reasonOf(t, err); r != "no_become_right" {
		t.Fatalf("reason = %q, want no_become_right", r)
	}

	// (b) become right implies augment: both modes succeed.
	g := newFixture(t)
	g.grantRight("g-op-become", permBecome, "user:*")
	if _, err := g.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome); err != nil {
		t.Fatalf("become right should permit become: %v", err)
	}
	if _, err := g.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeAugment); err != nil {
		t.Fatalf("become right should also permit augment (implies): %v", err)
	}
}

// TestStartNoRight: an operator with no impersonation right at all cannot start.
func TestStartNoRight(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Start(context.Background(), "op", "tgt", acctAcme, engine.ModeAugment)
	mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
	if r := reasonOf(t, err); r != "no_augment_right" {
		t.Fatalf("reason = %q, want no_augment_right", r)
	}
}

// TestStartRightScopedToTarget: the right's object pattern scopes WHICH targets
// may be impersonated. A right over "user:tgt" cannot reach "user:other".
func TestStartRightScopedToTarget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A second valid target in acme.
	if err := f.store.PutPrincipal(ctx, model.Principal{ID: "other", Kind: model.PrincipalUser, Identity: "user:other"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := f.store.PutMembership(ctx, model.Membership{PrincipalID: "other", AccountID: acctAcme}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// op may become only user:tgt.
	f.grantRight("g-op-become-tgt", permBecome, "user:tgt")

	if _, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome); err != nil {
		t.Fatalf("should permit the in-scope target: %v", err)
	}
	_, err := f.svc.Start(ctx, "op", "other", acctAcme, engine.ModeBecome)
	mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
	if r := reasonOf(t, err); r != "no_become_right" {
		t.Fatalf("reason = %q, want no_become_right", r)
	}
}

// TestCrossAccountRefusedAtStart: a target who is not a member of the active
// account cannot be impersonated; the session is refused.
func TestCrossAccountRefusedAtStart(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-become", permBecome, "user:*")
	// stranger is a member of "other", not acme.
	_, err := f.svc.Start(context.Background(), "op", "stranger", acctAcme, engine.ModeBecome)
	mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
	if r := reasonOf(t, err); r != "cross_account" {
		t.Fatalf("reason = %q, want cross_account", r)
	}
}

// TestOperatorNotMemberRefused: an operator who is not a member of the active
// account cannot start a session there.
func TestOperatorNotMemberRefused(t *testing.T) {
	f := newFixture(t)
	// op is a member of acme only; try to act in "other".
	if err := f.store.PutMembership(context.Background(), model.Membership{PrincipalID: "tgt", AccountID: acctOther}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := f.svc.Start(context.Background(), "op", "tgt", acctOther, engine.ModeBecome)
	mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
	if r := reasonOf(t, err); r != "operator_not_member" {
		t.Fatalf("reason = %q, want operator_not_member", r)
	}
}

// TestEngineRefusesCrossAccountContext: even a hand-forged active context whose
// target is outside the active account is refused by the engine itself — the
// account boundary is enforced on EVERY decision, not just at Start.
func TestEngineRefusesCrossAccountContext(t *testing.T) {
	f := newFixture(t)
	// stranger belongs to "other", not acme; forge an active become context.
	ic := engine.ImpersonationContext{
		RealActor:        "op",
		EffectiveSubject: "stranger",
		Mode:             engine.ModeBecome,
		ExpiresAt:        f.clk.now().Add(time.Hour),
	}
	dec := f.checkAs(acctAcme, "op", "read", objTgtOnly, ic)
	if dec.Allow {
		t.Fatalf("engine must refuse a cross-account impersonation context")
	}
	if dec.Impersonation == nil || dec.Impersonation.EffectiveSubject != "stranger" {
		t.Fatalf("refused decision should still record the attempted context for audit: %+v", dec.Impersonation)
	}
}

// TestNonImpersonatedPathUnchanged: CheckAs with a zero (none-mode) context
// behaves exactly like Check — the non-impersonated path is untouched.
func TestNonImpersonatedPathUnchanged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	plain, err := f.eng.Check(ctx, engine.Request{Account: acctAcme, Principal: "op", Action: "read", Object: objOpOnly})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	as := f.checkAs(acctAcme, "op", "read", objOpOnly, engine.ImpersonationContext{})
	if plain.Allow != as.Allow || as.Impersonation != nil {
		t.Fatalf("CheckAs with none-mode must equal Check: plain=%+v as=%+v", plain, as)
	}
}

// TestExplainAsRecordsBothActors: ExplainAs resolves over the effective subject
// set while the trace's Request still names the real operator, and the
// impersonation context is attached for audit.
func TestExplainAsRecordsBothActors(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-become", permBecome, "user:*")
	ctx := context.Background()

	sess, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tr, err := f.eng.ExplainAs(ctx, engine.Request{
		Account: acctAcme, Principal: "op", Action: "read", Object: objTgtOnly,
	}, sess.Context())
	if err != nil {
		t.Fatalf("explainAs: %v", err)
	}
	if tr.Request.Principal != "op" {
		t.Fatalf("trace must name the real operator, got %q", tr.Request.Principal)
	}
	if tr.Impersonation == nil || tr.Impersonation.EffectiveSubject != "tgt" {
		t.Fatalf("trace must attach impersonation context: %+v", tr.Impersonation)
	}
	// Become resolves over the target's subject set.
	foundTgt := false
	for _, s := range tr.Subjects {
		if s.Kind == model.SubjectPrincipal && s.ID == "tgt" {
			foundTgt = true
		}
		if s.Kind == model.SubjectPrincipal && s.ID == "op" {
			t.Fatalf("become trace must not include the operator's own subject")
		}
	}
	if !foundTgt {
		t.Fatalf("become trace must resolve over the target's subject set")
	}
	if !tr.Decision.Allow {
		t.Fatalf("become should allow the target's object")
	}
}

// TestEnumerateAsUsesEffectiveSet: EnumerateAs lists the objects the effective
// subject set may act on. Under become, that is the target's objects only.
func TestEnumerateAsUsesEffectiveSet(t *testing.T) {
	f := newFixture(t)
	f.grantRight("g-op-become", permBecome, "user:*")
	ctx := context.Background()

	sess, err := f.svc.Start(ctx, "op", "tgt", acctAcme, engine.ModeBecome)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ids, err := f.eng.EnumerateAs(ctx, engine.EnumerateRequest{
		Account: acctAcme, Principal: "op", Action: "read", Pattern: "account:acme/**",
	}, sess.Context())
	if err != nil {
		t.Fatalf("enumerateAs: %v", err)
	}
	// Become => target's object present, operator's own absent.
	var hasTgt, hasOp bool
	for _, id := range ids {
		switch id {
		case objTgtOnly:
			hasTgt = true
		case objOpOnly:
			hasOp = true
		}
	}
	if !hasTgt {
		t.Fatalf("become enumerate should include the target's object, got %v", ids)
	}
	if hasOp {
		t.Fatalf("become enumerate must not include the operator's own object, got %v", ids)
	}
}

// TestMayStartUnit unit-tests the pure gating rule across the four right/mode
// combinations, with no storage.
func TestMayStartUnit(t *testing.T) {
	target := identity.MustParsePattern("user:tgt")
	augmentRight := []authority{{
		grant:      model.Grant{Object: "user:*"},
		permission: model.Permission{Action: AugmentAction},
	}}
	becomeRight := []authority{{
		grant:      model.Grant{Object: "user:*"},
		permission: model.Permission{Action: BecomeAction},
	}}

	cases := []struct {
		name    string
		mode    engine.Mode
		held    []authority
		wantErr bool
	}{
		{"augment-right augments", engine.ModeAugment, augmentRight, false},
		{"augment-right cannot become", engine.ModeBecome, augmentRight, true},
		{"become-right becomes", engine.ModeBecome, becomeRight, false},
		{"become-right augments (implies)", engine.ModeAugment, becomeRight, false},
		{"no right, no augment", engine.ModeAugment, nil, true},
		{"no right, no become", engine.ModeBecome, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mayStart(tc.mode, target, tc.held)
			if tc.wantErr && err == nil {
				t.Fatalf("expected denial, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected allow, got %v", err)
			}
			if tc.wantErr {
				mustCode(t, err, aerr.APERTURE_IMPERSONATION_DENIED)
			}
		})
	}
}
