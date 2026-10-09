package runner

// TG-498 drills — the deterministic heal's grounding is CAPTURED EVIDENCE. The first live commit-confirmed
// traversal (2026-08-15 03:57Z) refused at the interceptor's evidence gate — "evidence unbound — no captured
// tool-result" — because the deterministic proposal was synthesized with zero ToolResults: the fast path's
// own confirmed-stopped observation was the grounding, but it was never RECORDED as evidence, so INV-11's
// gate had nothing to bind. The armed commit-confirm window then aborted correctly (provable non-execution)
// and the heal never ran. These drills pin the fix: the observation is captured as a ToolResult, cited by
// the proposal, threaded generically by the workflow, and the REAL evidence gate passes.
//
// EXECUTED KILLING MUTATIONS (2026-08-15, each witnessed red then restored green):
//   1. deterministic_heal.go: EvidenceIDs dropped from the synthesized proposal → the e2e reproduces the
//      LIVE failure verbatim ("evidence unbound", zero executions) → TestDeterministicHealExecutesThroughTheEvidenceGate red.
//   2. deterministic_heal.go: ToolResults dropped from the InvestigateResult (cited but uncaptured — the
//      dangling-citation direction) → the same e2e red.

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
	"go.temporal.io/sdk/testsuite"
)

// The activity-level binding: the synthesized proposal cites exactly the observation the activity captured.
func TestDeterministicHealCarriesItsGroundingEvidence(t *testing.T) {
	deps := testDeps()
	deps.Gate.GuestRunning = stopped
	res, err := NewActivities(deps).DeterministicGuestHealActivity(context.Background(),
		ingest.IncidentEnvelope{ExternalRef: "TG-498-unit", Host: healGuest, AlertRule: pveliveness.DeviceDownRule,
			SourceID: pveliveness.SourceType, Site: "dc1"})
	if err != nil || !res.Proposed {
		t.Fatalf("the confirmed-stopped fixture must propose: %+v err=%v", res, err)
	}
	if len(res.Proposal.EvidenceIDs) != 1 || res.Proposal.EvidenceIDs[0] != "det-liveness-TG-498-unit" {
		t.Fatalf("the proposal must cite the captured observation, got %v", res.Proposal.EvidenceIDs)
	}
	if len(res.ToolResults) != 1 || res.ToolResults[0].ID != res.Proposal.EvidenceIDs[0] ||
		!res.ToolResults[0].Success || res.ToolResults[0].Target != healGuest {
		t.Fatalf("the cited id must bind to a CAPTURED successful observation of the guest, got %+v", res.ToolResults)
	}
}

// The e2e: a deterministic proposal, voted through, EXECUTES — the real interceptor's evidence gate binds
// the captured observation. This is the exact lane the live traversal refused on.
func TestDeterministicHealExecutesThroughTheEvidenceGate(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	deps := testDeps() // no model script — the deterministic path must never consult the model
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.CommitConfirm = newCCT0292Fake()
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed(healGuest, pveliveness.DeviceDownRule, act)
	// The guest reads STOPPED until the actuator runs (the seal gate's observed-not-running + the REQ-2908
	// arm-time no-op guard + the emission precondition all read this), RUNNING after — the estate moving.
	deps.Gate.GuestRunning = func(context.Context, string) (bool, string, bool) {
		return act.execs > 0, "guest_liveness projection (fixture)", true
	}
	deps.CorrelationWindow = func(context.Context, time.Time) (correlate.Window, error) {
		return isolatedWindow("TG-498-e2e", pveliveness.SourceType, healGuest), nil
	}
	deps.ApproveByFor = func(context.Context, ApproveByQuery) []string { return []string{"user:kyr"} }
	deps.ApproveByConfigured = true

	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)
	env.RegisterActivity(acts.RecordPendingActivity)
	env.RegisterActivity(acts.ResolvePendingActivity)

	actionID, err := manifest.Action{
		Target: healGuest, OpClass: "start-guest", Op: "start",
		Params: map[string]string{"guest": healGuest}, Reversible: true,
	}.ID()
	if err != nil {
		t.Fatalf("derive the deterministic action id: %v", err)
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(VoteSignalName, VoteSignal{Approve: true, Voter: "kyr", ActionID: actionID})
	}, time.Minute)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-498-e2e", SourceID: pveliveness.SourceType, Host: healGuest,
		AlertRule: pveliveness.DeviceDownRule, Severity: ingest.SeverityCritical, Site: "dc1", ReceivedAt: healAt,
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if act.execs != 1 {
		t.Fatalf("TG-498: the approved deterministic heal must EXECUTE — the evidence gate must bind the "+
			"captured observation (the live traversal refused here with %q); execs=%d res=%+v",
			"evidence unbound — no captured tool-result", act.execs, res)
	}
}
