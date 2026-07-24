// Package breaker is the governed, persisted circuit breaker that bounds failure on every external
// dependency call (model gateway, judge, RAG, actuation reads). It is a faithful re-expression of the
// predecessor's scripts/lib/circuit_breaker.py (IFRNLLEI01PRD-631) — the three-state Hystrix/Fowler pattern
// with CROSS-PROCESS shared state — brought under the typed spine, with three deliberate improvements:
//
//  1. The Store is the single source of truth. Every Allow/Record reads-through and writes-back the shared
//     row, so sibling workers see one coordinated view of upstream health (the predecessor loaded state
//     once at construction, so cross-process coordination was loose). A pgx Store backs production; the
//     in-memory Store is the pure oracle used in tests.
//  2. The clock is injected (WithClock), so cooldown/half-open transitions are deterministic under test —
//     no wall-clock flake.
//  3. Typed state whose zero value is the safe one for THIS surface: a breaker guards the advisory/read
//     lane (inference, retrieval), which fails OPEN — so an absent or unreadable breaker resolves to
//     "closed" (allow), never blocking a healthy dependency it has simply never seen. A store error on
//     Allow therefore fails open (allow) and is surfaced, never silently swallowed (INV-15 observability).
//
// Provenance: [F] scripts/lib/circuit_breaker.py (three-state machine, thresholds, half-open probe, the
// SQLite-shared row) re-expressed on a repository interface + in-memory oracle. CONSTITUTION.md:130 ("named,
// observable circuit breakers with persisted state").
package breaker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// State is the breaker's position in the three-state machine. The zero value is StateClosed (allow) — the
// safe default for the read/advisory lane this breaker guards.
type State string

const (
	// StateClosed is normal operation: calls pass through; consecutive failures accrue toward the trip.
	StateClosed State = "closed"
	// StateOpen is tripped: calls short-circuit to their fallback until the cooldown elapses.
	StateOpen State = "open"
	// StateHalfOpen is probing: exactly one canary call is admitted to test whether the dependency recovered.
	StateHalfOpen State = "half_open"
)

// Record is the persisted, cross-process state of one named breaker. It is the whole coordination surface —
// the Store holds exactly this.
type Record struct {
	Name              string
	State             State
	FailureCount      int
	OpenedAt          time.Time // zero unless State == StateOpen
	HalfOpenSuccesses int
	LastTransitionAt  time.Time
	LastUpdatedAt     time.Time
}

// Store is the shared persistence for breaker state. A pgx implementation backs production (one row per
// name, keyed by Name); MemStore is the in-memory oracle. Load returns ok=false when the breaker has never
// been seen — the caller then treats it as a fresh closed breaker.
type Store interface {
	Load(ctx context.Context, name string) (rec Record, ok bool, err error)
	Save(ctx context.Context, rec Record) error
	List(ctx context.Context) ([]Record, error)
}

// Option configures a Breaker.
type Option func(*Breaker)

// WithThreshold sets the number of consecutive failures that trips a closed breaker to open (default 3).
func WithThreshold(n int) Option { return func(b *Breaker) { b.failureThreshold = n } }

// WithCooldown sets how long an open breaker waits before admitting a half-open probe (default 60s).
func WithCooldown(d time.Duration) Option { return func(b *Breaker) { b.cooldown = d } }

// WithHalfOpenSuccesses sets how many consecutive probe successes close a half-open breaker (default 1).
func WithHalfOpenSuccesses(n int) Option { return func(b *Breaker) { b.halfOpenNeeded = n } }

// WithClock injects the time source so cooldown transitions are deterministic in tests.
func WithClock(now func() time.Time) Option { return func(b *Breaker) { b.now = now } }

// Breaker is a per-named-dependency circuit breaker over a shared Store. It holds no mutable state itself —
// the Store is authoritative — so many Breaker values for the same name (across goroutines or processes)
// coordinate through one row.
type Breaker struct {
	name             string
	failureThreshold int
	cooldown         time.Duration
	halfOpenNeeded   int
	store            Store
	now              func() time.Time
	// mu makes the load→modify→save sequence ATOMIC. One breaker instance is shared across the model
	// gateway's concurrent calls to a rung (breakerFor caches it), so without this two concurrent
	// RecordFailure calls both read count N and both write N+1 — a LOST UPDATE that under-counts failures and
	// trips the breaker LATE, exactly when load is high and the trip matters most. (Cross-PROCESS atomicity
	// with a shared pgx store needs a DB-level compare-and-swap; this guards the in-process common case.)
	mu sync.Mutex
}

// New constructs a breaker for a named dependency over store. The name must be a non-empty slug
// ([A-Za-z0-9_-]) so it is safe as a stable metric label and row key.
func New(name string, store Store, opts ...Option) (*Breaker, error) {
	if !validName(name) {
		return nil, fmt.Errorf("breaker: invalid name %q (want non-empty [A-Za-z0-9_-])", name)
	}
	if store == nil {
		return nil, fmt.Errorf("breaker: nil store")
	}
	b := &Breaker{
		name:             name,
		failureThreshold: 3,
		cooldown:         60 * time.Second,
		halfOpenNeeded:   1,
		store:            store,
		now:              time.Now,
	}
	for _, o := range opts {
		o(b)
	}
	if b.failureThreshold < 1 {
		b.failureThreshold = 1
	}
	if b.halfOpenNeeded < 1 {
		b.halfOpenNeeded = 1
	}
	return b, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// load reads the current record, defaulting an unseen breaker to a fresh closed one.
func (b *Breaker) load(ctx context.Context) (Record, error) {
	rec, ok, err := b.store.Load(ctx, b.name)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{Name: b.name, State: StateClosed}, nil
	}
	if rec.State == "" {
		rec.State = StateClosed
	}
	return rec, nil
}

// Allow reports whether a caller should proceed. A closed breaker always allows; an open breaker denies
// until its cooldown elapses, at which point it admits one half-open probe (transitioning the shared row).
// A store error fails OPEN (allow) — this breaker guards the read/advisory lane, so losing the breaker must
// never block a dependency — and the error is returned so the caller can observe the degraded state.
func (b *Breaker) Allow(ctx context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx)
	if err != nil {
		return true, err // fail open (advisory lane), but surface the error
	}
	switch rec.State {
	case StateClosed:
		return true, nil
	case StateOpen:
		if rec.OpenedAt.IsZero() || b.now().Sub(rec.OpenedAt) > b.cooldown {
			b.transition(&rec, StateHalfOpen)
			return true, b.store.Save(ctx, rec) // admit the probe
		}
		return false, nil
	case StateHalfOpen:
		return true, nil // a probe is already in flight
	default:
		return true, nil
	}
}

// RecordSuccess reports that a protected call succeeded. In half-open it counts toward closing the breaker;
// in closed it clears any accrued failure count.
func (b *Breaker) RecordSuccess(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx)
	if err != nil {
		return err
	}
	switch rec.State {
	case StateHalfOpen:
		rec.HalfOpenSuccesses++
		if rec.HalfOpenSuccesses >= b.halfOpenNeeded {
			b.transition(&rec, StateClosed)
		}
		return b.store.Save(ctx, rec)
	case StateClosed:
		if rec.FailureCount > 0 {
			rec.FailureCount = 0
			return b.store.Save(ctx, rec)
		}
		return nil
	default:
		return nil
	}
}

// RecordFailure reports that a protected call failed. A failure in half-open re-opens immediately; in closed
// it accrues, tripping to open at the threshold.
func (b *Breaker) RecordFailure(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx)
	if err != nil {
		return err
	}
	switch rec.State {
	case StateHalfOpen:
		b.transition(&rec, StateOpen)
		return b.store.Save(ctx, rec)
	case StateClosed:
		rec.FailureCount++
		if rec.FailureCount >= b.failureThreshold {
			b.transition(&rec, StateOpen)
		}
		return b.store.Save(ctx, rec)
	default:
		return nil
	}
}

// transition moves rec into newState, resetting the fields that state requires, and stamps the clock.
func (b *Breaker) transition(rec *Record, newState State) {
	now := b.now()
	rec.State = newState
	rec.LastTransitionAt = now
	rec.LastUpdatedAt = now
	switch newState {
	case StateClosed:
		rec.FailureCount = 0
		rec.OpenedAt = time.Time{}
		rec.HalfOpenSuccesses = 0
	case StateOpen:
		rec.OpenedAt = now
		rec.HalfOpenSuccesses = 0
	case StateHalfOpen:
		rec.HalfOpenSuccesses = 0
	}
}

// Snapshot returns the current persisted record (a fresh closed record if never seen).
func (b *Breaker) Snapshot(ctx context.Context) (Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.load(ctx)
}

// Reset forces the breaker unconditionally CLOSED — the deliberate, administrative re-arm primitive. It is
// distinct from the automatic open→half_open→closed recovery (Allow admits a probe after the cooldown;
// RecordSuccess then closes it): the SAFETY mutation breaker never calls Allow, so it has no automatic
// recovery at all, and without Reset a trip is permanent. Reset is therefore the ONLY path back to closed
// for a breaker that is only ever Tripped/Recorded, and it is called solely from a governed, owner-gated
// site (spec/015 REQ-1523: a mode escalation into an actuating mode), never automatically — so the
// fail-closed default is preserved and only a deliberate operator action clears a trip. Idempotent: an
// already-closed, zero-failure breaker is left untouched (no write).
func (b *Breaker) Reset(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, err := b.load(ctx)
	if err != nil {
		return err
	}
	if rec.State == StateClosed && rec.FailureCount == 0 {
		return nil // already closed and clean — nothing to clear
	}
	b.transition(&rec, StateClosed)
	return b.store.Save(ctx, rec)
}

// Name returns the breaker's stable name.
func (b *Breaker) Name() string { return b.name }

// Guard runs fn only when the breaker allows, recording the outcome and returning fallback when the breaker
// is open. It is the ergonomic wrapper the model/judge/RAG call sites use: a persistently-failing dependency
// is short-circuited to its fallback instead of being retried on every call. fn's error is returned on a
// real (admitted) failure; when the breaker is open, ErrOpen wraps so callers can distinguish a trip from a
// live failure.
func (b *Breaker) Guard(ctx context.Context, fn func(context.Context) error) error {
	allowed, err := b.Allow(ctx)
	if err != nil {
		// degraded breaker (store error) fails open — run fn, but do not record against a store we cannot
		// read/write reliably; the error is already surfaced by Allow's caller path via the return below.
		return fn(ctx)
	}
	if !allowed {
		return fmt.Errorf("%w: %s", ErrOpen, b.name)
	}
	if ferr := fn(ctx); ferr != nil {
		_ = b.RecordFailure(ctx)
		return ferr
	}
	return b.RecordSuccess(ctx)
}

// ErrOpen is returned (wrapped) by Guard when the breaker is open and the call was short-circuited.
var ErrOpen = fmt.Errorf("breaker open")

// StateValue maps a state to the exported Prometheus gauge value (0 closed / 1 half-open / 2 open), matching
// the predecessor's circuit_breaker_state series so existing dashboards read unchanged.
func StateValue(s State) float64 {
	switch strings.ToLower(strings.TrimSpace(string(s))) {
	case string(StateClosed), "":
		return 0
	case string(StateHalfOpen):
		return 1
	case string(StateOpen):
		return 2
	default:
		return -1
	}
}
