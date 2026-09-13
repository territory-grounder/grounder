package opclasscat

// WHY a candidate is held, not merely THAT it is (TG-348).
//
// Measured 2026-08-06 on the deployed estate: all 8 op-class candidates sat at `observing`, and nothing
// distinguished "one distinct-ref short of the bar" from "the blast-radius provider is not wired at all".
// Only the second is a deployment defect; only the first is fixed by waiting. Same row, same status.
//
// The gate itself is unchanged and correctly fail-closed. These tests hold the EXPLANATION honest: it must
// agree with the gate exactly, it must name every failing leg rather than the first, and it must call out
// zero coverage separately because that is the signature of an unwired provider.

import (
	"strings"
	"testing"
)

func readyEvidence() Evidence { return Evidence{DistinctRefs: MinRefsForRatifyReady} }
func readyInput() ReadyInput {
	return ReadyInput{Family: "service", Tier: "t1", AutoBarredStamped: true, BlastRadiusCoverage: 1.0}
}

// THE EXPLANATION MUST AGREE WITH THE GATE. Two functions answering the same question that can disagree is
// worse than one opaque answer: whichever an operator trusts, they are sometimes wrong.
func TestGapsAgreeWithTheGateOnEveryLeg(t *testing.T) {
	cases := map[string]func(*Evidence, *ReadyInput){
		"complete":           func(e *Evidence, in *ReadyInput) {},
		"too few refs":       func(e *Evidence, in *ReadyInput) { e.DistinctRefs = MinRefsForRatifyReady - 1 },
		"no family":          func(e *Evidence, in *ReadyInput) { in.Family = "" },
		"no tier":            func(e *Evidence, in *ReadyInput) { in.Tier = "" },
		"screen not stamped": func(e *Evidence, in *ReadyInput) { in.AutoBarredStamped = false },
		"partial coverage":   func(e *Evidence, in *ReadyInput) { in.BlastRadiusCoverage = MinBlastRadiusCoverage / 2 },
		"zero coverage":      func(e *Evidence, in *ReadyInput) { in.BlastRadiusCoverage = 0 },
		"dismissal active":   func(e *Evidence, in *ReadyInput) { in.DismissActive = true },
	}
	var sawReady, sawHeld bool
	for name, mutate := range cases {
		e, in := readyEvidence(), readyInput()
		mutate(&e, &in)
		ok := MeetsRatifyReady(e, in)
		gaps := RatifyReadyGaps(e, in)
		if ok != (len(gaps) == 0) {
			t.Errorf("%s: MeetsRatifyReady=%v but RatifyReadyGaps returned %d gap(s) %v.\n"+
				"The gate and its explanation must never disagree — an operator trusting either one would "+
				"then be wrong some of the time, which is worse than the single opaque bit this replaces.",
				name, ok, len(gaps), gaps)
		}
		if ok {
			sawReady = true
		} else {
			sawHeld = true
		}
	}
	// VACUITY FLOOR: both outcomes must occur, or the agreement above is asserted over one branch.
	if !sawReady || !sawHeld {
		t.Fatalf("the case table produced ready=%v held=%v — it must exercise both, or 'they agree' is "+
			"a statement about one branch", sawReady, sawHeld)
	}
}

// EVERY failing leg, not the first. A candidate short on three counts that reports one gap sends an
// operator to fix it three times.
func TestEveryFailingLegIsNamed(t *testing.T) {
	e := Evidence{DistinctRefs: 0}
	in := ReadyInput{} // no family, no tier, screen unstamped, zero coverage
	gaps := RatifyReadyGaps(e, in)
	if len(gaps) < 5 {
		t.Fatalf("a candidate failing every leg reported %d gap(s): %v.\nReporting the first failure only "+
			"means an operator fixes one thing, waits a full cron cycle, and learns about the next.",
			len(gaps), gaps)
	}
	joined := strings.Join(gaps, " | ")
	for _, want := range []string{"distinct_refs", "family", "tier", "auto_barred", "blast_radius"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no gap mentions %q: %v", want, joined)
		}
	}
}

// ZERO coverage is called out separately from PARTIAL coverage. Zero is the signature of an unwired
// blast-radius provider — a deployment defect that never resolves on its own. Partial means the walk ran
// and did not reach every target, which is patience. Collapsing them hides the one that needs a human.
func TestZeroCoverageIsDistinguishedFromPartialCoverage(t *testing.T) {
	e := readyEvidence()

	zero := readyInput()
	zero.BlastRadiusCoverage = 0
	zeroGaps := strings.Join(RatifyReadyGaps(e, zero), " ")

	partial := readyInput()
	partial.BlastRadiusCoverage = MinBlastRadiusCoverage / 2
	partialGaps := strings.Join(RatifyReadyGaps(e, partial), " ")

	if zeroGaps == partialGaps {
		t.Fatalf("zero and partial blast-radius coverage produce the identical message %q. Zero means the "+
			"provider is not wired (a deployment defect); partial means the walk ran and fell short "+
			"(patience). One needs a human today and the other does not.", zeroGaps)
	}
	if !strings.Contains(zeroGaps, "no provider wired") {
		t.Errorf("the zero-coverage gap does not name the likely cause: %q", zeroGaps)
	}
}

// A complete dossier produces NO gaps — otherwise the explanation would hold back a candidate the gate
// admits, and the queue would look permanently blocked.
func TestACompleteDossierReportsNoGaps(t *testing.T) {
	if gaps := RatifyReadyGaps(readyEvidence(), readyInput()); len(gaps) != 0 {
		t.Fatalf("a complete dossier reported gaps %v — the explanation is stricter than the gate", gaps)
	}
	if !MeetsRatifyReady(readyEvidence(), readyInput()) {
		t.Fatal("the fixture itself does not satisfy the gate, so every assertion above is vacuous")
	}
}
