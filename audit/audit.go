// Package audit records Aperture's append-only audit trail (E4-S2, FR-25),
// weighted toward safety-critical events without sinking the decision hot path.
//
// Two recording disciplines, by event class:
//
//   - ALWAYS + SYNCHRONOUS — every mutation, impersonation event, and delegation
//     is recorded the moment it happens, reliably, on the calling goroutine.
//     Mutations are not the hot path, so a synchronous, durable write is correct.
//   - SAMPLED + ASYNCHRONOUS — decision checks (Check/Enumerate/Explain) are the
//     hot path. They are recorded only when the configured Sampler keeps them,
//     and the keep is handed to a background writer over a buffered channel. The
//     decision NEVER blocks on the audit write: if the buffer is full the event
//     is dropped (best-effort), so an audit backlog can never regress the
//     decision NFR (proven on/off in E4-S4).
//
// Determinism: the Sampler and the clock are injected, so tests are not flaky —
// inject a deterministic Sampler (e.g. keep 1-in-N) and a fixed clock rather
// than relying on wall-clock time or an unseeded global rand. Production builds
// a rate Sampler with WithSampleRate.
//
// The trail is persisted through a Sink (model.Storage satisfies it). Close
// flushes the buffered writer deterministically and must be called on shutdown.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/frankbardon/aperture/model"
)

// Sink is the append-only persistence the recorder writes through.
// model.Storage satisfies it.
type Sink interface {
	AppendAudit(ctx context.Context, ev model.AuditEvent) error
}

// Sampler decides whether one decision-check event is kept. It MUST be safe for
// concurrent use — many goroutines sample on the decision path at once.
type Sampler interface {
	// Sample reports whether to keep the next decision event.
	Sample() bool
}

// SamplerFunc adapts a function to a Sampler. Tests inject a deterministic
// SamplerFunc so sampling is reproducible.
type SamplerFunc func() bool

// Sample implements Sampler.
func (f SamplerFunc) Sample() bool { return f() }

// rateSampler keeps each decision event with independent probability rate. rate
// <= 0 keeps nothing; rate >= 1 keeps everything. It uses math/rand/v2, whose
// top-level source is auto-seeded and safe for concurrent use, so production
// sampling needs no external source — tests inject their own Sampler instead.
type rateSampler struct{ rate float64 }

func (r rateSampler) Sample() bool {
	if r.rate <= 0 {
		return false
	}
	if r.rate >= 1 {
		return true
	}
	return mrand.Float64() < r.rate //nolint:gosec // sampling, not security-sensitive
}

// Recorder writes the audit trail. Construct one with New; it is safe for
// concurrent use and owns a single background writer goroutine drained by Close.
type Recorder struct {
	sink    Sink
	now     func() time.Time
	newID   func() string
	sampler Sampler
	buffer  int
	onError func(error)

	ch     chan model.AuditEvent
	wg     sync.WaitGroup
	mu     sync.RWMutex // guards closed; RLock-held during a channel send so Close cannot race it
	closed bool
}

// Option configures a Recorder at construction.
type Option func(*Recorder)

// WithClock overrides the timestamp clock (default time.Now). Inject a fixed
// clock for deterministic tests.
func WithClock(now func() time.Time) Option {
	return func(r *Recorder) {
		if now != nil {
			r.now = now
		}
	}
}

// WithIDFunc overrides the event-id generator (default: a random hex id). Inject
// a deterministic generator for tests.
func WithIDFunc(fn func() string) Option {
	return func(r *Recorder) {
		if fn != nil {
			r.newID = fn
		}
	}
}

// WithSampler sets the decision Sampler explicitly. Use it in tests for
// deterministic sampling; it takes precedence over WithSampleRate.
func WithSampler(s Sampler) Option {
	return func(r *Recorder) {
		if s != nil {
			r.sampler = s
		}
	}
}

// WithSampleRate sets a probabilistic decision Sampler keeping each event with
// probability rate (clamped to [0,1]). 0 disables decision audit; 1 records
// every decision.
func WithSampleRate(rate float64) Option {
	return func(r *Recorder) { r.sampler = rateSampler{rate: rate} }
}

// WithBuffer sets the async decision-writer buffer capacity (default 1024). When
// the buffer is full, sampled decisions are dropped rather than blocking the
// decision path.
func WithBuffer(n int) Option {
	return func(r *Recorder) {
		if n > 0 {
			r.buffer = n
		}
	}
}

// WithErrorHandler installs a callback invoked when an asynchronous audit write
// fails or a sampled decision is dropped on buffer overflow. It is for
// observability only; the decision path is never affected.
func WithErrorHandler(fn func(error)) Option {
	return func(r *Recorder) { r.onError = fn }
}

const defaultBuffer = 1024

// New returns a started Recorder writing through sink. With no sampling option
// decision audit is OFF (rate 0); always-on events (mutation, impersonation,
// delegation) are recorded regardless.
func New(sink Sink, opts ...Option) *Recorder {
	r := &Recorder{
		sink:    sink,
		now:     time.Now,
		newID:   randomID,
		sampler: rateSampler{rate: 0},
		buffer:  defaultBuffer,
	}
	for _, opt := range opts {
		opt(r)
	}
	r.ch = make(chan model.AuditEvent, r.buffer)
	r.wg.Add(1)
	go r.run()
	return r
}

// run is the background writer: it drains the buffer and persists each event,
// detached from any request context so a cancelled request never aborts an
// audit write.
func (r *Recorder) run() {
	defer r.wg.Done()
	for ev := range r.ch {
		if err := r.sink.AppendAudit(context.Background(), ev); err != nil {
			r.reportError(err)
		}
	}
}

// Record persists an always-on event (mutation, impersonation, or delegation)
// synchronously, stamping its id and timestamp. It returns the storage error so
// a caller may surface a failed mutation-audit write if it chooses.
func (r *Recorder) Record(ctx context.Context, ev model.AuditEvent) error {
	r.stamp(&ev)
	return r.sink.AppendAudit(ctx, ev)
}

// RecordDecision samples a decision-check event and, on a keep, builds it via fn
// and enqueues it for asynchronous writing WITHOUT blocking the decision path.
// fn is only invoked when the event is kept, so an un-sampled decision pays
// nothing but the Sampler call. It returns whether the event was sampled (a kept
// event dropped on buffer overflow still reports true). Build the event lazily
// in fn to keep the un-sampled path allocation-free.
func (r *Recorder) RecordDecision(ctx context.Context, fn func() model.AuditEvent) bool {
	if !r.sampler.Sample() {
		return false
	}
	ev := fn()
	r.stamp(&ev)

	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false
	}
	select {
	case r.ch <- ev:
	default:
		// Buffer full: drop best-effort. The decision must never block on audit.
		r.reportError(errOverflow)
	}
	return true
}

// Close stops the recorder, flushes every buffered decision event, and waits for
// the background writer to finish. It is idempotent and safe to call once on
// shutdown; after Close, RecordDecision is a no-op (always-on Record still
// writes synchronously through the sink).
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.ch)
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}

func (r *Recorder) stamp(ev *model.AuditEvent) {
	if ev.ID == "" {
		ev.ID = r.newID()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = r.now().UTC()
	}
}

func (r *Recorder) reportError(err error) {
	if r.onError != nil {
		r.onError(err)
	}
}

// errOverflow is reported (best-effort, via WithErrorHandler) when a sampled
// decision is dropped because the async buffer is full.
var errOverflow = errOverflowError{}

type errOverflowError struct{}

func (errOverflowError) Error() string {
	return "audit: decision buffer full, event dropped"
}

// randomID is the default event-id generator: 16 random bytes, hex-encoded.
func randomID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error on the platforms aperture targets;
	// if it ever did, an empty id is still acceptable (the trail tolerates it).
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
