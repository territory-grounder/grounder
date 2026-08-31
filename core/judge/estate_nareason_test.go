package judge

// WHY A PERMANENTLY-N/A AXIS NEEDS A REASON.
//
// Measured 2026-08-05: across 3,233 judged sessions, estate_grounded had written ZERO rows and
// diagnosis_grounded ONE, while the four model-scored dimensions each had 3,233. The axis was wired at the
// composition root and the scorer ran on every session — it was correctly declining, and nothing recorded
// which of its four gates did the declining.
//
// That matters because TG-307 and TG-314 both wait on "once the axis has accrued samples". Without a
// reason, "not yet" and "never, in this deployment" are the same observation.

import "testing"

func TestNAReasonNamesTheEarliestGateNotALaterSymptom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts EstateFacts
		want  string
	}{
		{"no graph handed to the judge", EstateFacts{Relation: RelationUnknown}, NAWired},
		// An empty graph ALSO cannot place a symptom. Reporting symptom-unplaced here would send someone
		// to look at hostname resolution when the estate simply never seeded.
		{"empty snapshot", EstateFacts{Consulted: true, GraphEdges: 0, Relation: RelationUnknown}, NAEmptyGraph},
		{"symptom unplaceable", EstateFacts{Consulted: true, GraphEdges: 42, Relation: RelationUnknown}, NAUnplaced},
		{"placed but nothing related", EstateFacts{Consulted: true, GraphEdges: 42, Symptom: "hostA", Relation: RelationUnknown}, NAUnrelated},
		{"applicable", EstateFacts{Consulted: true, GraphEdges: 42, Symptom: "hostA", Relation: RelationAdjacent}, NAApplicable},
	} {
		if got := tc.facts.NAReason(); got != tc.want {
			t.Errorf("%s: NAReason() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The reason must agree with the applicability gate: anything EstateApplicable accepts must report
// NAApplicable, and anything it rejects must report a non-empty reason. A reason that disagrees with the
// gate it explains is worse than none — it sends the reader to the wrong place with confidence.
func TestNAReasonAgreesWithEstateApplicable(t *testing.T) {
	cases := []EstateFacts{
		{Relation: RelationUnknown},
		{Consulted: true, GraphEdges: 0, Relation: RelationUnknown},
		{Consulted: true, GraphEdges: 9, Relation: RelationUnknown},
		{Consulted: true, GraphEdges: 9, Symptom: "h", Relation: RelationUnknown},
		{Consulted: true, GraphEdges: 9, Symptom: "h", Relation: RelationAdjacent},
		{Consulted: true, GraphEdges: 9, Symptom: "h", Relation: RelationSibling},
	}
	for _, f := range cases {
		applicable := EstateApplicable(Session{Estate: f})
		reason := f.NAReason()
		if applicable && reason != NAApplicable {
			t.Errorf("EstateApplicable says yes but NAReason says %q for %+v", reason, f)
		}
		if !applicable && reason == NAApplicable {
			t.Errorf("EstateApplicable says no but NAReason reports applicable for %+v — a run would then "+
				"write no row AND report no reason, which is the state this exists to end", f)
		}
	}
}
