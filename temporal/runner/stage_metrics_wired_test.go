package runner

// TG-380 producer-scan guard. The ticket's two killing mutations:
//   1. a decision stage without its counter — RED;
//   2. a counter registered but never INCREMENTED from the stage (the declared-but-dead pattern) — RED.
//
// A grep for a Record call catches (1) but not (2): dead code satisfies a grep. So this guard EXERCISES
// each stage in metrics.DecisionStages by driving its REAL decision function and asserting the tally
// actually moved — proving the wiring is LIVE, not merely present ("implemented ≠ reachable" applied to
// the instrument). Adding a stage to DecisionStages without wiring it (mutation 1) OR wiring a dead
// Record (mutation 2) both leave offered=0 → RED. metrics.PendingDecisionStages is the honest frontier:
// the five stages not yet instrumented are named there, so "not wired yet" is distinguishable from
// "silently missing", and moving one to DecisionStages forces adding its exercise here.
//
// KILLING MUTATION (executed 2026-08-11): remove `g.Stages.Record("suppress", …)` from
// LiveSuppressGate.Decide — TestEveryDecisionStageIncrementsItsTally fails with "stage suppress: offered
// stayed 0". Restore → green. Second: add "correlate" to DecisionStages without an exercise here — the
// coverage assertion fails naming the unexercised stage.

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/observe"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/suppression"
	"github.com/territory-grounder/grounder/core/verify"
)

// stageExercisers drives each instrumented stage's REAL decision against a shared tally and returns the
// stage name it exercised. One entry per member of metrics.DecisionStages — the coverage assertion below
// fails if a declared stage has no exerciser (so a new stage cannot ship without proving its tally moves).
var stageExercisers = map[string]func(t *testing.T, tally *observe.StageTally){
	"suppress": func(t *testing.T, tally *observe.StageTally) {
		// A live gate with an empty chain: a WARNING-severity alert reaches the stages and escalates
		// (eligible, not acted). Drives the real LiveSuppressGate.Decide → g.Stages.Record.
		g := &LiveSuppressGate{Window: time.Minute, Log: NewRecentTriageLog(time.Minute), Stages: tally}
		a := suppression.Alert{Host: "h1", AlertRule: "DeviceDown", Severity: ingest.SeverityWarning, ExternalRef: "tg380-x", ObservedAt: time.Now()}
		if _, err := g.Decide(context.Background(), a, time.Now()); err != nil {
			t.Fatalf("suppress exercise: %v", err)
		}
	},
	"correlate": func(t *testing.T, tally *observe.StageTally) {
		// A live activity with a READABLE but empty correlation window: the stage runs (offered), the window
		// was readable (eligible), and finds no correlation (not acted) — the analogue of the suppress
		// exerciser's escalate case. Drives the real CorrelateActivity → a.D.Stages.Record("correlate", …).
		a := &Activities{D: Deps{
			Stages:            tally,
			CorrelationWindow: func(context.Context, time.Time) (correlate.Window, error) { return correlate.Window{}, nil },
		}}
		in := CorrelateInput{ExternalRef: "tg380-corr", Host: "h1", AlertRule: "DeviceDown", Severity: "warning", At: time.Now()}
		if _, err := a.CorrelateActivity(context.Background(), in); err != nil {
			t.Fatalf("correlate exercise: %v", err)
		}
	},
	"predict": func(t *testing.T, tally *observe.StageTally) {
		// A live prediction gate that COMMITS a reversible restart: op-class "restart-service" declares no state
		// precondition, so the gate predicts + commits — the stage runs (offered), the precondition is
		// establishable (eligible), and the prediction commits (acted). Drives the real GateActivity →
		// a.D.Stages.Record("predict", …). The gate construction mirrors runner_test.go's testDeps.
		graph := predict.NewDependencyGraph(map[string][]string{"web01": {"db01"}})
		a := &Activities{D: Deps{
			Stages: tally,
			Gate: &predict.PredictionGate{
				Store: predict.NewMemPredictionStore(),
				Model: &predict.InfragraphModel{Graph: graph, DefaultRules: []string{"HighLatency"}, MaxDepth: 3},
				Mode:  predict.ModeEnforce,
			},
		}}
		in := GateInput{
			Proposal: proposal.Proposal{ExternalRef: "tg380-pred", Action: manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true}},
			Band:     safety.BandAuto, PlanHash: "ph", Site: "nl",
		}
		if _, err := a.GateActivity(context.Background(), in); err != nil {
			t.Fatalf("predict exercise: %v", err)
		}
	},
	"gate": func(t *testing.T, tally *observe.StageTally) {
		// A live interceptor gate chain that REFUSES (the target healed before execute → the necessity gate
		// refuses): the chain produces a verdict (offered), is not rate-limited (eligible), and does not execute
		// (acted=0) — the analogue of the correlate/predict refusal exercisers. Drives the real ExecuteActivity →
		// a.D.Stages.Record("gate", …). Construction mirrors necessity_wire_test.go's executeWith.
		ctx := context.Background()
		choke := safety.NewActuatingChokepoint()
		m := unitManifest(t)
		sink := &fakeManifestSink{}
		if err := sink.Seal(ctx, m); err != nil {
			t.Fatalf("seal: %v", err)
		}
		deps := Deps{
			Stages:           tally,
			Interceptor:      withPermissivePolicy(actuate.NewInterceptor(choke, &recordingActuator{}, audit.NewLedger())),
			Manifests:        sink,
			Mutation:         choke,
			PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return []verify.ObservedAlert{}, true },
			ClearObserve:     func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }, // host quiet ⇒ necessity refusal
		}
		if _, err := NewActivities(deps).ExecuteActivity(ctx, ExecuteInput{
			ActionID: m.ActionID, ExternalRef: "tg380-gate", PlanHash: "plan#g", TargetHost: "web01", Site: "nl", Band: safety.BandAuto,
		}); err != nil {
			t.Fatalf("gate exercise: %v", err)
		}
	},
}

func TestEveryDecisionStageIncrementsItsTally(t *testing.T) {
	for _, stage := range metrics.DecisionStages {
		exercise, ok := stageExercisers[stage]
		if !ok {
			t.Errorf("decision stage %q is in metrics.DecisionStages but has NO exerciser here — a declared "+
				"instrumented stage must prove its tally increments (TG-380). Add an exerciser or move it to "+
				"PendingDecisionStages.", stage)
			continue
		}
		tally := observe.NewStageTally()
		exercise(t, tally)
		off, elig, acted := tally.Snapshot(stage)
		if off == 0 {
			t.Errorf("stage %s: offered stayed 0 after driving its real decision — the stage's counter is "+
				"not wired (mutation 1) or the Record is dead (mutation 2). offered/eligible/acted=%d/%d/%d",
				stage, off, elig, acted)
		}
		if !(off >= elig && elig >= acted) {
			t.Errorf("stage %s: subset invariant offered>=eligible>=acted violated: %d/%d/%d", stage, off, elig, acted)
		}
	}
}

// TestNoStageIsBothDeclaredAndPending: the two frontiers are disjoint — a stage cannot be claimed as wired
// and pending at once (that would let a "wired" claim hide an unexercised stage).
func TestNoStageIsBothDeclaredAndPending(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range metrics.DecisionStages {
		declared[s] = true
	}
	for _, s := range metrics.PendingDecisionStages {
		if declared[s] {
			t.Errorf("stage %q is in BOTH DecisionStages and PendingDecisionStages — the frontier lies", s)
		}
	}
}
