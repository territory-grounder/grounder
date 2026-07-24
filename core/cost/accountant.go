package cost

import (
	"context"
	"fmt"
	"time"
)

// Accountant is the spend guard: a cost accumulator + the $-ceiling breaker over a shared Store. It holds
// no mutable budget state itself — the Store is authoritative — so many Accountant values (across
// goroutines or processes) coordinate through the same day/session rows and the same breaker-state row,
// exactly like core/safety.MutationBreaker over core/breaker.Store.
//
// It is the ANALOGUE of the mutation breaker with one deliberate inversion: it FAILS OPEN. Where the
// mutation breaker treats an unreadable store as OPEN (tripped) because it guards a safety floor, the
// Accountant treats an unreadable store as NOT tripped because it guards spend — a cost-store outage must
// never halt legitimate work. Every store error is LOGGED (Logf) so the degradation is loud, never silent.
type Accountant struct {
	store  Store
	cfg    Config
	forcer ShadowForcer
	rec    TripRecorder
	logf   func(format string, args ...any)
	now    func() time.Time
}

// Option configures an Accountant.
type Option func(*Accountant)

// WithLogf injects the loud-failure logger (the worker passes log.Printf). Fail-open degradations are
// surfaced through it — a nil logger discards them.
func WithLogf(f func(format string, args ...any)) Option { return func(a *Accountant) { a.logf = f } }

// WithClock injects the time source so the UTC day key is deterministic under test.
func WithClock(now func() time.Time) Option { return func(a *Accountant) { a.now = now } }

// New constructs the spend guard over store with cfg, forcing the mode to Shadow (forcer.ForceShadow) when
// a budget is exceeded. A nil store or nil forcer is an error — the guard cannot accrue or halt without
// them (fail LOUD at construction, never a silently dead guard). rec is optional (nil ⇒ no ledger note).
func New(store Store, cfg Config, forcer ShadowForcer, rec TripRecorder, opts ...Option) (*Accountant, error) {
	if store == nil {
		return nil, fmt.Errorf("cost: nil store — refusing to arm the spend guard")
	}
	if forcer == nil {
		return nil, fmt.Errorf("cost: nil shadow forcer — refusing to arm the spend guard")
	}
	a := &Accountant{store: store, cfg: cfg, forcer: forcer, rec: rec, now: time.Now}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// log surfaces a fail-open degradation loudly (nil logger ⇒ discarded).
func (a *Accountant) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// AccrueLLM records the approximate USD cost of ONE model-gateway completion — approxTokens * the model's
// per-1k rate — into the durable day (UTC) and session accumulators, then evaluates the breaker. It is the
// LIVE accrual path (the metering completer calls it per completion). A nil Accountant is a no-op.
//
// Order of operations, chosen for the cross-process kill AND the fail-open contract:
//  1. Honor a sibling's existing trip: read the shared breaker state; if OPEN, force THIS worker's mode to
//     Shadow (so a budget trip in any worker force-Shadows every sibling on its next completion).
//  2. Accrue this call's cost to the shared day + session totals.
//  3. If a positive budget is now met/exceeded, TRIP: force Shadow, record to the ledger, set the shared
//     OPEN state.
//
// Every store error fails OPEN — it is LOGGED and the accrual proceeds/returns without halting; a positive
// halt decision is taken ONLY on a SUCCESSFUL read that shows over-budget or already-open. A store outage
// therefore degrades to "no enforcement", never to a halt.
func (a *Accountant) AccrueLLM(ctx context.Context, model, sessionKey string, approxTokens int) {
	if a == nil {
		return
	}
	a.honorSharedTrip(ctx)
	a.accrue(ctx, a.cfg.usdForTokens(model, approxTokens), sessionKey)
}

// AccrueActuation records the flat per-actuation cost (Config.PerActuationUSD) for ONE actuation into the
// day + session accumulators and evaluates the breaker — the effect path's per-op cost component of the
// model. It is INERT while mutation is OFF (nothing actuates), armed for the flip. A nil Accountant, or a
// zero PerActuationUSD, is a no-op.
func (a *Accountant) AccrueActuation(ctx context.Context, sessionKey string) {
	if a == nil || a.cfg.PerActuationUSD <= 0 {
		return
	}
	a.honorSharedTrip(ctx)
	a.accrue(ctx, a.cfg.PerActuationUSD, sessionKey)
}

// honorSharedTrip reads the shared breaker state and, if a sibling already tripped it OPEN, force-Shadows
// THIS worker's mode. This is the cross-process kill's READ side on the accrual path: every completion
// re-consults the shared state, so a trip anywhere halts everyone on their next spend. FAIL-OPEN: a read
// error is logged and treated as NOT open (no force) — an unreadable cost store never halts a worker. With
// no budget armed the breaker can never be open (nothing trips it), so the read is skipped (meter-only).
func (a *Accountant) honorSharedTrip(ctx context.Context) {
	if !a.cfg.enforces() {
		return
	}
	open, reason, err := a.store.BreakerOpen(ctx)
	if err != nil {
		a.log("cost: breaker-state read failed — failing OPEN (spend guard, not a safety floor), no halt: %v", err)
		return
	}
	if open {
		a.forcer.ForceShadow("cost breaker OPEN (a sibling worker exceeded the budget): " + reason)
	}
}

// accrue adds usd to the day + session totals and trips the breaker if a positive budget is met/exceeded.
func (a *Accountant) accrue(ctx context.Context, usd float64, sessionKey string) {
	day := utcDayKey(a.now())
	dayTotal, dayErr := a.store.Accrue(ctx, BucketDay, day, usd)
	if dayErr != nil {
		a.log("cost: daily accrual failed — failing OPEN, no halt: %v", dayErr)
	}
	var (
		sessTotal float64
		sessErr   error
	)
	if sessionKey != "" {
		sessTotal, sessErr = a.store.Accrue(ctx, BucketSession, sessionKey, usd)
		if sessErr != nil {
			a.log("cost: session accrual failed — failing OPEN, no halt: %v", sessErr)
		}
	}
	if !a.cfg.enforces() {
		return // no budget armed ⇒ meter only, never trips (disabled = no enforcement)
	}
	// A positive halt decision is taken ONLY on a SUCCESSFUL read (fail-open): a failed accrual's total is
	// meaningless, so it can never push the breaker over.
	overDaily := a.cfg.DailyBudgetUSD > 0 && dayErr == nil && dayTotal >= a.cfg.DailyBudgetUSD
	overSession := a.cfg.SessionCeilingUSD > 0 && sessErr == nil && sessTotal >= a.cfg.SessionCeilingUSD
	if !overDaily && !overSession {
		return
	}
	var reason string
	switch {
	case overDaily && overSession:
		reason = fmt.Sprintf("daily budget $%.4f >= $%.2f and session %q $%.4f >= $%.2f", dayTotal, a.cfg.DailyBudgetUSD, sessionKey, sessTotal, a.cfg.SessionCeilingUSD)
	case overDaily:
		reason = fmt.Sprintf("daily budget exceeded: $%.4f >= $%.2f (UTC %s)", dayTotal, a.cfg.DailyBudgetUSD, day)
	default:
		reason = fmt.Sprintf("session ceiling exceeded: session %q $%.4f >= $%.2f", sessionKey, sessTotal, a.cfg.SessionCeilingUSD)
	}
	a.trip(ctx, reason, dayTotal)
}

// trip opens the cost breaker: force the mode to Shadow (the SAME kill the mutation breaker uses), record
// the trip to the ledger, and set the shared OPEN state so every sibling honors it on their next spend.
// The force + record happen BEFORE the durable write, so the local halt takes effect even if the store
// write fails (fail-open only governs the READ path — an over-budget signal we DID observe still halts).
func (a *Accountant) trip(ctx context.Context, reason string, usdAtTrip float64) {
	a.forcer.ForceShadow("cost breaker trip: " + reason)
	if a.rec != nil {
		a.rec.RecordTrip(reason)
	}
	if err := a.store.TripBreaker(ctx, reason, usdAtTrip); err != nil {
		a.log("cost: breaker tripped locally but shared state write failed (siblings will not see it until their own budget read): %v", err)
	}
}

// Tripped reports whether the SHARED cost breaker is durably OPEN — the read side of the system-wide spend
// kill (every worker may consult it). It FAILS OPEN, the deliberate inverse of the mutation breaker: a
// breaker whose store cannot be read reports NOT tripped (false) and the error is LOGGED — an unobservable
// spend guard must never halt legitimate work. A nil Accountant is not tripped.
func (a *Accountant) Tripped(ctx context.Context) bool {
	if a == nil {
		return false
	}
	open, _, err := a.store.BreakerOpen(ctx)
	if err != nil {
		a.log("cost: Tripped read failed — failing OPEN (not tripped), no halt: %v", err)
		return false
	}
	return open
}

// TodayUSD returns the durable UTC-day accrued total, for the tg_cost_usd_today gauge. A read error fails
// open to 0 (logged) — a metrics read must never halt. A nil Accountant returns 0.
func (a *Accountant) TodayUSD(ctx context.Context) float64 {
	if a == nil {
		return 0
	}
	total, err := a.store.Total(ctx, BucketDay, utcDayKey(a.now()))
	if err != nil {
		a.log("cost: today-usd read failed — reporting 0: %v", err)
		return 0
	}
	return total
}

// StateValue is the tg_cost_breaker_state gauge value (0 closed / 2 open). An unreadable store reports 0
// (closed) — fail-open, consistent with Tripped; the mutation breaker reports its unreadable state as OPEN,
// this one as CLOSED, because they guard opposite things. A nil Accountant reports 0.
func (a *Accountant) StateValue(ctx context.Context) float64 {
	return stateValue(a.Tripped(ctx))
}
