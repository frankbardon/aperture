package seed

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/provider"
)

// `declared_keys:` reaches the registry, and reserving is what it does there.
//
// The field already had a definition-time job — a rule may name only the keys a
// declaring slot declares — which the facade enforces against the stored wiring. It
// now has a second: the declaring layer is the only layer that ANSWERS those keys,
// so an inline `attributes:` block stops contributing them even where the shared
// source omits one for a row or has no record for the subject at all.
//
// These tests are at the SEED layer because that is where the two sections become
// two layers, and because the wiring here is the only place the pointer-shaped
// `declared_keys:` is turned into provider.WithDeclaredKeys. A build that dropped
// the set would leave the registry merging as it did before, on a document that
// says it should not — and nothing else in this package would go red.

// declaredKeysCSV is the shared directory: alice has no clearance column value
// (the (a) shape — a hole a NULL leaves), and carol is not in it at all (the (b)
// shape — a subject the directory has dropped).
const declaredKeysCSV = `id,department,clearance:int
alice,eng,
bob,ops,5
`

// declaredKeysDoc wires that file as the shared layer of the user slot and an
// inline block as its local layer, with the two contesting `clearance` for alice
// and the local block still listing carol. declared is spliced in verbatim so a
// case can write the key, write it empty, or leave it out.
func declaredKeysDoc(t *testing.T, declared string) *Document {
	t.Helper()
	return attributeDoc(t, `
attribute_providers:
  - subject: user
    kind: csv
    path: users.csv
`+declared+`
attributes:
  - subject: user
    id: alice
    metadata:
      clearance: 9
      team: atlas
  - subject: user
    id: carol
    metadata:
      clearance: 9
      team: atlas
`)
}

// TestAttributeProviders_ADeclaredKeySetReservesItsKeysToTheSharedLayer is the
// change, end to end from a document: a declared `clearance` is the shared layer's
// to answer, so the inline block answers it neither for the row the file leaves
// blank nor for the subject the file does not list. `team`, which nothing declares,
// still answers — that is the local layer's whole purpose and suppression must stay
// scoped to the declared set.
func TestAttributeProviders_ADeclaredKeySetReservesItsKeysToTheSharedLayer(t *testing.T) {
	ctx := context.Background()
	dir := writeCSV(t, "users.csv", declaredKeysCSV)
	reg, err := declaredKeysDoc(t, "    declared_keys: [department, clearance]\n").BuildAttributeRegistry(dir)
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}

	// (a) The directory has a row for alice and no clearance in it.
	alice, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(alice): %v", err)
	}
	if got, ok := alice["clearance"]; ok {
		t.Errorf("alice clearance = %#v; the shared entry DECLARES clearance, so a row it "+
			"leaves empty must not be filled in from this instance's file", got)
	}
	if alice["department"] != "eng" {
		t.Errorf("alice department = %#v; the shared layer's own value must stand", alice["department"])
	}
	if alice["team"] != "atlas" {
		t.Errorf("alice team = %#v; team is undeclared, so the local layer still adds it", alice["team"])
	}

	// (b) The directory has no row for carol. This is the revocation: removing her
	// from the shared source removes every declared key for her, on this instance,
	// whatever its own file still says — and there is nothing to invalidate, since
	// the inline layer is registered with WithTTL(0).
	carol, err := reg.Attributes(ctx, "user", "carol")
	if err != nil {
		t.Fatalf("Attributes(carol): %v", err)
	}
	if got, ok := carol["clearance"]; ok {
		t.Errorf("carol clearance = %#v; she is not in the shared directory and clearance is "+
			"declared, so the local file must not keep authorizing her", got)
	}
	if carol["team"] != "atlas" {
		t.Errorf("carol bag = %#v; the undeclared remainder still answers", carol)
	}

	// The control: a row the directory does populate is read from it.
	bob, err := reg.Attributes(ctx, "user", "bob")
	if err != nil {
		t.Fatalf("Attributes(bob): %v", err)
	}
	if bob["clearance"] != int64(5) {
		t.Errorf("bob clearance = %#v; want the directory's 5", bob["clearance"])
	}
}

// TestAttributeProviders_DeclaringNothingReservesNothing is the opt-in bar at the
// seed layer, and it is the half that must never move: every document that has
// never written a `declared_keys:` builds exactly the registry it always did.
//
// `declared_keys: []` is in the same table because the two suppress the same amount
// — an empty set names no key — while being different states. A build that inferred
// "declared" from a non-empty list would pass the first row and the second for the
// wrong reason, so both are asserted against the same fixture.
func TestAttributeProviders_DeclaringNothingReservesNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		declared string
	}{
		{name: "not declared", declared: ""},
		{name: "declared empty", declared: "    declared_keys: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCSV(t, "users.csv", declaredKeysCSV)
			reg, err := declaredKeysDoc(t, tc.declared).BuildAttributeRegistry(dir)
			if err != nil {
				t.Fatalf("BuildAttributeRegistry: %v", err)
			}
			alice, err := reg.Attributes(ctx, "user", "alice")
			if err != nil {
				t.Fatalf("Attributes(alice): %v", err)
			}
			if alice["clearance"] != int64(9) {
				t.Errorf("alice clearance = %#v; with no key reserved, the local layer fills the "+
					"hole the directory left, exactly as it did before declaring existed", alice["clearance"])
			}
			carol, err := reg.Attributes(ctx, "user", "carol")
			if err != nil {
				t.Fatalf("Attributes(carol): %v", err)
			}
			if carol["clearance"] != int64(9) {
				t.Errorf("carol bag = %#v; a subject only the local layer knows is answered from "+
					"it in full when nothing is reserved", carol)
			}
		})
	}
}

// TestAttributeProviders_ADeclaredSetIsNormalisedRatherThanRefusedAtBuild: a blank
// or repeated name in a document's declared set is dropped here, not refused.
// `aperture wiring push` refuses both, naming the slot and the key, which is where
// a person wrote them; taking a booting instance down over a malformation that
// cannot change a verdict — a blank name is not a legal rule variable segment, and a
// set has no use for a name twice — would be a refusal with no remedy at the only
// moment the process has to start.
func TestAttributeProviders_ADeclaredSetIsNormalisedRatherThanRefusedAtBuild(t *testing.T) {
	ctx := context.Background()
	dir := writeCSV(t, "users.csv", declaredKeysCSV)
	reg, err := declaredKeysDoc(t, "    declared_keys: [\"  clearance  \", clearance, \"\"]\n").
		BuildAttributeRegistry(dir)
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	alice, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(alice): %v", err)
	}
	if got, ok := alice["clearance"]; ok {
		t.Errorf("alice clearance = %#v; the name is trimmed, so the padded spelling still "+
			"reserves the key", got)
	}
	if alice["team"] != "atlas" {
		t.Errorf("alice team = %#v; the blank name reserved something it should not have", alice["team"])
	}
}

// TestAttributeProviders_TheInlineBlockAloneNeverDeclares: with no
// attribute_providers: entry the inline block is the slot's only layer, and it is
// the LOCAL one. A local layer may not declare — provider.AttributeRegistry refuses
// it — so there is no document shape in which an inline block reserves a key
// against the deployment's directory. This asserts the build does not try.
func TestAttributeProviders_TheInlineBlockAloneNeverDeclares(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
attributes:
  - subject: user
    id: alice
    metadata:
      clearance: 9
`)
	reg, err := doc.BuildAttributeRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes: %v", err)
	}
	if bag["clearance"] != int64(9) {
		t.Fatalf("bag = %#v; a single local layer answers verbatim", bag)
	}
	if layers := reg.Layers(provider.AttributeSlotUser); len(layers) != 1 || layers[0] != provider.AttributeLayerLocal {
		t.Fatalf("layers = %v; the inline block is the LOCAL layer", layers)
	}
}
