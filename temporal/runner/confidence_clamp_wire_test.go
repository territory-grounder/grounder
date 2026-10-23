package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"

	"go.temporal.io/sdk/testsuite"
)

// TG-408 — the policy min_confidence clamp had no input.
//
// core/policy/confidence.go is thoroughly tested as a PURE function: five oracles plus two property
// tests pin ClampConfidence's semantics exactly. Every one of them passed while the control was
// unusable, because none of them asks the question this file asks: does the number the clamp compares
// ever ARRIVE?
//
// It did not. `actuate.Request.Confidence` was never assigned anywhere in the tree — the sole
// construction site (activities.go's execute path) omitted the field — so policy.EvalInput.Confidence
// was 0.0 on every executed action and `confidence < minConfidence` was true for ANY positive
// threshold. A safety control that can only ever fire cannot be switched on: set min_confidence on an
// auto-eligible rule and 100% of autos clamp to approve, with an audit reason reading
// "confidence 0 < min_confidence 0.6" — indistinguishable from an unconfident model, so the debugging
// starts at the rule and never reaches the wiring.
//
// Latent, not active, when found (2026-08-07): all 650 live `auto` decisions carried min_confidence=0
// and ClampConfidence returns early on a non-positive threshold, so the zero input had never mattered.
// That is exactly the condition under which a wiring defect survives — it costs nothing until the day
// someone turns the control on.

// confidenceRecordingDecider captures the EvalInput the interceptor actually composed, so the assertion
// is about what the policy layer RECEIVED rather than about what the caller believes it sent.
type confidenceRecordingDecider struct{ got []policy.EvalInput }

func (d *confidenceRecordingDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	d.got = append(d.got, in)
	return policy.NewPolicyDecision(policy.VerdictAuto, "test-recording", in.Band, nil, in.Mode, "test", policy.DecisionAudit{}), nil
}

// executeWithConfidence runs one ExecuteActivity carrying `conf` and returns the EvalInputs the policy
// layer saw.
func executeWithConfidence(t *testing.T, conf float64) []policy.EvalInput {
	t.Helper()
	ctx := context.Background()
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only)
	m := unitManifest(t)
	sink := &fakeManifestSink{}
	if err := sink.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	dec := &confidenceRecordingDecider{}
	deps := Deps{
		Interceptor: actuate.NewInterceptor(gate, &recordingActuator{}, audit.NewLedger()).
			WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto }),
		Manifests: sink,
		Mutation:  gate,
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
	}
	if _, err := NewActivities(deps).ExecuteActivity(ctx, ExecuteInput{
		ActionID: m.ActionID, ExternalRef: "TG-408-wire", PlanHash: "plan#c", TargetHost: "web01", Site: "nl",
		Band:        safety.BandAuto,
		EvidenceIDs: []string{"tr-1"},
		ToolResults: []agent.ToolResult{{ID: "tr-1", Target: "web01", Output: "web01 nginx is failed", Success: true}},
		Confidence:  conf,
	}); err != nil {
		t.Fatalf("execute must be a recorded refusal or an execution, never an error: %v", err)
	}
	if len(dec.got) == 0 {
		t.Fatal("the policy decider was never consulted — this oracle would then be asserting nothing")
	}
	return dec.got
}

// The wiring itself: the confidence the activity is handed is the confidence the policy layer compares.
func TestExecuteSiteSuppliesTheConfidenceTheClampCompares(t *testing.T) {
	got := executeWithConfidence(t, 0.84)
	if got[0].Confidence != 0.84 {
		t.Fatalf("the policy layer must receive the agent's emitted confidence, got %v — "+
			"actuate.Request.Confidence is unassigned again and the clamp is comparing a hardwired zero", got[0].Confidence)
	}
}

// The consequence, stated as the behaviour an operator would observe. With a positive threshold, a
// CONFIDENT action must retain `auto` — which is only possible if the real number arrived. Driving
// ClampConfidence with what the execute path actually delivered is the difference between testing the
// clamp and testing the control.
func TestAConfidentActionRetainsAutoUnderAPositiveThreshold(t *testing.T) {
	const minConf = 0.60
	got := executeWithConfidence(t, 0.84)

	verdict, rec := policy.ClampConfidence(policy.VerdictAuto, got[0].Confidence, minConf)
	if verdict != policy.VerdictAuto {
		t.Fatalf("0.84 >= %.2f must retain auto, got %q (%s)", minConf, verdict, rec.Reason)
	}
	if rec.Clamped {
		t.Errorf("a confident action must not be clamped: %s", rec.Reason)
	}
}

// And the clamp must still FIRE on a genuinely unconfident action, so the fix cannot be mistaken for
// disabling the control. Same threshold, same path, a low number instead of a missing one.
func TestAnUnconfidentActionIsStillClampedToApprove(t *testing.T) {
	const minConf = 0.60
	got := executeWithConfidence(t, 0.20)

	verdict, rec := policy.ClampConfidence(policy.VerdictAuto, got[0].Confidence, minConf)
	if verdict != policy.VerdictApprove {
		t.Fatalf("0.20 < %.2f must clamp auto→approve, got %q (%s)", minConf, verdict, rec.Reason)
	}
	if !rec.Clamped {
		t.Error("the clamp record must say it tightened, or the console packet-tracer cannot explain the verdict")
	}
}

// The regression that the whole file exists to catch, pinned as its own case: an execute path that
// stops supplying the field delivers 0.0, which is BELOW every positive threshold. Distinguishing
// "unconfident" from "unwired" is impossible downstream — both are the number 0 — so it has to be
// caught here, at the seam.
func TestAMissingConfidenceIsIndistinguishableFromZeroAndSoIsCaughtHere(t *testing.T) {
	got := executeWithConfidence(t, 0)
	if got[0].Confidence != 0 {
		t.Fatalf("precondition: an unsupplied confidence arrives as 0, got %v", got[0].Confidence)
	}
	// 0 clamps under any positive threshold — which is precisely why the value must be asserted at the
	// seam above rather than inferred from a verdict here.
	if v, _ := policy.ClampConfidence(policy.VerdictAuto, 0, 0.60); v != policy.VerdictApprove {
		t.Fatalf("a zero confidence must clamp — otherwise the fail-closed direction is wrong: got %q", v)
	}
}

// The seam ONE LAYER UP, and the reason this file has two oracles instead of one.
//
// The mutation that removed `Confidence: in.Confidence` from the execute site went red immediately.
// The mutation that removed `Confidence: inv.Confidence` from the WORKFLOW's ExecuteInput literal
// SURVIVED — every oracle above hands ExecuteActivity an explicit confidence, so none of them ever
// exercises the workflow→activity hop where the value is actually chosen. A guard that covers one end
// of a two-seam path leaves the other end free to break silently, which is the same defect shape this
// whole ticket is about.
//
// So this drives the REAL workflow end to end. proposeWeb01 emits confidence 0.85; the assertion is
// that 0.85 is what the policy layer compared.
func TestTheWorkflowSuppliesTheProposalsOwnConfidenceToThePolicyLayer(t *testing.T) {
	// The proposal's own confidence is 0.85, and that is the number the policy layer must compare.
	investigateThenPropose := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-408-wf","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only) so the execute branch is really reached
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	dec := &confidenceRecordingDecider{}
	deps := testDeps(investigateThenPropose...)
	deps.Mutation = gate
	deps.Interceptor = actuate.NewInterceptor(gate, act, audit.NewLedger()).
		WithPolicyDecider(dec, func() policy.Mode { return policy.ModeFullAuto })
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-408-wf", SourceID: "prometheus-dc1", AlertRule: "NginxDown",
		Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1",
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}

	// VACUITY FLOORS. Both are load-bearing: an execution that never happened, or a policy layer never
	// consulted, would let every assertion below pass over an empty set — and the mutation this oracle
	// exists to kill would survive exactly as it did the first time.
	if act.execs == 0 {
		t.Fatal("the execute path was never reached — this oracle proved nothing about the wiring")
	}
	if len(dec.got) == 0 {
		t.Fatal("the policy decider was never consulted — this oracle proved nothing about the wiring")
	}
	for i, in := range dec.got {
		if in.Confidence != 0.85 {
			t.Fatalf("decision %d: the policy layer must compare the PROPOSAL's own confidence 0.85, got %v — "+
				"the workflow's ExecuteInput dropped it, and the clamp is back to comparing a hardwired zero", i, in.Confidence)
		}
	}
}
