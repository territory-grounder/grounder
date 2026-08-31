package governance

// The ARMED half of judge-death detection (spec/004 REQ-307/REQ-308, TG-222).
//
// The monitors could already MEASURE a dead judge; what they could not do is STOP anything. A warning
// routed to a channel nobody reads is the failure mode the predecessor's incident record is made of — the
// local judge was dead for three weeks while every purely-local metric read healthy. So judge-death gets an
// ACTUATOR, and it is deliberately shaped like the two armed breakers this repo already trusts:
//
//   - core/safety.MutationBreaker — a trip forces the mode chokepoint to Shadow, is hash-chained to the
//     governance ledger, NEVER self-heals, and its read FAILS CLOSED (a safety breaker we cannot observe is
//     treated as tripped). JudgeDeadMan mirrors every one of those properties.
//   - eval/ci/check-baseline-freshness.sh — the eval plane's dead-man, whose whole design principle is that
//     the degraded state must make a gate REFUSE, not print a line. Halted() is that refusal.
//
// It is a core/breaker.Breaker named "judge-death" with threshold 1 (the first confirmed death halts —
// fail toward the tightest setting), consulted by the judged-accrual choke point before any judged evidence
// becomes a graduation decision. Like the mutation breaker it never calls Allow, so it has NO automatic
// recovery: the only path back is Rearm, a deliberate operator act after the judge is proven alive again.

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/breaker"
)

// BreakerName is the stable, metric-safe slug of the judge-death breaker. It shares the breaker store with
// every other named breaker, so `circuit_breaker_state{name="judge-death"}` is alertable exactly like the
// mutation and model breakers.
const BreakerName = "judge-death"

// HaltRecorder records a judged-accrual halt on a durable, tamper-evident surface (the worker binds it to
// the governance ledger, INV-19). OPTIONAL — a nil recorder still halts; it just adds no audit note.
// Declared here so core/governance need not import core/audit.
type HaltRecorder interface {
	RecordJudgeHalt(reason string)
}

// Halter is the actuator a monitor drives when it CONFIRMS the judge is dead. Injected so the monitors stay
// pure decision functions and the halt stays oracle-observable.
type Halter interface {
	Halt(ctx context.Context, reason string) error
}

// AtomicHalter is the OPTIONAL, race-free upgrade of Halter (TG-432). HaltOpen performs the halt AND reports
// in the same atomic step whether THIS call is the false→true transition — so a monitor pages exactly on the
// transition, deduped ACROSS every monitor and worker that drives the shared judge-death breaker, without the
// separate Halted() read that made the dedup check-then-act (two workers could both read not-yet-halted and
// both page; a read blip could suppress the one genesis page while the halt still landed). A monitor prefers
// this when the injected Halter provides it and falls back to Halted()+Halt() otherwise.
type AtomicHalter interface {
	Halter
	// HaltOpen halts judged accrual and returns openedNow=true iff this call caused the breaker's closed→open
	// transition (so the caller pages), false if it was already open (already paged by a sibling/earlier tick).
	HaltOpen(ctx context.Context, reason string) (openedNow bool, err error)
}

// JudgeDeadMan halts judged accrual when the judge is confirmed dead.
type JudgeDeadMan struct {
	b     *breaker.Breaker
	rec   HaltRecorder
	trips atomic.Int64

	mu     sync.Mutex
	reason string
}

// NewJudgeDeadMan arms the dead-man over the shared breaker store. Threshold is 1: one CONFIRMED death is
// enough, because the two detectors that call it are already conservative (the liveness monitor needs more
// than three eligible sessions; the frontier cross-check needs an independent model to score what the local
// judge left unscored). A nil store is refused rather than silently degrading to an unarmed dead-man.
func NewJudgeDeadMan(store breaker.Store, rec HaltRecorder) (*JudgeDeadMan, error) {
	b, err := breaker.New(BreakerName, store, breaker.WithThreshold(1))
	if err != nil {
		return nil, err
	}
	return &JudgeDeadMan{b: b, rec: rec}, nil
}

// Halt records a confirmed judge death and opens the breaker, so judged accrual stops everywhere the shared
// store is read. It is idempotent (a second death on an already-open breaker is a no-op transition) and it
// is ALWAYS safe: it can only make the posture more restrictive. A store-write failure is returned but the
// in-process reason is still recorded, so a halt is never lost to a database hiccup.
func (d *JudgeDeadMan) Halt(ctx context.Context, reason string) error {
	if d == nil {
		return nil
	}
	d.trips.Add(1)
	d.mu.Lock()
	d.reason = reason
	d.mu.Unlock()
	if d.rec != nil {
		d.rec.RecordJudgeHalt(reason)
	}
	return d.b.RecordFailure(ctx)
}

// HaltOpen is the atomic (TG-432) halt-and-report: it does the same durable halt as Halt — records the reason
// on the tamper-evident surface and opens the shared breaker — but reports, in ONE cross-process compare-and-
// swap, whether THIS call caused the closed→open transition. A monitor pages iff openedNow, so the human page
// is deduped across sibling workers with no separate, racy Halted() read. Idempotent and always-safe like
// Halt: a second death on an open breaker returns (false, nil) and only ever tightens the posture. A store
// error is returned (openedNow=false) and, as with Halt, the in-process reason is still recorded — so on the
// next tick the still-closed breaker is opened and the page fires then, never silently swallowed.
func (d *JudgeDeadMan) HaltOpen(ctx context.Context, reason string) (openedNow bool, err error) {
	if d == nil {
		return false, nil
	}
	d.trips.Add(1)
	d.mu.Lock()
	d.reason = reason
	d.mu.Unlock()
	if d.rec != nil {
		d.rec.RecordJudgeHalt(reason)
	}
	return d.b.TripOpen(ctx)
}

// haltJudgeAndPage runs the confirmed-death halt and reports whether THIS tick is the false→true transition
// that should PAGE the operator (deduped across every monitor and worker driving the shared judge-death
// breaker). It prefers the atomic path — AtomicHalter.HaltOpen, one cross-process compare-and-open, so two
// workers racing the same death page exactly once and a read blip cannot swallow the genesis page (TG-432).
// A plain Halter falls back to the pre-TG-432 check-then-act (optional Halted() read, then Halt): racy across
// processes, but no worse than before. HALT-FIRST, PAGE-SECOND (REQ-308): the halt lands before the page
// decision, so a broken notifier can never swallow the accrual stop. A nil Halter cannot halt OR dedup, so it
// pages every tick (halted=false) — the additive behaviour the monitors had with no dead-man wired.
func haltJudgeAndPage(ctx context.Context, halt Halter, reason string) (halted, page bool, err error) {
	if halt == nil {
		return false, true, nil
	}
	if ah, ok := halt.(AtomicHalter); ok {
		openedNow, e := ah.HaltOpen(ctx, reason)
		if e != nil {
			return false, false, e
		}
		return true, openedNow, nil
	}
	alreadyDead := false
	if hr, ok := halt.(HaltStateReader); ok {
		alreadyDead, _ = hr.Halted(ctx)
	}
	if e := halt.Halt(ctx, reason); e != nil {
		return false, false, e
	}
	return true, !alreadyDead, nil
}

// Halted reports whether judged accrual is halted. It FAILS CLOSED: a breaker whose store cannot be read
// reads HALTED, because accruing graduation decisions on a judge whose health is unobservable is the exact
// unsafe direction this control exists to close. A nil dead-man is not halted (no armed dead-man ⇒ nothing
// to honor) — the honest no-op for a boot with no breaker store.
func (d *JudgeDeadMan) Halted(ctx context.Context) (bool, string) {
	if d == nil {
		return false, ""
	}
	snap, err := d.b.Snapshot(ctx)
	if err != nil {
		return true, "judge-death breaker state is unreadable — judged accrual fails CLOSED"
	}
	if snap.State != breaker.StateOpen {
		return false, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reason != "" {
		return true, d.reason
	}
	// The breaker was tripped by a SIBLING worker through the shared row, so this process holds no reason
	// text. The halt still stands — the durable state is authoritative, never the local memory of it.
	return true, "judge-death halt is durably OPEN (tripped by this or a sibling worker)"
}

// State returns the dead-man's three-state position. An unreadable store reports OPEN, matching Halted's
// fail-closed direction so the gauge and the gate never disagree.
func (d *JudgeDeadMan) State(ctx context.Context) breaker.State {
	if d == nil {
		return breaker.StateClosed
	}
	snap, err := d.b.Snapshot(ctx)
	if err != nil {
		return breaker.StateOpen
	}
	return snap.State
}

// StateValue is the Prometheus gauge value for circuit_breaker_state (0 closed / 1 half-open / 2 open).
func (d *JudgeDeadMan) StateValue(ctx context.Context) float64 {
	return breaker.StateValue(d.State(ctx))
}

// Halts returns the running count of confirmed judge deaths this process observed.
func (d *JudgeDeadMan) Halts() int64 {
	if d == nil {
		return 0
	}
	return d.trips.Load()
}

// Rearm CLOSES the dead-man — the governed recovery counterpart to Halt, and the ONLY path back. It is
// never automatic: a dead-man that self-heals on a cooldown would resume accruing on a judge nobody proved
// alive, which is precisely the three-week silence this control exists to make impossible. A store-write
// failure leaves the halt STANDING (fail-safe: a re-arm that cannot persist keeps accrual stopped, never
// half-resumed).
func (d *JudgeDeadMan) Rearm(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if err := d.b.Reset(ctx); err != nil {
		return err
	}
	d.trips.Store(0)
	d.mu.Lock()
	d.reason = ""
	d.mu.Unlock()
	return nil
}
