package provider

import (
	"maps"
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// The two LAYERS a slot's attributes can come from, and why the shared one wins.
//
// A slot used to hold exactly one provider. It holds up to two, and they are not
// peers: they are a SHARED layer and a LOCAL layer, and the shared layer wins
// every key both serve.
//
// The two exist because a deployment's attribute wiring genuinely has two
// origins, and collapsing them cost something either way:
//
//   - SHARED is the wiring every instance of a deployment reads — the row in the
//     database, or the directory a `kind: sql` / `kind: csv` entry names. It is
//     the deployment's own statement about a party, identical on every instance,
//     and it is what a rule written by whoever administers the deployment
//     compares against.
//   - LOCAL is this instance's own: the seed file's `attributes:` block, or a
//     provider a Go host registered itself. It belongs to the machine it is
//     written on.
//
// Refusing the second registration (which is what this package did) made the two
// mutually exclusive, so an instance could not add a field the shared directory
// does not carry without abandoning the shared directory entirely. Merging them
// as peers would be worse: whichever registered last would silently redefine
// every key, and "last writer wins" over a directory is how one deployment's user
// table quietly shadows another's.
//
// # Shared wins, and that is the whole safety argument
//
// A rule is written against a deployment, and the deployment's statement about a
// party is the shared layer's. If a local bag could override a key the shared
// layer serves, then a file on one machine could change what
// `principal.clearance >= 3` compares against on that machine only — the same
// rule, the same grant, a different answer, with nothing in a verdict, a trace or
// a note to say which layer answered. Precedence is therefore not configurable
// and not order-dependent: the shared layer's value stands, always, and the local
// layer can only ADD keys the shared layer does not serve.
//
// This mirrors rules.principalBag / rules.accountBag exactly, including the
// mechanism: the winner is stamped LAST over a fresh map (see
// mergeAttributeBags), and the engine's floor then stamps over BOTH, so the three
// tiers compose in one direction — floor over shared over local. A floor that can
// be shadowed is not a floor, and neither is a shared layer.
//
// # A DECLARED KEY SET makes its layer AUTHORITATIVE for the keys in it
//
// "Shared wins" on its own is a statement about the keys the shared layer
// SERVES, and absence is not a value: a key the shared bag does not carry used to
// be answered out of the local one, which is exactly what the local layer is for.
// That left two holes no code could tell from the intended case:
//
//   - a shared layer that SERVES a key and omits it for one ROW fell through to
//     the local layer. sqlprovider's rowMetadata drops a NULL column's field
//     ENTIRELY — metadataValue maps a SQL NULL and a JSON null to an ABSENT field
//     on purpose — so one subject's `clearance` was read out of one machine's file,
//     INSIDE the shared layer's own declared set, and declaring the key did not
//     mitigate it: the rule validated against the declaration and then compared
//     the local value.
//   - a shared layer with NO RECORD for a subject reports APERTURE_NOT_FOUND,
//     which a fetch reads as "this layer has no record for this key" — the
//     ordinary, necessary case — so DELETING a principal from the shared directory
//     revoked nothing on any instance whose own file still listed them. And it
//     never expired: seed registers the inline layer with WithTTL(0), because
//     inline data cannot change while the process runs, so there was nothing
//     stale for an invalidation to drop either.
//
// A declared key set closes both, by doing a second job with the same list:
// **when a layer declares a key set, only that layer may ANSWER the keys in it.**
// A local layer's value for a declared key is DROPPED — whether the declaring
// layer returned a different value, returned the key ABSENT, or returned NO RECORD
// at all. The local layer may still add keys OUTSIDE the declared set, and that is
// not a detail: adding a field the shared directory does not carry is the whole
// reason the local layer exists (see below), so suppression is scoped to the
// declared set and is never a discard.
//
// The second hole is the security half. Removing a subject from the shared
// directory now removes every DECLARED key for them, on every instance, whatever
// any local file still says. What survives is the undeclared remainder, which is
// the part no deployment-wide rule can name anyway: a rule reading a key a
// declaring slot does not declare is refused at DEFINITION time
// (service.WithDeclaredAttributeKeys). The two halves of `declared_keys:` are
// therefore one contract read twice — "these are the keys a rule may name" and
// "these are the keys this layer answers for" — and they only compose because it
// is the same list.
//
// It is OPT-IN and stays opt-in. A layer that declares NOTHING suppresses
// NOTHING, so every deployment that has never written a declared_keys: merges
// exactly as it did before this existed. DECLARED EMPTY (`declared_keys: []`) is a
// declaration whose set happens to be empty, so it likewise suppresses nothing —
// but it is not the same STATE, and declaredKeySet keeps the two apart for the
// reason model.DeclaredKeys does: collapsing them is how a slot silently loses the
// enforcement its operator asked for.
//
// Only the SHARED layer may declare. A local declared set would be one machine
// deciding which keys the deployment's own directory is allowed to answer —
// precedence inversion wearing an opt-in's clothes — so AttributeRegistry.register
// refuses WithDeclaredKeys on the local layer outright.
//
// # What a layer does NOT change
//
// Leniency. Which codes collapse to a nil bag — APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED
// and APERTURE_NOT_FOUND, in Attributes and AccountAttributes — is the same set
// it was, and it is asked of the SLOT, not of a layer. Inside a fetch, one
// layer's APERTURE_NOT_FOUND means "this layer has no record for this key" and
// the other layer's bag is the answer — minus the declaring layer's keys, which is
// the suppression above and not a widening of the code set; every OTHER error
// surfaces verbatim from whichever layer raised it, so an unreachable shared
// directory is never silently answered out of the local file. That distinction is
// the one this seam exists to preserve: an outage must not read as "this principal
// has no attributes", and it must not read as "this principal has the LOCAL
// machine's attributes" either.

// AttributeLayer names which of a slot's two registration layers a provider
// occupies. It is a defined string type for the same reason AttributeSlot is: a
// layer is never a slot, a kind, or a fetch key at a call site.
type AttributeLayer string

const (
	// AttributeLayerShared is the deployment-wide layer: the wiring row in the
	// database, or a seed document's attribute_providers: entry. It WINS every
	// key both layers serve.
	AttributeLayerShared AttributeLayer = "shared"
	// AttributeLayerLocal is this instance's own layer: the seed document's
	// attributes: block, or a provider a Go host registers itself. It layers
	// UNDER the shared one and can only contribute keys the shared layer does not
	// serve.
	AttributeLayerLocal AttributeLayer = "local"
)

// AttributeLayers returns the closed set of layers in PRECEDENCE order, highest
// first. The order is the contract, not a convenience: a caller that walks it and
// takes the first answer gets the same precedence Fetch applies.
func AttributeLayers() []AttributeLayer {
	return []AttributeLayer{AttributeLayerShared, AttributeLayerLocal}
}

// String renders the layer as its bare key ("shared", "local").
func (l AttributeLayer) String() string { return string(l) }

// Valid reports whether l is one of the two declared layers.
func (l AttributeLayer) Valid() bool { return slices.Contains(AttributeLayers(), l) }

// layerNames renders the closed set for an error context.
func layerNames() []string {
	layers := AttributeLayers()
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		out = append(out, string(l))
	}
	return out
}

// attributeLayerError reports a layer outside the closed set. It exists so the
// unexported register path has one refusal to return rather than a panic, even
// though every exported caller passes a constant.
func attributeLayerError(slot AttributeSlot, layer AttributeLayer) error {
	return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
		"provider: not an attribute layer",
		map[string]any{"slot": string(slot), "layer": string(layer), "layers": layerNames()})
}

// declaredKeySet is ONE layer's declared key set: the keys that layer answers
// for, and therefore the keys no layer under it may answer.
//
// It carries `declared` as its own bit rather than inferring it from the map's
// size, for the reason model.DeclaredKeys does: NOT DECLARED and DECLARED EMPTY
// are different answers all the way down (they are "" and "[]" in both dialects'
// declared_keys column and two different words in `aperture wiring show`), and a
// representation that could not tell them apart is how a slot loses the
// enforcement its operator asked for. They happen to SUPPRESS the same amount —
// nothing — because an empty set names no key; that is a consequence of the rule,
// not a collapse of the states.
//
// The map is the copy: a set is built once, at registration, from the caller's
// slice, and never written again. It is read on the decision path with no lock
// held, which is sound for exactly the reason the rest of an attributeLayerEntry
// is (see its doc): the entry is published by the slot-map write and is immutable
// afterwards.
type declaredKeySet struct {
	declared bool
	keys     map[string]struct{}
}

// newDeclaredKeySet builds a DECLARED set from keys. It is declared whatever keys
// is — nil and empty both mean declared-empty, which is why this constructor is
// only reached from WithDeclaredKeys and never from a zero value.
//
// Names are trimmed and the blank ones dropped rather than refused. `aperture
// wiring push` refuses a blank or repeated name already, naming the slot and the
// key, which is where a person wrote it; refusing it again here would take a
// process down over a malformation that cannot change a verdict, since a blank
// name is not a legal rule variable segment and a set has no use for a name twice.
// internal/cli's seedDeclaredKeySet normalises identically, for the same reason.
func newDeclaredKeySet(keys []string) declaredKeySet {
	out := declaredKeySet{declared: true, keys: make(map[string]struct{}, len(keys))}
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		out.keys[key] = struct{}{}
	}
	return out
}

// authoritative reports whether this set actually reserves any key, which is the
// only question the merge has to ask. Both a set that was never declared and one
// declared EMPTY reserve nothing, so both answer false — see the type doc for why
// that is not the two states being confused.
func (s declaredKeySet) authoritative() bool { return s.declared && len(s.keys) > 0 }

// answersFor reports whether key is one this layer declared, and therefore one no
// layer under it may contribute.
func (s declaredKeySet) answersFor(key string) bool {
	if !s.declared {
		return false
	}
	_, ok := s.keys[key]
	return ok
}

// mergeAttributeBags merges a slot's two layers into the bag a decision reads,
// with shared stamped LAST so it wins every key both serve, and the shared
// layer's DECLARED keys reserved to it even where its own bag is silent.
//
// declared is the SHARED layer's set. When it reserves nothing — every deployment
// that has never written a declared_keys:, and every one that wrote an empty
// one — this is the merge it always was.
//
// A single-layer slot never reaches here at all (Fetch takes sole()), and a
// two-layer slot whose local bag is empty is returned VERBATIM, the same map the
// cache is holding, with no copy and no allocation. What is NOT a safe shortcut
// any more is an empty SHARED bag: that is precisely case (b) of the file doc —
// the shared layer has no record for this subject — so suppression still has to
// run, and the fast path is taken only when the set reserves nothing.
//
// The merged bag is a FRESH map, for the reason rules.principalBag builds one:
// both inputs are cached values shared across every object in the decision and
// every concurrent decision for the same key, so merging INTO either would be a
// write through a read-only value at the widest blast radius Aperture has. The
// result is read-only too, transitively — nested containers are the providers'
// own and are not copied, exactly as a single layer's bag is not (see the
// blast-radius note in attribute.go).
func mergeAttributeBags(shared, local Metadata, declared declaredKeySet) Metadata {
	if len(local) == 0 {
		return shared
	}
	if len(shared) == 0 && !declared.authoritative() {
		return local
	}
	out := make(Metadata, len(shared)+len(local))
	if declared.authoritative() {
		// The local layer contributes only the keys the shared layer did not
		// RESERVE — not merely the ones it did not answer. A declared key the
		// shared bag is silent about is silent in the merge too, which is what
		// makes a deletion from the shared directory a revocation.
		for k, v := range local {
			if declared.answersFor(k) {
				continue
			}
			out[k] = v
		}
	} else {
		maps.Copy(out, local)
	}
	// Shared LAST: it wins every key both layers serve. See the file doc.
	maps.Copy(out, shared)
	return out
}

// mergeAttributeRecords merges two layers' enumerations into one record set,
// keyed by attribute key, with the shared layer's bag winning per key exactly as
// mergeAttributeBags does for a single subject.
//
// Order is shared-first: every key the shared layer returned, in the order it
// returned them, then the keys only the local layer knows, in the order IT
// returned them. A listing is rendered to an operator, so a stable order matters;
// deriving it from the layers rather than sorting keeps a provider's own ordering
// (a `get_all` with an ORDER BY) visible instead of replacing it.
//
// SUPPRESSION APPLIES HERE TOO, through the same mergeAttributeBags, and that is a
// decision rather than a side effect. This listing's entire promise is that the bag
// it shows for a key is the bag a Fetch of that key would return; a declared key
// shown with a local value no decision could ever read would break that promise on
// exactly the subjects it matters most for — the ones an operator is reading the
// listing to CHECK, because the shared directory no longer has a record for them.
// `aperture attributes query user` is how a revocation is verified, so it must not
// be the one surface that still displays the revoked value. A record whose every
// key was suppressed stays in the listing with an EMPTY bag, because that is what a
// Fetch of it returns: the local layer does have a record, and it now answers for
// nothing.
func mergeAttributeRecords(shared, local []AttributeRecord, declared declaredKeySet) []AttributeRecord {
	if len(local) == 0 {
		return shared
	}
	if len(shared) == 0 && !declared.authoritative() {
		return local
	}
	at := make(map[string]int, len(shared)+len(local))
	out := make([]AttributeRecord, 0, len(shared)+len(local))
	for _, rec := range shared {
		if i, dup := at[rec.ID]; dup {
			out[i] = rec
			continue
		}
		at[rec.ID] = len(out)
		out = append(out, rec)
	}
	for _, rec := range local {
		i, ok := at[rec.ID]
		if !ok {
			// A key only the LOCAL layer returned. It is still merged — against a
			// nil shared bag — rather than appended verbatim, so the suppression a
			// declared set applies has ONE implementation and the two arms of this
			// loop cannot drift. With nothing declared, mergeAttributeBags hands
			// the bag straight back.
			at[rec.ID] = len(out)
			out = append(out, AttributeRecord{
				ID:         rec.ID,
				Attributes: mergeAttributeBags(nil, rec.Attributes, declared),
			})
			continue
		}
		// The shared layer already answered for this key: merge the local bag
		// UNDER it, so a listing shows exactly the bag a Fetch of that key would
		// return.
		out[i].Attributes = mergeAttributeBags(out[i].Attributes, rec.Attributes, declared)
	}
	return out
}
