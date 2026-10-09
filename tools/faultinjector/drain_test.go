package faultinjector

import (
	"testing"
	"time"
)

// REGRESSION — found by the FIRST live validation run of this engine, 2026-07-26.
//
// The engine stopped at target=2 while both faults were still inside their 5-minute hold window. It
// reconciled ONCE (nothing was due yet) and exited, leaving both ledger rows 'pending' forever. The estate
// itself was fine — the belt-and-braces in-guest timers fired correctly — but the LEDGER was wrong, and since
// busy-ness derives from the ledger (INVARIANT 1), both hosts would have been quarantined from every future
// campaign.
//
// Reconcile is correct in only returning what is DUE; the bug was the caller treating one pass as sufficient
// at shutdown. These tests pin the distinction so a future refactor cannot collapse it again.
func TestReconcile_DoesNotReturnObligationsStillInsideTheirHoldWindow(t *testing.T) {
	held := []Outstanding{
		{ID: 62, Host: "mealie01", Class: ClassDeviceDown, RestoreDueAt: now.Add(3 * time.Minute)},
		{ID: 63, Host: "linkwarden02", Class: ClassDiskFill, RestoreDueAt: now.Add(4 * time.Minute)},
	}
	if got := Reconcile(now, held); len(got) != 0 {
		t.Fatalf("Reconcile returned %d not-yet-due obligations; it must repair only what is DUE", len(got))
	}
	// ...and once the window passes, BOTH must come back — a shutdown that waits gets everything.
	later := now.Add(5 * time.Minute)
	if got := Reconcile(later, held); len(got) != 2 {
		t.Fatalf("after the hold window, want both obligations repairable, got %d — a drain that waits must be able to discharge them all", len(got))
	}
}

// The drain deadline must be generous enough to outlast a fault injected moments before the stop signal,
// otherwise shutdown abandons exactly the obligation most likely to be outstanding.
func TestDrainDeadline_OutlastsTheHoldWindow(t *testing.T) {
	for _, hold := range []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour} {
		e := &Engine{Limits: Limits{RestoreAfter: hold}}
		if d := e.DrainDeadline(); d <= hold {
			t.Fatalf("RestoreAfter=%s gives drain deadline %s — a fault injected just before shutdown would be abandoned", hold, d)
		}
	}
}

// A tiny hold window must not make the drain give up almost immediately.
func TestDrainDeadline_HasAFloor(t *testing.T) {
	e := &Engine{Limits: Limits{RestoreAfter: time.Second}}
	if d := e.DrainDeadline(); d < 10*time.Minute {
		t.Fatalf("drain deadline %s is below the floor — a short hold window must not shorten the safety margin", d)
	}
}
