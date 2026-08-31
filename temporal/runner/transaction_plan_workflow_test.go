package runner

// spec/030 T-030-3/T-030-4 — the all-or-nothing plan workflow's oracles (TG-58; owner-ruled 2026-08-22:
// ONE approval for the whole plan, any step failing auto-reverts everything). The claims:
//
//   1. REQ-3002 one approval binds everything: the workflow runs ONLY carrying the propose terminal's
//      taken approval (a start without it is refused before anything classifies), and every step SEALS
//      at POLL_PAUSE (no step smuggles an auto band). The vote surface itself — plan-bound votes,
//      misbound counting, elapse-denies — lives at the propose terminal: transaction_plan_poll_test.go.
//   2. REQ-3003 fail-closed admission: one unclassifiable step refuses the WHOLE plan before any
//      seal; one gate refusal refuses before any execute; an unrecordable plan refuses to execute.
//   3. REQ-3004 the bank-transfer property: step k failing compensates steps k-1..1 in REVERSE
//      order, each through SealRollbackExecuteActivity with the plan's pre-authorized basis
//      (AutoFired + ApprovedBasis) — terminal `reverted`, nothing left applied.
//   4. REQ-3005 revert-failed honesty: a failed compensation stops the unwind, TRIPS the mutation
//      breaker, PAGES with the exact applied set, and the result names AppliedRemaining.
//
// KILLING MUTATIONS (executed 2026-08-22, this file's red→green evidence):
//   - compensation loop reversed to forward order (i:=0..failedAt-1) → the reverse-order oracle
//     goes red (comps observed [act-s1 act-s2], want [act-s2 act-s1]). Restored, green.
//   - TripMutationBreakerActivity dispatch dropped from the compensate-failed arm → the
//     revert-failed oracle goes red on trips=0. Restored, green.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// tpT0303Capture records everything the mocked chain saw, so the oracles assert on ORDER and
// CONTENT, not just terminal strings.
type tpT0303Capture struct {
	mu        sync.Mutex
	gates     []GateInput
	execs     []ExecuteInput
	comps     []RollbackExecuteInput
	events    []PlanEventInput
	planMoves []PlanTransitionInput
	stepMoves []PlanStepTransitionInput
	records   []RecordPlanInput
	trips     []string
	pages     []NotifyInput
}

func (c *tpT0303Capture) eventDecisions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Decision
	}
	return out
}

// tpT0303Faults selects which seam misbehaves; zero value = the clean chain.
type tpT0303Faults struct {
	classifyErrOpClass string // ClassifyActivity errors for this op-class
	gateRefuseTarget   string // GateActivity returns ActionID:"" for this target
	execFailTarget     string // ExecuteActivity returns Executed:false for this target
	compFailForwardID  string // SealRollbackExecuteActivity fails for this forward action id
	recordErr          error  // RecordPlanActivity returns this
}

// tpT0303Env mocks the ENTIRE activity surface the plan workflow dispatches, so the workflow's
// orchestration — order, binding, compensation, terminals — is the only thing under test. Gate
// action ids are derived from the target ("act-<target>") so compensation order is assertable.
func tpT0303Env(t *testing.T, f tpT0303Faults) (*testsuite.TestWorkflowEnvironment, *tpT0303Capture) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	a := NewActivities(Deps{})
	cap := &tpT0303Capture{}
	env.RegisterWorkflow(TransactionPlanWorkflow)

	env.OnActivity(a.ClassifyActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in ClassifyInput) (risk.Decision, error) {
			if f.classifyErrOpClass != "" && in.OpClass == f.classifyErrOpClass {
				return risk.Decision{}, errors.New("tpT0303: classification unavailable for " + in.OpClass)
			}
			return risk.Decision{Band: safety.BandPollPause}, nil
		})
	env.OnActivity(a.GateActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in GateInput) (GateResult, error) {
			cap.mu.Lock()
			cap.gates = append(cap.gates, in)
			cap.mu.Unlock()
			if f.gateRefuseTarget != "" && in.Proposal.Action.Target == f.gateRefuseTarget {
				return GateResult{}, nil // a refusal is a nil-error empty seal, not a retryable fault
			}
			return GateResult{ActionID: "act-" + in.Proposal.Action.Target, PredictionHash: "pred-1"}, nil
		})
	env.OnActivity(a.RecordPlanActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in RecordPlanInput) error {
			cap.mu.Lock()
			cap.records = append(cap.records, in)
			cap.mu.Unlock()
			return f.recordErr
		})
	env.OnActivity(a.PlanTransitionActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in PlanTransitionInput) error {
			cap.mu.Lock()
			cap.planMoves = append(cap.planMoves, in)
			cap.mu.Unlock()
			return nil
		})
	env.OnActivity(a.PlanStepTransitionActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in PlanStepTransitionInput) error {
			cap.mu.Lock()
			cap.stepMoves = append(cap.stepMoves, in)
			cap.mu.Unlock()
			return nil
		})
	env.OnActivity(a.PlanEventActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in PlanEventInput) error {
			cap.mu.Lock()
			cap.events = append(cap.events, in)
			cap.mu.Unlock()
			return nil
		})
	env.OnActivity(a.ExecuteActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in ExecuteInput) (ExecuteResult, error) {
			cap.mu.Lock()
			cap.execs = append(cap.execs, in)
			cap.mu.Unlock()
			if f.execFailTarget != "" && in.TargetHost == f.execFailTarget {
				return ExecuteResult{Executed: false, Note: "tpT0303: refused"}, nil
			}
			return ExecuteResult{Executed: true, ActionID: in.ActionID, Verdict: "match"}, nil
		})
	env.OnActivity(a.SealRollbackExecuteActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in RollbackExecuteInput) (ExecuteResult, error) {
			cap.mu.Lock()
			cap.comps = append(cap.comps, in)
			cap.mu.Unlock()
			if f.compFailForwardID != "" && in.In.ForwardActionID == f.compFailForwardID {
				return ExecuteResult{Executed: false, Note: "tpT0303: compensation refused"}, nil
			}
			return ExecuteResult{Executed: true, ActionID: "inv-" + in.In.ForwardActionID, Verdict: "match"}, nil
		})
	env.OnActivity(a.TripMutationBreakerActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, reason string) error {
			cap.mu.Lock()
			cap.trips = append(cap.trips, reason)
			cap.mu.Unlock()
			return nil
		})
	env.OnActivity(a.NotifyActivity, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in NotifyInput) (NotifyResult, error) {
			cap.mu.Lock()
			cap.pages = append(cap.pages, in)
			cap.mu.Unlock()
			return NotifyResult{Delivered: true}, nil
		})
	return env, cap
}

const tpT0303PlanID = "0303plan4cafe1234deadbeef" // long enough to exercise the short-id prefixing

func tpT0303Input(n int) TransactionPlanInput {
	in := TransactionPlanInput{
		PlanID:      tpT0303PlanID,
		Recipe:      "restart-then-verify-unit",
		ExternalRef: "TG-plan-wf-1",
		Site:        "dc1",
		RiskLevel:   "medium",
		Host:        "web01",
		// the ONE approval, taken at the propose terminal (T-030-4) — the workflow refuses without it
		ApprovedVoter: "operator",
	}
	for i := 1; i <= n; i++ {
		in.Steps = append(in.Steps, TransactionPlanStepInput{
			Ordinal: i, OpClass: "restart-service", Op: "restart",
			Target: fmt.Sprintf("s%d", i), Params: map[string]string{"unit": "nginx"}, Reversible: true,
		})
	}
	return in
}

func tpT0303Result(t *testing.T, env *testsuite.TestWorkflowEnvironment) TransactionPlanResult {
	t.Helper()
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("plan workflow must complete cleanly: %v", env.GetWorkflowError())
	}
	var res TransactionPlanResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	return res
}

// Claim 1: the carried approval commits the whole plan — every step sealed at POLL_PAUSE, every
// execute pre-approved by THE vote, the plan machine walked proposed→approved→executing→committed.
func TestTransactionPlanOneApprovalExecutesEveryStepInOrder(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(2))
	res := tpT0303Result(t, env)
	if res.Terminal != "committed" || res.StepsExecuted != 2 || res.StepsCompensated != 0 {
		t.Fatalf("want committed 2/0, got %+v", res)
	}
	if len(cap.gates) != 2 {
		t.Fatalf("every step seals its OWN manifest, got %d seals", len(cap.gates))
	}
	for i, g := range cap.gates {
		if g.Band != safety.BandPollPause {
			// VACUITY GUARD too: an auto band here would let a step execute without THE vote.
			t.Errorf("step %d sealed at band %v — every plan step must seal POLL_PAUSE (REQ-3002)", i+1, g.Band)
		}
	}
	if len(cap.execs) != 2 || cap.execs[0].ActionID != "act-s1" || cap.execs[1].ActionID != "act-s2" {
		t.Fatalf("execution must run the composed order [act-s1 act-s2], got %+v", cap.execs)
	}
	for _, e := range cap.execs {
		if !e.Approved {
			t.Errorf("step %s executed without the plan's one approval carried (REQ-3002)", e.ActionID)
		}
	}
	wantMoves := []PlanTransitionInput{
		{PlanID: tpT0303PlanID, From: "proposed", To: "approved"},
		{PlanID: tpT0303PlanID, From: "approved", To: "executing"},
		{PlanID: tpT0303PlanID, From: "executing", To: "committed"},
	}
	if len(cap.planMoves) != len(wantMoves) {
		t.Fatalf("plan machine walked %+v, want %+v", cap.planMoves, wantMoves)
	}
	for i, m := range wantMoves {
		if cap.planMoves[i] != m {
			t.Fatalf("plan move %d = %+v, want %+v", i, cap.planMoves[i], m)
		}
	}
	if len(cap.records) != 1 || len(cap.records[0].Steps) != 2 || cap.records[0].Steps[1].ActionID != "act-s2" {
		t.Fatalf("the durable record must bind each step's sealed action id, got %+v", cap.records)
	}
	if ds := cap.eventDecisions(); !strings.Contains(strings.Join(ds, " "), "plan:approved") ||
		!strings.Contains(strings.Join(ds, " "), "plan:committed") {
		t.Fatalf("the ledger story must carry plan:approved and plan:committed, got %v", ds)
	}
}

// Claim 1 (the other half): a start WITHOUT the taken approval is refused before anything runs —
// there is exactly one vote surface (the propose terminal) and this workflow is not it.
func TestTransactionPlanRefusesToRunWithoutTheTakenApproval(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{})
	in := tpT0303Input(2)
	in.ApprovedVoter = ""
	env.ExecuteWorkflow(TransactionPlanWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
		t.Fatal("an unapproved plan must be refused with an error")
	}
	if len(cap.gates) != 0 || len(cap.execs) != 0 || len(cap.records) != 0 {
		t.Fatalf("an unapproved plan must classify/seal/record/execute NOTHING, got gates=%d execs=%d records=%d",
			len(cap.gates), len(cap.execs), len(cap.records))
	}
}

// Claim 2a: one unclassifiable step refuses the WHOLE plan — before any seal, vote, or execute.
func TestTransactionPlanOneUnclassifiableStepRefusesTheWholePlan(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{classifyErrOpClass: "restart-service"})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(2))
	res := tpT0303Result(t, env)
	if res.Terminal != "refused:classify" {
		t.Fatalf("want refused:classify, got %+v", res)
	}
	if len(cap.gates) != 0 || len(cap.execs) != 0 || len(cap.records) != 0 {
		t.Fatalf("a refused plan must seal/record/execute NOTHING, got gates=%d execs=%d records=%d",
			len(cap.gates), len(cap.execs), len(cap.records))
	}
}

// Claim 2b: one gate refusal refuses the plan before anything executes.
func TestTransactionPlanGateRefusalRefusesBeforeExecution(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{gateRefuseTarget: "s2"})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(2))
	res := tpT0303Result(t, env)
	if res.Terminal != "refused:gate" || len(cap.execs) != 0 {
		t.Fatalf("want refused:gate with 0 executes, got %+v (%d executes)", res, len(cap.execs))
	}
}

// Claim 2c: a plan that cannot be durably recorded refuses to poll (it must not run untracked).
func TestTransactionPlanUnrecordablePlanRefusesToPoll(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{recordErr: errors.New("tpT0303: store down")})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(2))
	res := tpT0303Result(t, env)
	if res.Terminal != "refused:record" || len(cap.execs) != 0 {
		t.Fatalf("want refused:record with 0 executes, got %+v (%d executes)", res, len(cap.execs))
	}
}

// Claim 3: step 3 failing compensates steps 2 then 1 — REVERSE order, each with the plan's
// pre-authorized basis, terminal `reverted`, and the step machine walked to compensated.
func TestTransactionPlanStepFailureCompensatesInReverseOrder(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{execFailTarget: "s3"})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(3))
	res := tpT0303Result(t, env)
	if res.Terminal != "reverted" || res.StepsExecuted != 2 || res.StepsCompensated != 2 {
		t.Fatalf("want reverted 2 executed / 2 compensated, got %+v", res)
	}
	if len(cap.comps) != 2 || cap.comps[0].In.ForwardActionID != "act-s2" || cap.comps[1].In.ForwardActionID != "act-s1" {
		got := []string{}
		for _, c := range cap.comps {
			got = append(got, c.In.ForwardActionID)
		}
		t.Fatalf("compensation must unwind in REVERSE order [act-s2 act-s1], got %v", got)
	}
	for _, c := range cap.comps {
		if !c.AutoFired || !c.ApprovedBasis {
			t.Errorf("compensation for %s must carry the plan's pre-authorized basis (AutoFired+ApprovedBasis), got %+v",
				c.In.ForwardActionID, c)
		}
		if !strings.HasSuffix(c.In.RollbackExternalRef, "/plan-revert") {
			t.Errorf("compensation ref must mark the plan-revert lane, got %q", c.In.RollbackExternalRef)
		}
	}
	if len(res.AppliedRemaining) != 0 {
		t.Fatalf("a clean revert leaves NOTHING applied, got %v", res.AppliedRemaining)
	}
	// The failed step (3) never executed, so it must never move past pending; steps 1–2 must reach
	// compensated.
	for _, m := range cap.stepMoves {
		if m.Ordinal == 3 {
			t.Fatalf("the FAILED step must stay pending in the step machine, saw %+v", m)
		}
	}
}

// Claim 4: a failed compensation stops the unwind, trips the breaker, pages with the applied set,
// and the result names exactly which steps remain applied.
func TestTransactionPlanFailedCompensationTripsPagesAndNamesTheAppliedSet(t *testing.T) {
	// Step 3 fails forward; compensation for step 2 (the FIRST unwind) fails ⇒ steps 1 and 2 remain.
	env, cap := tpT0303Env(t, tpT0303Faults{execFailTarget: "s3", compFailForwardID: "act-s2"})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(3))
	res := tpT0303Result(t, env)
	if res.Terminal != "revert-failed" {
		t.Fatalf("want revert-failed, got %+v", res)
	}
	if len(res.AppliedRemaining) != 2 || res.AppliedRemaining[0] != 1 || res.AppliedRemaining[1] != 2 {
		t.Fatalf("REQ-3005: the result must name steps [1 2] as remaining applied, got %v", res.AppliedRemaining)
	}
	if len(cap.trips) != 1 || !strings.Contains(cap.trips[0], "revert-failed") {
		t.Fatalf("REQ-3005: the mutation breaker must trip exactly once with the revert-failed reason, got %v", cap.trips)
	}
	if len(cap.pages) != 1 || !strings.Contains(cap.pages[0].Body, "REMAIN APPLIED") ||
		!strings.Contains(cap.pages[0].Body, "[1 2]") {
		t.Fatalf("the page must name the exact applied set, got %+v", cap.pages)
	}
	if len(cap.comps) != 1 {
		t.Fatalf("the unwind must STOP at the failed compensation (state unknown — do not thrash), got %d", len(cap.comps))
	}
	sawPlanFail, sawStepFail := false, false
	for _, m := range cap.planMoves {
		if m.From == "executing" && m.To == "revert-failed" {
			sawPlanFail = true
		}
	}
	for _, m := range cap.stepMoves {
		if m.Ordinal == 2 && m.From == "executed" && m.To == "compensate-failed" {
			sawStepFail = true
		}
	}
	if !sawPlanFail || !sawStepFail {
		t.Fatalf("the durable machines must record the failure (plan→revert-failed %v, step2→compensate-failed %v)",
			sawPlanFail, sawStepFail)
	}
}

// The input guard: a plan needs at least two steps — one step is a single action and must use the
// single-action lane, not this workflow.
func TestTransactionPlanRefusesFewerThanTwoSteps(t *testing.T) {
	env, _ := tpT0303Env(t, tpT0303Faults{})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(1))
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
		t.Fatal("a one-step plan must be refused with an error")
	}
}

// REQ-3006 (T-030-5): the ledger tells the WHOLE story under one plan — every transition present in
// order for a reverted run, every per-step event carrying that step's own action id, verdicts and the
// inverse's identity in the narration. (The activity-level full-plan_id claim is the next test.)
func TestTransactionPlanLedgerStoryCoversEveryTransition(t *testing.T) {
	env, cap := tpT0303Env(t, tpT0303Faults{execFailTarget: "s3"})
	env.ExecuteWorkflow(TransactionPlanWorkflow, tpT0303Input(3))
	res := tpT0303Result(t, env)
	if res.Terminal != "reverted" {
		t.Fatalf("fixture must revert (step 3 fails), got %+v", res)
	}
	want := []string{"plan:proposed", "plan:approved", "plan:step-executed", "plan:step-executed",
		"plan:step-failed", "plan:compensated", "plan:compensated", "plan:reverted"}
	got := cap.eventDecisions()
	if len(got) != len(want) {
		t.Fatalf("the story must cover every transition, want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d: want %s got %s (full story %v)", i, want[i], got[i], got)
		}
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	sawVerdict, sawInverse := false, false
	for _, e := range cap.events {
		switch e.Decision {
		case "plan:step-executed", "plan:step-failed", "plan:compensated":
			if e.ActionID == "" {
				t.Errorf("REQ-3006: per-step event %s must carry the step's own action id, got empty (reason %q)", e.Decision, e.Reason)
			}
		}
		if e.Decision == "plan:step-executed" && strings.Contains(e.Reason, "verdict=match") {
			sawVerdict = true
		}
		if e.Decision == "plan:compensated" && strings.Contains(e.Reason, "inverse inv-act-s2") {
			sawInverse = true
		}
	}
	if !sawVerdict || !sawInverse {
		t.Fatalf("the narration must carry each verdict and the inverse's identity (verdict=%v inverse=%v)", sawVerdict, sawInverse)
	}
}

// tpT0305Sink records governance-ledger appends for the REQ-3006 activity oracle (distinct name —
// this package has collided on test helpers before).
type tpT0305Sink struct {
	mu      sync.Mutex
	entries []audit.LedgerEntry
}

func (s *tpT0305Sink) Persist(e audit.LedgerEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *tpT0305Sink) rows() []audit.LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.LedgerEntry(nil), s.entries...)
}

// REQ-3006 at the append: the governance-ledger row carries the step's action id in the ActionID
// column (plan-level events fall back to plan/<plan_id>) and the FULL plan id — never a truncated
// prefix — in the reason, so the spine answers "what did this plan do" by the id the approval bound.
func TestPlanEventAppendsTheFullPlanIDAndTheStepAction(t *testing.T) {
	const fullID = "0303planfeedfacecafebeefdeadc0dedeadbeeffeedfacecafebeefdeadc0de"
	sink := &tpT0305Sink{}
	a := NewActivities(Deps{Ledger: audit.NewLedger().WithSink(sink)})
	ctx := context.Background()
	if err := a.PlanEventActivity(ctx, PlanEventInput{PlanID: fullID, Decision: "plan:step-executed", Reason: "step 1", ActionID: "act-1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := a.PlanEventActivity(ctx, PlanEventInput{PlanID: fullID, Decision: "plan:committed", Reason: "2/2"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries := sink.rows()
	if len(entries) != 2 {
		t.Fatalf("want 2 ledger rows, got %d", len(entries))
	}
	if entries[0].ActionID != "act-1" || !strings.Contains(entries[0].Reason, fullID) {
		t.Fatalf("the per-step row must carry the step action id AND the FULL plan id, got %+v", entries[0])
	}
	if entries[1].ActionID != "plan/"+fullID {
		t.Fatalf("a plan-level row must fall back to plan/<plan_id>, got %q", entries[1].ActionID)
	}
	if err := (&Activities{D: Deps{}}).PlanEventActivity(ctx, PlanEventInput{PlanID: fullID, Decision: "plan:committed"}); err == nil {
		t.Fatal("a deployment without the one accountability spine must not run plans — nil ledger must error")
	}
}
