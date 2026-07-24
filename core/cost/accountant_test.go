package cost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeForcer records ForceShadow calls — the test's stand-in for the mode chokepoint's kill seam.
type fakeForcer struct {
	mu      sync.Mutex
	count   int
	reasons []string
}

func (f *fakeForcer) ForceShadow(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.reasons = append(f.reasons, reason)
}
func (f *fakeForcer) forced() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// fakeRecorder records ledger trip notes.
type fakeRecorder struct {
	mu   sync.Mutex
	trip []string
}

func (r *fakeRecorder) RecordTrip(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trip = append(r.trip, reason)
}
func (r *fakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.trip)
}

// errStore fails every read/write — used to prove the fail-OPEN contract (a cost-store outage never halts).
type errStore struct{}

var errBoom = errors.New("cost store down")

func (errStore) Accrue(context.Context, string, string, float64) (float64, error) {
	return 0, errBoom
}
func (errStore) Total(context.Context, string, string) (float64, error) { return 0, errBoom }
func (errStore) BreakerOpen(context.Context) (bool, string, error)      { return false, "", errBoom }
func (errStore) TripBreaker(context.Context, string, float64) error     { return errBoom }

func fixedClock(day string) func() time.Time {
	t, _ := time.Parse("2006-01-02", day)
	return func() time.Time { return t }
}

func TestConfigRateMath(t *testing.T) {
	cfg := Config{Rates: map[string]float64{"fast": 0.5, "primary": 2.0}, DefaultRate: 1.0}
	cases := []struct {
		model  string
		tokens int
		want   float64
	}{
		{"fast", 1000, 0.5},
		{"primary", 500, 1.0},
		{"unknown", 2000, 2.0}, // falls back to DefaultRate 1.0
		{"fast", 0, 0},
		{"fast", -5, 0},
	}
	for _, c := range cases {
		if got := cfg.usdForTokens(c.model, c.tokens); got != c.want {
			t.Errorf("usdForTokens(%q,%d)=%v want %v", c.model, c.tokens, got, c.want)
		}
	}
}

func TestConfigEnabledAndEnforces(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("an empty config must be disabled")
	}
	if !(Config{DailyBudgetUSD: 1}).Enabled() {
		t.Error("a daily budget enables cost tracking")
	}
	if !(Config{Rates: map[string]float64{"fast": 0.1}}).Enabled() {
		t.Error("a positive rate enables cost tracking")
	}
	if (Config{Rates: map[string]float64{"fast": 0}}).Enabled() {
		t.Error("a zero rate does not enable")
	}
	if (Config{DefaultRate: 1}).enforces() {
		t.Error("a rate without a budget enforces nothing (meter only)")
	}
	if !(Config{SessionCeilingUSD: 1}).enforces() {
		t.Error("a session ceiling arms enforcement")
	}
}

// REQ-1211: cost accrues per completion into the durable day (UTC) and session accumulators.
func TestAccrual_DayAndSession(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	a, err := New(st, Config{DefaultRate: 1.0}, f, nil, WithClock(fixedClock("2026-07-20")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.AccrueLLM(ctx, "fast", "sess-1", 1000) // $1.00
	a.AccrueLLM(ctx, "fast", "sess-1", 2000) // +$2.00
	day, _ := st.Total(ctx, BucketDay, "2026-07-20")
	if day != 3.0 {
		t.Fatalf("daily total = %v want 3.0", day)
	}
	sess, _ := st.Total(ctx, BucketSession, "sess-1")
	if sess != 3.0 {
		t.Fatalf("session total = %v want 3.0", sess)
	}
	if a.TodayUSD(ctx) != 3.0 {
		t.Fatalf("TodayUSD = %v want 3.0", a.TodayUSD(ctx))
	}
	if f.forced() != 0 {
		t.Fatalf("no budget armed ⇒ no force-Shadow, got %d", f.forced())
	}
	if a.Tripped(ctx) {
		t.Fatal("no budget armed ⇒ never tripped")
	}
}

// REQ-1212: exceeding the daily budget trips → ForceShadow + ledger + shared OPEN state.
func TestDailyBudgetTrip(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	r := &fakeRecorder{}
	a, _ := New(st, Config{DefaultRate: 1.0, DailyBudgetUSD: 5.0}, f, r, WithClock(fixedClock("2026-07-20")))
	ctx := context.Background()
	a.AccrueLLM(ctx, "fast", "sess-1", 4000) // $4 — under budget
	if f.forced() != 0 || a.Tripped(ctx) {
		t.Fatalf("under budget must not trip: forced=%d tripped=%v", f.forced(), a.Tripped(ctx))
	}
	a.AccrueLLM(ctx, "fast", "sess-1", 2000) // +$2 ⇒ $6 >= $5 ⇒ trip
	if f.forced() == 0 {
		t.Fatal("exceeding the daily budget must force Shadow")
	}
	if r.count() != 1 {
		t.Fatalf("a trip must record exactly one ledger note, got %d", r.count())
	}
	if !a.Tripped(ctx) {
		t.Fatal("the shared breaker state must be OPEN after a daily trip")
	}
	if a.StateValue(ctx) != 2 {
		t.Fatalf("tg_cost_breaker_state must be 2 (open), got %v", a.StateValue(ctx))
	}
}

// REQ-1212: a session ceiling trips independently of the daily budget.
func TestSessionCeilingTrip(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	a, _ := New(st, Config{DefaultRate: 1.0, SessionCeilingUSD: 2.0}, f, nil, WithClock(fixedClock("2026-07-20")))
	ctx := context.Background()
	a.AccrueLLM(ctx, "fast", "sess-hot", 3000) // $3 >= $2 session ceiling ⇒ trip
	if f.forced() == 0 || !a.Tripped(ctx) {
		t.Fatalf("exceeding the session ceiling must trip: forced=%d tripped=%v", f.forced(), a.Tripped(ctx))
	}
	// A different session under the ceiling still accrues, and (because the shared breaker is already open)
	// honoring the shared trip force-Shadows again — proving the cross-session shared state.
	before := f.forced()
	a.AccrueLLM(ctx, "fast", "sess-cool", 500) // $0.50 for a fresh session
	if f.forced() <= before {
		t.Fatal("a later completion must honor the already-open shared breaker (cross-session force-Shadow)")
	}
}

// REQ-1214: a 0 budget disables enforcement — no trip regardless of accrued spend.
func TestDisabledZeroBudget(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	a, _ := New(st, Config{DefaultRate: 1.0 /* budgets both 0 */}, f, nil, WithClock(fixedClock("2026-07-20")))
	ctx := context.Background()
	a.AccrueLLM(ctx, "fast", "sess-1", 1_000_000) // $1000 accrued
	if f.forced() != 0 {
		t.Fatalf("a disabled budget must never force Shadow, got %d", f.forced())
	}
	if a.Tripped(ctx) {
		t.Fatal("a disabled budget must never trip")
	}
	if a.TodayUSD(ctx) != 1000 {
		t.Fatalf("accrual still meters spend when disabled: %v want 1000", a.TodayUSD(ctx))
	}
}

// REQ-1215: an unreadable cost store fails OPEN — no halt — and is loud (logged). The deliberate inverse of
// the mutation breaker's fail-CLOSED, because this guards spend, not a safety floor.
func TestFailOpenOnStoreError(t *testing.T) {
	f := &fakeForcer{}
	var logs []string
	a, _ := New(errStore{}, Config{DefaultRate: 1.0, DailyBudgetUSD: 0.01}, f, nil,
		WithLogf(func(format string, args ...any) { logs = append(logs, format) }))
	ctx := context.Background()
	// Even a tiny budget with a broken store must NOT trip (fail-open) — and it must log loudly.
	a.AccrueLLM(ctx, "fast", "sess-1", 100000)
	if f.forced() != 0 {
		t.Fatalf("a store outage must fail OPEN (no force-Shadow), got %d", f.forced())
	}
	if a.Tripped(ctx) {
		t.Fatal("Tripped must fail OPEN (false) on a store read error")
	}
	if a.StateValue(ctx) != 0 {
		t.Fatalf("StateValue must fail OPEN to 0 (closed) on a read error, got %v", a.StateValue(ctx))
	}
	if len(logs) == 0 {
		t.Fatal("a fail-open degradation must be LOGGED loudly, not silent")
	}
}

// REQ-1213: cross-process — a trip through one Accountant is honored by a SIBLING sharing the durable store.
func TestCrossProcessSiblingForceShadow(t *testing.T) {
	shared := NewMemStore()
	ctx := context.Background()
	// worker-1: a tiny daily budget so its first spend trips.
	f1 := &fakeForcer{}
	a1, _ := New(shared, Config{DefaultRate: 1.0, DailyBudgetUSD: 1.0}, f1, nil, WithClock(fixedClock("2026-07-20")))
	// worker-2 (sibling): a HUGE budget of its own — it would never trip on its own spend, but it must honor
	// the shared OPEN state worker-1 set.
	f2 := &fakeForcer{}
	a2, _ := New(shared, Config{DefaultRate: 1.0, DailyBudgetUSD: 1_000_000}, f2, nil, WithClock(fixedClock("2026-07-20")))

	a1.AccrueLLM(ctx, "fast", "sess-1", 2000) // $2 >= $1 ⇒ worker-1 trips, shared state OPEN
	if f1.forced() == 0 {
		t.Fatal("worker-1 must trip")
	}
	if f2.forced() != 0 {
		t.Fatal("worker-2 has not spent yet — no force expected before its next completion")
	}
	a2.AccrueLLM(ctx, "fast", "sess-2", 100) // sibling's next completion reads the shared OPEN state
	if f2.forced() == 0 {
		t.Fatal("the sibling must force its own mode to Shadow after reading the shared OPEN cost breaker")
	}
}

// REQ-1211: the per-actuation cost component accrues (armed for the flip; inert at 0).
func TestPerActuationAccrual(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	a, _ := New(st, Config{PerActuationUSD: 0.25, DailyBudgetUSD: 0.5}, f, nil, WithClock(fixedClock("2026-07-20")))
	ctx := context.Background()
	a.AccrueActuation(ctx, "sess-1") // $0.25
	a.AccrueActuation(ctx, "sess-1") // +$0.25 ⇒ $0.50 >= $0.50 ⇒ trip
	if f.forced() == 0 || !a.Tripped(ctx) {
		t.Fatalf("per-actuation accrual must count toward the budget: forced=%d tripped=%v", f.forced(), a.Tripped(ctx))
	}
	// A zero PerActuationUSD is a no-op.
	st2 := NewMemStore()
	f2 := &fakeForcer{}
	a2, _ := New(st2, Config{DailyBudgetUSD: 0.5}, f2, nil, WithClock(fixedClock("2026-07-20")))
	a2.AccrueActuation(ctx, "sess-1")
	if v, _ := st2.Total(ctx, BucketDay, "2026-07-20"); v != 0 {
		t.Fatalf("zero per-actuation cost must accrue nothing, got %v", v)
	}
}

func TestNewRejectsNilCollaborators(t *testing.T) {
	if _, err := New(nil, Config{}, &fakeForcer{}, nil); err == nil {
		t.Error("New must reject a nil store")
	}
	if _, err := New(NewMemStore(), Config{}, nil, nil); err == nil {
		t.Error("New must reject a nil forcer")
	}
}

func TestNilAccountantIsNoOp(t *testing.T) {
	var a *Accountant
	ctx := context.Background()
	a.AccrueLLM(ctx, "fast", "s", 100) // must not panic
	a.AccrueActuation(ctx, "s")
	if a.Tripped(ctx) || a.TodayUSD(ctx) != 0 || a.StateValue(ctx) != 0 {
		t.Fatal("a nil Accountant must be an inert no-op")
	}
}

// Race: many goroutines accruing concurrently must coordinate through the shared store without a data race
// and land on the correct total (go test -race).
func TestConcurrentAccrualIsRaceFree(t *testing.T) {
	st := NewMemStore()
	f := &fakeForcer{}
	a, _ := New(st, Config{DefaultRate: 1.0}, f, nil, WithClock(fixedClock("2026-07-20")))
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.AccrueLLM(ctx, "fast", "sess-shared", 1000) // $1 each
		}()
	}
	wg.Wait()
	if got := a.TodayUSD(ctx); got != 50.0 {
		t.Fatalf("concurrent accrual total = %v want 50.0", got)
	}
}
