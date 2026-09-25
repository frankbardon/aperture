package provider

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// E3-S5: an admin listing may not narrow the decision path's bag.
//
// AttributeProvider has two read methods with two different jobs. Fetch answers
// "what does the directory say about this one subject?" and its answer is what a
// rule sees. Query answers "who is in this slot?" and its answer is rendered to an
// operator. Nothing in the contract makes them equal, and the SQL loader makes the
// inequality explicit and legal: AttributeConfig.ListQuery is OPTIONAL and is only
// required to select a bare id, so a get_all that projects two columns beside a
// get_one that projects four is the documented shape, not a mistake.
//
// The registry used to warm the slot's cache from Query's bags. That made the
// admin listing rewrite every listed subject's DECISION bag for the whole ttl,
// and the direction of the damage is what makes it a security bug rather than a
// stale read: a key that is absent is not a key that is wrong. Every membership
// and comparison predicate over it goes false, and in an EXCLUSIVE grant a rule
// that stops selecting stops excluding — so `aperture attributes query user`
// widens access, with nothing in any verdict, trace or note to say so.
//
// These cases are the layer-of-the-fix proof. The same bug end to end, over the
// fake SQL driver, is seed.TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag;
// over a live Postgres it is the gated
// seed.TestPostgresIntegration_AttributeProviderServesASlot, which is the run
// that found it.

// projectingAttributes is a provider whose Query returns a strict SUBSET of what
// Fetch returns for the same subject — the exact shape of a
// `get_one: SELECT department, clearance, to_jsonb(teams) AS teams ...` paired
// with a `get_all: SELECT u.id AS id, u.department ...`.
type projectingAttributes struct {
	full    map[string]Metadata // Fetch's answer: the authoritative bag
	listed  map[string]Metadata // Query's answer: the display projection
	fetches atomic.Int64
}

func (p *projectingAttributes) Fetch(_ context.Context, id string) (Metadata, error) {
	p.fetches.Add(1)
	md, ok := p.full[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"test: no such subject", map[string]any{"key": id})
	}
	return md, nil
}

func (p *projectingAttributes) List(ctx context.Context) ([]AttributeRecord, error) {
	return p.Query(ctx, AttributeFilter{})
}

func (p *projectingAttributes) Query(_ context.Context, _ AttributeFilter) ([]AttributeRecord, error) {
	out := make([]AttributeRecord, 0, len(p.listed))
	for _, id := range []string{"alice", "bob"} {
		if md, ok := p.listed[id]; ok {
			out = append(out, AttributeRecord{ID: id, Attributes: md})
		}
	}
	return out, nil
}

func newProjectingAttributes() *projectingAttributes {
	return &projectingAttributes{
		full: map[string]Metadata{
			"alice": {"department": "eng", "clearance": int64(3), "teams": []any{"platform", "oncall"}},
			"bob":   {"department": "sales", "clearance": int64(1), "teams": []any{"crm"}},
		},
		// No teams, no clearance — the projection an operator's listing needs.
		listed: map[string]Metadata{
			"alice": {"department": "eng"},
			"bob":   {"department": "sales"},
		},
	}
}

// TestAnEnumerationDoesNotNarrowTheFetchedBag is the regression. The order is the
// one the bug needs: list first, decide second.
func TestAnEnumerationDoesNotNarrowTheFetchedBag(t *testing.T) {
	ctx := context.Background()
	dir := newProjectingAttributes()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, dir)

	if _, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{}); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	bag, err := reg.Fetch(ctx, AttributeSlotUser, "alice")
	if err != nil {
		t.Fatalf("Fetch after Enumerate: %v", err)
	}
	want := Metadata{"department": "eng", "clearance": int64(3), "teams": []any{"platform", "oncall"}}
	if !reflect.DeepEqual(map[string]any(bag), map[string]any(want)) {
		t.Fatalf("bag after an enumeration = %#v, want %#v\n"+
			"An admin listing must not substitute Query's projection for Fetch's bag: "+
			"an ABSENT clearance or teams makes every predicate over it false, which "+
			"DENIES in an inclusive grant and WIDENS in an exclusive one.", bag, want)
	}
	// The named keys, spelled out, so a failure says which fidelity was lost
	// rather than only that two maps differ.
	if _, ok := bag["teams"]; !ok {
		t.Error("teams is absent: the listing's projection reached the decision path")
	}
	if got, ok := bag["clearance"].(int64); !ok || got != 3 {
		t.Errorf("clearance = %#v (%T), want int64(3)", bag["clearance"], bag["clearance"])
	}
}

// TestOnlyFetchWritesTheSlotCache pins the mechanism rather than one symptom, so
// a future reintroduction of the warm fails here even if the projection fixture
// above were ever changed to agree with itself.
func TestOnlyFetchWritesTheSlotCache(t *testing.T) {
	ctx := context.Background()
	dir := newProjectingAttributes()
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, dir)

	// Enumerate twice: neither call may leave anything in the cache.
	for range 2 {
		if _, err := reg.Enumerate(ctx, AttributeSlotUser, AttributeFilter{}); err != nil {
			t.Fatalf("Enumerate: %v", err)
		}
	}
	st, ok := reg.Stats(AttributeSlotUser)
	if !ok {
		t.Fatal("no stats for the user slot")
	}
	if st.Entries != 0 {
		t.Errorf("the slot cache holds %d entries after two enumerations, want 0: "+
			"Enumerate is an admin read and the cache is the decision path's", st.Entries)
	}

	// Fetch still caches its own answer — the change is confined to WHICH call
	// may write, not to whether the decision path is cached at all.
	for range 3 {
		if _, err := reg.Fetch(ctx, AttributeSlotUser, "alice"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	if n := dir.fetches.Load(); n != 1 {
		t.Errorf("provider Fetch called %d times for three reads, want 1: "+
			"Fetch must still populate and serve the slot cache", n)
	}
}
