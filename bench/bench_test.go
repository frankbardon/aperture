package bench

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/service"
)

// newService builds the facade under test. With audit on it wires a sampled,
// asynchronous recorder (the production shape: decision audit off the critical
// path); with audit off s.audit stays nil — the off path the brief names.
func newService(tb testing.TB, m benchModel, withAudit bool) (*service.Service, func()) {
	tb.Helper()
	eng := engine.New(m.store)
	if !withAudit {
		return service.New(eng), func() {}
	}
	rec := audit.New(m.store, audit.WithSampleRate(0.01), audit.WithBuffer(4096))
	return service.New(eng, service.WithAudit(rec)), func() { _ = rec.Close() }
}

func toQuery(r engine.Request) service.Query {
	return service.Query{Account: r.Account, Principal: r.Principal, Action: r.Action, Object: r.Object}
}

// warm runs the request once and asserts the expected verdict so a benchmark
// never silently measures a fail-closed deny (which would short-circuit the hot
// path and report a meaningless number). It also primes the engine's parsed-
// pattern cache, so subsequent iterations measure the steady, cached state.
func warm(tb testing.TB, svc *service.Service, q service.Query, wantAllow bool) {
	tb.Helper()
	res, err := svc.Check(context.Background(), q)
	if err != nil {
		tb.Fatalf("warm Check: %v", err)
	}
	if res.Allow != wantAllow {
		tb.Fatalf("warm Check: allow=%v want %v (reason: %s)", res.Allow, wantAllow, res.Reason)
	}
}

// BenchmarkCheckCachedAuditOff measures single cached Check latency + allocs on
// the off-audit path (s.audit == nil).
func BenchmarkCheckCachedAuditOff(b *testing.B) { benchmarkCheck(b, false) }

// BenchmarkCheckCachedAuditOn measures the same with sampled async audit wired,
// proving audit-on does not regress the target.
func BenchmarkCheckCachedAuditOn(b *testing.B) { benchmarkCheck(b, true) }

func benchmarkCheck(b *testing.B, withAudit bool) {
	m := buildModel(b)
	svc, closeFn := newService(b, m, withAudit)
	defer closeFn()
	q := toQuery(m.req)
	warm(b, svc, q, true)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := svc.Check(ctx, q)
		if err != nil || !res.Allow {
			b.Fatalf("Check: allow=%v err=%v", res.Allow, err)
		}
	}
	b.StopTimer()
	// Computed p99 over the measured run, reported alongside ns/op.
	reportP99(b, svc, q)
}

// BenchmarkCheckThroughputAuditOff measures sustained throughput (the parallel,
// many-goroutine shape that maps to checks/sec/instance).
func BenchmarkCheckThroughputAuditOff(b *testing.B) { benchmarkThroughput(b, false) }

// BenchmarkCheckThroughputAuditOn does the same with audit wired.
func BenchmarkCheckThroughputAuditOn(b *testing.B) { benchmarkThroughput(b, true) }

func benchmarkThroughput(b *testing.B, withAudit bool) {
	m := buildModel(b)
	svc, closeFn := newService(b, m, withAudit)
	defer closeFn()
	q := toQuery(m.req)
	warm(b, svc, q, true)

	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			res, err := svc.Check(ctx, q)
			if err != nil || !res.Allow {
				b.Fatalf("Check: allow=%v err=%v", res.Allow, err)
			}
		}
	})
	b.StopTimer()
	elapsed := time.Since(start)
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "checks/sec")
	}
}

// BenchmarkEnumerateBounded measures a bounded Enumerate and asserts the result
// never exceeds the engine's hard cache/result limit, demonstrating the op stays
// within bounds regardless of how many candidates a grant set produces.
func BenchmarkEnumerateBounded(b *testing.B) {
	m := buildModel(b)
	svc, closeFn := newService(b, m, false)
	defer closeFn()
	eq := service.EnumerateQuery{
		Account: m.enumReq.Account, Principal: m.enumReq.Principal,
		Action: m.enumReq.Action, Pattern: m.enumReq.Pattern,
	}
	ctx := context.Background()
	ids, err := svc.Enumerate(ctx, eq)
	if err != nil {
		b.Fatalf("Enumerate: %v", err)
	}
	if len(ids) == 0 {
		b.Fatal("Enumerate returned nothing; fixture should yield concrete documents")
	}
	if len(ids) > engine.DefaultEnumerateLimit {
		b.Fatalf("Enumerate returned %d ids, exceeding the bound %d", len(ids), engine.DefaultEnumerateLimit)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ids, err := svc.Enumerate(ctx, eq)
		if err != nil {
			b.Fatalf("Enumerate: %v", err)
		}
		if len(ids) > engine.DefaultEnumerateLimit {
			b.Fatalf("Enumerate exceeded bound: %d", len(ids))
		}
	}
}

// reportP99 measures per-op latency over a fixed sample and reports the 99th
// percentile as a benchmark metric (informational; the hard gate is
// TestCheckNFR).
func reportP99(b *testing.B, svc *service.Service, q service.Query) {
	const samples = 20000
	ctx := context.Background()
	p99 := measureP99(ctx, svc, q, samples)
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns")
}

// measureP99 runs n cached Checks, timing each, and returns the 99th-percentile
// latency. The engine is already warm, so this measures steady-state cost.
func measureP99(ctx context.Context, svc *service.Service, q service.Query, n int) time.Duration {
	lat := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		_, _ = svc.Check(ctx, q)
		lat[i] = time.Since(t0)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	idx := int(float64(n) * 0.99)
	if idx >= n {
		idx = n - 1
	}
	return lat[idx]
}

// TestCheckNFR is the HARD, gated NFR assertion (FR-31): p99 cached Check < 1ms
// and >= 10k checks/sec/instance, audit on AND off. It is environment-sensitive,
// so it is GATED: it self-skips unless APERTURE_BENCH_ASSERT=1, and also skips
// under -short. The default `make test` therefore never runs a wall-clock
// assertion. See docs/benchmarks.md.
func TestCheckNFR(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	const (
		p99Samples     = 100_000
		p99Ceiling     = time.Millisecond // the FR-31 target; actuals run far under it
		throughputMin  = 10_000.0         // checks/sec/instance
		throughputRunN = 200_000
	)

	for _, withAudit := range []bool{false, true} {
		name := "audit-off"
		if withAudit {
			name = "audit-on"
		}
		t.Run(name, func(t *testing.T) {
			m := buildModel(t)
			svc, closeFn := newService(t, m, withAudit)
			defer closeFn()
			q := toQuery(m.req)
			warm(t, svc, q, true)
			ctx := context.Background()

			p99 := measureP99(ctx, svc, q, p99Samples)
			t.Logf("%s: p99 cached Check = %v (ceiling %v)", name, p99, p99Ceiling)
			if p99 >= p99Ceiling {
				t.Errorf("%s: p99 cached Check %v exceeds NFR ceiling %v", name, p99, p99Ceiling)
			}

			// Sustained throughput, single goroutine (a conservative floor; real
			// instances parallelise across cores well above this).
			start := time.Now()
			for i := 0; i < throughputRunN; i++ {
				if _, err := svc.Check(ctx, q); err != nil {
					t.Fatalf("Check: %v", err)
				}
			}
			elapsed := time.Since(start)
			tput := float64(throughputRunN) / elapsed.Seconds()
			t.Logf("%s: throughput = %.0f checks/sec (floor %.0f)", name, tput, throughputMin)
			if tput < throughputMin {
				t.Errorf("%s: throughput %.0f checks/sec is below NFR floor %.0f", name, tput, throughputMin)
			}
		})
	}
}
