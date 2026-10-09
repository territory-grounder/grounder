package eval

import "github.com/territory-grounder/grounder/agent"

// TG-525 — the THIRD deterministic, report-only axis: trajectory_grounded. It grades a session's ORDERED tool
// path with the SAME deterministic veto that halts a stuck agent at runtime (agent.TrajectoryVeto: a loop of
// identical consecutive calls, or one call thrashing across the whole trajectory). INV-08: no model token is
// consulted — a property of the agent's OWN actions.
//
// FULLY ADDITIVE + REPORT-ONLY, on exactly the diagnosis_grounded / estate_grounded discipline (eval.go): it
// rides the scorecard's DimMeans/DimSamples, stays OUT of Overall's fixed denominator, and is NOT in
// gate.Dimensions — so it CANNOT gate a merge (an axis outside gate.Compare's set is reported, never a bar).
// Wiring the veto TO gate is a separate operator decision (TG-525 slice 3, ratify-to-gating); an LLM
// ordered-path judge with this veto HARD-OVERRIDING it is slice 2.
const dimTrajectoryGrounded = "trajectory_grounded"

// trajectoryScore grades one session's ordered tool path on the 1..5 axis scale. ok=false — N/A, omitted from
// the mean and never floored — when the session carries no trajectory (a pre-feature run, or a tools/rejudge
// DB-replayed capture), exactly as diagnosis_grounded reads N/A on a pre-TG-201 capture.
//
// THE DETERMINISTIC VETO OVERRIDES THE LLM JUDGE (TG-525 slice 2): a trajectory that trips agent.TrajectoryVeto
// (a loop of identical calls, or one call thrashing across the path) is forced to 1 (worst) REGARDLESS of the
// LLM ordered-path grade — a property of the agent's own actions outranks a model opinion (INV-08), so a
// lenient judge cannot rescue a demonstrably stuck path. A clean (non-vetoed) path takes the LLM ordered-path
// grade (Session.TrajectoryJudgeScore) when the harness computed one; without it — no gateway, a pre-feature /
// replayed capture — it defaults to 5, the slice-1 deterministic read (a non-thrashing path is presumed clean).
func trajectoryScore(s Session) (int, bool) {
	if len(s.Trajectory) == 0 {
		return 0, false
	}
	if veto, _ := agent.TrajectoryVeto(s.Trajectory); veto {
		return 1, true
	}
	if s.TrajectoryJudgeScore >= 1 && s.TrajectoryJudgeScore <= 5 {
		return s.TrajectoryJudgeScore, true
	}
	return 5, true
}
