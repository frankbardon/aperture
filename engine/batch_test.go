package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
)

// --- Acceptance: bulk alignment + partial-error handling ---

func TestCheckBatch_AlignmentAndPartialError(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")

	reqs := []Request{
		{Account: acctAcme, Principal: "alice", Action: "read", Object: "document:42"}, // allow
		{Account: acctAcme, Principal: "ghost", Action: "read", Object: "document:42"}, // unknown principal -> error
		{Account: acctAcme, Principal: "alice", Action: "read", Object: "document:99"}, // default deny
		{Account: acctAcme, Principal: "alice", Action: "read"},                        // invalid input -> error
	}
	out := f.eng.CheckBatch(context.Background(), reqs)
	if len(out) != len(reqs) {
		t.Fatalf("result length %d != query length %d", len(out), len(reqs))
	}

	// [0] allow, no error.
	if out[0].Err != nil || !out[0].Result.Allow {
		t.Fatalf("item 0: want allow/no-error, got allow=%v err=%v", out[0].Result.Allow, out[0].Err)
	}
	// [1] unknown principal: error item, the rest unaffected.
	if code := aerr.CodeOf(out[1].Err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("item 1: want APERTURE_NOT_FOUND, got %q (err=%v)", code, out[1].Err)
	}
	// [2] default deny, no error.
	if out[2].Err != nil || out[2].Result.Allow {
		t.Fatalf("item 2: want deny/no-error, got allow=%v err=%v", out[2].Result.Allow, out[2].Err)
	}
	// [3] invalid input: error item.
	if code := aerr.CodeOf(out[3].Err); code != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("item 3: want APERTURE_INVALID_INPUT, got %q", code)
	}
}

func TestEnumerateBatch_AlignmentAndPartialError(t *testing.T) {
	f := newEnumFixture(t, "account:acme/document:1", "account:acme/document:2")
	f.principal("alice")
	f.perm("p-impl", "implicit")
	f.grant("g-all", model.EffectAllow, "p-impl", "account:acme/**")

	reqs := []EnumerateRequest{
		{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"}, // ok
		{Account: acctAcme, Principal: "ghost", Action: "read", Pattern: "account:acme/**"}, // unknown principal -> error
		{Account: acctAcme, Principal: "alice", Action: "read", Pattern: ""},                // invalid input -> error
	}
	out := f.eng.EnumerateBatch(context.Background(), reqs)
	if len(out) != len(reqs) {
		t.Fatalf("result length %d != query length %d", len(out), len(reqs))
	}
	if out[0].Err != nil || len(out[0].Result) != 2 {
		t.Fatalf("item 0: want 2 ids/no-error, got %v err=%v", out[0].Result, out[0].Err)
	}
	if code := aerr.CodeOf(out[1].Err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("item 1: want APERTURE_NOT_FOUND, got %q", code)
	}
	if code := aerr.CodeOf(out[2].Err); code != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("item 2: want APERTURE_INVALID_INPUT, got %q", code)
	}
}

func TestExplainBatch_AlignmentAndPartialError(t *testing.T) {
	f := newFixture(t)
	f.principal("alice")
	f.grant("g1", acctAcme, subjPrincipal("alice"), model.EffectAllow, permRead, "document:42")

	reqs := []Request{
		{Account: acctAcme, Principal: "alice", Action: "read", Object: "document:42"},     // allow trace
		{Account: acctAcme, Principal: "alice", Action: "read", Object: "not-an-identity"}, // identity invalid -> error
	}
	out := f.eng.ExplainBatch(context.Background(), reqs)
	if len(out) != len(reqs) {
		t.Fatalf("result length %d != query length %d", len(out), len(reqs))
	}
	if out[0].Err != nil || !out[0].Result.Decision.Allow {
		t.Fatalf("item 0: want allow trace/no-error, got allow=%v err=%v", out[0].Result.Decision.Allow, out[0].Err)
	}
	if code := aerr.CodeOf(out[1].Err); code != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("item 1: want APERTURE_IDENTITY_INVALID, got %q", code)
	}
}

func TestBatch_NilInputYieldsNil(t *testing.T) {
	f := newFixture(t)
	if got := f.eng.CheckBatch(context.Background(), nil); got != nil {
		t.Fatalf("CheckBatch(nil) = %v, want nil", got)
	}
	if got := f.eng.EnumerateBatch(context.Background(), nil); got != nil {
		t.Fatalf("EnumerateBatch(nil) = %v, want nil", got)
	}
	if got := f.eng.ExplainBatch(context.Background(), nil); got != nil {
		t.Fatalf("ExplainBatch(nil) = %v, want nil", got)
	}
}
