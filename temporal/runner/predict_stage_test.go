package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-380 slice 4 — the predict stage's eligible/acted are derived from Gate.Commit's ERROR, not from clean
// booleans like correlate's verdict, so this pins the two non-trivial classifications directly (the generic
// producer-scan guard only asserts offered!=0 + the subset invariant). eligible = the state precondition could
// be established (NOT ErrPreconditionUnestablished/Violated); acted = a prediction committed (err == nil).
func TestGateActivityBooksPredictStageEligibility(t *testing.T) {
	newActs := func(gate *predict.PredictionGate, tally *observe.StageTally) *Activities {
		return &Activities{D: Deps{Stages: tally, Gate: gate}}
	}

	t.Run("commit is eligible and acted", func(t *testing.T) {
		tally := observe.NewStageTally()
		graph := predict.NewDependencyGraph(map[string][]string{"web01": {"db01"}})
		gate := &predict.PredictionGate{
			Store: predict.NewMemPredictionStore(),
			Model: &predict.InfragraphModel{Graph: graph, DefaultRules: []string{"HighLatency"}, MaxDepth: 3},
			Mode:  predict.ModeEnforce,
		}
		// restart-service declares no state precondition, so the gate predicts + commits.
		in := GateInput{Proposal: proposal.Proposal{ExternalRef: "tg380-ok", Action: manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}}, Band: safety.BandAuto, PlanHash: "ph", Site: "nl"}
		if _, err := newActs(gate, tally).GateActivity(context.Background(), in); err != nil {
			t.Fatalf("GateActivity: %v", err)
		}
		if off, elig, acted := tally.Snapshot("predict"); off != 1 || elig != 1 || acted != 1 {
			t.Errorf("commit path offered/eligible/acted = %d/%d/%d, want 1/1/1", off, elig, acted)
		}
	})

	t.Run("precondition unestablished is offered but not eligible", func(t *testing.T) {
		tally := observe.NewStageTally()
		// start-guest declares requires_target_state=not-running; with NO GuestRunning reader wired the gate
		// refuses at the precondition (ErrPreconditionUnestablished) before predicting anything.
		gate := &predict.PredictionGate{}
		in := GateInput{Proposal: proposal.Proposal{ExternalRef: "tg380-refuse", Action: manifest.Action{Target: "g1", OpClass: "start-guest", Op: "start", Reversible: true}}, Band: safety.BandAuto, PlanHash: "ph", Site: "nl"}
		_, err := newActs(gate, tally).GateActivity(context.Background(), in)
		if !errors.Is(err, predict.ErrPreconditionUnestablished) {
			t.Fatalf("GateActivity err = %v, want ErrPreconditionUnestablished", err)
		}
		// offered (the stage ran) but the precondition was not establishable ⇒ not eligible, not acted. This is
		// the classification that would silently invert if the errors.Is guard were dropped.
		if off, elig, acted := tally.Snapshot("predict"); off != 1 || elig != 0 || acted != 0 {
			t.Errorf("precondition-refusal offered/eligible/acted = %d/%d/%d, want 1/0/0", off, elig, acted)
		}
	})
}
