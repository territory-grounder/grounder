package eval

import (
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// TG-314: the OFFLINE plane now computes estate_grounded — the second deterministic axis — when the harness
// passes an estate snapshot, so the flywheel's pre-filter and the committed baseline see the same axis the
// live judge cron already scores ("one claim, one measurement, in both planes"). Without a graph the axis
// stays honestly N/A (unchanged). It rides the scorecard but stays OUT of Overall's fixed denominator and out
// of gate.Dimensions. Killing mutation: delete the estate block in Aggregate → the "with a graph" assertion
// goes RED; make the filter unconditional → the "no graph" assertion goes RED.
func TestAggregateComputesEstateGroundedOffline(t *testing.T) {
	g := estate.NewGraph()
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeVM, Name: "vm-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "node-1"},
		Rel: estate.RelRunsOn, Source: estate.SourcePVE, Confidence: 0.95,
	})
	// A session whose typed diagnosis names the alerting host's parent — the estate graph joins them, so it
	// grounds (adjacent, score 5).
	grounded := Session{Ref: "s1", Host: "vm-a", Proposed: true, DiagnosisRecorded: true,
		Diagnosis: proposal.Diagnosis{RootCause: "node-1"}}
	scores := []Score{{Ref: "s1", Scores: map[string]int{"correct_diagnosis": 4}}}

	// WITH a graph: the axis is computed.
	sc := Aggregate([]Session{grounded}, scores, g)
	mean, reported := sc.DimMeans[judge.DimEstateGrounded]
	if !reported {
		t.Fatal("estate_grounded was NOT computed in the offline plane despite a graph being passed (TG-314) — " +
			"the flywheel pre-filter and baseline still cannot see the axis")
	}
	if mean < 4 { // adjacent/self grounding scores 4-5
		t.Errorf("estate_grounded mean = %v, want a grounded score (>=4) for a diagnosis the graph joins to the host", mean)
	}
	if sc.DimSamples[judge.DimEstateGrounded] != 1 {
		t.Errorf("estate_grounded samples = %d, want 1", sc.DimSamples[judge.DimEstateGrounded])
	}
	// It must NOT be in the FIXED Overall denominator — widening it moves every historical Overall for a
	// reason unrelated to agent quality (exactly as diagnosis_grounded is kept out).
	for _, d := range Dimensions {
		if d == judge.DimEstateGrounded {
			t.Fatal("estate_grounded leaked into gate.Dimensions / Overall's fixed denominator")
		}
	}

	// WITHOUT a graph: the axis stays N/A, exactly as before — no accidental scoring on a nil snapshot.
	bare := Aggregate([]Session{grounded}, scores)
	if _, reported := bare.DimMeans[judge.DimEstateGrounded]; reported {
		t.Error("estate_grounded must stay N/A when no estate snapshot is passed (unchanged behaviour)")
	}
	// And a nil graph passed explicitly is the same as none.
	nilG := Aggregate([]Session{grounded}, scores, nil)
	if _, reported := nilG.DimMeans[judge.DimEstateGrounded]; reported {
		t.Error("a nil estate snapshot must leave estate_grounded N/A")
	}
}
