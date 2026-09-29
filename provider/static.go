package provider

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// The in-memory ObjectProvider.
//
// Not every host object set comes from a file or a database. A seed document
// declares object metadata inline, a test needs three objects and no fixture on
// disk, and an embedded demo has its data compiled in. All three want the SAME
// provider semantics as csvprovider — Fetch's APERTURE_NOT_FOUND, List's stable
// order, Query's Filter contract — over a slice already in memory, so Static
// lives here rather than being re-derived per caller.
//
// It is deliberately immutable: the whole object set is supplied once, at
// construction, validated once, and never changes. That is what makes it safe
// for concurrent use with no lock, and what makes the read-only Metadata
// contract trivially true — there is no reload that could edit a map the
// Registry already cached.

// compile-time assertion: a *Static is a usable ObjectProvider, and one whose
// listings may warm the Registry's metadata cache.
var (
	_ ObjectProvider      = (*Static)(nil)
	_ FetchCompleteLister = (*Static)(nil)
)

// Static is an in-memory ObjectProvider for one object-type, built from a fixed
// slice of Objects. It is safe for concurrent use because it is immutable after
// NewStatic returns.
//
// Register it under an object-type exactly as any other provider:
//
//	p, err := provider.NewStatic([]provider.Object{
//		{ID: identity.MustParse("account:acme/brand:1"),
//			Metadata: provider.Metadata{"tags": []any{"premium"}}},
//	})
//	reg.MustRegister("brand", p, provider.WithTTL(0))
//
// A TTL of 0 (never expire) is the natural cache setting: the data cannot go
// stale, because nothing can change it.
type Static struct {
	byID  map[string]Metadata
	order []identity.Identity // preserves declaration order for stable List/Query output
}

// NewStatic builds a Static from objects, in declaration order.
//
// Construction is where every check happens, so a Fetch/List/Query can never
// fail for a reason the caller could have been told about at wiring time:
//
//   - an object with an empty (zero-value) identity is APERTURE_PROVIDER_INVALID;
//   - a duplicate identity is APERTURE_PROVIDER_INVALID naming the id — the last
//     writer silently winning is how one object's metadata becomes another's;
//   - metadata violating the shared value model (metadata.go) is
//     APERTURE_METADATA_INVALID naming the id, the field, and the offending path.
//     Static does not re-implement the model, and a caller that already validated
//     is not trusted to have done so: this is the load-time gate that keeps a
//     malformed value off the Check hot path.
//
// Every value is DEEP-COPIED into the provider, so the object set Static serves
// is its own: a caller that keeps and mutates the maps it passed in cannot reach
// through into metadata the Registry has already cached. Nothing is copied on
// READ — Fetch/List/Query hand out the provider's own maps by reference, per the
// read-only Metadata contract in the package doc.
func NewStatic(objects []Object) (*Static, error) {
	s := &Static{
		byID:  make(map[string]Metadata, len(objects)),
		order: make([]identity.Identity, 0, len(objects)),
	}
	for _, obj := range objects {
		key := obj.ID.String()
		if key == "" {
			return nil, aerr.New(aerr.APERTURE_PROVIDER_INVALID,
				"provider: static object has an empty identity")
		}
		if _, dup := s.byID[key]; dup {
			return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_INVALID,
				"provider: static object is declared more than once",
				map[string]any{"id": key})
		}
		if err := ValidateMetadata(obj.Metadata); err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
				"provider: object %s has metadata rejected by the value model", key)
		}
		s.byID[key] = cloneMetadata(obj.Metadata)
		s.order = append(s.order, obj.ID)
	}
	return s, nil
}

// Fetch returns id's metadata, or APERTURE_NOT_FOUND when no object was declared
// under it (so the Registry can distinguish absent from an operational failure).
func (s *Static) Fetch(_ context.Context, id identity.Identity) (Metadata, error) {
	md, ok := s.byID[id.String()]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"provider: no static object with this id", map[string]any{"id": id.String()})
	}
	return md, nil
}

// List returns every object in declaration order.
func (s *Static) List(_ context.Context) ([]Object, error) {
	out := make([]Object, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, Object{ID: id, Metadata: s.byID[id.String()]})
	}
	return out, nil
}

// Query returns the objects satisfying filter, in declaration order.
// Filter.Fields go through MatchFields — the shared implementation of the Fields
// contract, so a collection field matches by MEMBERSHIP and everything else by
// typed equality; Filter.Pattern and Filter.Limit are honoured directly. The
// Registry re-enforces Pattern and Limit, so honouring them here is an
// optimisation that also makes Query correct when called standalone.
func (s *Static) Query(_ context.Context, filter Filter) ([]Object, error) {
	out := make([]Object, 0, len(s.order))
	for _, id := range s.order {
		if filter.Pattern != nil && !filter.Pattern.Matches(id) {
			continue
		}
		md := s.byID[id.String()]
		if !MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, Object{ID: id, Metadata: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// ListedMetadataMatchesFetch is Static's unconditional FetchCompleteLister
// promise: a listing through it may warm the Registry's per-type metadata cache.
//
// It is true by construction rather than by assertion. There is ONE map per
// object — s.byID[id] — and Fetch, List and Query all hand back that same map by
// reference. There is no second projection to diverge from the first, no reload
// that could replace one and not the other, and no filter that edits a bag on the
// way out (Query selects whole objects, it does not narrow them). A listed bag is
// therefore not merely equal to the fetched bag, it is the same value.
func (s *Static) ListedMetadataMatchesFetch() bool { return true }

// Len reports how many objects were declared. It is the cheap way for a caller
// (a seed loader logging what it wired, a test) to confirm a set landed without
// walking List.
func (s *Static) Len() int { return len(s.order) }

// cloneMetadata deep-copies one object's metadata, returning a non-nil map so a
// declared object with no fields behaves like one whose fields are all absent
// rather than surfacing a nil map.
func cloneMetadata(md Metadata) Metadata {
	out := make(Metadata, len(md))
	for k, v := range md {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies a metadata value. It only has to handle the shapes the
// value model admits — scalars, []any, and map[string]any — because
// ValidateMetadata has already run; anything else is a scalar by elimination and
// is copied by value.
func cloneValue(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = cloneValue(elem)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, elem := range x {
			out[k] = cloneValue(elem)
		}
		return out
	default:
		return v
	}
}
