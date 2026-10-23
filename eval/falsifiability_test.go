package eval

import (
	"testing"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestFalsifiabilityFixture is the non-degeneracy proof for TG's falsifiability differentiator: over the
// REAL 359-node estate graph, every hand-authored placement-grounded scenario's prediction must catch real
// observed cascades (RealTP>0) AND beat its degree-preserving shuffled control (Falsifiable, ControlTP <
// RealTP), yielding an aggregate SignalRatio > 1. Before this, NOTHING produced these numbers anywhere —
// the grounding API summed columns no writer filled. Pure + deterministic (seeded shuffle): runs in `make all`.
func TestFalsifiabilityFixture(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	scenarios, err := LoadFalsifiability("falsifiability_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	results, agg := ScoreFalsifiability(g, scenarios)
	var expected float64
	for i, r := range results {
		s := scenarios[i]
		t.Logf("%s: real_tp=%d real_fp=%d control_tp=%d control_fp=%d ratio=%.2f falsifiable=%v (strict=%v)",
			r.Ref, r.RealTP, r.RealFP, r.ControlTP, r.ControlFP, r.Ratio, r.Falsifiable, s.ExpectFalsifiable)
		if r.RealTP == 0 {
			t.Errorf("%s: the real prediction caught NO observed cascade (RealTP=0) — the graph is not predicting", r.Ref)
		}
		if !s.ExpectFalsifiable {
			// A documented sparse-tier limitation (see the fixture comment): the prediction must still
			// catch the cascade, but the 11-edge depends_on shuffle is too underpowered to be a fair null.
			continue
		}
		expected++
		if !r.Falsifiable {
			t.Errorf("%s: prediction did not beat its degree-preserving control (ratio %.2f > 0.5)", r.Ref, r.Ratio)
		}
		if r.ControlTP >= r.RealTP {
			t.Errorf("%s: control caught as much as the real prediction (control_tp=%d >= real_tp=%d) — no topology signal", r.Ref, r.ControlTP, r.RealTP)
		}
	}
	if agg.AvgRealTP <= 0 {
		t.Fatalf("aggregate AvgRealTP must be positive, got %v", agg.AvgRealTP)
	}
	if agg.SignalRatio <= 1 {
		t.Fatalf("aggregate SignalRatio must exceed 1 (real beats control), got %v", agg.SignalRatio)
	}
	if want := expected / float64(len(results)); agg.FalsifiableRate < want {
		t.Fatalf("every strict scenario must be falsifiable: rate %v < expected %v", agg.FalsifiableRate, want)
	}
}

// TestShuffleAsPredictionIsNotFalsifiable is the anti-rigging guard: swapping the sets — pretending the
// shuffled control WAS the prediction and the real blast radius its control — must FAIL the falsifiability
// bar on every scenario. If this ever passes swapped, the control is vacuous and the whole INV-22 claim is
// rigged (e.g. someone regenerated the fixture's observed sets from the graph).
func TestShuffleAsPredictionIsNotFalsifiable(t *testing.T) {
	g := loadEstateGraph(t, "estate_fixture.json")
	scenarios, err := LoadFalsifiability("falsifiability_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		pred, ctrl := fxPredictionAndControl(g, s)
		observed := make([]verify.ObservedAlert, 0, len(s.Observed))
		for _, o := range s.Observed {
			observed = append(observed, verify.ObservedAlert{Host: o.Host, Rule: o.Rule, Site: o.Site})
		}
		swapped := predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: s.Ref, PlanHash: "swapped-" + s.Ref, TargetHost: s.TargetHost, Site: s.Site,
				PredictedHosts: ctrl, PredictedRules: map[string]struct{}{},
			},
			ControlHosts: pred.PredictedHosts,
		}
		if cs := predict.ScoreControl(swapped, observed); cs.Falsifiable() && cs.RealTP > 0 {
			t.Errorf("%s: the SHUFFLED set scored as falsifiable (real_tp=%d control_tp=%d) — the control is vacuous / the fixture is graph-derived", s.Ref, cs.RealTP, cs.ControlTP)
		}
	}
}
