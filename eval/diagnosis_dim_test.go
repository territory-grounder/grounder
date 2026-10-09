package eval

import (
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// TG-201 — THE OFFLINE PLANE MEASURES THE TYPED CLAIM TOO.
//
// The live judge cron writes diagnosis_grounded to session_judgment; if the offline scorecard did not
// report the same axis, every A/B run and every change-gate would be blind to the one signal TG-201 added
// — a change that taught the agent to bind its claims (or one that broke it) would show up nowhere in the
// numbers that decide a merge.
//
// KILLING MUTATION: delete the ScoreDiagnosis block from Aggregate. RED — the axis vanishes from the card
// and a corpus full of self-contradicted diagnoses scores identically to a corpus of clean ones.
func TestScorecardReportsTheDiagnosisAxis(t *testing.T) {
	contradicted := Session{
		Ref: "c1", Proposed: true, DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{
			RootCause:     "the guest crashed",
			Supporting:    []proposal.EvidenceRef{{ID: "lnms-1", Claim: "not running", Cited: true}},
			Contradicting: []proposal.EvidenceRef{{ID: "pve-101", Claim: "the stop was deliberate", Cited: true}},
		},
	}
	honest := Session{
		Ref: "h1", Proposed: false, DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{
			RuledOut: []proposal.RuledOut{{Cause: "disk full", Reason: "/ at 12%", ID: "disk-1", Cited: true}},
		},
	}
	sc := Aggregate([]Session{contradicted, honest}, []Score{
		{Ref: "c1", Scores: map[string]int{"correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "appropriate_band": 4, "falsifiable_prediction": 4}},
		{Ref: "h1", Scores: map[string]int{"correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "appropriate_band": 4}},
	})

	mean, reported := sc.DimMeans[judge.DimDiagnosisGrounded]
	if !reported {
		t.Fatal("the scorecard carries no diagnosis_grounded mean — the offline plane cannot see the typed " +
			"claim at all, so no A/B run could ever measure a change to it")
	}
	if sc.DimSamples[judge.DimDiagnosisGrounded] != 2 {
		t.Fatalf("diagnosis samples=%d, want 2 — a mean with the wrong denominator is not a measurement",
			sc.DimSamples[judge.DimDiagnosisGrounded])
	}
	// 1 (contradicted) + 5 (honest uncertainty, fully cited) = 3.0. Both halves matter: the floor must be
	// reachable, and honest uncertainty must be able to pull the mean UP.
	if mean != 3 {
		t.Fatalf("diagnosis_grounded mean = %v, want 3 (a self-contradicted claim at 1 and honest, fully-cited "+
			"uncertainty at 5) — if honest uncertainty scored low, the corpus mean would reward fabricated confidence", mean)
	}

	// THE DENOMINATOR MUST NOT MOVE. Overall is a fixed-denominator mean over the five LLM axes and every
	// committed card — the trend baseline included — was computed that way. A sixth axis entering it would
	// shift every historical Overall for a reason unrelated to agent quality (the 3.077-over-5 vs
	// 4.14-over-4 artifact this formula exists to end).
	if sc.OverallFormula != OverallFormulaV2 {
		t.Fatalf("overall formula changed to %q", sc.OverallFormula)
	}
	// Every LLM axis means 4 here, so Overall is 4 — NOT the 3.83 a six-axis denominator would produce by
	// dragging the deterministic 3.0 in. That difference is the whole assertion.
	if sc.Overall != 4 {
		t.Fatalf("overall=%v, want 4 — the five LLM axes all mean 4; a six-axis denominator would read ~3.83 "+
			"and silently re-base every committed card, which is a scoring change with no behaviour change", sc.Overall)
	}
}

// A session that predates the typed claim must be OMITTED from the axis, not floored into it — the same
// N/A discipline falsifiable_prediction follows, and for the same reason (TG-61).
func TestPreFeatureSessionsAreOmittedFromTheDiagnosisAxis(t *testing.T) {
	sc := Aggregate([]Session{{Ref: "old", Proposed: true}}, []Score{
		{Ref: "old", Scores: map[string]int{"correct_diagnosis": 5}},
	})
	if _, reported := sc.DimMeans[judge.DimDiagnosisGrounded]; reported {
		t.Fatal("a run of pre-TG-201 sessions published a diagnosis_grounded mean — an imputed floor over a " +
			"whole corpus reads as a capability collapse that never happened")
	}
	if sc.DimSamples[judge.DimDiagnosisGrounded] != 0 {
		t.Fatal("samples were counted for an axis that measured nothing")
	}
}
