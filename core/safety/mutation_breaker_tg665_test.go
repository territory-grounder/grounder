package safety

// TG-665 (G4A F7): a DEVIATION trip opens the durable breaker and LEAVES the owner-set mode standing; every
// other cause (and the legacy unattributed Trip) still forces Shadow. The one compensating inverse of the
// deviating action is admissible exactly once; a kill-switch that escalates the record closes that door.
//
// KILLING MUTATION (executed 2026-11-07): in TripFor, delete the `if cause == CauseDeviation { ...; return }`
// branch so a deviation forces Shadow like every other cause (the pre-TG-665 shape). RED:
// TestDeviationTripOpensTheBreakerButKeepsTheMode (the chokepoint reads read-only). Restored from a copy, green.

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/breaker"
)

type tripLog struct{ reasons []string }

func (l *tripLog) RecordTrip(reason string) { l.reasons = append(l.reasons, reason) }

func TestDeviationTripOpensTheBreakerButKeepsTheMode(t *testing.T) {
	ctx := context.Background()
	cp := NewActuatingChokepoint()
	log := &tripLog{}
	mb, err := NewMutationBreaker(cp, breaker.NewMemStore(), 1, log)
	if err != nil {
		t.Fatal(err)
	}
	tripped, err := mb.TripFor(ctx, "deviation verdict on act-1", "act-1", CauseDeviation)
	if err != nil || !tripped {
		t.Fatalf("a deviation at threshold 1 must trip, got tripped=%v err=%v", tripped, err)
	}
	if !mb.Tripped(ctx) {
		t.Fatal("the breaker must be OPEN (durably) after a deviation trip")
	}
	if !cp.MayActuate() {
		t.Fatal("TG-665: a DEVIATION trip must NOT force Shadow — the owner-set mode stays so the one compensation can pass the mode chokepoint")
	}
	rec, ok := mb.TripRecord(ctx)
	if !ok || rec.TrippedBy != "act-1" || rec.Cause != CauseDeviation {
		t.Fatalf("the record must carry the attribution, got %+v ok=%v", rec, ok)
	}
	if len(log.reasons) != 1 {
		t.Fatalf("the trip must be recorded once on the audit seam, got %v", log.reasons)
	}
	// The one compensation: admitted once, for the deviating action only.
	if ok, _ := mb.AdmitCompensation(ctx, "inv-1", "act-OTHER"); ok {
		t.Fatal("an inverse of another action must not be admitted")
	}
	if ok, err := mb.AdmitCompensation(ctx, "inv-1", "act-1"); err != nil || !ok {
		t.Fatalf("the inverse of the deviating action must be admitted once, got ok=%v err=%v", ok, err)
	}
	if ok, _ := mb.AdmitCompensation(ctx, "inv-2", "act-1"); ok {
		t.Fatal("a second compensation must be refused")
	}
	// Rearm clears the attribution and the compensation record.
	if err := mb.Rearm(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _ := mb.TripRecord(ctx); rec.TrippedBy != "" || rec.Cause != "" || rec.CompensationActionID != "" {
		t.Fatalf("Rearm must clear the attribution, got %+v", rec)
	}
}

func TestEveryOtherCauseStillForcesShadow(t *testing.T) {
	ctx := context.Background()
	for _, cause := range []string{CauseKillSwitch, CauseOperator, CauseCost, CauseChainGap, CauseRevertFailed, CauseUnknown, ""} {
		cp := NewActuatingChokepoint()
		mb, err := NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tripped, err := mb.TripFor(ctx, "trip", "act-1", cause); err != nil || !tripped {
			t.Fatalf("cause %q: must trip, got %v %v", cause, tripped, err)
		}
		if cp.MayActuate() {
			t.Fatalf("cause %q must force Shadow (compensation included)", cause)
		}
		if ok, _ := mb.AdmitCompensation(ctx, "inv", "act-1"); ok {
			t.Fatalf("cause %q must admit no compensation", cause)
		}
	}
	// The legacy unattributed Trip keeps its pre-TG-665 consequence.
	cp := NewActuatingChokepoint()
	mb, _ := NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if disabled, err := mb.Trip(ctx, "legacy"); err != nil || !disabled || cp.MayActuate() {
		t.Fatalf("Trip (unattributed) must force Shadow, got disabled=%v err=%v mayActuate=%v", disabled, err, cp.MayActuate())
	}
}

// A kill-switch landing AFTER a deviation escalates the record and closes the compensation door, and forces
// Shadow — the operator's /halt always wins.
func TestKillSwitchAfterDeviationBlocksTheCompensation(t *testing.T) {
	ctx := context.Background()
	cp := NewActuatingChokepoint()
	mb, _ := NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if _, err := mb.TripFor(ctx, "deviation", "act-1", CauseDeviation); err != nil {
		t.Fatal(err)
	}
	if !cp.MayActuate() {
		t.Fatal("precondition: the deviation kept the mode")
	}
	if _, err := mb.TripFor(ctx, "/halt", "", CauseKillSwitch); err != nil {
		t.Fatal(err)
	}
	if cp.MayActuate() {
		t.Fatal("the kill-switch must force Shadow")
	}
	if rec, _ := mb.TripRecord(ctx); rec.Cause != CauseKillSwitch || rec.TrippedBy != "act-1" {
		t.Fatalf("the record must escalate to kill-switch and keep the tripping action, got %+v", rec)
	}
	if ok, _ := mb.AdmitCompensation(ctx, "inv-1", "act-1"); ok {
		t.Fatal("no compensation may be admitted once a kill-switch escalated the record")
	}
}
