package cli

import (
	"strings"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
)

// Where a booting instance finds the DECLARED ATTRIBUTE KEY SETS.
//
// A declared set belongs to the SHARED layer of a slot, and the shared layer has
// exactly two sources — the wiring rows in the database, and this instance's own
// `attribute_providers:` block when no wiring has been pushed. Both are read here,
// in that precedence, and handed to the facade
// (service.WithDeclaredAttributeKeys), which turns them into the definition-time
// gate on a rule's attribute reads.
//
// The inline `attributes:` block is NOT a source. It is the slot's LOCAL layer,
// and the whole point of the declared set is that a local layer cannot change what
// a deployment-wide rule may name: a set a local file could widen would be one
// machine deciding which keys every instance's rules may read.
//
// # Why the ROWS, and not the projected Document
//
// A DB-wired boot assembles an effective seed.Document from the wiring rows
// (wiringDocument), and that projection does not carry DeclaredKeys — see
// wiringSeedAttributeProvider. So the rows are read directly rather than through
// the Document, which is both the shape that function's own comment predicted and
// the right dependency: a declared set is not wiring the registry builders need,
// it is a contract the rule gate needs, and routing it through a document
// conversion would be one more place it could be dropped without anything going
// red.

// declaredAttributeKeySets collects the declared key set of every shared attribute
// slot this instance is wired with, keyed by slot.
//
// wiring is the shared wiring read from the database — empty for the file-only
// boot every existing single-instance deployment does. local is this instance's
// own seed document.
//
// Precedence follows the boot's: when the database has wiring, its rows are
// authoritative and the local document may only ADD a slot the database never
// declared (a local entry for a slot the database DOES declare already fails the
// boot with APERTURE_WIRING_LOCAL_COLLISION, so the two can never contest one
// slot here). When it has none, the local document is the shared wiring, exactly
// as it always was.
//
// A slot absent from the result declares nothing, which is the opt-out: it keeps
// today's leniency completely unchanged.
func declaredAttributeKeySets(wiring model.WiringSet, local *seed.Document) map[provider.AttributeSlot]model.DeclaredKeys {
	out := make(map[provider.AttributeSlot]model.DeclaredKeys)
	if local != nil {
		for _, ap := range local.AttributeProviders {
			slot, ok := attributeSlotOf(ap.Subject)
			if !ok {
				continue
			}
			out[slot] = seedDeclaredKeySet(ap.DeclaredKeys)
		}
	}
	// Second, so the database wins any slot both name. They cannot both name one
	// (see above), and stating the precedence anyway is cheaper than relying on a
	// refusal in another file to stay where it is.
	for _, ap := range wiring.AttributeProviders {
		slot, ok := attributeSlotOf(ap.Subject)
		if !ok {
			continue
		}
		out[slot] = ap.DeclaredKeys
	}
	return out
}

// attributeSlotOf parses an entry's subject into a slot, reporting false for
// anything that is not one of the three.
//
// An unknown subject is unreachable by the time this runs: both the registry build
// and the wiring push refuse one with APERTURE_ATTRIBUTE_SLOT_UNKNOWN. It is
// skipped rather than propagated because inventing a second refusal for it here
// would report the same malformation twice, in the wrong order, from the layer
// that cares about it least.
func attributeSlotOf(subject string) (provider.AttributeSlot, bool) {
	slot, err := provider.ParseAttributeSlot(strings.TrimSpace(subject))
	if err != nil {
		return "", false
	}
	return slot, true
}

// seedDeclaredKeySet projects a seed entry's pointer-shaped declared_keys: onto
// model.DeclaredKeys. It is wiringDeclaredKeys' read-only twin: a nil pointer is
// NOT DECLARED and a non-nil one is DECLARED whatever it points at, so
// `declared_keys: []` stays declared-empty — opted in, permitting nothing — and
// does not collapse into the opt-out.
//
// The names are trimmed and the empty and repeated ones dropped, rather than
// refused. `aperture wiring push` refuses both, naming the slot and the key, which
// is where a person wrote them; refusing them again on a BOOT would take an
// instance down over a malformation that cannot change a verdict — an empty name
// is not a legal rule variable segment, so no rule can read it, and a set has no
// use for a name twice.
func seedDeclaredKeySet(declared *[]string) model.DeclaredKeys {
	if declared == nil {
		return model.DeclaredKeys{}
	}
	out := model.DeclaredKeys{Declared: true, Keys: make([]string, 0, len(*declared))}
	seen := make(map[string]struct{}, len(*declared))
	for _, raw := range *declared {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out.Keys = append(out.Keys, key)
	}
	return out
}
