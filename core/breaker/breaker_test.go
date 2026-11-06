package breaker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source so cooldown transitions are deterministic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time  { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newBreaker(t *testing.T, store Store, opts ...Option) (*Breaker, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_000_000, 0).UTC()}
	all := append([]Option{WithThreshold(3), WithCooldown(60 * time.Second), WithClock(c.now)}, opts...)
	b, err := New("model-gateway", store, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, c
}

func state(t *testing.T, b *Breaker) State {
	t.Helper()
	rec, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return rec.State
}

func TestClosedAccruesFailuresThenTrips(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	// A fresh breaker allows and is closed.
	if ok, _ := b.Allow(ctx); !ok {
		t.Fatal("fresh breaker must allow")
	}
	// Two failures stay closed (threshold 3).
	_ = b.RecordFailure(ctx)
	_ = b.RecordFailure(ctx)
	if s := state(t, b); s != StateClosed {
		t.Fatalf("2 failures < threshold must stay closed, got %s", s)
	}
	if ok, _ := b.Allow(ctx); !ok {
		t.Fatal("below-threshold breaker must still allow")
	}
	// The third consecutive failure trips it open.
	_ = b.RecordFailure(ctx)
	if s := state(t, b); s != StateOpen {
		t.Fatalf("threshold failures must trip open, got %s", s)
	}
	if ok, _ := b.Allow(ctx); ok {
		t.Fatal("open breaker must short-circuit (deny)")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	_ = b.RecordFailure(ctx)
	_ = b.RecordFailure(ctx) // 2/3
	_ = b.RecordSuccess(ctx) // resets to 0
	_ = b.RecordFailure(ctx)
	_ = b.RecordFailure(ctx) // 2/3 again — must NOT be tripped
	if s := state(t, b); s != StateClosed {
		t.Fatalf("a success must reset the failure count, got %s", s)
	}
}

func TestOpenAdmitsHalfOpenProbeAfterCooldown(t *testing.T) {
	ctx := context.Background()
	b, c := newBreaker(t, NewMemStore())
	for i := 0; i < 3; i++ {
		_ = b.RecordFailure(ctx)
	}
	// Still within cooldown → denied.
	c.add(30 * time.Second)
	if ok, _ := b.Allow(ctx); ok {
		t.Fatal("within cooldown the open breaker must deny")
	}
	// Past cooldown → one half-open probe admitted, state becomes half_open.
	c.add(31 * time.Second) // total 61s > 60s cooldown
	if ok, _ := b.Allow(ctx); !ok {
		t.Fatal("after cooldown the breaker must admit a probe")
	}
	if s := state(t, b); s != StateHalfOpen {
		t.Fatalf("admitting a probe must transition to half_open, got %s", s)
	}
}

func TestHalfOpenProbeSuccessCloses(t *testing.T) {
	ctx := context.Background()
	b, c := newBreaker(t, NewMemStore())
	for i := 0; i < 3; i++ {
		_ = b.RecordFailure(ctx)
	}
	c.add(61 * time.Second)
	_, _ = b.Allow(ctx) // → half_open
	_ = b.RecordSuccess(ctx)
	if s := state(t, b); s != StateClosed {
		t.Fatalf("a successful probe must close the breaker, got %s", s)
	}
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	ctx := context.Background()
	b, c := newBreaker(t, NewMemStore())
	for i := 0; i < 3; i++ {
		_ = b.RecordFailure(ctx)
	}
	c.add(61 * time.Second)
	_, _ = b.Allow(ctx) // → half_open
	_ = b.RecordFailure(ctx)
	if s := state(t, b); s != StateOpen {
		t.Fatalf("a failed probe must re-open the breaker, got %s", s)
	}
	// And it is denied again until the NEXT cooldown from the re-open moment.
	if ok, _ := b.Allow(ctx); ok {
		t.Fatal("re-opened breaker must deny immediately after the failed probe")
	}
}

// The defining property: state is shared through the Store, so two breaker values for the same name (as two
// sibling processes would be) see one coordinated view of upstream health.
func TestCrossInstanceSharedState(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	// Two breaker values for the same name, sharing one store AND one clock — exactly the coordination two
	// sibling processes have (shared row + shared wall clock).
	c := &clock{t: time.Unix(1_000_000, 0).UTC()}
	mk := func() *Breaker {
		b, err := New("model-gateway", store, WithThreshold(3), WithCooldown(60*time.Second), WithClock(c.now))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return b
	}
	a, b := mk(), mk()
	// Trip via instance A.
	for i := 0; i < 3; i++ {
		_ = a.RecordFailure(ctx)
	}
	// Instance B, sharing the store, must see OPEN and short-circuit.
	if s := state(t, b); s != StateOpen {
		t.Fatalf("sibling breaker must see the shared open state, got %s", s)
	}
	if ok, _ := b.Allow(ctx); ok {
		t.Fatal("sibling breaker must short-circuit on the shared open state")
	}
}

func TestGuardShortCircuitsWhenOpen(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	for i := 0; i < 3; i++ {
		_ = b.RecordFailure(ctx)
	}
	calls := 0
	err := b.Guard(ctx, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Guard on an open breaker must return ErrOpen, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("Guard must NOT invoke fn when open, called %d times", calls)
	}
}

func TestGuardRecordsOutcome(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	// Three guarded failures must trip the breaker exactly as manual RecordFailure would.
	boom := errors.New("upstream 503")
	for i := 0; i < 3; i++ {
		if err := b.Guard(ctx, func(context.Context) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("Guard must propagate the live failure, got %v", err)
		}
	}
	if s := state(t, b); s != StateOpen {
		t.Fatalf("three guarded failures must trip open, got %s", s)
	}
}

// errStore fails every Load — modeling the breaker's own persistence being down.
type errStore struct{}

func (errStore) Load(context.Context, string) (Record, bool, error) {
	return Record{}, false, errors.New("store down")
}
func (errStore) Save(context.Context, Record) error   { return errors.New("store down") }
func (errStore) List(context.Context) ([]Record, error) { return nil, errors.New("store down") }

func TestAllowFailsOpenOnStoreError(t *testing.T) {
	ctx := context.Background()
	b, err := New("model-gateway", errStore{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ok, aerr := b.Allow(ctx)
	if !ok {
		t.Fatal("a store error must fail OPEN (allow) — the breaker guards the read lane")
	}
	if aerr == nil {
		t.Fatal("a store error must be surfaced, never silently swallowed")
	}
}

func TestInvalidConstruction(t *testing.T) {
	if _, err := New("", NewMemStore()); err == nil {
		t.Fatal("empty name must be rejected")
	}
	if _, err := New("bad name!", NewMemStore()); err == nil {
		t.Fatal("non-slug name must be rejected")
	}
	if _, err := New("ok", nil); err == nil {
		t.Fatal("nil store must be rejected")
	}
}

func TestStateValueMatchesPredecessorGauge(t *testing.T) {
	cases := map[State]float64{StateClosed: 0, StateHalfOpen: 1, StateOpen: 2, "": 0, "weird": -1}
	for s, want := range cases {
		if got := StateValue(s); got != want {
			t.Errorf("StateValue(%q) = %v, want %v", s, got, want)
		}
	}
}

// ADVERSARIAL (INV-22, concurrent inputs): a breaker instance is SHARED across concurrent calls to its rung.
// Concurrent RecordFailure must not lose updates — under-counting failures would trip the breaker LATE. With
// a high threshold (so it never trips), the final count must equal the number of concurrent failures.
func TestBreakerConcurrentFailuresNoLostUpdate(t *testing.T) {
	b, err := New("rung", NewMemStore(), WithThreshold(100000))
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.RecordFailure(context.Background()); err != nil {
				t.Errorf("record failure: %v", err)
			}
		}()
	}
	wg.Wait()
	snap, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.FailureCount != n {
		t.Fatalf("lost failure updates under concurrency: FailureCount=%d, want %d", snap.FailureCount, n)
	}
}

// --- Reset: the deliberate re-arm primitive (spec/015 REQ-1523) -----------------------------------------

func TestResetForcesOpenBreakerClosed(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	for i := 0; i < 3; i++ { // trip it (threshold 3)
		_ = b.RecordFailure(ctx)
	}
	if s, _ := b.Snapshot(ctx); s.State != StateOpen {
		t.Fatalf("precondition: breaker must be open, got %v", s.State)
	}
	if err := b.Reset(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	s, _ := b.Snapshot(ctx)
	if s.State != StateClosed || s.FailureCount != 0 || !s.OpenedAt.IsZero() {
		t.Fatalf("after Reset: state=%v fails=%d openedAt=%v, want closed / 0 / zero", s.State, s.FailureCount, s.OpenedAt)
	}
}

func TestResetIsIdempotentOnAClosedBreaker(t *testing.T) {
	ctx := context.Background()
	b, _ := newBreaker(t, NewMemStore())
	if err := b.Reset(ctx); err != nil { // never tripped — already closed
		t.Fatalf("reset on a closed breaker must be a no-op, got %v", err)
	}
	if s, _ := b.Snapshot(ctx); s.State != StateClosed {
		t.Fatalf("state after no-op Reset = %v, want closed", s.State)
	}
}
