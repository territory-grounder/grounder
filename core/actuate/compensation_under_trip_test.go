package actuate

// TG-665 (G4A F7) with the REAL breaker in the chain: a forward DEVIATION opens the breaker durably and keeps the
// mode; gate 4e then admits exactly ONE compensating inverse of that action (ledger
// actuate:compensation-under-trip) through every remaining gate, refuses a second inverse and every forward,
// and a kill-switch trip blocks the compensation too.
//
// KILLING MUTATION (executed 2026-11-07): at gate 4e drop the `r.InvertsActionID != ""` guard (admit any request
// through AdmitCompensation with actionID as the inverse). RED: TestForwardsStayRefusedUnderADeviationTrip
// (a FORWARD is admitted through the open breaker). Restored from a copy, green.

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/safety"
)

// tg665Chain is an actuating interceptor armed with a REAL mutation breaker over a shared MemStore (threshold 1).
func tg665Chain(t *testing.T) (*Interceptor, *safety.Chokepoint, *safety.MutationBreaker, *fakeActuator, *ledgerCapture) {
	t.Helper()
	cp := safety.NewActuatingChokepoint()
	mb, err := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	act := &fakeActuator{}
	cap := &ledgerCapture{}
	i := actuatingInterceptor(cp, act, audit.NewLedger().WithSink(cap)).WithMutationBreaker(mb)
	return i, cp, mb, act, cap
}

// deviateThenInverse runs the forward deviation and returns the inverse request bound to it.
func deviateThenInverse(t *testing.T, i *Interceptor, cp *safety.Chokepoint, mb *safety.MutationBreaker, act *fakeActuator) Request {
	t.Helper()
	ctx := context.Background()
	fwd := deviationRequest(t)
	out, err := i.Do(ctx, fwd)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !out.Executed || out.Verdict != safety.VerdictDeviation || act.execs != 1 {
		t.Fatalf("precondition: the forward must execute and DEVIATE, got %+v execs=%d", out, act.execs)
	}
	if !mb.Tripped(ctx) {
		t.Fatal("precondition: the deviation must open the breaker")
	}
	if !cp.MayActuate() {
		t.Fatal("TG-665: the deviation trip must leave the owner-set mode standing")
	}
	inv := goodRequest(t)
	inv.InvertsActionID = fwd.Manifest.ActionID
	return inv
}

func TestDeviationAdmitsExactlyOneCompensation(t *testing.T) {
	ctx := context.Background()
	i, cp, mb, act, cap := tg665Chain(t)
	inv := deviateThenInverse(t, i, cp, mb, act)

	out, err := i.Do(ctx, inv)
	if err != nil {
		t.Fatalf("inverse: %v", err)
	}
	if !out.Executed || act.execs != 2 {
		t.Fatalf("the ONE compensating inverse must execute through the open breaker, got %+v execs=%d", out, act.execs)
	}
	if !cap.has("actuate:compensation-under-trip") {
		t.Fatalf("the admission must be ledgered as compensation-under-trip, got %v", cap.decisions)
	}
	if !cp.MayActuate() {
		t.Fatal("the mode must still be owner-set after the compensation")
	}
	if rec, _ := mb.TripRecord(ctx); rec.CompensationActionID != inv.Manifest.ActionID {
		t.Fatalf("the admitted compensation must be recorded on the breaker row, got %+v", rec)
	}
	// A SECOND inverse of the same action is refused AT THE BREAKER — the compensation is spent. A fresh request
	// object, so no other gate (manifest lifecycle, limiter) can be the one refusing; the reason must be 4e's.
	second := goodRequest(t)
	second.InvertsActionID = inv.InvertsActionID
	out, err = i.Do(ctx, second)
	if err != nil {
		t.Fatalf("second inverse: %v", err)
	}
	if out.Executed || !out.Refused || act.execs != 2 || !strings.Contains(out.Reason, "only the one compensating inverse") {
		t.Fatalf("a second compensation must be refused AT THE BREAKER (spent), got %+v execs=%d", out, act.execs)
	}
}

func TestForwardsStayRefusedUnderADeviationTrip(t *testing.T) {
	ctx := context.Background()
	i, cp, mb, act, cap := tg665Chain(t)
	_ = deviateThenInverse(t, i, cp, mb, act)
	fwd := goodRequest(t)
	out, err := i.Do(ctx, fwd)
	if err != nil {
		t.Fatalf("forward under trip: %v", err)
	}
	if out.Executed || !out.Refused || act.execs != 1 {
		t.Fatalf("every FORWARD must be refused while the breaker is open on a deviation, got %+v execs=%d", out, act.execs)
	}
	if cap.has("actuate:compensation-under-trip") {
		t.Fatal("a forward must never be admitted as a compensation")
	}
	if !cp.MayActuate() {
		t.Fatal("refusing a forward under a deviation trip must not force Shadow (the mode row stays owner-set)")
	}
}

func TestKillSwitchBlocksTheCompensation(t *testing.T) {
	ctx := context.Background()
	i, cp, mb, act, cap := tg665Chain(t)
	inv := deviateThenInverse(t, i, cp, mb, act)
	// The operator's /halt lands before the window fires its inverse.
	if _, err := mb.TripFor(ctx, "/halt", "", safety.CauseKillSwitch); err != nil {
		t.Fatal(err)
	}
	if cp.MayActuate() {
		t.Fatal("precondition: the kill-switch forces Shadow")
	}
	out, err := i.Do(ctx, inv)
	if err != nil {
		t.Fatalf("inverse under kill-switch: %v", err)
	}
	if out.Executed || !out.Refused || act.execs != 1 || cap.has("actuate:compensation-under-trip") {
		t.Fatalf("a kill-switch must block the compensation too, got %+v execs=%d ledger=%v", out, act.execs, cap.decisions)
	}
}

// An inverse of a DIFFERENT action than the one that tripped is a forward in disguise: refused.
func TestAnInverseOfAnotherActionIsNotACompensation(t *testing.T) {
	ctx := context.Background()
	i, cp, mb, act, _ := tg665Chain(t)
	inv := deviateThenInverse(t, i, cp, mb, act)
	inv.InvertsActionID = "some-other-forward"
	out, err := i.Do(ctx, inv)
	if err != nil {
		t.Fatalf("stranger inverse: %v", err)
	}
	if out.Executed || !out.Refused || act.execs != 1 {
		t.Fatalf("only the inverse of the DEVIATING action is admissible, got %+v execs=%d", out, act.execs)
	}
}
