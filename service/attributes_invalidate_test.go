package service

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/storage/memory"
)

// E5-S3: dropping a cached bag is a system-tier operation.
//
// The fixture is attributeFixture (attributes_test.go): alice is a system-admin
// in acme, mallory is an ordinary principal, and one registry serves both the
// decision path and the admin doors. Reusing it is the point — the claim is that
// invalidation sits behind THE SAME gate the listing does, checked in the same
// order, so it must be provable on the same wiring.

// TestInvalidatingAttributesRequiresSystemAdmin: the same three calls, made by
// two authenticated principals.
func TestInvalidatingAttributesRequiresSystemAdmin(t *testing.T) {
	svc, spy, ctx := attributeFixture(t)

	// One admin listing, so the directory has been read once and the counts below
	// have a baseline. A listing does NOT write the slot's cache (only a
	// decision-path Fetch does — see provider.AttributeRegistry.Enumerate), which
	// is why the baseline taken here is the FETCH count: it is zero, and the claim
	// below is that a refused invalidation leaves it zero.
	if _, err := svc.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{}); err != nil {
		t.Fatalf("warm-up listing: %v", err)
	}
	fetchesBefore, _, _ := spy.counts()

	if _, err := svc.InvalidateAttribute(ctx, deniedActor, "user", "mallory"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Errorf("non-admin InvalidateAttribute = %v, want APERTURE_AUTHZ_DENIED", err)
	}
	if err := svc.InvalidateAttributeSlot(ctx, deniedActor, "user"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Errorf("non-admin InvalidateAttributeSlot = %v, want APERTURE_AUTHZ_DENIED", err)
	}
	if err := svc.InvalidateAllAttributes(ctx, deniedActor); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Errorf("non-admin InvalidateAllAttributes = %v, want APERTURE_AUTHZ_DENIED", err)
	}

	// A refusal must not have dropped anything, and it must not have cost the
	// decision path a directory round-trip. This is the DoS half of the reason the
	// gate is there — a refused caller in a loop must not be able to make every
	// decision re-read the host's user table.
	if _, err := svc.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{}); err != nil {
		t.Fatalf("post-refusal listing: %v", err)
	}
	if got, _, _ := spy.counts(); got != fetchesBefore {
		t.Errorf("the directory was fetched %d extra time(s) after a REFUSED invalidation", got-fetchesBefore)
	}

	// The admin's own calls succeed, all three forms.
	if _, err := svc.InvalidateAttribute(ctx, adminActor, "user", "mallory"); err != nil {
		t.Errorf("admin InvalidateAttribute: %v", err)
	}
	if err := svc.InvalidateAttributeSlot(ctx, adminActor, "user"); err != nil {
		t.Errorf("admin InvalidateAttributeSlot: %v", err)
	}
	if err := svc.InvalidateAllAttributes(ctx, adminActor); err != nil {
		t.Errorf("admin InvalidateAllAttributes: %v", err)
	}
}

// TestAnInvalidationRefusalDisclosesNothingAboutTheSlot: the gate runs BEFORE
// the slot name is parsed, so an unauthorized caller gets one indistinguishable
// refusal whether the slot is real, unwired, or invented. Parsing first would
// turn the error code into a directory of which slots this deployment has.
func TestAnInvalidationRefusalDisclosesNothingAboutTheSlot(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	// user is wired, account is a real slot with no provider, unicorn is not a
	// slot at all.
	for _, slot := range []string{"user", "account", "unicorn"} {
		_, err := svc.InvalidateAttribute(ctx, deniedActor, slot, "mallory")
		if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
			t.Errorf("refused InvalidateAttribute(%q) = %v, want APERTURE_AUTHZ_DENIED for every slot alike", slot, err)
		}
		if err := svc.InvalidateAttributeSlot(ctx, deniedActor, slot); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
			t.Errorf("refused InvalidateAttributeSlot(%q) = %v, want APERTURE_AUTHZ_DENIED for every slot alike", slot, err)
		}
	}

	// The caller who IS allowed gets the real, actionable diagnostics: for that
	// caller they are wiring facts, not disclosures.
	if _, err := svc.InvalidateAttribute(ctx, adminActor, "unicorn", "x"); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
		t.Errorf("admin invalidating an unknown slot = %v, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN", err)
	}
	if err := svc.InvalidateAttributeSlot(ctx, adminActor, "account"); aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
		t.Errorf("admin clearing an unwired slot = %v, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", err)
	}
	// The key guard reaches the admin too, and stays one coded error deep.
	_, err := svc.InvalidateAttribute(ctx, adminActor, "user", "*")
	if aerr.CodeOf(err) != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
		t.Errorf("admin invalidating the wildcard key = %v, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", err)
	}
	if d := codedAttributeDepth(err); d != 1 {
		t.Errorf("coded chain depth = %d, want exactly 1", d)
	}
}

// TestInvalidationClosesTheStalenessWindow drives the whole point end to end
// through the facade: a bag cached by a real decision keeps being served after
// the host has changed it, and the admin's invalidation is what makes the change
// take effect.
func TestInvalidationClosesTheStalenessWindow(t *testing.T) {
	ctx := context.Background()
	dir := &mutableDirectory{bags: map[string]provider.Metadata{
		"mallory": {"tier": "gold"},
	}}
	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotUser, dir)

	svc, _, _ := attributeFixture(t)
	// A second facade over the SAME gate wiring but a directory this test can
	// rewrite. The gate comes from the fixture's engine, so authority still
	// resolves; only the directory differs.
	svc = New(svc.eng, WithGate(svc.gate), WithAttributes(attrs))

	first, err := svc.ListAttributes(ctx, adminActor, "user", provider.AttributeFilter{})
	if err != nil {
		t.Fatalf("first listing: %v", err)
	}
	if len(first) != 1 || first[0].Attributes["tier"] != "gold" {
		t.Fatalf("first listing = %+v, want mallory at tier gold", first)
	}

	// The cache is warmed by a DECISION-PATH fetch, which is the only thing that
	// warms it: an admin listing reads the directory without writing the slot's
	// cache, because Query's bag is allowed to be a narrower projection than
	// Fetch's (see provider.AttributeRegistry.Enumerate). That is also what the
	// window this test is about is made of — a bag some decision resolved.
	if md, err := attrs.Fetch(ctx, provider.AttributeSlotUser, "mallory"); err != nil {
		t.Fatalf("warming fetch: %v", err)
	} else if md["tier"] != "gold" {
		t.Fatalf("warming fetch = %v, want gold", md["tier"])
	}

	// The host demotes mallory. The decision path is still answering "gold".
	dir.bags["mallory"] = provider.Metadata{"tier": "bronze"}
	md, err := attrs.Fetch(ctx, provider.AttributeSlotUser, "mallory")
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Fatalf("cached fetch = %v, want the stale gold (the window Invalidate closes)", md["tier"])
	}

	dropped, err := svc.InvalidateAttribute(ctx, adminActor, "user", "mallory")
	if err != nil {
		t.Fatalf("InvalidateAttribute: %v", err)
	}
	if !dropped {
		t.Fatal("InvalidateAttribute reported no cached entry for a key the listing had just warmed")
	}
	md, err = attrs.Fetch(ctx, provider.AttributeSlotUser, "mallory")
	if err != nil {
		t.Fatalf("post-invalidation fetch: %v", err)
	}
	if md["tier"] != "bronze" {
		t.Fatalf("post-invalidation fetch = %v, want bronze", md["tier"])
	}
}

// TestTheUnwiredAndUnauthenticatedInvalidationRefusals mirrors the listing's
// equivalent: the two failures that precede the authority check, in the same
// order, because two doors onto one directory that checked their preconditions
// differently is how one ends up disclosing what the other refuses.
func TestTheUnwiredAndUnauthenticatedInvalidationRefusals(t *testing.T) {
	svc, _, ctx := attributeFixture(t)

	if _, err := svc.InvalidateAttribute(ctx, Actor{Account: "acme"}, "user", "alice"); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("anonymous invalidation = %v, want APERTURE_UNAUTHENTICATED", err)
	}
	if err := svc.InvalidateAllAttributes(ctx, Actor{Account: "acme"}); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("anonymous InvalidateAllAttributes = %v, want APERTURE_UNAUTHENTICATED", err)
	}

	// No registry at all.
	bare := New(engine.New(memory.New()))
	if err := bare.InvalidateAllAttributes(ctx, adminActor); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("unwired facade invalidation = %v, want APERTURE_UNIMPLEMENTED", err)
	}

	// A registry but NO GATE — the fail-open shape. A cache drop with nothing to
	// authorize against must refuse rather than proceed.
	attrs := provider.NewAttributeRegistry()
	attrs.MustRegister(provider.AttributeSlotUser, &mutableDirectory{bags: map[string]provider.Metadata{"u": {}}})
	ungated := New(engine.New(memory.New()), WithAttributes(attrs))
	if err := ungated.InvalidateAttributeSlot(ctx, adminActor, "user"); aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Errorf("gateless invalidation = %v, want APERTURE_UNIMPLEMENTED", err)
	}
}

// mutableDirectory is an AttributeProvider whose bags can be rewritten during a
// test, so a stale cache and a fresh read differ by VALUE rather than by a count.
type mutableDirectory struct {
	bags map[string]provider.Metadata
}

func (d *mutableDirectory) Fetch(_ context.Context, id string) (provider.Metadata, error) {
	md, ok := d.bags[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND, "test: no such key", map[string]any{"key": id})
	}
	return md, nil
}

func (d *mutableDirectory) List(context.Context) ([]provider.AttributeRecord, error) {
	return d.records(), nil
}

func (d *mutableDirectory) Query(context.Context, provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	return d.records(), nil
}

func (d *mutableDirectory) records() []provider.AttributeRecord {
	out := make([]provider.AttributeRecord, 0, len(d.bags))
	for id, md := range d.bags {
		out = append(out, provider.AttributeRecord{ID: id, Attributes: md})
	}
	return out
}
