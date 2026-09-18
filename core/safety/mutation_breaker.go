package safety

// The armed mutation breaker (Phase-2 readiness review §4.B.2/§4.B.3): the breaker→gate wire that turns a
// deviation/chain-gap into an in-process mutation HALT, with no restart. It lives in the safety core so
// the enable/disable/kill machinery is one package, and it injects its audit recorder rather than
// importing core/audit (which imports core/safety — that would be an import cycle).

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/territory-grounder/grounder/core/breaker"
)

// TripRecorder records a mutation-breaker trip to a durable audit surface (the worker binds it to the
// governance ledger). It is OPTIONAL — a nil recorder still trips the gate; it just adds no audit note.
// Defined here, in the safety core, so this package need not import core/audit.
type TripRecorder interface {
	// RecordTrip is called AFTER the breaker has tripped and the gate has been disabled.
	RecordTrip(reason string)
}

// MutationBreaker is the armed safety breaker for the mutation lane: a thin wrapper over a three-state
// core/breaker whose ONLY reaction to a trip is to force the mode chokepoint to Shadow (ForceShadow). It is
// the "armed breaker" the canary needs — the breaker→chokepoint wire the readiness review found missing
// ("breaker armed is currently a fiction"). Because it guards a SAFETY lane, a trip means "drop to Shadow"
// (the most restrictive direction), and there is NO re-enable here — re-enabling actuation is a separate,
// owner-gated mode transition, never automatic. Under Shadow nothing ever trips it (the interceptor refuses
// before it executes, so no deviation/chain-gap is produced), so it is INERT today and becomes load-bearing
// only once a canary escalates the mode. It holds the ShadowForcer (the *Chokepoint) rather than the retired
// MutationGate — the absorption keeps the breaker→kill wire intact through the new single source of truth.
type MutationBreaker struct {
	b          *breaker.Breaker
	forcer     ShadowForcer
	rec        TripRecorder
	deviations atomic.Int64
}

// NewMutationBreaker arms the breaker over store, forcing the mode to Shadow after `threshold` safety events. A
// threshold below 1 clamps to 1 (fail toward the tightest first-canary setting — a single deviation trips).
func NewMutationBreaker(forcer ShadowForcer, store breaker.Store, threshold int, rec TripRecorder) (*MutationBreaker, error) {
	if forcer == nil {
		return nil, errors.New("safety: nil shadow forcer — refusing to arm the mutation breaker")
	}
	if threshold < 1 {
		threshold = 1
	}
	// The breaker is named "mutation" — a stable, metric-safe slug for the circuit_breaker_state series.
	b, err := breaker.New("mutation", store, breaker.WithThreshold(threshold))
	if err != nil {
		return nil, err
	}
	return &MutationBreaker{b: b, forcer: forcer, rec: rec}, nil
}

// Trip records one trip-worthy safety event (a deviation verdict, a failed post-execution health re-check,
// or a chain-integrity gap) and, when the accrued events reach the threshold, opens the breaker and forces the
// mode to Shadow (ForceShadow) — actuation halts in-process, no restart. It returns whether THIS event forced
// Shadow. It is always safe: it can only ever make the posture more restrictive, and a breaker-store error
// fails toward the safe state (force Shadow anyway) rather than swallowing the deviation. A nil breaker is a
// no-op.
func (m *MutationBreaker) Trip(ctx context.Context, reason string) (disabled bool, err error) {
	if m == nil {
		return false, nil
	}
	m.deviations.Add(1)
	if ferr := m.b.RecordFailure(ctx); ferr != nil {
		// A store we cannot write must never leave a deviation unhandled — fail closed and force Shadow anyway.
		m.forcer.ForceShadow(reason + " (breaker store error)")
		m.recordTrip(reason + " (breaker store error)")
		return true, ferr
	}
	snap, serr := m.b.Snapshot(ctx)
	if serr != nil || snap.State == breaker.StateOpen {
		m.forcer.ForceShadow(reason)
		m.recordTrip(reason)
		return true, serr
	}
	return false, nil
}

// Rearm CLOSES a tripped breaker — the governed recovery counterpart to Trip. Where Trip opens the breaker
// and forces Shadow, Rearm clears the open row so actuation can resume, and it resets the in-process
// deviation counter to match the cleared durable state. It is NEVER automatic: a safety breaker must not
// self-heal, so the ONLY caller is the mode controller re-arming on an owner-gated escalation INTO an
// actuating mode (spec/015 REQ-1523) — that deliberate "resume actuation" decision is what makes a false or
// already-resolved deviation trip recoverable instead of a permanent, estate-wide actuation kill. A nil
// breaker is a no-op. A store-write failure is returned and the breaker is LEFT OPEN (fail-safe: a re-arm
// that cannot persist keeps actuation halted, never silently half-enabled). Because the store is the shared
// cross-process row, one worker's Rearm closes the breaker for every sibling that reads it.
func (m *MutationBreaker) Rearm(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := m.b.Reset(ctx); err != nil {
		return err
	}
	m.deviations.Store(0)
	return nil
}

func (m *MutationBreaker) recordTrip(reason string) {
	if m.rec != nil {
		m.rec.RecordTrip(reason)
	}
}

// State returns the breaker's current three-state position (closed / half_open / open). A breaker whose
// store cannot be read is reported OPEN — a SAFETY breaker we cannot observe is treated as tripped.
func (m *MutationBreaker) State(ctx context.Context) breaker.State {
	if m == nil {
		return breaker.StateClosed
	}
	snap, err := m.b.Snapshot(ctx)
	if err != nil {
		return breaker.StateOpen
	}
	return snap.State
}

// Tripped reports whether the SHARED breaker is durably OPEN — tripped by THIS worker or, through the
// cross-process store, by ANY sibling worker. It is the READ side of the system-wide kill (design-wisdom #3):
// every worker consults it before it actuates, so a deviation/chain-gap trip in one worker halts mutation
// everywhere the same durable store is read. It FAILS CLOSED: a breaker whose store cannot be read reads OPEN
// (via State), so an unreadable breaker is treated as tripped and a sibling can never actuate on an
// unobservable safety breaker. A nil breaker is not tripped (no armed breaker ⇒ nothing to honor). A
// half-open probe is NOT "tripped" — one canary call is deliberately admitted — so this returns true only for
// a fully OPEN breaker.
func (m *MutationBreaker) Tripped(ctx context.Context) bool {
	if m == nil {
		return false
	}
	return m.State(ctx) == breaker.StateOpen
}

// StateValue is the Prometheus gauge value for circuit_breaker_state (0 closed / 1 half-open / 2 open),
// matching the predecessor's series so existing dashboards read unchanged.
func (m *MutationBreaker) StateValue(ctx context.Context) float64 {
	return breaker.StateValue(m.State(ctx))
}

// Deviations returns the running count of trip-worthy safety events observed (the deviation_count gauge).
func (m *MutationBreaker) Deviations() int64 {
	if m == nil {
		return 0
	}
	return m.deviations.Load()
}
