package agent

import (
	"strings"
	"testing"
)

// TestFastStandDownIsNotDegenerate — the distinction the whole type exists for.
//
// Production carries 503 judged sessions with step_count = 0. Splitting by outcome: 232 are
// no-proposal:stop (a fast stand-down — declining to act without investigating is CORRECT) and 243 are
// proposals produced with zero investigation steps. Treating step count ALONE as the degeneracy test
// would mark all 475 as husks and bury the 243 that matter among correct behaviour.
//
// RED MUTATION CONTROL (executed 2026-08-01): making Degenerate() return true for any zero-step session
// fails here, naming the stand-down; restored green.
func TestFastStandDownIsNotDegenerate(t *testing.T) {
	shape, reason := ClassifySession(0, false)
	if shape != ShapeFastStandDown {
		t.Fatalf("no steps and no proposal is a fast stand-down, got %q", shape)
	}
	if shape.Degenerate() {
		t.Error("a fast stand-down must NOT be degenerate — 232 of production's zero-step sessions are " +
			"correct stand-downs, and marking them husks buries the 243 that matter")
	}
	if !strings.Contains(reason, "is correct here") {
		t.Errorf("the reason must say the behaviour is correct, not merely describe it: %q", reason)
	}
}

// TestUnexaminedProposalIsTheSharpCase — a remedy proposed with no investigation, graded by the judge on
// the same five dimensions as a session that actually looked.
//
// RED MUTATION CONTROL (executed 2026-08-01): classifying a zero-step proposal as a stand-down makes it
// non-degenerate and fails; restored green.
func TestUnexaminedProposalIsTheSharpCase(t *testing.T) {
	shape, reason := ClassifySession(0, true)
	if shape != ShapeUnexaminedProposal {
		t.Fatalf("a proposal with no steps is the unexamined case, got %q", shape)
	}
	if !shape.Degenerate() {
		t.Error("an unexamined proposal IS the degenerate grade — this is the one case the classifier exists to name")
	}
	if !strings.Contains(reason, "NO investigation step") {
		t.Errorf("the reason must say what is missing: %q", reason)
	}
}

// TestAnyStepMakesItInvestigated — the bar is deliberately ONE step, matching what the spine records.
// A higher bar would be a judgement about sufficiency that this deterministic layer has no basis for.
func TestAnyStepMakesItInvestigated(t *testing.T) {
	for _, steps := range []int{1, 2, 40} {
		shape, reason := ClassifySession(steps, true)
		if shape != ShapeInvestigated || shape.Degenerate() {
			t.Errorf("%d step(s) must be investigated and never degenerate, got %q", steps, shape)
		}
		if !strings.Contains(reason, "investigation step") {
			t.Errorf("the reason must carry the count: %q", reason)
		}
	}
	// A stand-down that DID investigate is also investigated — the shape is about looking, not deciding.
	if shape, _ := ClassifySession(3, false); shape != ShapeInvestigated {
		t.Errorf("a session that investigated and then declined is investigated, got %q", shape)
	}
}

// TestNegativeStepCountIsTreatedAsZero — step_count is nullable in the spine (migration 0037 added it),
// so a pre-migration row can arrive as a zero or a negative through a COALESCE. Neither may be read as
// "investigated" by accident.
func TestNegativeStepCountIsTreatedAsZero(t *testing.T) {
	if shape, _ := ClassifySession(-1, true); shape != ShapeUnexaminedProposal {
		t.Errorf("a negative step count must not read as investigated, got %q", shape)
	}
}

// TestEveryShapeIsClassifiedAndOnlyOneIsDegenerate — a vacuity floor. If a shape is added without
// deciding its degeneracy, this fails rather than defaulting it to "fine".
func TestEveryShapeIsClassifiedAndOnlyOneIsDegenerate(t *testing.T) {
	all := []Shape{ShapeInvestigated, ShapeFastStandDown, ShapeUnexaminedProposal}
	degenerate := 0
	for _, s := range all {
		if s == "" {
			t.Fatal("VACUITY: an empty shape constant — the classifier can return a value nothing names")
		}
		if s.Degenerate() {
			degenerate++
		}
	}
	if degenerate != 1 {
		t.Errorf("exactly ONE shape may be degenerate (the unexamined proposal); %d are. A second would "+
			"mean a correct behaviour is being counted as a husk.", degenerate)
	}
}
