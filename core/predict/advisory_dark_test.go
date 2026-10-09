package predict

// REQ-105's ANALYSIS-ONLY LANE IS DECLARED, NOT BUILT — and this pins the gap so it cannot be mistaken
// for a working posture (TG-249 item 6).
//
// REQ-105 is Approved law: "WHILE the prediction gate is in analysis-only mode ... the gate SHALL record
// the prediction and its shadow verdict for evaluation without blocking the approval, keeping the advisory
// lane fail-open."
//
// It is not implemented end to end. ApprovalPoll.Blocking is computed from the mode here and read by
// NOBODY — written at gate.go, copied into GateResult at temporal/runner/activities.go, and consulted
// nowhere in non-test code. So selecting analysis-only sets a field and changes no behaviour: the poll
// still blocks.
//
// The gap is DECLARED at the composition root (wiring.SeamPredictAdvisory) so an operator sees it in the
// boot report rather than discovering it by selecting a mode that does nothing. These tests hold both
// halves of that declaration honest: the flag must still be computed correctly (so the day someone wires a
// reader, it is right), and the enforce default must be the zero value (so a misconfiguration cannot
// silently land in a posture that does not work).

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// gatedFixture commits a real proposal through the gate, so these tests exercise the production path
// rather than a hand-built struct whose `gated` field could be set without the gate's constraint holding.
func gatedFixture(t *testing.T, mode Mode) GatedProposal {
	t.Helper()
	g := testGate(mode)
	gp, err := g.Commit(context.Background(), testProposal(), "plan-advisory", "dc1", safety.BandPollPause, true)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return gp
}

// The flag must still be COMPUTED correctly even though nothing reads it. If it were wrong as well as
// unread, wiring a reader later would silently invert the posture.
func TestBlockingStillTracksTheModeEvenThoughNothingReadsIt(t *testing.T) {
	gp := gatedFixture(t, ModeEnforce)

	enforce, err := BuildApprovalPoll(gp, ModeEnforce)
	if err != nil {
		t.Fatalf("BuildApprovalPoll(ModeEnforce): %v", err)
	}
	if !enforce.Blocking {
		t.Error("ModeEnforce produced a NON-blocking poll. Nothing reads this today, so the error would " +
			"be invisible until someone wired a reader — at which point the fail-closed default would " +
			"have become fail-open with no change to this line.")
	}

	advisory, err := BuildApprovalPoll(gp, ModeAnalysisOnly)
	if err != nil {
		t.Fatalf("BuildApprovalPoll(ModeAnalysisOnly): %v", err)
	}
	if advisory.Blocking {
		t.Error("ModeAnalysisOnly produced a BLOCKING poll — the flag no longer tracks the mode at all")
	}
}

// THE FAIL-CLOSED DEFAULT IS THE ZERO VALUE. A Mode left unset must be enforce, so a construction that
// forgets the field cannot land in the advisory posture — which is doubly important while that posture
// does not work.
func TestTheZeroModeIsEnforceNotAdvisory(t *testing.T) {
	var unset Mode
	if unset != ModeEnforce {
		t.Fatalf("the zero Mode is %v, want ModeEnforce. A PredictionGate built without naming the mode "+
			"would default to the advisory lane — which is fail-open AND unimplemented.", unset)
	}
	gp := gatedFixture(t, ModeEnforce)
	poll, err := BuildApprovalPoll(gp, unset)
	if err != nil {
		t.Fatalf("BuildApprovalPoll(zero): %v", err)
	}
	if !poll.Blocking {
		t.Error("a zero-value Mode produced a non-blocking poll")
	}
}

// Default-deny survives regardless of mode: an ungated proposal cannot produce a poll in EITHER posture.
// An advisory lane that also relaxed the gating constraint would be a much larger hole than the one
// REQ-105 describes.
func TestAdvisoryModeDoesNotRelaxDefaultDeny(t *testing.T) {
	for _, mode := range []Mode{ModeEnforce, ModeAnalysisOnly} {
		if _, err := BuildApprovalPoll(GatedProposal{}, mode); err == nil {
			t.Errorf("mode %v built a poll for an UNGATED proposal — the compile-time GatedProposal "+
				"constraint's runtime face (ErrNotGated) is the structural closure of H-02 and must hold "+
				"in every posture", mode)
		}
	}
}
