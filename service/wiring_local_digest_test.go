package service

import (
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// THE LOCAL DIGEST, AND WHY IT IS A SECOND FIELD.
//
// The shared digest answers "did this instance get the push?" and has to stay
// byte-comparable with what `aperture wiring push` wrote and with what
// `aperture wiring diff` reports. It is therefore a digest of the five shared
// tables and of nothing else — which left the fleet sweep the operator
// documentation recommends overstating what it proved, because a rebuild ALSO
// re-reads the instance's own local document and that document decides things.
// Two instances could report the identical Digest and return different verdicts.
//
// So there are two digests, reported side by side and never mixed, and the cases
// here hold the two properties that make the pair worth having:
//
//   - the fields are SEPARATE, so Digest stays what it was; and
//   - they advance TOGETHER, because a posture naming a fresh shared digest
//     beside a superseded local one is worse than one naming no local digest at
//     all: it is a pair no version was ever built from, and an operator sweeping
//     it concludes two instances agree when they do not.

// TestBothDigestsAreReportedAndNeitherIsMixedIntoTheOther is the separation, from
// the recorder's own side.
//
// It matters most for Digest. The tempting implementation of this feature was to
// fold the local content into the existing digest, which would have produced a
// value matching neither a push nor a wiring diff — breaking the one sweep the
// field exists for while looking like an improvement.
func TestBothDigestsAreReportedAndNeitherIsMixedIntoTheOther(t *testing.T) {
	h := NewWiringHealth(30*time.Second, WiringDigests{Shared: "shared-boot", Local: "local-boot"}, nil)

	p := h.Posture()
	if p.Digest != "shared-boot" {
		t.Errorf("Digest = %q, want %q verbatim: it must stay comparable with what a push wrote and with "+
			"what `aperture wiring diff` reports, so nothing local may be mixed into it", p.Digest, "shared-boot")
	}
	if p.LocalDigest != "local-boot" {
		t.Errorf("LocalDigest = %q, want %q: the local half is what makes a fleet sweep conclusive",
			p.LocalDigest, "local-boot")
	}
}

// TestARefreshAdvancesBothDigestsOrNeither is the pair's load-bearing property,
// and the empty-Local case is the one that catches the likely bug.
//
// An implementation that assigned only the shared half would pass any assertion
// written with two non-empty digests that both changed, because the stale local
// value would simply have been overwritten by... nothing, and nobody would look.
// Advancing to a pair whose local half is EMPTY — an instance that dropped its
// --seed file between two versions — is the shape where a half-assignment leaves
// the previous document's digest standing, reporting a configuration that no
// longer exists.
func TestARefreshAdvancesBothDigestsOrNeither(t *testing.T) {
	h := NewWiringHealth(30*time.Second, WiringDigests{Shared: "shared-1", Local: "local-1"}, nil)

	h.Refreshed(WiringDigests{Shared: "shared-2", Local: "local-2"})
	if p := h.Posture(); p.Digest != "shared-2" || p.LocalDigest != "local-2" {
		t.Errorf("after a refresh the posture reported %q / %q, want shared-2 / local-2: the pair is what "+
			"the installed version was built from, and both halves move with it",
			p.Digest, p.LocalDigest)
	}

	h.Refreshed(WiringDigests{Shared: "shared-3"})
	if p := h.Posture(); p.LocalDigest != "" {
		t.Errorf("LocalDigest = %q after a refresh that reported no local document; want empty. The pair is "+
			"recorded WHOLE — a recorder that assigned only the shared half would go on naming the "+
			"document of a version this process no longer runs, which is a pair no version was built from",
			p.LocalDigest)
	} else if p.Digest != "shared-3" {
		t.Errorf("Digest = %q, want shared-3", p.Digest)
	}
}

// TestAFailedRefreshLeavesBOTHDigestsWhereTheyAre is last-good, restated for the
// pair. A failure adopts nothing, so it must move neither half: the instance is
// still deciding through the version it had, from the document it had.
func TestAFailedRefreshLeavesBOTHDigestsWhereTheyAre(t *testing.T) {
	h := NewWiringHealth(30*time.Second, WiringDigests{Shared: "shared-1", Local: "local-1"}, nil)

	h.Failed(aerr.New(aerr.APERTURE_STORAGE, "no route to host"))

	p := h.Posture()
	if !p.Stale {
		t.Fatal("the alarm did not fire")
	}
	if p.Digest != "shared-1" || p.LocalDigest != "local-1" {
		t.Errorf("a failed refresh moved a digest (%q / %q), want shared-1 / local-1: nothing was adopted, "+
			"so this instance is still deciding from the wiring and the document it had",
			p.Digest, p.LocalDigest)
	}
}

// TestARefusedReadDisclosesNoLocalDigestEither is the gate, for the new field.
//
// The check order is the contract: the gate runs before the recorder is consulted,
// so a refusal is identical whatever the recorder holds. This asserts the two
// halves of that for the local digest — the refused caller gets the zero posture
// (no local digest at all), and the refusal's TEXT is the same whether or not the
// instance has a local document.
func TestARefusedReadDisclosesNoLocalDigestEither(t *testing.T) {
	refusalFor := func(t *testing.T, d WiringDigests) string {
		t.Helper()
		svc, _, ctx := attributeFixture(t)
		WithWiringHealth(NewWiringHealth(30*time.Second, d, nil))(svc)
		p, err := svc.WiringPosture(ctx, deniedActor)
		if err == nil {
			t.Fatal("a non-admin read was not refused")
		}
		if p != (WiringPosture{}) {
			t.Errorf("a refused read returned %+v; it must return nothing at all, the local digest included", p)
		}
		return err.Error()
	}

	withLocal := refusalFor(t, WiringDigests{Shared: "shared-boot", Local: "abc123localdigest"})
	withoutLocal := refusalFor(t, WiringDigests{Shared: "shared-boot"})
	if withLocal != withoutLocal {
		t.Errorf("the refusal distinguishes an instance with a local document from one without:\n"+
			"  with: %s\n  without: %s\nthe gate must run before the recorder is consulted",
			withLocal, withoutLocal)
	}
	if strings.Contains(withLocal, "abc123localdigest") {
		t.Errorf("the refusal carries the local digest: %s", withLocal)
	}
}

// TestTheAdminReadCarriesBothDigests is the same read from the other side of the
// gate: the authorised caller gets the pair, which is the whole point of adding
// the field rather than leaving the fleet sweep inconclusive.
func TestTheAdminReadCarriesBothDigests(t *testing.T) {
	svc, _, ctx := attributeFixture(t)
	WithWiringHealth(NewWiringHealth(30*time.Second,
		WiringDigests{Shared: "shared-boot", Local: "local-boot"}, nil))(svc)

	p, err := svc.WiringPosture(ctx, adminActor)
	if err != nil {
		t.Fatalf("a system-admin must be able to read the wiring posture: %v", err)
	}
	if p.Digest != "shared-boot" || p.LocalDigest != "local-boot" {
		t.Errorf("the admin read %q / %q, want shared-boot / local-boot", p.Digest, p.LocalDigest)
	}
}

// TestCapabilitiesDidNotAbsorbTheLocalDigestEither extends the existing
// Capabilities contract to the new field, and for exactly the same reason.
//
// "These two instances are running different local documents" is a fact about a
// configuration fault, and the digest of an instance's own file is not something
// an anonymous caller is owed. Capabilities carries booleans of immutable
// boot-time configuration and cannot fail; this moves with every adoption.
func TestCapabilitiesDidNotAbsorbTheLocalDigestEither(t *testing.T) {
	h := NewWiringHealth(30*time.Second, WiringDigests{Shared: "shared-1", Local: "local-1"}, nil)
	svc := New(nil, WithWiringHealth(h))

	before := svc.Capabilities()
	h.Refreshed(WiringDigests{Shared: "shared-2", Local: "local-2"})
	if after := svc.Capabilities(); after != before {
		t.Errorf("Capabilities changed when the local digest advanced (%+v -> %+v). It promises immutable "+
			"boot-time booleans, which is what lets every surface expose it unauthenticated; the digests "+
			"live on the gated WiringPosture read", before, after)
	}
}
