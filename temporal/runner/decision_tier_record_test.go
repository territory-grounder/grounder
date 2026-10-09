package runner

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// proposeGroundedWeb01 cites tr-1 — the id runner_test's readTool captures — so it survives the
// post-observation citation gate and becomes a REAL terminal proposal. proposeWeb01 (uncited) would be
// bounced once observations exist, which is the wrong shape for a decide-nudge scenario.
const proposeGroundedWeb01 = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"evidence_ids":["tr-1"]}}`

// TG-198 — the DURABLE record must name the model that DECIDED.
//
// A session investigates on investigateTierFor(env, "") ("fast" for an ordinary incident) and, once the TG-60
// poll-limit nudge fires, decides on decisionTierFor() ("primary"). session_triage.model_tier was filled from
// the investigation tier alone, so the terminal record claimed "fast" decided — for every one of the 537
// recorded incidents, including the ones the reasoning tier authored. The three-arm tier A/B (TG-204) has no
// dependent variable until the row says which arm produced the proposal.
//
// This is the WIRED oracle: it drives the real RunnerWorkflow through the real InvestigateActivity and
// RecordTriageActivity and reads the judge.TriageRow the workflow actually hands the store — not the
// activity's return value, which could carry the field while the composition root drops it.
//
// KILLING MUTATION (executed): restore the hardcode in agent/loop.go — `res.DecisionTier = a.ModelName`
// instead of `= modelName`. This fails with:
//
//	the durable triage record says tier "fast" decided, but the terminal proposal was produced on
//	"primary" — the corpus cannot attribute this decision to a model (TG-198)
//
// Restored ⇒ green. (Dropping `DecisionTier: res.DecisionTier` at either the activity boundary or the
// workflow's TriageRow literal fails it the same way, with "" for the recorded tier.)
func TestTriageRecordCarriesTheTerminalDecisionTier(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	// Five distinct tool calls exhaust HandoffPoll (DefaultLimits{5,10}) without tripping the repeated-step
	// trajectory veto; the sixth response lands on the NUDGED decision cycle, which runs on "primary".
	script := append(distinctToolScript(5), proposeGroundedWeb01)
	deps := testDeps(script...)
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		recorded = append(recorded, row)
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-198-decide", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d — the assertions below would be vacuous", len(recorded))
	}
	row := recorded[0]
	if !row.Proposed {
		t.Fatalf("setup: the nudged cycle must produce a real proposal, got outcome %q — the scenario no longer exercises a terminal decision", row.Outcome)
	}
	// The two tiers are the two DIFFERENT facts. The scenario is built so they diverge; if the selectors ever
	// make them equal this test stops being able to tell a right answer from the old hardcode, so it says so.
	investigate := investigateTierFor(ingest.IncidentEnvelope{ExternalRef: "TG-198-decide", Host: "web01", AlertRule: "HostDown", Severity: ingest.SeverityWarning}, "")
	decide := decisionTierFor()
	if investigate == decide {
		t.Fatalf("setup: investigate (%q) and decision (%q) tiers must differ for this oracle to discriminate", investigate, decide)
	}
	if row.ModelTier != investigate {
		t.Fatalf("the record must keep the INVESTIGATION tier %q, got %q — the reading tier is a separate fact and must not be overwritten", investigate, row.ModelTier)
	}
	if row.DecisionTier != decide {
		t.Fatalf("the durable triage record says tier %q decided, but the terminal proposal was produced on %q — the corpus cannot attribute this decision to a model (TG-198)",
			row.DecisionTier, decide)
	}
	// The nudge is not derivable from the pair (an eval arm can set both tiers to one model), so it is
	// recorded on the seed-provenance channel alongside the other session notes.
	if !hasNote(row.SkillLoads, "decide-nudge:fired") {
		t.Fatalf("the TG-60 nudge fired but the record does not say so, skill_loads=%v", row.SkillLoads)
	}
}

// The control: a session that decides on its FIRST cycle was never nudged, so the decision tier is the
// investigation tier and no nudge note is recorded. Without this, a DecisionTier hardcoded to "primary"
// would pass the test above — which is the same class of bug, wearing the other constant.
func TestTriageRecordDecisionTierWithoutNudge(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	deps := testDeps(proposeWeb01) // decides immediately: no tool calls, no poll limit, no nudge
	var recorded []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		recorded = append(recorded, row)
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-198-direct", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if len(recorded) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d", len(recorded))
	}
	row := recorded[0]
	if !row.Proposed {
		t.Fatalf("setup: expected an immediate proposal, got outcome %q", row.Outcome)
	}
	if row.DecisionTier != row.ModelTier || row.DecisionTier == "" {
		t.Fatalf("an un-nudged session decided on the tier it investigated with: model_tier=%q decision_model_tier=%q",
			row.ModelTier, row.DecisionTier)
	}
	if hasNote(row.SkillLoads, "decide-nudge:fired") {
		t.Fatalf("no nudge fired, but the record claims one — a forced decision and a self-converged one must stay distinguishable, skill_loads=%v", row.SkillLoads)
	}
}
