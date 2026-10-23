package acceptance

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// ---------------------------------------------------------------------------------------------------------
// THE REAL /v1/vote PATH for REQ-1516 (TG-254).
//
// The "the principal votes on /v1/vote" step used to call policy.MayApprove directly on worker-side state.
// It never touched the vote path, so it was STRUCTURALLY INCAPABLE of failing when the handler and the
// workflow performed no approver check at all — which is exactly what shipped: MayApprove had zero
// production callers, and any authenticated operator could approve any governed action while this scenario
// stayed green.
//
// The handler itself is not drivable from here (it needs an authenticated session + a live Temporal client),
// so the step drives the layer the handler signals INTO — the real RunnerWorkflow's vote wait — and asserts
// the OUTCOME the requirement is actually about: did the non-member's vote release the action?
// ---------------------------------------------------------------------------------------------------------

// voteScriptedModel replays fixed model turns so the session reaches a proposal deterministically.
type voteScriptedModel struct {
	responses []string
	i         int
}

func (m *voteScriptedModel) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

type voteReadTool struct{}

func (voteReadTool) Name() string   { return "get-logs" }
func (voteReadTool) ReadOnly() bool { return true }
func (voteReadTool) Invoke(_ context.Context, _ map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{Target: "web01", ID: "tr-1", Tool: "get-logs", Output: "web01 nginx down", Success: true}, nil
}

// votePathAction is the action the scripted proposal describes; its content hash is the action_id a real
// vote must NAME (INV-12), so the vote gets past the bind check and approver admission is what decides it.
var votePathAction = manifest.Action{
	Target: "web01", OpClass: "restart-service", Op: "restart",
	Params: map[string]string{"unit": "nginx"}, Reversible: true,
}

const votePathProposal = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-1516","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`

// runVotePath drives the REAL RunnerWorkflow to a POLL_PAUSE poll whose approve_by set is approveBy, delivers
// ONE approving vote from voter, and returns the terminal result. The approve_by set arrives the way it does
// in production: as the return value of the gate ACTIVITY, resolved through the Deps.ApproveByFor seam the
// worker's composition root fills from the policy engine.
func runVotePath(approveBy []string, voter string) (runner.RunnerResult, error) {
	tools := agent.NewReadOnlyToolSet()
	if err := tools.Register(voteReadTool{}); err != nil {
		return runner.RunnerResult{}, err
	}
	graph := predict.NewDependencyGraph(map[string][]string{"web01": {"db01", "cache01"}})
	deps := runner.Deps{
		Model: &voteScriptedModel{responses: []string{
			`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
			votePathProposal,
		}},
		Tools:  tools,
		Limits: agent.DefaultLimits(),
		Gate: &predict.PredictionGate{
			Store: predict.NewMemPredictionStore(),
			Model: &predict.InfragraphModel{Graph: graph, DefaultRules: []string{"HighLatency"}, MaxDepth: 3},
			Mode:  predict.ModeEnforce,
		},
		Ledger:   audit.NewLedger(),
		Mutation: safety.NewReadOnlyChokepoint(), // mutation OFF — the vote outcome is the subject, not actuation
		// A canary pin forces POLL_PAUSE on a reversible action (REQ-009), so the session actually parks on a
		// human vote instead of resolving without one.
		CanaryPinned: func(string, string) (bool, string) { return true, "canary: staged first mutation" },
		// The seam under test: WHO may approve this poll.
		ApproveByFor: func(context.Context, runner.ApproveByQuery) []string { return approveBy },
		// ...and the BUNDLE fact that makes the answer binding. Both scenarios below describe a decision that
		// HAS an approve_by, so by construction the bundle they come from declares an approver regime and
		// admission ENFORCES. (Set explicitly rather than inferred from approveBy: the two are independent —
		// a configured bundle can still gate an action whose own rule names nobody, which is what
		// temporal/runner's TestAConfiguredBundleRefusesAnActionThatNamesNoApproverOfItsOwn pins.)
		ApproveByConfigured: true,
	}

	actionID, err := votePathAction.ID()
	if err != nil {
		return runner.RunnerResult{}, err
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	runner.RegisterActivities(env, runner.NewActivities(deps))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(runner.VoteSignalName, runner.VoteSignal{Approve: true, Voter: voter, ActionID: actionID})
	}, time.Minute)
	env.ExecuteWorkflow(runner.RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-1516", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() {
		return runner.RunnerResult{}, fmt.Errorf("the vote-path workflow did not complete")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		return runner.RunnerResult{}, fmt.Errorf("the vote-path workflow errored: %w", werr)
	}
	var res runner.RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		return runner.RunnerResult{}, err
	}
	if res.Band != safety.BandPollPause.String() {
		return res, fmt.Errorf("the fixture must reach a POLL_PAUSE poll (else no vote is being decided), band=%q", res.Band)
	}
	return res, nil
}
