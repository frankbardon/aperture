package cli

import (
	"context"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
)

// A pushed `declared_keys:` reaches the REGISTRY of a DB-wired boot, not only the
// rule gate.
//
// The projection onto a seed.Document used to drop the set deliberately, because the
// set's only job was definition-time — which keys a rule may name — and the facade
// decides that against the stored rows directly (declared_keys.go). The set now also
// RESERVES its keys to the slot's shared layer, and the thing that applies the
// reservation is the attribute registry the projected document builds. So a
// projection that still dropped it would give a DB-wired instance a merge that
// suppresses nothing while a file-wired instance with the identical wiring
// suppressed correctly: the same declaration, two different bags, on the two
// topologies wiring_boot.go exists to keep identical, and the difference reads as an
// ordinary attribute value in every verdict, trace and note.
//
// This is asserted through the real builders over a real database (hostUsersDB), for
// the reason the layering tests beside it are: a fake pool proves the layers were
// registered and nothing about which one a key comes out of.

// declaringUserSlot is sharedUserSlot with a declared set on the row. The wiring is
// otherwise identical, so the only variable between the two cases below is the
// declaration.
func declaringUserSlot(now time.Time, declared model.DeclaredKeys) model.WiringSet {
	set := sharedUserSlot(now)
	set.AttributeProviders[0].DeclaredKeys = declared
	return set
}

// declaredBootLocal is this instance's own file: it contests `department` for a
// subject the shared directory HAS, adds an undeclared `cost_centre`, and still
// lists `carol`, whom the directory does NOT have. The three cases are the contested
// key, the additive key, and the revoked subject.
const declaredBootLocal = `
attributes:
  - subject: user
    id: alice
    metadata: {department: hr, cost_centre: cc-42}
  - subject: user
    id: carol
    metadata: {department: hr, cost_centre: cc-9}
`

// TestAPushedDeclaredKeySetReservesTheSlotOnADBWiredBoot: with `department`
// declared on the pushed row, the local file answers it for nobody — not for alice,
// whom the directory answers for anyway, and not for carol, whom the directory no
// longer lists. `cost_centre`, which the row does not declare, still answers, which
// is the additive guarantee the projection must not trade away to get the
// reservation.
func TestAPushedDeclaredKeySetReservesTheSlotOnADBWiredBoot(t *testing.T) {
	ctx := context.Background()
	attrs := declaredBootRegistry(t, model.DeclaredKeys{Declared: true, Keys: []string{"department"}})

	alice, err := attrs.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(alice): %v", err)
	}
	if alice["department"] != "eng" {
		t.Errorf("alice department = %#v, want the directory's \"eng\"", alice["department"])
	}
	if alice["cost_centre"] != "cc-42" {
		t.Errorf("alice cost_centre = %#v; an undeclared key is still the local layer's to "+
			"add, and a reservation that swallowed it would be the discard this layering "+
			"replaced", alice["cost_centre"])
	}

	// The revocation. carol is not in the directory, so a declared key of hers is
	// answered by nobody — which is what makes deleting her from the shared source
	// take effect on an instance whose own file still names her.
	carol, err := attrs.Attributes(ctx, "user", "carol")
	if err != nil {
		t.Fatalf("Attributes(carol): %v", err)
	}
	if got, ok := carol["department"]; ok {
		t.Errorf("carol department = %#v; the pushed row declares department and the directory "+
			"has no record for her, so the projection must have carried the declared set — if it "+
			"dropped it, this instance keeps authorizing a subject the fleet's directory dropped",
			got)
	}
	if carol["cost_centre"] != "cc-9" {
		t.Errorf("carol bag = %#v; the undeclared remainder still answers", carol)
	}
}

// TestAPushedRowThatDeclaresNothingReservesNothingOnADBWiredBoot is the opt-in bar
// on this path: every fleet that has never pushed a `declared_keys:` boots the
// registry it always did. It shares the fixture with the case above, so the only
// difference between green and red here is the declaration itself.
func TestAPushedRowThatDeclaresNothingReservesNothingOnADBWiredBoot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		declared model.DeclaredKeys
	}{
		{name: "not declared", declared: model.DeclaredKeys{}},
		// DECLARED EMPTY names no key, so it reserves none — a different STATE with
		// the same effect here, and the state is what a plain slice would lose.
		{name: "declared empty", declared: model.DeclaredKeys{Declared: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := declaredBootRegistry(t, tc.declared)
			carol, err := attrs.Attributes(ctx, "user", "carol")
			if err != nil {
				t.Fatalf("Attributes(carol): %v", err)
			}
			if carol["department"] != "hr" {
				t.Errorf("carol department = %#v; with nothing reserved, a subject only the local "+
					"layer knows is answered from it in full, exactly as before declaring existed",
					carol["department"])
			}
		})
	}
}

// declaredBootRegistry runs the whole DB-wired boot for one declared set: the rows
// are projected onto a document, the document builds both registries over the host
// database, and the attribute registry comes back for the caller to fetch through.
func declaredBootRegistry(t *testing.T, declared model.DeclaredKeys) attributeFetcher {
	t.Helper()
	db := hostUsersDB(t)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	local, err := seed.Parse([]byte(declaredBootLocal), seed.FormatYAML)
	if err != nil {
		t.Fatalf("parse the local document: %v", err)
	}
	doc, err := wiringDocument(declaringUserSlot(time.Now().UTC(), declared), local)
	if err != nil {
		t.Fatalf("wiringDocument: %v", err)
	}
	_, conns, err := doc.BuildRegistryWithConnections("",
		seed.WithConnectionOpener(func(string, seed.ConnectionSettings) (seed.Pool, error) {
			return db, nil
		}))
	if err != nil {
		t.Fatalf("building the object registry over the projected document: %v", err)
	}
	t.Cleanup(func() { _ = conns.Close() })

	attrs, err := doc.BuildAttributeRegistryWithConnections("", conns)
	if err != nil {
		t.Fatalf("building the attribute registry over the projected document: %v", err)
	}
	return attrs
}

// attributeFetcher is the one method these cases use, named so the helper's return
// type says what it is for rather than exposing the whole registry.
type attributeFetcher interface {
	Attributes(ctx context.Context, kind, principal string) (map[string]any, error)
}
