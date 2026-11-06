package judge

import "fmt"

// DIAGNOSIS_GROUNDED — the judge dimension that puts a PRICE on the typed claim (TG-201 part 1).
//
// ★ WHY THIS EXISTS. TG-201 gave the agent a typed, source-bound diagnosis (core/proposal/diagnosis.go):
// root cause, mechanism, evidence FOR, evidence AGAINST, alternatives ruled out — every assertion bound by
// the ORCHESTRATOR against the ToolResults it actually captured. And then nothing graded it. A structured
// claim nobody scores is decoration: the agent could emit a diagnosis whose own cited evidence refutes its
// stated root cause, propose the action anyway, and pay exactly nothing for it — which is the recorded A2
// failure verbatim (the predecessor reads PVE task history, sees the guest was stopped DELIBERATELY, and
// stands down; TG holds the SAME observation and proposes a restart). The claim only becomes load-bearing
// on the day being wrong about it costs a score.
//
// ★ WHY IT IS SCORED IN GO AND NOT BY THE LLM JUDGE. The other five dimensions are opinions about a
// session and an LLM is the right instrument for them. This one is not an opinion: `Cited` was decided by
// the orchestrator against ids the model could not author (INV-11), so "did the agent's own grounded
// evidence contradict its stated cause" is a FACT the record already carries. Handing that fact to a model
// to re-judge would make a checkable property re-forgeable through the same channel it was screened out of
// — the judge reads the session's untrusted free text, and a model that can talk its way out of a
// contradiction has un-done the binding. Deterministic here also means the score is reproducible: the same
// record scores identically forever, at temperature 0 or otherwise.
//
// ★ WHY IT IS NOT IN judge.Dimensions. Dimensions is the LLM reply schema and the eval Overall's fixed
// denominator; adding a sixth axis there would (a) ask the judge model for a score we do not want it to
// author and (b) move every historical scorecard's Overall by ~0.6 through denominator change alone,
// making the committed baseline and the change-gate incomparable for a reason that has nothing to do with
// agent quality. It is declared in rubric.json's `deterministic_dimensions` instead — still ONE rubric
// source, still versioned, still stamped on every row it writes.

// DimDiagnosisGrounded is the deterministic dimension's name — the value written to
// session_judgment.dimension. Sourced from the embedded rubric (the one source), never re-declared, so the
// stored dimension name and the rubric's declaration cannot drift apart.
// (Looked up BY NAME, not by array index: TG-202 added a second deterministic axis, and an index-keyed var
// would silently re-point the durable rows' dimension name if the rubric's array were ever reordered.)
var DimDiagnosisGrounded = mustDeterministicDim("diagnosis_grounded")

// DiagnosisApplicable reports whether diagnosis_grounded is a meaningful axis for this session.
//
// Two exclusions, both load-bearing, and both in the SAME direction as PredictionApplicable (TG-61 seq C):
// a dimension that is not meaningful for a session must be OMITTED, never scored 1 — a floor imputed
// across a whole population is how the flywheel's Regressed trigger once fired for every skill at once.
//
//  1. NOT RECORDED. A session whose durable record predates the diagnosis column (migration 0056) carries
//     no claim because the field did not exist when it ran, not because the agent withheld one. Scoring
//     those would retroactively grade ~every historical session against a rule it was never offered —
//     the exact "no backward-compatible default" failure. NULL column ⇒ N/A, forever.
//  2. NO CLAIM TO GRADE. A session that supplied no diagnosis AND proposed no action asserted no cause;
//     a grounded stand-down ("the device is administratively DISABLED") is scored by correct_diagnosis and
//     evidence_grounded, and owes no formal root-cause artifact. Proposing IS the causal claim: a remedy
//     asserts what it remedies, so a proposal with no diagnosis is graded, and graded down.
func DiagnosisApplicable(s Session) bool {
	if !s.DiagnosisRecorded {
		return false
	}
	return s.Diagnosis.Present() || s.Proposed
}

// ScoreDiagnosis grades the session's typed diagnosis 1..5 and returns the one-line reason the score is
// written with. ok=false means the dimension is N/A for this session (see DiagnosisApplicable) — the caller
// must then write NO row at all, never a floored one.
//
// THE SCALE (deterministic, in severity order — the first matching rule wins):
//
//	1  the agent STATED a root cause and cites GROUNDED evidence AGAINST it. The A2 failure: it held the
//	   refutation and asserted the claim anyway. This is the worst score on the axis on purpose — it is
//	   strictly worse than saying nothing, because it demonstrates the disconfirming evidence was in hand.
//	2  a causal claim with nothing bound under it — a proposal that supplied no diagnosis at all, or a
//	   stated root cause with ZERO grounded assertions (an assertion whose support is empty is a guess with
//	   a citation field) — OR three or more assertions the orchestrator could not match to anything it
//	   captured. This band floors at 2 and never reaches 1: sloppy citation is a real defect and still a
//	   lesser one than demonstrably holding the refutation and asserting the claim anyway.
//	3  exactly two such unmatched assertions.
//	4  exactly one.
//	5  every assertion grounded and no contradiction of a stated cause.
//
// ★ HONEST UNCERTAINTY SCORES 5, AND THAT IS THE POINT. "I ruled out X and Y against these observations;
// the root cause is still unknown" names no cause, so it can neither contradict itself nor assert without
// support: it lands on the 5. A rubric that graded "root cause unknown" as a failure would pay the agent
// to invent a confident cause it cannot ground — it would train exactly the fabrication the typed claim
// was introduced to expose. Likewise a diagnosis that records disconfirming evidence WITHOUT committing to
// a cause is disclosure, not contradiction, and is not penalised for it (TestHonestUncertaintyScoresWell).
func ScoreDiagnosis(s Session) (int, string, bool) {
	if !DiagnosisApplicable(s) {
		return 0, "", false
	}
	d := s.Diagnosis
	switch {
	case !d.Present():
		return 2, "the session proposed an action — a claim about what caused the fault — and bound no diagnosis to it", true
	case d.AssertsRootCause() && d.HasContradiction():
		return 1, "the stated root cause is contradicted by the session's OWN grounded evidence, and it was asserted anyway", true
	case d.AssertsRootCause() && d.CitedAssertions() == 0:
		return 2, "a root cause was named with no assertion bound to any captured observation", true
	}
	score := 5 - d.UncitedAssertions()
	if score < 2 {
		score = 2
	}
	if u := d.UncitedAssertions(); u > 0 {
		return score, fmt.Sprintf("%d assertion(s) cite no observation the orchestrator captured", u), true
	}
	if !d.AssertsRootCause() {
		return score, "no cause claimed beyond what the evidence supports; every assertion is grounded (honest uncertainty)", true
	}
	return score, "every assertion in the diagnosis is bound to a captured observation and none contradicts the stated cause", true
}

// DiagnosisRule returns the rubric's written calibration for this dimension — the same one source the LLM
// dimensions take their guidance from, so an operator reading rubric.json sees every scored axis, not just
// the five a model grades.
func DiagnosisRule() string { return rubric.DiagnosisRule }
