package eval

// Falsifiability scoring: prove, with a deterministic fixture, that the REAL estate graph's predictions
// beat their own degree-preserving shuffled negative control (INV-22) — the property the grounding API's
// SignalRatio reports but which had NO producer anywhere (production scoring waits on mutation-ON + an
// observe poller; see the verify-time writer design). The scenarios are hand-authored, placement-grounded
// SYNTHETIC outcome records (falsifiability_fixture.json) — never generated from the graph itself, which
// would rig the control. Pure and deterministic: no gateway, no box — runs in `make all`.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/verify"
)

// FalsScenario is one hand-authored outcome record: a target that "failed" and the alerts observed after.
// ExpectFalsifiable marks whether the scenario sits on a tier whose shuffled null model has enough edges to
// be statistically meaningful (the placement tier). A false value documents a MEASURED limitation — e.g. the
// 11-edge depends_on tier, where a degree-preserving shuffle largely reproduces the real set — rather than a
// pass; the test still requires the prediction itself to catch the real cascade (RealTP>0).
type FalsScenario struct {
	Ref               string       `json:"ref"`
	TargetHost        string       `json:"target_host"`
	Site              string       `json:"site"`
	ExpectFalsifiable bool         `json:"expect_falsifiable"`
	Observed          []ObservedFx `json:"observed"`
}

// ObservedFx is one observed alert in a scenario's outcome record.
type ObservedFx struct {
	Host string `json:"host"`
	Rule string `json:"rule"`
	Site string `json:"site"`
}

// ControlResult is the per-scenario falsifiability score: the real prediction's hits vs the degree-
// preserving shuffled control's hits, via the production predict.ScoreControl.
type ControlResult struct {
	Ref         string  `json:"ref"`
	RealTP      int     `json:"real_tp"`
	RealFP      int     `json:"real_fp"`
	ControlTP   int     `json:"control_tp"`
	ControlFP   int     `json:"control_fp"`
	Ratio       float64 `json:"control_ratio"` // control_tp / max(real_tp,1); falsifiable when <= 0.5
	Falsifiable bool    `json:"falsifiable"`
}

// FalsifiabilityAgg is the aggregate the scorecard surfaces — the same shape the grounding API computes
// from infragraph_prediction rows (AvgRealTP/AvgControlTP/SignalRatio), produced here by the fixture.
type FalsifiabilityAgg struct {
	AvgRealTP       float64 `json:"avg_real_tp"`
	AvgControlTP    float64 `json:"avg_control_tp"`
	SignalRatio     float64 `json:"signal_ratio"`     // real/control, >1 means the topology adds signal
	FalsifiableRate float64 `json:"falsifiable_rate"` // fraction of scenarios beating the control
}

// LoadFalsifiability reads the hand-authored scenario fixture.
func LoadFalsifiability(path string) ([]FalsScenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("falsifiability fixture: %w", err)
	}
	var f struct {
		Scenarios []FalsScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("falsifiability fixture json: %w", err)
	}
	if len(f.Scenarios) == 0 {
		return nil, fmt.Errorf("falsifiability fixture: no scenarios")
	}
	return f.Scenarios, nil
}

// fxPredictionAndControl computes one scenario's real prediction (estate blast radius + siblings) and its
// degree-preserving shuffled control, seeded deterministically from the scenario ref. NOTE: unlike the
// production gate (gate.Commit, which since TG-61 gates siblings by alert class and mirrors that gate in the
// control), this fixture keeps the real prediction siblings-ON and the control blast-only — an ASYMMETRIC
// pair retained to preserve the fixture's established baseline. See the TODO below.
func fxPredictionAndControl(g *estate.Graph, s FalsScenario) (verify.Prediction, map[string]struct{}) {
	m := &predict.InfragraphModel{Estate: g, DefaultRules: []string{"HostDown", "HighLatency"}, MaxDepth: 3}
	seed := "falsifiability-" + s.Ref
	// These placement-grounded scenarios model a shared-parent (common-cause) cascade, so the real prediction
	// includes siblings (commonCause=true). The negative control is SIBLINGS-SYMMETRIC (includeSiblings=true) —
	// a same-shape null model, per the ShuffledControl symmetry contract (TG-87). An honest same-shape control
	// costs several scenarios their falsifiability (control_tp==real_tp on tiers where the target's siblings ARE
	// the whole cascade); those are marked expect_falsifiable=false in the fixture, not propped by a rigged
	// asymmetry. Production scoring (gate.Commit → Predict + controlHosts) is already symmetric; this matches it.
	pred := m.Predict(s.Ref, seed, s.TargetHost, s.Site, true)
	ctrl := map[string]struct{}{}
	if target, ok := g.Resolve(s.TargetHost); ok {
		for _, imp := range g.ShuffledControl(target, 3, seed, true) {
			ctrl[imp.Entity.Name] = struct{}{}
		}
	}
	return pred, ctrl
}

// ScoreFalsifiability scores every scenario with the production predict.ScoreControl and aggregates.
func ScoreFalsifiability(g *estate.Graph, scenarios []FalsScenario) ([]ControlResult, FalsifiabilityAgg) {
	results := make([]ControlResult, 0, len(scenarios))
	var sumReal, sumCtrl, falsifiable float64
	for _, s := range scenarios {
		pred, ctrl := fxPredictionAndControl(g, s)
		observed := make([]verify.ObservedAlert, 0, len(s.Observed))
		for _, o := range s.Observed {
			observed = append(observed, verify.ObservedAlert{Host: o.Host, Rule: o.Rule, Site: o.Site})
		}
		cs := predict.ScoreControl(predict.PredictionRecord{Prediction: pred, ControlHosts: ctrl}, observed)
		r := ControlResult{
			Ref: s.Ref, RealTP: cs.RealTP, RealFP: cs.RealFP, ControlTP: cs.ControlTP, ControlFP: cs.ControlFP,
			Ratio: cs.Ratio(), Falsifiable: cs.Falsifiable(),
		}
		results = append(results, r)
		sumReal += float64(cs.RealTP)
		sumCtrl += float64(cs.ControlTP)
		if r.Falsifiable {
			falsifiable++
		}
	}
	n := float64(len(scenarios))
	agg := FalsifiabilityAgg{AvgRealTP: sumReal / n, AvgControlTP: sumCtrl / n, FalsifiableRate: falsifiable / n}
	denom := agg.AvgControlTP
	if denom < 1 {
		denom = 1 // same floor the grounding read surface applies — a zero control reads as ratio=real
	}
	agg.SignalRatio = agg.AvgRealTP / denom
	return results, agg
}
