package provider

import (
	"maps"
	"slices"

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
// # What a layer does NOT change
//
// Leniency. Which codes collapse to a nil bag — APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED
// and APERTURE_NOT_FOUND, in Attributes and AccountAttributes — is the same set
// it was, and it is asked of the SLOT, not of a layer. Inside a fetch, one
// layer's APERTURE_NOT_FOUND means "this layer has no record for this key" and
// the other layer's bag is the answer; every OTHER error surfaces verbatim from
// whichever layer raised it, so an unreachable shared directory is never silently
// answered out of the local file. That distinction is the one this seam exists to
// preserve: an outage must not read as "this principal has no attributes", and it
// must not read as "this principal has the LOCAL machine's attributes" either.

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

// mergeAttributeBags merges a slot's two layers into the bag a decision reads,
// with shared stamped LAST so it wins every key both serve.
//
// A single-layer slot is the case every deployment with one directory is in, and
// it costs nothing: when either side is absent the other is returned VERBATIM,
// the same map the cache is holding, with no copy and no allocation. The merge is
// paid only by a slot that really has two sources.
//
// The merged bag is a FRESH map, for the reason rules.principalBag builds one:
// both inputs are cached values shared across every object in the decision and
// every concurrent decision for the same key, so merging INTO either would be a
// write through a read-only value at the widest blast radius Aperture has. The
// result is read-only too, transitively — nested containers are the providers'
// own and are not copied, exactly as a single layer's bag is not (see the
// blast-radius note in attribute.go).
func mergeAttributeBags(shared, local Metadata) Metadata {
	if len(local) == 0 {
		return shared
	}
	if len(shared) == 0 {
		return local
	}
	out := make(Metadata, len(shared)+len(local))
	maps.Copy(out, local)
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
func mergeAttributeRecords(shared, local []AttributeRecord) []AttributeRecord {
	if len(local) == 0 {
		return shared
	}
	if len(shared) == 0 {
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
			at[rec.ID] = len(out)
			out = append(out, AttributeRecord{ID: rec.ID, Attributes: rec.Attributes})
			continue
		}
		// The shared layer already answered for this key: merge the local bag
		// UNDER it, so a listing shows exactly the bag a Fetch of that key would
		// return.
		out[i].Attributes = mergeAttributeBags(out[i].Attributes, rec.Attributes)
	}
	return out
}
