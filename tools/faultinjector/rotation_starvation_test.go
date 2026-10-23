package faultinjector

import (
	"context"
	"testing"
	"time"
)

// A CLASS THAT CANNOT ACT MUST NOT STARVE THE ROTATION.
//
// ★ THIS IS A LIVE INCIDENT, NOT A HYPOTHETICAL. Between 02:19Z and 09:34Z on 2026-07-29 the estate campaign
// selected `log-fill` on 148 consecutive cycles and injected NOTHING. Six classes were configured; one ran.
// Alert volume reaching TG fell from ~25/hour to ~1/hour, and every benchmark axis kept being computed over
// that window as though the campaign were healthy.
//
// The mechanism was a single conflated counter. `plan.go` chose the class with `rotation[Injected % len]`,
// and `engine.go` advanced `injected` only when InjectOnce returned TRUE. A class that could not act left the
// cursor exactly where it was, so the next cycle chose the same class, forever. Nothing about it is specific
// to log-fill — it is a property of the cursor, and ANY class that stops being satisfiable acquires the
// campaign permanently.
//
// The whole faultinjector suite was GREEN throughout, because every oracle called PlanNext once. A planner
// bug that only exists ACROSS cycles cannot be found by a test that never takes a second cycle.
func TestAClassThatCannotActDoesNotStarveTheRotation(t *testing.T) {
	rotation := []Class{ClassDeviceDown, ClassDiskFill, ClassContainerDown, ClassServiceDown, ClassLogFill}

	// The live pool shape: guests that can satisfy every class EXCEPT log-fill, which needs a declared path.
	pool := []PoolGuest{
		{VMID: "101", Name: "guest-a", Node: "node1", Container: "app", Unit: "nginx"},
		{VMID: "102", Name: "guest-b", Node: "node1", Container: "app", Unit: "nginx"},
	}
	st := State{
		Now:       time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
		Pool:      pool,
		Allowlist: map[string]bool{"guest-a": true, "guest-b": true},
		Status:    map[string]string{"101": "running", "102": "running"},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5},
	}

	// Drive the campaign the way the engine drives it: the cursor advances every CYCLE, and `Injected` only
	// when a fault actually landed. log-fill can never land here, so the two diverge — which is the point.
	const cycles = 40
	picks := make([]Class, 0, cycles)
	injected := 0
	for cycle := range cycles {
		st.Cycle = cycle
		st.Injected = injected
		d := PlanNext(st, rotation)
		if !d.Act {
			picks = append(picks, "")
			continue
		}
		picks = append(picks, d.Class)
		if d.Class != ClassLogFill {
			injected++
		}
	}

	// ★ ASSERT ON THE SECOND HALF, NOT THE WHOLE RUN. THE FIRST VERSION OF THIS ORACLE ENCODED THE BUG.
	//
	// It asked "was every satisfiable class selected at least once, and was log-fill not selected on ALL 40
	// cycles" — and the buggy code satisfies both. With the cursor on `Injected`, the campaign rotates
	// perfectly until it first reaches the unsatisfiable class and then freezes there forever: cycles 0-3
	// select the four healthy classes, cycle 4 selects log-fill, and cycles 4-39 are all log-fill. Every
	// class HAS been seen, log-fill is 36/40 rather than 40/40, and the oracle passes.
	//
	// That is precisely the live incident (the campaign was healthy for hours, then froze), so an oracle that
	// green-lights it is worse than none. Mutation AR was GREEN against the first version and RED against
	// this one — the freeze shows up only when you ask whether the campaign is still rotating LATER.
	late := map[Class]int{}
	for _, c := range picks[cycles/2:] {
		if c != "" {
			late[c]++
		}
	}
	if len(late) < 2 {
		t.Fatalf("in the second half of the campaign only %d class(es) were selected (%v) — the rotation "+
			"cursor froze on the class that cannot act, which is the 2026-07-29 incident exactly", len(late), late)
	}
	for _, c := range []Class{ClassDeviceDown, ClassDiskFill, ClassContainerDown, ClassServiceDown} {
		if late[c] == 0 {
			t.Errorf("class %q got no cycles in the SECOND HALF of a 40-cycle campaign (late mix: %v) — a "+
				"configured, satisfiable class starved out contributes nothing to A1/A3/A5, and nothing says so",
				c, late)
		}
	}
}

// THE CURSOR IS NOT THE CAMPAIGN TARGET, AND CONFLATING THEM IS THE BUG. The target must still count only
// faults that LANDED — a campaign told to inject 10 faults must inject ten, not stop after ten barren ticks.
func TestTheCampaignTargetCountsLandedFaultsNotCycles(t *testing.T) {
	rotation := []Class{ClassDeviceDown}
	st := State{
		Now:       time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
		Pool:      []PoolGuest{{VMID: "101", Name: "guest-a", Node: "node1"}},
		Allowlist: map[string]bool{"guest-a": true},
		Status:    map[string]string{"101": "running"},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, Target: 3},
		Cycle:     500, // hundreds of barren cycles have passed
		Injected:  2,   // but only two faults ever landed
	}
	if d := PlanNext(st, rotation); !d.Act {
		t.Fatalf("a campaign with target 3 and 2 landed faults refused to inject the third: %s", d.Reason)
	}
	st.Injected = 3
	if d := PlanNext(st, rotation); d.Act {
		t.Error("the campaign injected past its target — Target must be measured against LANDED faults, and " +
			"the fix for the rotation cursor must not have moved it onto the cycle count")
	}
}

// The pool sweep must move too. Pinning the starting offset to landed faults meant a barren class also
// hammered the same guest ordering every cycle — visible in the incident log as the same four hosts in the
// same order for seven hours.
func TestThePoolSweepAdvancesOnBarrenCycles(t *testing.T) {
	rotation := []Class{ClassDeviceDown}
	pool := []PoolGuest{
		{VMID: "101", Name: "guest-a", Node: "node1"},
		{VMID: "102", Name: "guest-b", Node: "node1"},
		{VMID: "103", Name: "guest-c", Node: "node1"},
	}
	st := State{
		Now:       time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
		Pool:      pool,
		Allowlist: map[string]bool{"guest-a": true, "guest-b": true, "guest-c": true},
		Status:    map[string]string{"101": "running", "102": "running", "103": "running"},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5},
		Injected:  0, // nothing has ever landed
	}
	first := map[string]bool{}
	for cycle := range len(pool) {
		st.Cycle = cycle
		d := PlanNext(st, rotation)
		if !d.Act {
			t.Fatalf("cycle %d refused: %s", cycle, d.Reason)
		}
		first[d.Guest.Name] = true
	}
	if len(first) != len(pool) {
		t.Errorf("across %d barren cycles the planner offered %d distinct guests, want %d — the sweep offset "+
			"is still tied to landed faults, so one guest absorbs the whole campaign", len(pool), len(first), len(pool))
	}
}

// THE ENGINE HALF. The two oracles above drive the cursor themselves, so they can prove the PLANNER honours a
// cycle counter and nothing at all about whether the loop advances one. Mutation AU — move `cycle++` inside
// the landed branch, which is exactly the shape of the original defect — left them GREEN.
//
// This runs the real Run loop over a rotation whose first class can never act, and asserts the campaign gets
// past it.
func TestTheRunLoopAdvancesItsCursorOnABarrenCycle(t *testing.T) {
	st := newMemStore()
	e := &Engine{
		Exec: &scriptedRunner{verifyRunning: true}, Store: st, SnapNode: "node-a",
		// service-down cannot act: no guest declares a Unit. device-down can. If the cursor only moved on a
		// landed fault, the campaign would sit on service-down forever and record nothing.
		Rotation:  []Class{ClassServiceDown, ClassDeviceDown},
		Pool:      []PoolGuest{{VMID: "100", Name: "guest-a", Node: "node-a"}},
		Allowlist: map[string]bool{"guest-a": true},
		Limits:    Limits{MaxDown: 3, MaxBusy: 5, RestoreAfter: time.Hour, Target: 1},
		Cadence:   time.Millisecond,
		Log:       func(string, ...any) {},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the campaign never reached its target of ONE fault in 5s over a two-class rotation whose " +
			"first class can never act — the run loop's cursor is tied to landed faults, so an unsatisfiable " +
			"class owns the campaign permanently (the 2026-07-29 incident, engine side)")
	}
	if !st.recorded {
		t.Error("no obligation was ever recorded — the reachable class never got a cycle")
	}
}
