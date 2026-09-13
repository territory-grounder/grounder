package main

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// THE ALIVENESS ORACLE for the widened ladder's wiring (spec/028 REQ-2807/2809, T-028-6).
//
// It drives ladderRungFor — the SAME function main() installs as Deps.LadderRungFor — against a REAL
// policy.Ladder, then pushes each resulting rung through the REAL runner predicates and the REAL classifier.
// Nothing here re-types the truth table: a test that carries its own copy proves the copy is right and says
// nothing about the binary, which is how a control ships unreachable.

// rungLadder returns a real ladder with opClass driven to the requested level by real Record calls, so the
// rung under test was EARNED through the state machine rather than assigned.
func rungLadder(t *testing.T, opClass string, want policy.Level) *policy.Ladder {
	t.Helper()
	const first = 2
	l := policy.NewLadder(first, policy.NewMemGraduationStore(), nil)
	ctx := context.Background()
	if want == policy.LevelApprove {
		return l
	}
	for i := 0; i < first; i++ {
		if _, err := l.Record(ctx, opClass, policy.OutcomeVerifiedClean); err != nil {
			t.Fatalf("climb: %v", err)
		}
	}
	if want == policy.LevelAuto {
		for i := 0; i < policy.DefaultNoticeThreshold; i++ {
			if _, err := l.Record(ctx, opClass, policy.OutcomeVerifiedClean); err != nil {
				t.Fatalf("climb: %v", err)
			}
		}
	}
	if got := l.LevelOf(ctx, opClass); got != want {
		t.Fatalf("setup: ladder at %v, want %v", got, want)
	}
	return l
}

// TestLadderRungWiringIsAliveEndToEnd is the reachability proof: each earned rung produces the band the rung
// promises, through the shipped construction.
//
// THREE RED CONTROLS EXECUTED, one per link in the chain — the truth table, the floor predicate, and the
// poll predicate — because a single control can only ever prove the earliest assertion it trips:
//
//  1. ladderRungFor maps LevelAutoNotice -> RungAuto
//     -> "ladderRungFor = 2, want 1"
//  2. runner.NoticeFloor forced false (rung resolves correctly, floor never applied)
//     -> "auto_notice: band = AUTO, want AUTO_NOTICE — the rung's guarantee did not survive the wiring"
//  3. runner.Ungraduated drops RungAutoNotice from its non-polling arm
//     -> "auto_notice: band = POLL_PAUSE, want AUTO_NOTICE — the rung's guarantee did not survive the wiring"
//
// Controls 2 and 3 fail in OPPOSITE directions from the same assertion, which is what makes the middle rung
// worth testing at all: it is the only band that can be missed by acting too freely AND by acting too little.
func TestLadderRungWiringIsAliveEndToEnd(t *testing.T) {
	const op = "restart-service" // embedded, so it can actually reach the silent rung

	for _, tc := range []struct {
		name      string
		level     policy.Level
		wantRung  runner.LadderRung
		wantBand  safety.Band
		wantNotif bool
	}{
		// A class that has not earned autonomy polls, so the `approve` verdict the policy engine composes is
		// actually askable.
		{"approve", policy.LevelApprove, runner.RungApprove, safety.BandPollPause, true},
		// The rung this stage exists for: it ACTS (no poll) and it PAGES (the floor).
		{"auto_notice", policy.LevelAutoNotice, runner.RungAutoNotice, safety.BandAutoNotice, true},
		// Fully earned: acts silently.
		{"auto", policy.LevelAuto, runner.RungAuto, safety.BandAuto, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := rungLadder(t, op, tc.level)

			// 1. The shipped truth table.
			if got := ladderRungFor(l, op); got != tc.wantRung {
				t.Fatalf("ladderRungFor = %v, want %v", got, tc.wantRung)
			}

			// 2. The real runner predicates, wired exactly as main() wires them.
			acts := &runner.Activities{D: runner.Deps{
				LadderRungFor: func(oc string) runner.LadderRung { return ladderRungFor(l, oc) },
			}}
			gi := risk.GatedInput{
				ActionID: "act-1", PlanHash: "plan-1", OpClass: op,
				Reversible: risk.Reversible, HasPrediction: true, Signals: map[string]string{},
			}
			gi.UngraduatedClass = acts.Ungraduated(op)
			if acts.NoticeFloor(op) {
				gi.BandFloor = safety.BandAutoNotice
				gi.BandFloorApplies = true
				gi.BandFloorReason = "ladder-auto-notice"
			}

			// 3. The real classifier.
			d := risk.Classify(gi)
			if d.Band != tc.wantBand {
				t.Fatalf("%s: band = %v, want %v — the rung's guarantee did not survive the wiring",
					tc.name, d.Band, tc.wantBand)
			}
			if d.NotifyRequired != tc.wantNotif {
				t.Errorf("%s: notify_required = %v, want %v", tc.name, d.NotifyRequired, tc.wantNotif)
			}
			// The two predicates must never disagree: acting without a vote and acting unobserved are
			// different permissions, and only RungAuto holds both.
			if !gi.UngraduatedClass && !acts.NoticeFloor(op) && tc.level != policy.LevelAuto {
				t.Errorf("%s: the class acts with NO poll and NO notice at a rung below auto — the two "+
					"predicates disagreed, which is the silent failure one resolver exists to prevent", tc.name)
			}
		})
	}
}

// TestNilLadderIsInert pins the unwired deployment's behaviour: no ladder means no policy engine composing
// `approve` either, so polling everything would be a behaviour change rather than a safety gain.
//
// RED CONTROL EXECUTED: made ladderRungFor return runner.RungApprove for a nil ladder ->
//
//	"an UNWIRED deployment polled every action — with no graduation store there is no approve verdict to
//	 make askable, so this is a behaviour change, not a safety gain"
func TestNilLadderIsInert(t *testing.T) {
	if got := ladderRungFor(nil, "restart-service"); got != runner.RungAuto {
		t.Fatalf("an UNWIRED deployment resolved to %v — with no graduation store there is no approve "+
			"verdict to make askable, so polling every action is a behaviour change, not a safety gain", got)
	}
	acts := &runner.Activities{D: runner.Deps{}} // resolver itself nil — the other unwired shape
	if acts.Ungraduated("restart-service") || acts.NoticeFloor("restart-service") {
		t.Error("a nil LadderRungFor must be inert in BOTH predicates")
	}
}
