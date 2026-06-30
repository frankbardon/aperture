package audit_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/model"
)

// memSink is a minimal concurrency-safe audit.Sink for the recorder tests. It
// is independent of the storage backends so the audit package tests stay
// hermetic.
type memSink struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (m *memSink) AppendAudit(_ context.Context, ev model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

func (m *memSink) snapshot() []model.AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.AuditEvent, len(m.events))
	copy(out, m.events)
	return out
}

func (m *memSink) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// fixedClock returns a deterministic, monotonic clock for stamping.
func fixedClock() func() time.Time {
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	var n int64
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := base.Add(time.Duration(n) * time.Second)
		n++
		return t
	}
}

// seqIDs returns a deterministic id generator.
func seqIDs() func() string {
	var n int64
	return func() string { return fmt.Sprintf("id-%d", atomic.AddInt64(&n, 1)) }
}

// TestRecordStampsAndPersists proves an always-on event is written synchronously
// with an id and timestamp stamped.
func TestRecordStampsAndPersists(t *testing.T) {
	sink := &memSink{}
	rec := audit.New(sink, audit.WithClock(fixedClock()), audit.WithIDFunc(seqIDs()))
	defer rec.Close()

	if err := rec.Record(context.Background(), model.AuditEvent{EventType: model.AuditMutation, Action: "PutGrant"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].ID == "" || got[0].Timestamp.IsZero() {
		t.Fatalf("event not stamped: %+v", got[0])
	}
}

// TestSampledDecisionsRespectRate proves an injected deterministic sampler is
// honoured: with a keep-1-in-3 sampler, exactly a third of the decisions are
// written, and the async writer flushes on Close.
func TestSampledDecisionsRespectRate(t *testing.T) {
	sink := &memSink{}
	var n int64
	keepEveryThird := audit.SamplerFunc(func() bool { return atomic.AddInt64(&n, 1)%3 == 0 })
	rec := audit.New(sink,
		audit.WithSampler(keepEveryThird),
		audit.WithIDFunc(seqIDs()),
		audit.WithClock(fixedClock()),
	)

	const total = 30
	sampled := 0
	for i := 0; i < total; i++ {
		built := false
		if rec.RecordDecision(context.Background(), func() model.AuditEvent {
			built = true
			return model.AuditEvent{EventType: model.AuditDecision, Action: "Check"}
		}) {
			sampled++
		}
		// The builder must only run when the event is sampled — the un-sampled
		// path must not pay the build cost.
		if !built && (i+1)%3 == 0 {
			t.Fatalf("event %d should have been sampled+built", i)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if sampled != total/3 {
		t.Fatalf("sampled %d of %d, want %d", sampled, total, total/3)
	}
	if got := sink.len(); got != total/3 {
		t.Fatalf("persisted %d decisions, want %d", got, total/3)
	}
}

// TestSampleRateZeroAndOne proves the rate sampler's boundary behaviour.
func TestSampleRateZeroAndOne(t *testing.T) {
	t.Run("zero records nothing", func(t *testing.T) {
		sink := &memSink{}
		rec := audit.New(sink, audit.WithSampleRate(0))
		for i := 0; i < 10; i++ {
			rec.RecordDecision(context.Background(), func() model.AuditEvent {
				return model.AuditEvent{EventType: model.AuditDecision}
			})
		}
		rec.Close()
		if sink.len() != 0 {
			t.Fatalf("rate 0 recorded %d, want 0", sink.len())
		}
	})
	t.Run("one records all", func(t *testing.T) {
		sink := &memSink{}
		rec := audit.New(sink, audit.WithSampleRate(1), audit.WithIDFunc(seqIDs()))
		for i := 0; i < 10; i++ {
			rec.RecordDecision(context.Background(), func() model.AuditEvent {
				return model.AuditEvent{EventType: model.AuditDecision}
			})
		}
		rec.Close()
		if sink.len() != 10 {
			t.Fatalf("rate 1 recorded %d, want 10", sink.len())
		}
	})
}

// TestDecisionNeverBlocksOnOverflow proves a full buffer drops decisions
// best-effort rather than blocking the caller. With a buffer of 1 and a blocked
// writer, every RecordDecision returns promptly; once unblocked and closed, no
// more than buffer+in-flight events are persisted and nothing deadlocks.
func TestDecisionNeverBlocksOnOverflow(t *testing.T) {
	release := make(chan struct{})
	sink := &blockingSink{release: release}
	rec := audit.New(sink,
		audit.WithSampleRate(1),
		audit.WithBuffer(1),
		audit.WithIDFunc(seqIDs()),
	)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			rec.RecordDecision(context.Background(), func() model.AuditEvent {
				return model.AuditEvent{EventType: model.AuditDecision}
			})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordDecision blocked on a full buffer — the decision path must never block")
	}

	close(release) // let the writer drain
	rec.Close()
	// Best-effort: most events were dropped, but the writer persisted at least one
	// and the recorder shut down cleanly.
	if sink.count() == 0 {
		t.Fatal("expected at least one decision to be persisted")
	}
}

type blockingSink struct {
	release chan struct{}
	n       int64
	once    sync.Once
}

func (b *blockingSink) AppendAudit(_ context.Context, _ model.AuditEvent) error {
	// Block the first write until released, simulating a slow sink so the buffer
	// fills and subsequent decisions must drop rather than block.
	b.once.Do(func() { <-b.release })
	atomic.AddInt64(&b.n, 1)
	return nil
}

func (b *blockingSink) count() int64 { return atomic.LoadInt64(&b.n) }

// TestCloseIsIdempotent proves Close can be called more than once safely.
func TestCloseIsIdempotent(t *testing.T) {
	rec := audit.New(&memSink{})
	if err := rec.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// RecordDecision after close is a no-op, not a panic.
	if rec.RecordDecision(context.Background(), func() model.AuditEvent { return model.AuditEvent{} }) {
		t.Fatal("RecordDecision after Close should report not-sampled")
	}
}

// TestConcurrentRecording exercises the recorder under concurrent always-on and
// sampled writes; run with -race it proves the writer + sampler are race-clean.
func TestConcurrentRecording(t *testing.T) {
	sink := &memSink{}
	rec := audit.New(sink,
		audit.WithSampleRate(1),
		audit.WithIDFunc(seqIDs()),
		audit.WithClock(fixedClock()),
		audit.WithBuffer(256),
	)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = rec.Record(context.Background(), model.AuditEvent{EventType: model.AuditMutation})
				rec.RecordDecision(context.Background(), func() model.AuditEvent {
					return model.AuditEvent{EventType: model.AuditDecision}
				})
			}
		}()
	}
	wg.Wait()
	rec.Close()
	// 8*50 synchronous mutations are always persisted; decisions are best-effort.
	if sink.len() < 400 {
		t.Fatalf("want >= 400 always-on events, got %d", sink.len())
	}
}
