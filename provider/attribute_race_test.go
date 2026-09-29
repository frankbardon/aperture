package provider

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A registration that lands while decisions are in flight.
//
// A slot used to be filled exactly once, so the map write under the registry's
// write lock and the map read under its read lock were the whole of the
// happens-before: an entry was fully built before it was published and never
// touched again, which is why every reader is allowed to release the lock before
// it reads the entry's layers. Two layers per slot made a second registration
// WRITE to an entry a reader may already be holding, and nothing ordered that
// write against the read.
//
// Both consequences were real and neither is visible in a single-threaded test:
//
//   - a reader in the window can see shared == nil while local != nil, so sole()
//     returns the LOCAL layer as the slot's whole answer — the precedence
//     inversion attribute_layer.go exists to forbid, arriving as a different
//     verdict on one machine with nothing in a trace to say why;
//   - nothing publishes the *attributeLayerEntry's own fields before the pointer
//     to it, so a reader can reach cache.Get on a half-built entry.
//
// register is therefore copy-on-write. These tests hold the guarantee down from
// the outside. They assert real invariants, so they are worth running plainly —
// but `make test` runs no -race, so the detector half is `make test-race`, where
// the pre-fix code reports the write/read pair directly.

// TestASharedRegistrationInFlightNeverLosesToTheLocalLayer hammers Fetch while a
// LOCAL provider is being registered over an already-registered shared one.
//
// The assertion is the precedence itself, not merely the absence of a crash: the
// shared layer's `department` must stand in every bag any reader ever observes.
// Before the copy-on-write fix a reader could load the entry, see the local
// pointer written and the shared one not yet read, and take the local bag whole —
// so "sales" appearing even once is the inversion.
func TestASharedRegistrationInFlightNeverLosesToTheLocalLayer(t *testing.T) {
	ctx := context.Background()
	const rounds = 64
	const readers = 4

	for round := 0; round < rounds; round++ {
		shared := &countingAttributes{bags: map[string]Metadata{
			"k": {"department": "eng", "clearance": int64(5)},
		}}
		local := &countingAttributes{bags: map[string]Metadata{
			// department collides and clearance is the LOWER value, so an
			// inversion reads as an access change rather than as a typo.
			"k": {"department": "sales", "clearance": int64(1), "team": "atlas"},
		}}
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, shared)

		var (
			wg      sync.WaitGroup
			started sync.WaitGroup
			stop    atomic.Bool
			mu      sync.Mutex
			bad     []Metadata
		)
		observe := func(md Metadata) {
			if md["department"] == "eng" {
				return
			}
			mu.Lock()
			bad = append(bad, md)
			mu.Unlock()
		}

		started.Add(readers)
		wg.Add(readers)
		for i := 0; i < readers; i++ {
			go func() {
				defer wg.Done()
				started.Done()
				for !stop.Load() {
					md, err := reg.Fetch(ctx, AttributeSlotUser, "k")
					if err != nil {
						mu.Lock()
						bad = append(bad, Metadata{"error": err.Error()})
						mu.Unlock()
						return
					}
					observe(md)
				}
			}()
		}
		started.Wait()

		if err := reg.RegisterLocal(AttributeSlotUser, local); err != nil {
			t.Fatalf("RegisterLocal: %v", err)
		}
		// Keep reading past the registration, so the settled two-layer answer is
		// checked as well as the window around the swap.
		for i := 0; i < 64; i++ {
			md, err := reg.Fetch(ctx, AttributeSlotUser, "k")
			if err != nil {
				t.Fatalf("Fetch after RegisterLocal: %v", err)
			}
			observe(md)
		}
		stop.Store(true)
		wg.Wait()

		if len(bad) > 0 {
			t.Fatalf("round %d: the local layer was observed winning: %#v", round, bad[0])
		}
	}
}

// TestALayerRegisteredConcurrentlyIsNeverObservedHalfBuilt drives every reader
// that takes a slot entry out of the map and then reads its fields without the
// lock — Fetch, Layers, Stats, CacheConfigFor, CacheConfigForLayer, Invalidate —
// against a registration filling the slot's second layer.
//
// Here the slot starts LOCAL-only, so a reader holding the pre-registration
// snapshot legitimately sees the local bag and precedence says nothing. What is
// asserted instead is coherence: no reader panics (a half-published
// attributeLayerEntry means a nil cache map), no reader errors, a slot that had a
// layer never reports none, and a bag that carries the shared layer's own key
// carries the shared layer's value for the contested one.
func TestALayerRegisteredConcurrentlyIsNeverObservedHalfBuilt(t *testing.T) {
	ctx := context.Background()
	const rounds = 64
	const readers = 4

	for round := 0; round < rounds; round++ {
		local := &countingAttributes{bags: map[string]Metadata{
			"k": {"department": "sales", "team": "atlas"},
		}}
		shared := &countingAttributes{bags: map[string]Metadata{
			"k": {"department": "eng", "clearance": int64(5)},
		}}
		reg := NewAttributeRegistry()
		reg.MustRegisterLocal(AttributeSlotUser, local)

		var (
			wg      sync.WaitGroup
			started sync.WaitGroup
			stop    atomic.Bool
			mu      sync.Mutex
			bad     []string
		)
		fail := func(format string, args ...any) {
			mu.Lock()
			bad = append(bad, fmt.Sprintf(format, args...))
			mu.Unlock()
		}

		started.Add(readers)
		wg.Add(readers)
		for i := 0; i < readers; i++ {
			go func() {
				defer wg.Done()
				started.Done()
				for !stop.Load() {
					md, err := reg.Fetch(ctx, AttributeSlotUser, "k")
					if err != nil {
						fail("Fetch: %v", err)
						return
					}
					// The shared layer's exclusive key can only be present once
					// the shared layer is visible, and then it must have won the
					// contested one too — the two arrive together or the entry was
					// torn.
					if _, ok := md["clearance"]; ok && md["department"] != "eng" {
						fail("torn bag: %#v", md)
					}
					if layers := reg.Layers(AttributeSlotUser); len(layers) == 0 {
						fail("Layers reported no layer for a registered slot")
					}
					if _, ok := reg.Stats(AttributeSlotUser); !ok {
						fail("Stats reported no entry for a registered slot")
					}
					if _, ok := reg.CacheConfigFor(AttributeSlotUser); !ok {
						fail("CacheConfigFor reported no entry for a registered slot")
					}
					if _, ok := reg.CacheConfigForLayer(AttributeSlotUser, AttributeLayerLocal); !ok {
						fail("CacheConfigForLayer lost the local layer")
					}
					if _, err := reg.Invalidate(AttributeSlotUser, "k"); err != nil {
						fail("Invalidate: %v", err)
						return
					}
				}
			}()
		}
		started.Wait()

		if err := reg.Register(AttributeSlotUser, shared); err != nil {
			t.Fatalf("Register: %v", err)
		}
		md, err := reg.Fetch(ctx, AttributeSlotUser, "k")
		if err != nil {
			t.Fatalf("Fetch after Register: %v", err)
		}
		if md["department"] != "eng" {
			t.Fatalf("the settled bag reads %#v; the shared layer must win", md["department"])
		}
		stop.Store(true)
		wg.Wait()

		if len(bad) > 0 {
			t.Fatalf("round %d: %s", round, bad[0])
		}
	}
}
