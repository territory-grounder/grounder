package runner

// TG-42 — the exec-class routing must ACT: execclass.SkipsAgent had ZERO callers, so a class the
// classifier decided (BEFORE expensive context construction, exactly so a cheap incident does not pay
// the ceremonial lifecycle) changed nothing about whether the model loop ran. THE core oracle here was
// written and run RED against main first: a DETERMINISTIC-class envelope completed its session only
// after one-or-more model calls — the decided-but-dead machinery the ticket names.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// countingModel counts every Complete invocation — the instrument behind the zero-model-calls oracle.
type countingModel struct{ calls int32 }

func (m *countingModel) Complete(context.Context, string, string, []model.Message) (string, error) {
	atomic.AddInt32(&m.calls, 1)
	return `{"action":"stop","confidence":0.9,"reason":"fixture stop","evidence_ids":[]}`, nil
}

// TestDeterministicClassCompletesWithZeroModelCalls is THE core oracle (red on main before TG-42):
// a Deterministic-class envelope produces a COMPLETED session with ZERO model calls, recorded as a
// deterministic disposition — never a fabricated investigation.
func TestDeterministicClassCompletesWithZeroModelCalls(t *testing.T) {
	deps := testDeps()
	cm := &countingModel{}
	deps.Model = cm
	res, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
		ExternalRef: "TG-42-det", Host: "web01", AlertRule: "ScheduledReboot",
		Severity: ingest.SeverityWarning, Site: "dc1",
	}, string(execclass.Deterministic), ClusterMemberContext{})
	if err != nil {
		t.Fatalf("a deterministic-class session must complete, got %v", err)
	}
	if got := atomic.LoadInt32(&cm.calls); got != 0 {
		t.Fatalf("a DETERMINISTIC-class envelope must complete with ZERO model calls (execclass.SkipsAgent), "+
			"got %d — the agent loop ran anyway, so the routing decision is still decided-but-dead", got)
	}

	// THE RECORD IS HONEST: a skipped agent is a recorded decision, never a fabricated investigation.
	if res.Proposed {
		t.Fatalf("a skipped agent can propose nothing, got %+v", res.Proposal)
	}
	if res.Outcome != "deterministic-skip" {
		t.Errorf("outcome must name the skip (deterministic-skip), got %q — a skip recorded as an agent "+
			"outcome would be indistinguishable from a session that ran", res.Outcome)
	}
	if res.Reason != "deterministic class: agent skipped" {
		t.Errorf("the orchestrator-computed reason must say the agent was skipped, got %q", res.Reason)
	}
	if res.ModelTier != "" || res.DecisionTier != "" {
		t.Errorf("no model ran — the tier stamps must be empty, got investigate=%q decide=%q", res.ModelTier, res.DecisionTier)
	}
	if res.SeedHash != "" || res.PromptVersion != "" {
		t.Errorf("no seed was composed — a fingerprint/version here would fabricate a prompt that never "+
			"existed, got hash=%q version=%q", res.SeedHash, res.PromptVersion)
	}
	if res.StepCount != 0 || len(res.ToolResults) != 0 {
		t.Errorf("no investigation happened — steps=%d tools=%d must be zero", res.StepCount, len(res.ToolResults))
	}
	var skipNote, shapeNote bool
	for _, n := range res.SkillLoads {
		if n == "execclass:deterministic:agent-skipped" {
			skipNote = true
		}
		if strings.HasPrefix(n, "session-shape:") {
			shapeNote = true
		}
	}
	if !skipNote {
		t.Errorf("the skip must be visible in the session provenance, got %v", res.SkillLoads)
	}
	if !shapeNote {
		t.Errorf("the session shape must still be recorded (a skip is a classified session, not a hole), got %v", res.SkillLoads)
	}
}

// TestOnlyTheDeterministicClassSkipsTheAgent asserts over the CLOSED class enumeration: exactly one of
// the five classes skips the loop, and an absent/garbage class (the classFor legacy fallback) always
// runs it. "Implemented" is proven "reachable-for-exactly-the-right-input" here, not assumed.
func TestOnlyTheDeterministicClassSkipsTheAgent(t *testing.T) {
	all := []execclass.Class{execclass.Deterministic, execclass.FastAgent, execclass.StandardAgent,
		execclass.DeepInvestigation, execclass.HumanLed}
	for _, c := range all {
		if !execclass.Valid(c) {
			t.Fatalf("enumeration drifted from core/execclass: %q is not a valid class", c)
		}
	}
	run := func(class string) int32 {
		deps := testDeps()
		cm := &countingModel{}
		deps.Model = cm
		if _, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
			ExternalRef: "TG-42-enum", Host: "web01", AlertRule: "NginxDown",
			Severity: ingest.SeverityWarning, Site: "dc1",
		}, class, ClusterMemberContext{}); err != nil {
			t.Fatalf("class %q: %v", class, err)
		}
		return atomic.LoadInt32(&cm.calls)
	}
	skipped := 0
	for _, c := range all {
		calls := run(string(c))
		if calls == 0 {
			skipped++
			if c != execclass.Deterministic {
				t.Errorf("class %s skipped the agent — only DETERMINISTIC may (SkipsAgent is the single source)", c)
			}
		} else if c == execclass.Deterministic {
			t.Errorf("DETERMINISTIC ran the agent (%d call(s)) — the short-circuit is unreachable", calls)
		}
	}
	if skipped != 1 {
		t.Errorf("exactly ONE class skips the agent, got %d", skipped)
	}
	for _, raw := range []string{"", "NOT-A-CLASS"} {
		if calls := run(raw); calls == 0 {
			t.Errorf("unclassified %q must fail open to a REAL agent session, but the loop never ran", raw)
		}
	}
}

// TestWorkflowRecordsTheDeterministicSkipHonestly drives the FULL RunnerWorkflow with the correlation
// stage deciding DETERMINISTIC (mocked — the live correlate feeds only the Correlated signal today, so
// the class arrives exactly the way a future KnownProcedure/ReadOnly wiring will deliver it): the
// threaded class short-circuits the REAL InvestigateActivity, the session terminates on the normal
// no-proposal path with ZERO model calls, and the DURABLE triage row says so — outcome
// `no-proposal:deterministic-skip`, the orchestrator's stop reason, and the provenance note. No
// manifest, no action id, no operator poll, no handoff page: a recorded decision, not a fabricated
// investigation.
func TestWorkflowRecordsTheDeterministicSkipHonestly(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps()
	cm := &countingModel{}
	deps.Model = cm
	var rows []judge.TriageRow
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		rows = append(rows, row)
		return nil
	}
	a := NewActivities(deps)
	registerAll(env, a)
	env.OnActivity(a.CorrelateActivity, mock.Anything, mock.Anything).Return(CorrelateResult{
		ExecClass: string(execclass.Deterministic), Reason: "isolated", Elected: true,
	}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-42-wf", SourceID: "prometheus-dc1", AlertRule: "ScheduledReboot",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)

	if got := atomic.LoadInt32(&cm.calls); got != 0 {
		t.Fatalf("the deterministic session spent %d model call(s) end-to-end, want 0", got)
	}
	if res.Proposed || res.ActionID != "" {
		t.Fatalf("a skipped agent proposes nothing and seals nothing, got %+v", res)
	}
	if res.Outcome != "no-proposal:deterministic-skip" {
		t.Errorf("terminal outcome = %q, want no-proposal:deterministic-skip", res.Outcome)
	}
	if res.StopReason != "deterministic class: agent skipped" {
		t.Errorf("stop reason = %q — the orchestrator's account must reach the terminal record", res.StopReason)
	}
	if res.ExecClass != string(execclass.Deterministic) {
		t.Errorf("the recorded class = %q, want DETERMINISTIC", res.ExecClass)
	}
	if res.Notified {
		t.Error("a deterministic skip must not page a human — nothing was concluded that needs one")
	}
	if len(rows) != 1 {
		t.Fatalf("exactly one durable triage row, got %d", len(rows))
	}
	row := rows[0]
	if row.Proposed || row.Outcome != "no-proposal:deterministic-skip" || row.StopReason != "deterministic class: agent skipped" {
		t.Errorf("the durable row is dishonest: %+v", row)
	}
	if row.ModelTier != "" {
		t.Errorf("no model ran — the row must not name a tier, got %q", row.ModelTier)
	}
	var skipNote bool
	for _, n := range row.SkillLoads {
		if n == "execclass:deterministic:agent-skipped" {
			skipNote = true
		}
	}
	if !skipNote {
		t.Errorf("the durable row must carry the skip provenance, got %v", row.SkillLoads)
	}
}
