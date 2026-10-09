package safety

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/breaker"
)

// TestForceShadowIsIdempotentAndOnlyMoreOff proves ForceShadow is the safe actuating→Shadow primitive (the
// absorbed gate.Disable, REQ-1520): it drops an actuating chokepoint to read-only, and forcing an already-off
// chokepoint is a no-op that leaves it off (never re-actuates). The actuating precondition is set through a
// FixedModeAuthority (the mode is the single source of truth) — the test never re-enables.
func TestForceShadowIsIdempotentAndOnlyMoreOff(t *testing.T) {
	cp := NewReadOnlyChokepoint()
	if cp.MayActuate() {
		t.Fatal("a read-only chokepoint must never actuate")
	}
	// Forcing an already-off chokepoint is a safe no-op.
	cp.ForceShadow("noop")
	if cp.MayActuate() || cp.GuardMutation() == nil {
		t.Fatalf("forcing an off chokepoint must leave it off: mayActuate=%v guard=%v", cp.MayActuate(), cp.GuardMutation())
	}
	// An actuating chokepoint (mode Semi/Full + preflight green): MayActuate true, then ForceShadow flips it OFF.
	cp2 := NewActuatingChokepoint()
	if !cp2.MayActuate() {
		t.Fatal("precondition: an actuating chokepoint must actuate before ForceShadow")
	}
	cp2.ForceShadow("kill")
	if cp2.MayActuate() {
		t.Fatal("ForceShadow must drop an actuating chokepoint to read-only")
	}
	if cp2.GuardMutation() == nil {
		t.Fatal("after ForceShadow, GuardMutation must refuse (mode Shadow)")
	}
	// ForceShadow never touches the preflight bit — it can only make the posture more restrictive, never re-actuate.
	cp2.ForceShadow("kill again")
	if cp2.MayActuate() {
		t.Fatal("a second ForceShadow must keep the chokepoint read-only (idempotent)")
	}
}

// TestMutationBreakerTripForcesShadow proves the armed breaker→chokepoint wire: with an actuating chokepoint and
// a threshold-1 breaker, a single forced trip opens the breaker and forces the mode to Shadow (ForceShadow) —
// the in-process kill the canary relies on. It also records the trip to the injected recorder.
func TestMutationBreakerTripForcesShadow(t *testing.T) {
	ctx := context.Background()
	cp := NewActuatingChokepoint()

	var recorded []string
	rec := recorderFunc(func(reason string) { recorded = append(recorded, reason) })
	mb, err := NewMutationBreaker(cp, breaker.NewMemStore(), 1, rec)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	if got := mb.StateValue(ctx); got != 0 {
		t.Fatalf("a fresh mutation breaker must read closed (0), got %v", got)
	}

	disabled, err := mb.Trip(ctx, "forced deviation verdict")
	if err != nil {
		t.Fatalf("trip: %v", err)
	}
	if !disabled {
		t.Fatal("a threshold-1 breaker must force Shadow on the first trip")
	}
	if cp.MayActuate() {
		t.Fatal("after the breaker tripped, the chokepoint must be read-only (mode Shadow)")
	}
	if mb.State(ctx) != breaker.StateOpen || mb.StateValue(ctx) != 2 {
		t.Fatalf("the breaker must read open (gauge 2), got state=%v value=%v", mb.State(ctx), mb.StateValue(ctx))
	}
	if len(recorded) != 1 || recorded[0] != "forced deviation verdict" {
		t.Fatalf("the trip must be recorded once with its reason, got %v", recorded)
	}
	if mb.Deviations() != 1 {
		t.Fatalf("deviation count must be 1, got %d", mb.Deviations())
	}
	// A second trip on an already-open breaker is a safe no-op for the (already-off) chokepoint.
	if _, err := mb.Trip(ctx, "second deviation"); err != nil {
		t.Fatalf("second trip: %v", err)
	}
	if cp.MayActuate() {
		t.Fatal("the chokepoint must stay read-only after a second trip")
	}
}

// TestMutationBreakerThresholdClampsToOne proves a below-1 threshold clamps to the tightest setting.
func TestMutationBreakerThresholdClampsToOne(t *testing.T) {
	ctx := context.Background()
	cp := NewActuatingChokepoint()
	mb, err := NewMutationBreaker(cp, breaker.NewMemStore(), 0, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	if disabled, _ := mb.Trip(ctx, "one"); !disabled || cp.MayActuate() {
		t.Fatal("threshold 0 must clamp to 1 so a single trip forces Shadow")
	}
}

// TestMutationBreakerTrippedIsCrossProcess proves the design-wisdom #3 shared kill at the breaker layer: two
// MutationBreakers over ONE shared store are exactly two sibling worker processes coordinating through the same
// durable row (breaker.MemStore is documented as the in-memory twin of the pgx row). A trip in worker-1 opens
// the shared row; worker-2 then reads Tripped()==true WITHOUT itself having recorded any failure — the trip
// crossed the process boundary. Its own chokepoint is UNTOUCHED until it honors the trip (that is the
// interceptor's job); this test isolates the read-side signal the interceptor consults.
func TestMutationBreakerTrippedIsCrossProcess(t *testing.T) {
	ctx := context.Background()
	shared := breaker.NewMemStore() // the one durable row both "processes" read/write

	cp1 := NewActuatingChokepoint()
	mb1, err := NewMutationBreaker(cp1, shared, 1, nil) // worker-1
	if err != nil {
		t.Fatalf("arm breaker 1: %v", err)
	}
	cp2 := NewActuatingChokepoint()
	mb2, err := NewMutationBreaker(cp2, shared, 1, nil) // worker-2 (a sibling)
	if err != nil {
		t.Fatalf("arm breaker 2: %v", err)
	}

	// Before any trip, neither sibling reads the breaker as tripped.
	if mb1.Tripped(ctx) || mb2.Tripped(ctx) {
		t.Fatal("a fresh shared breaker must read not-tripped in both workers")
	}

	// Worker-1 trips (a deviation). This opens the SHARED row.
	if disabled, err := mb1.Trip(ctx, "deviation on worker-1"); err != nil || !disabled {
		t.Fatalf("worker-1 trip = (disabled=%v, err=%v), want (true, nil)", disabled, err)
	}

	// Worker-2, which recorded NOTHING itself, now reads the trip across the shared store — the cross-process kill.
	if !mb2.Tripped(ctx) {
		t.Fatal("CROSS-PROCESS FAIL: worker-2 must read the shared breaker as tripped after worker-1 tripped it")
	}
	if mb2.State(ctx) != breaker.StateOpen {
		t.Fatalf("worker-2 must read the shared breaker OPEN, got %v", mb2.State(ctx))
	}
	// Worker-2's own posture is still nominally actuating until it HONORS the trip (interceptor.ForceShadow) — the
	// signal exists; acting on it is the read side wired in core/actuate. This test proves the signal crosses.
	if cp2.MayActuate() && !mb2.Tripped(ctx) {
		t.Fatal("precondition sanity: worker-2 must at least observe the trip")
	}
}

// TestMutationBreakerTrippedFailsClosedOnStoreError proves the fail-CLOSED property: a breaker whose durable
// store cannot be read reports Tripped()==true (State OPEN). A store outage therefore HALTS mutation everywhere
// rather than letting a worker actuate on an unobservable safety breaker — never fail-open to actuating.
func TestMutationBreakerTrippedFailsClosedOnStoreError(t *testing.T) {
	ctx := context.Background()
	cp := NewActuatingChokepoint()
	mb, err := NewMutationBreaker(cp, errStore{}, 1, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	if !mb.Tripped(ctx) {
		t.Fatal("FAIL-OPEN: a breaker whose store errors must read tripped (fail closed), got not-tripped")
	}
	if mb.State(ctx) != breaker.StateOpen {
		t.Fatalf("an unreadable safety breaker must report OPEN, got %v", mb.State(ctx))
	}
	// A nil breaker is not tripped (no armed breaker ⇒ nothing to honor).
	var nilMB *MutationBreaker
	if nilMB.Tripped(ctx) {
		t.Fatal("a nil breaker must not read tripped")
	}
}

// errStore is a breaker.Store whose Load always errors — the store-outage oracle for the fail-closed path.
type errStore struct{}

func (errStore) Load(context.Context, string) (breaker.Record, bool, error) {
	return breaker.Record{}, false, errStoreErr
}
func (errStore) Save(context.Context, breaker.Record) error     { return errStoreErr }
func (errStore) List(context.Context) ([]breaker.Record, error) { return nil, errStoreErr }

var errStoreErr = errors.New("breaker store unreachable")

// recorderFunc adapts a func to the TripRecorder interface for the oracle.
type recorderFunc func(string)

func (f recorderFunc) RecordTrip(reason string) { f(reason) }

// --- Rearm: governed recovery from a trip (spec/015 REQ-1523) -------------------------------------------

func TestMutationBreakerRearmClosesTrippedBreaker(t *testing.T) {
	ctx := context.Background()
	mb, err := NewMutationBreaker(NewActuatingChokepoint(), breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if disabled, _ := mb.Trip(ctx, "deviation"); !disabled {
		t.Fatal("trip must open the breaker")
	}
	if !mb.Tripped(ctx) || mb.State(ctx) != breaker.StateOpen || mb.Deviations() == 0 {
		t.Fatalf("precondition: open+counted, got tripped=%v state=%v devs=%d", mb.Tripped(ctx), mb.State(ctx), mb.Deviations())
	}
	if err := mb.Rearm(ctx); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if mb.Tripped(ctx) || mb.State(ctx) != breaker.StateClosed || mb.Deviations() != 0 {
		t.Fatalf("after Rearm: tripped=%v state=%v devs=%d, want not-tripped / closed / 0", mb.Tripped(ctx), mb.State(ctx), mb.Deviations())
	}
}

func TestMutationBreakerRearmIsCrossProcess(t *testing.T) {
	ctx := context.Background()
	shared := breaker.NewMemStore() // the one durable row both "workers" read/write
	mb1, err := NewMutationBreaker(NewActuatingChokepoint(), shared, 1, nil)
	if err != nil {
		t.Fatalf("arm mb1: %v", err)
	}
	mb2, err := NewMutationBreaker(NewActuatingChokepoint(), shared, 1, nil)
	if err != nil {
		t.Fatalf("arm mb2: %v", err)
	}
	if _, err := mb1.Trip(ctx, "deviation on worker-1"); err != nil {
		t.Fatalf("trip: %v", err)
	}
	if !mb2.Tripped(ctx) {
		t.Fatal("precondition: the sibling must read the shared breaker OPEN after a trip")
	}
	if err := mb1.Rearm(ctx); err != nil { // worker-1 re-arms
		t.Fatalf("rearm: %v", err)
	}
	if mb2.Tripped(ctx) {
		t.Fatal("after a sibling's Rearm the shared breaker must read CLOSED for every worker")
	}
}

func TestMutationBreakerRearmNilSafe(t *testing.T) {
	var mb *MutationBreaker
	if err := mb.Rearm(context.Background()); err != nil {
		t.Fatalf("nil-breaker Rearm must be a no-op, got %v", err)
	}
}
