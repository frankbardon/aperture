package service

import (
	"slices"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// The slot-to-root collapse, stated once.
//
// A declared key set is written per attribute SLOT (`attribute_providers:` →
// model.WiringAttributeProvider.DeclaredKeys), and rule validation enforces per
// rule ROOT. The two are not the same shape, and this file is the only place the
// conversion happens: `rules` knows roots and never imports model, `provider` owns
// the closed slot set and imports nothing but identity and errors, and this facade
// is the one layer that can see all three.

// WithDeclaredAttributeKeys installs the deployment's DECLARED ATTRIBUTE KEY SETS,
// per slot, and so turns on definition-time key enforcement for the rule roots
// whose slots declare. Without it — and with an empty or nil map — nothing is
// declared, nothing is enforced, and every rule that validated before declaring
// existed still validates.
//
// Enforcement means PutRule, ValidateRule and EvaluateRulePreview refuse a rule
// that reads an attribute key the wiring does not guarantee, with
// APERTURE_RULE_UNDECLARED_ATTRIBUTE naming the key and the slot. It is a
// definition-time gate and touches no decision: a rule already stored decides
// exactly as it did, because refusing a live decision would take access away from
// a deployment whose wiring changed under it, and taking the rule's own authority
// away is not this contract's job.
//
// # The `account` root
//
// One slot backs it, so it is enforced exactly when that slot declares.
//
// # The `principal` root is enforced only when BOTH principal slots declare
//
// `principal.*` resolves to the user slot or the machine slot depending on the
// kind of the principal ASKING, and validation cannot know which kind that will
// be. So the permitted set is the UNION of the principal slots' declared sets, and
// the root is enforced only when every one of those slots declares:
//
//   - Union rather than intersection, because a rule may legitimately be about one
//     kind — `principal.kind == "machine" && principal.fleet == "batch"` is the
//     documented way to say so — and intersecting would refuse it for a key the
//     machine slot really does guarantee. A user principal reading that key still
//     reads a missing path, but that is per-KIND leniency, which is deployment-wide
//     and identical on every instance; it is not the per-INSTANCE divergence this
//     enforcement exists to remove.
//   - Every slot must declare, because a slot that declares nothing guarantees
//     nothing and permits everything. Treating one declaring slot as enough would
//     make `principal.anything` refusable on the strength of a set the other slot
//     never agreed to, which is the opt-in bar broken: declaring the user slot
//     would silently start enforcing machine principals too.
//
// The floor keys (`principal.id`, `principal.kind`, `account.id`) are not part of
// any declared set and are always readable — see rules/declared.go.
func WithDeclaredAttributeKeys(sets map[provider.AttributeSlot]model.DeclaredKeys) Option {
	return func(s *Service) { s.declaredKeys = DeclaredAttributeKeysFor(sets) }
}

// DeclaredAttributeKeysFor is the slot-to-root collapse, exported for a caller that
// has to apply the same gate WITHOUT a facade.
//
// The boot path is that caller: applying a --seed document writes rules straight to
// storage, with no Service in front of it, and a writer that skipped the check would
// falsify the inertness argument the whole declared set rests on (rules/declared.go).
// Exporting the collapse is what keeps it stated ONCE: the alternative is internal/cli
// reimplementing "union the principal slots, and only when every one of them
// declares", which is silently wrong in the widening direction if the two drift.
func DeclaredAttributeKeysFor(sets map[provider.AttributeSlot]model.DeclaredKeys) rules.DeclaredAttributeKeys {
	return declaredAttributeKeys(sets)
}

// declaredAttributeKeys collapses the per-slot declarations onto the two rule
// roots. It is separate from the option so it can be asserted directly: the
// collapse is where the "union, and only when every slot declares" rule lives, and
// both halves are silently wrong in the widening direction if they drift.
func declaredAttributeKeys(sets map[provider.AttributeSlot]model.DeclaredKeys) rules.DeclaredAttributeKeys {
	return rules.DeclaredAttributeKeys{
		Principal: unionDeclaredKeys(sets, principalAttributeSlots()...),
		Account:   unionDeclaredKeys(sets, provider.AttributeSlotAccount),
	}
}

// principalAttributeSlots is the slots the `principal` root resolves over: every
// attribute slot that is not the account slot.
//
// It is DERIVED from provider.AttributeSlots() rather than spelling the two
// principal slots out, because spelling them out is silently wrong in the
// widening direction. A fourth slot is a contract change and not a feature (see
// CLAUDE.md), but if one is ever added for a new principal kind, a literal list
// here would leave it out of the union — so `principal.newkey` would be refused
// over a set the new slot's own wiring declared — and, worse, would leave it out
// of the every-slot-must-declare bar, so declaring the user slot alone would
// start enforcing the new kind against a set it never agreed to. Deriving makes
// both follow from the slot set, which is where the closed set is already stated.
//
// The account slot is excluded by NAME rather than by a "is this a principal
// slot?" predicate on AttributeSlot, because provider must stay a strict leaf
// that knows nothing about rule roots (TestProviderPackageImportsOnlyIdentityAndErrors),
// and the slot-to-root collapse is this file's job — stated once, at the top.
func principalAttributeSlots() []provider.AttributeSlot {
	all := provider.AttributeSlots()
	out := make([]provider.AttributeSlot, 0, len(all))
	for _, slot := range all {
		if slot == provider.AttributeSlotAccount {
			continue
		}
		out = append(out, slot)
	}
	return out
}

// unionDeclaredKeys unions the declared sets of slots, and reports NOT DECLARED —
// the opt-out — as soon as any one of them does not declare. See
// WithDeclaredAttributeKeys for why both halves are that way round.
//
// A slot that declares an EMPTY set still declares: the result is declared-empty,
// which permits the floor and nothing else. That is the state a len() test would
// lose, and it is the one where losing it silently permits every key.
func unionDeclaredKeys(sets map[provider.AttributeSlot]model.DeclaredKeys, slots ...provider.AttributeSlot) rules.DeclaredKeySet {
	out := rules.DeclaredKeySet{
		Declared: true,
		Keys:     []string{},
		Slots:    make([]string, 0, len(slots)),
	}
	for _, slot := range slots {
		d, ok := sets[slot]
		if !ok || !d.Declared {
			return rules.DeclaredKeySet{}
		}
		out.Slots = append(out.Slots, slot.String())
		for _, key := range d.Keys {
			if !slices.Contains(out.Keys, key) {
				out.Keys = append(out.Keys, key)
			}
		}
	}
	return out
}
