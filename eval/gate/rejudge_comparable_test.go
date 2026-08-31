package gate

import (
	"strings"
	"testing"
)

// TG-359. A rubric edit could not satisfy its own eval gate.
//
// scripts/lint-eval-evidence.sh lists core/judge/rubric.json FIRST in its behavior set, so the edit needs
// on-box eval evidence. The change gate produces that evidence by measuring the candidate against a fresh
// origin/main arm, each in its own tree — so the candidate judges under the NEW rubric and the base under
// the OLD one, and VerifyComparable correctly refuses to pool them (TG-194). Observed 2026-08-06 gating
// TG-60: both arms returned INTEGRITY: OK, then
//
//	evalgate: arms not comparable: cards were judged under 2 different rubric versions
//	  [2026-08-04.3 2026-08-06.1]
//
// The resolution is not to relax TG-194 but to change what is measured — re-judge the SAME captured
// sessions under both rubrics, so triage nondeterminism is zero and the rubric is the only variable.

func cards(n int, versions ...string) []Scorecard {
	out := make([]Scorecard, 0, len(versions))
	for _, v := range versions {
		out = append(out, Scorecard{N: n, Judged: n, RubricVersion: v})
	}
	return out
}

func joined(probs []string) string { return strings.Join(probs, " | ") }

// The ordinary change gate must keep refusing a rubric difference — this is the behaviour TG-194 exists
// for and TG-359 must not weaken it.
//
// KILLING MUTATION: point VerifyComparable at the rejudge implementation. RED.
func TestTheOrdinaryChangeGateStillRefusesTwoRubrics(t *testing.T) {
	probs := VerifyComparable(cards(20, "2026-08-04.3"), cards(20, "2026-08-06.1"))
	if len(probs) == 0 {
		t.Fatal("the ordinary change gate accepted arms judged under two rubric versions — that is the " +
			"defect TG-194 was filed on: the delta could be entirely the rubric's")
	}
	if !strings.Contains(joined(probs), "rubric version") {
		t.Errorf("the refusal does not name the rubric versions, so a reader cannot act on it: %v", probs)
	}
}

// The re-judge comparison ACCEPTS exactly that pair — it is the point of the mode.
//
// KILLING MUTATION: delete VerifyComparableRejudge's inverted check and call VerifyComparable. RED —
// a rubric change becomes ungateable again.
func TestTheRejudgeComparisonAcceptsTwoDifferentRubrics(t *testing.T) {
	if probs := VerifyComparableRejudge(cards(20, "2026-08-04.3"), cards(20, "2026-08-06.1")); len(probs) > 0 {
		t.Fatalf("a re-judge of the same 20 sessions under the old and new rubric was refused: %v\n"+
			"That leaves a rubric edit unable to produce the evidence its own gate demands.", probs)
	}
}

// THE ANTI-VACUITY GUARD, and the reason this is an inversion rather than a removal. A re-judge that
// scored both arms under the SAME rubric compared nothing, and would pass by construction — a green that
// means the harness ran, not that the change is safe.
//
// KILLING MUTATION: drop the `baseV == candV` branch. RED.
func TestARejudgeOfOneRubricAgainstItselfIsRefused(t *testing.T) {
	probs := VerifyComparableRejudge(cards(20, "2026-08-04.3"), cards(20, "2026-08-04.3"))
	if len(probs) == 0 {
		t.Fatal("a re-judge comparing rubric 2026-08-04.3 against ITSELF was accepted — nothing was " +
			"compared, so a PASS would carry no information at all")
	}
	if !strings.Contains(joined(probs), "VACUOUS") {
		t.Errorf("the refusal does not say the comparison was vacuous, which is the one thing the operator "+
			"needs to know: %v", probs)
	}
}

// A single arm judged under two rubrics is incoherent in EVERY mode. The inversion applies BETWEEN arms,
// never within one.
//
// KILLING MUTATION: make singleRubricVersion return the first version it sees instead of reporting the
// mix. RED.
func TestAnInternallyMixedArmIsRefusedInRejudgeToo(t *testing.T) {
	mixed := cards(10, "2026-08-04.3", "2026-08-06.1") // one arm, two rubrics
	probs := VerifyComparableRejudge(mixed, cards(20, "2026-08-06.1"))
	if len(probs) == 0 {
		t.Fatal("an arm whose own cards carry two rubric versions was accepted — one arm must be one rubric")
	}
	if !strings.Contains(joined(probs), "internally mixed") {
		t.Errorf("the refusal does not identify WHICH arm is mixed: %v", probs)
	}
}

// The corpus-size check survives the inversion: a re-judge scores the same captured sessions, so a size
// difference means the arms were not given the same input file.
//
// KILLING MUTATION: drop the totalN comparison from VerifyComparableRejudge. RED.
func TestARejudgeOnDifferentSessionCountsIsRefused(t *testing.T) {
	probs := VerifyComparableRejudge(cards(12, "2026-08-04.3"), cards(20, "2026-08-06.1"))
	if len(probs) == 0 {
		t.Fatal("a re-judge comparing 12 sessions against 20 was accepted — the sessions must be held fixed " +
			"or the rubric is not the only variable")
	}
}

// An empty arm must be refused rather than silently satisfying the version check by having no versions.
//
// KILLING MUTATION: have singleRubricVersion return ("", "") for an empty arm. RED — the empty arm then
// differs from the populated one and the whole comparison passes on no data.
func TestAnEmptyArmIsRefused(t *testing.T) {
	// The obvious fixture — nil against 20 real cards — is NOT a test of this branch: the corpus-size
	// check (0 != 20) fires first and the assertion passes for the wrong reason. The mutation that
	// returned a fabricated version for an empty arm SURVIVED that version of this test.
	//
	// Both arms therefore pool to n=0 here, so the size check cannot fire and only the empty-arm branch
	// can produce a problem.
	probs := VerifyComparableRejudge(nil, cards(0, "2026-08-06.1"))
	if len(probs) == 0 {
		t.Fatal("a re-judge with NO base arm was accepted — no data is not a pass")
	}
	if !strings.Contains(joined(probs), "no scorecards") {
		t.Errorf("the refusal came from some other check, so the empty-arm branch is untested here: %v", probs)
	}
	probs = VerifyComparableRejudge(cards(0, "2026-08-04.3"), nil)
	if len(probs) == 0 || !strings.Contains(joined(probs), "no scorecards") {
		t.Errorf("a re-judge with NO candidate arm was not refused as empty: %v", probs)
	}
}

// The pre-versioning empty identity must read legibly, not as an empty string — an operator comparing
// `""` against `"2026-08-06.1"` cannot tell a missing stamp from a blank one.
func TestPreVersioningIdentityRendersLegibly(t *testing.T) {
	probs := VerifyComparableRejudge(cards(20, ""), cards(20, ""))
	if len(probs) == 0 {
		t.Fatal("two pre-versioning arms compared against each other is still vacuous")
	}
	if !strings.Contains(joined(probs), "(pre-versioning)") {
		t.Errorf("an empty rubric identity was rendered as an empty string: %v", probs)
	}
}
