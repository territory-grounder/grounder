package runner

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// slowModel wraps a scripted model with a per-call delay, so the agent loop takes a KNOWN MINIMUM amount of
// real time. Without it this oracle would be timing a loop that answers instantly from a slice, and any
// assertion on the recorded duration would be a coin toss against the scheduler.
type slowModel struct {
	inner *scriptedModel
	delay time.Duration
}

func (m *slowModel) Complete(ctx context.Context, system, tier string, msgs []model.Message) (string, error) {
	time.Sleep(m.delay)
	return m.inner.Complete(ctx, system, tier, msgs)
}

// TG-205 — the DURABLE record must say HOW LONG the decision took, not only how many steps it took.
//
// docs/BENCHMARK-AXES.md defines A6 as MTTR — "resolving faster … detection latency, decision latency,
// actuation path" — while every implementation measured decision STEPS (a6a_mean_decision_steps in
// cmd/axisscore, MeanDecisionSteps in eval/gate, session_triage.step_count from migration 0037). The axis
// name and its implementations had drifted apart, so no scored surface reported time: TG could not state
// time-to-decision for any incident, including its own measured ~39s-vs-~11min detection result.
//
// The number already existed in process — temporal/runner/activities.go times the loop as `loopDur` — and was
// handed only to observe.RecordAgentLoop, whose emitter is nil unless metrics are wired and which — when they
// are — accumulates into ONE counter, tg_agent_run_seconds_total: a running sum of all loop seconds, with no
// distribution, no per-incident attribution, and a reset on restart. This is the same boundary migration 0037
// crossed for step_count.
//
// This is the WIRED oracle: it drives the real RunnerWorkflow through the real InvestigateActivity and
// RecordTriageActivity and reads the judge.TriageRow the workflow actually hands the store — not the
// activity's return value, which could carry the field while the composition root drops it.
//
// KILLING MUTATION (executed): drop `res.DecisionMillis = inv.DecisionMillis` from workflow.go. This fails
// with:
//
//	the durable triage record says the decision took 0ms, but the loop demonstrably ran for at least 90ms —
//	A6b (time to decision) cannot be measured off a corpus that records no time (TG-205)
//
// Restored ⇒ green. Dropping `DecisionMillis: loopDur.Milliseconds()` at the activity boundary, or
// `DecisionMillis: res.DecisionMillis` from the workflow's TriageRow literal, fails it identically.
func TestTriageRecordCarriesTheDecisionWallClock(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	// Three model calls: two tool cycles then a grounded proposal. At 30ms each the loop cannot finish in
	// under 90ms, which is the floor asserted below — a real bound on elapsed time, not a nonzero check that
	// a fast machine could satisfy by accident in either direction.
	const perCall = 30 * time.Millisecond
	const calls = 3
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"tool","tool":"get-logs","args":{"host":"web01","tail":"200"},"confidence":0.8}`,
		proposeGroundedWeb01,
	}
	deps := testDeps(script...)
	deps.Model = &slowModel{inner: &scriptedModel{responses: script}, delay: perCall}
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		recorded = append(recorded, row)
		return nil
	}
	registerAll(env, NewActivities(deps))

	started := time.Now()
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-205-clock", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	wall := time.Since(started)

	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d — the assertions below would be vacuous", len(recorded))
	}
	row := recorded[0]
	floorMs := (perCall * calls).Milliseconds()
	if row.DecisionMillis < floorMs {
		t.Fatalf("the durable triage record says the decision took %dms, but the loop demonstrably ran for at "+
			"least %dms — A6b (time to decision) cannot be measured off a corpus that records no time (TG-205)",
			row.DecisionMillis, floorMs)
	}
	// The UPPER bound is the other half. A field filled from a wall-clock that starts anywhere but the loop —
	// process start, workflow start, the epoch — would sail past the floor above while measuring the wrong
	// interval entirely, and the axis would silently report something other than time-to-decision.
	if row.DecisionMillis > wall.Milliseconds() {
		t.Fatalf("the record claims a %dms decision inside a %dms workflow — the recorded duration is not the "+
			"agent loop's, so A6b would publish an interval nobody chose", row.DecisionMillis, wall.Milliseconds())
	}
	// A6a and A6b are two facts about one decision, and the split exists because neither implies the other:
	// this session ran 3 cycles and took ~90ms, and a build that dropped either would still satisfy the other.
	if row.StepCount == 0 {
		t.Fatalf("the steps half (A6a) is gone: step_count=%d — the split must keep BOTH measurements", row.StepCount)
	}
}

// The stand-down mirror. A grounded stop spends real reasoning time and proposes nothing, and it is the
// commonest terminus in this corpus. Recording the wall-clock only on the propose path would leave A6b blind
// to exactly the sessions where TG thought hardest and decided not to act — and would bias the published
// median toward whichever branch happened to be instrumented.
//
// KILLING MUTATION (executed): move `res.DecisionMillis = inv.DecisionMillis` in workflow.go from above the
// propose/stop branch into the `if inv.Proposed` arm. RED — "a grounded stop recorded a 0ms decision".
func TestTriageRecordCarriesTheDecisionWallClockOnAGroundedStop(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	const perCall = 30 * time.Millisecond
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"stop","confidence":0.9,"conclusion":"transient; no action warranted","evidence_ids":["tr-1"]}`,
	}
	deps := testDeps(script...)
	deps.Model = &slowModel{inner: &scriptedModel{responses: script}, delay: perCall}
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		recorded = append(recorded, row)
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-205-stop", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d", len(recorded))
	}
	row := recorded[0]
	if row.Proposed {
		t.Fatalf("setup: this scenario must end in a grounded stop, got a proposal (outcome %q)", row.Outcome)
	}
	if row.DecisionMillis < (perCall * 2).Milliseconds() {
		t.Fatalf("a grounded stop recorded a %dms decision after at least %dms of real reasoning — A6b would be "+
			"measured only over sessions that proposed, biasing the published median toward one branch",
			row.DecisionMillis, (perCall * 2).Milliseconds())
	}
}
