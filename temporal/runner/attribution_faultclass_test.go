package runner

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/ingest"
	"go.temporal.io/sdk/testsuite"
)

// TestRunnerSelfNoopDoesNotFireOnAMismatchedFaultClass is the END-TO-END form of REQ-2323, and it reproduces
// the live incident: a stopped nginx, a correct `restart-service` proposal, and a `vzstart` (GUEST start) that
// TG's own identity performed minutes earlier for an unrelated fault. The old behaviour terminated the session
// `already-remediated` and left the service down. The workflow must now carry on and propose.
func TestRunnerSelfNoopDoesNotFireOnAMismatchedFaultClass(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01) // restart-service
	reader := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam!tg-actuate", ActionKind: "vzstart", Target: "web01",
			ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:1", Covered: true},
	}}
	attributeDeps(t, reader)(&deps)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-mismatch",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if res.Outcome == "already-remediated" {
		t.Fatalf("a GUEST start was accepted as proof that a SERVICE restart had been done — the session stood "+
			"down and the service stays broken (%+v)", res)
	}
	if !res.Proposed {
		t.Errorf("the session must go on to propose a remediation, got %+v", res)
	}
}
