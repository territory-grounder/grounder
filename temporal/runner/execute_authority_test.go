package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	"github.com/territory-grounder/grounder/temporal/runner/runnertest"
)

// TG-660 part c / pack item 2 — ExecuteActivity re-derives its authority from DURABLE records and refuses when
// they are missing, mismatched or spent. These oracles use STRICT fakes (nothing is answered that was not
// recorded), unlike the permissive helper the chain oracles use.
//
// KILLING MUTATIONS (each executed by hand on 2026-11-07 and recorded in the TG-660 delivery bar):
//   - restoring the old `if perr == nil && ok { pred = ... }` (proceed on a missing prediction) turned
//     TestExecuteRefusesWithoutACommittedPrediction RED: the action EXECUTED with an empty prediction.
//   - deleting the 1d prediction-binding block turned TestExecuteRefusesAPredictionThatNamesAnotherExecution RED
//     in all four sub-cases: a row for another session/action/target/site executed.
//   - `effBand := in.Band` (trusting the input band) turned TestExecuteRefusesABandLooserThanTheRecord RED: an
//     AUTO claim over a POLL_PAUSE record executed with no approval row.
//   - `approved := in.Approved` turned TestExecuteAdmitsOnDurableRecords RED in both directions.
//   - deleting the 1b already-executed check turned TestExecuteRefusesAnAlreadyExecutedAction RED: the spent
//     approval executed a second time.
//   - deleting the 1c binding (`approvalID = in.ApprovalActionID` unconditionally) turned
//     TestExecuteRefusesABorrowedApproval RED: another action's approval row admitted this one.

// captureLedger records every appended governance entry (an audit.LedgerSink).
type captureLedger struct{ entries []audit.LedgerEntry }

func (c *captureLedger) Persist(e audit.LedgerEntry) error {
	c.entries = append(c.entries, e)
	return nil
}

func (c *captureLedger) has(prefix string) bool {
	for _, e := range c.entries {
		if strings.HasPrefix(e.Decision, prefix) {
			return true
		}
	}
	return false
}

// erroringPredictions is a PredictionReader whose store is unreadable.
type erroringPredictions struct{}

func (erroringPredictions) Get(context.Context, string) (predict.PredictionRecord, bool, error) {
	return predict.PredictionRecord{}, false, errors.New("pg: connection refused")
}

// erroringExecutions is an execution reader whose store is unreadable.
type erroringExecutions struct{}

func (erroringExecutions) ExecutionFor(context.Context, string, string) (db.ForwardExecution, bool, error) {
	return db.ForwardExecution{}, false, errors.New("pg: connection refused")
}

type authorityRig struct {
	deps   Deps
	act    *recordingActuator
	led    *captureLedger
	preds  *runnertest.Predictions
	appr   *runnertest.Approvals
	risk   *runnertest.RiskAudits
	execs  *runnertest.Executions
	steps  *runnertest.PlanSteps
	action string
}

const authRef, authPlan, authHost, authSite = "TG-660-exec", "plan#660", "web01", "nl"

// strictRig is the gate_stage rig with STRICT authority fakes: a still-faulted host (the necessity gate passes),
// mutation ON, a permissive policy, a sealed restart — so the ONLY thing that can refuse is the authority re-derivation.
func strictRig(t *testing.T) *authorityRig {
	t.Helper()
	ctx := context.Background()
	choke := safety.NewActuatingChokepoint()
	m := unitManifest(t)
	sink := &fakeManifestSink{}
	if err := sink.Seal(ctx, m); err != nil {
		t.Fatalf("seal: %v", err)
	}
	r := &authorityRig{
		act:    &recordingActuator{},
		led:    &captureLedger{},
		preds:  &runnertest.Predictions{},
		appr:   &runnertest.Approvals{},
		risk:   &runnertest.RiskAudits{Absent: true},
		execs:  &runnertest.Executions{},
		steps:  &runnertest.PlanSteps{},
		action: m.ActionID,
	}
	r.deps = Deps{
		Interceptor: withPermissivePolicy(actuate.NewInterceptor(choke, r.act, audit.NewLedger())),
		Manifests:   sink,
		Mutation:    choke,
		Ledger:      audit.NewLedger().WithSink(r.led),
		PostStateObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{}, true
		},
		ClearObserve: func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: authHost, Rule: "NginxDown", Site: authSite}}, true
		},
		Predictions: r.preds,
		Approvals:   r.appr,
		RiskAudits:  r.risk,
		Executions:  r.execs,
		PlanSteps:   r.steps,
	}
	return r
}

// predicted commits the record the gate would have committed for this rig's action in the session.
func (r *authorityRig) predicted() { r.preds.For(authRef, r.action, authPlan, authHost, authSite) }

func (r *authorityRig) run(t *testing.T, in ExecuteInput) ExecuteResult {
	t.Helper()
	in.ActionID, in.TargetHost, in.Site = r.action, authHost, authSite
	in.EvidenceIDs = []string{"tr-1"}
	in.ToolResults = []agent.ToolResult{{ID: "tr-1", Target: authHost, Output: "web01 nginx is failed", Success: true}}
	res, err := NewActivities(r.deps).ExecuteActivity(context.Background(), in)
	if err != nil {
		t.Fatalf("ExecuteActivity must be a recorded refusal or an execution, never an error: %v", err)
	}
	return res
}

func expectRefusal(t *testing.T, r *authorityRig, res ExecuteResult, why string) {
	t.Helper()
	if res.Executed || r.act.execs != 0 {
		t.Fatalf("expected a refusal (%s) but the action EXECUTED: %+v execs=%d", why, res, r.act.execs)
	}
	if !strings.Contains(res.Note, why) {
		t.Fatalf("refusal note must name %q, got %q", why, res.Note)
	}
	if !r.led.has("actuate:refuse:" + why) {
		t.Fatalf("the refusal must be ledgered as actuate:refuse:%s; ledger=%v", why, decisions(r.led))
	}
}

func expectExecuted(t *testing.T, r *authorityRig, res ExecuteResult, what string) {
	t.Helper()
	if !res.Executed || r.act.execs != 1 {
		t.Fatalf("%s must execute: %+v execs=%d ledger=%v", what, res, r.act.execs, decisions(r.led))
	}
	if r.led.has("actuate:refuse:") {
		t.Fatalf("no refusal may be ledgered on the admitted path: %v", decisions(r.led))
	}
}

func decisions(c *captureLedger) []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Decision)
	}
	return out
}

// autoInput is a fully-bound AUTO execution of the rig's action.
func autoInput() ExecuteInput {
	return ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandAuto}
}

// --- the prediction: missing, unreadable, malformed, mismatched ---

func TestExecuteRefusesWithoutACommittedPrediction(t *testing.T) {
	r := strictRig(t)
	r.risk.Set(authRef, r.action, safety.BandAuto, authPlan)
	expectRefusal(t, r, r.run(t, autoInput()), "no-committed-prediction")
	// an empty plan hash is the same refusal — the gate never ran for this plan
	r2 := strictRig(t)
	r2.predicted()
	in := autoInput()
	in.PlanHash = ""
	expectRefusal(t, r2, r2.run(t, in), "no-committed-prediction")
	// and so is an unwired store
	r3 := strictRig(t)
	r3.deps.Predictions = nil
	expectRefusal(t, r3, r3.run(t, autoInput()), "no-committed-prediction")
}

func TestExecuteRefusesWhenThePredictionStoreIsUnreadable(t *testing.T) {
	r := strictRig(t)
	r.deps.Predictions = erroringPredictions{}
	expectRefusal(t, r, r.run(t, autoInput()), "prediction-unreadable")
}

// A MALFORMED row — the reader hands back a record that is not for the hash it was asked for (an empty or foreign
// PlanHash) — is a mismatch, never an implicit zero value that executes.
func TestExecuteRefusesAMalformedPrediction(t *testing.T) {
	r := strictRig(t)
	r.risk.Set(authRef, r.action, safety.BandAuto, authPlan)
	r.preds.Commit(authPlan, predict.PredictionRecord{})
	expectRefusal(t, r, r.run(t, autoInput()), "prediction-mismatch")
	r2 := strictRig(t)
	r2.risk.Set(authRef, r2.action, safety.BandAuto, authPlan)
	r2.preds.Commit(authPlan, predict.PredictionRecord{
		Prediction:  verify.Prediction{ActionID: r2.action, PlanHash: "plan#other", TargetHost: authHost, Site: authSite},
		ExternalRef: authRef,
	})
	expectRefusal(t, r2, r2.run(t, autoInput()), "prediction-mismatch")
}

// THE PREDICTION MUST NAME THIS EXECUTION: a row under the right hash that names another session, action, target
// or site is a reader defect or a forgery — refused, with the field named.
func TestExecuteRefusesAPredictionThatNamesAnotherExecution(t *testing.T) {
	cases := map[string]func(r *authorityRig){
		"session": func(r *authorityRig) { r.preds.For("TG-another-session", r.action, authPlan, authHost, authSite) },
		"action":  func(r *authorityRig) { r.preds.For(authRef, "act-somebody-else", authPlan, authHost, authSite) },
		"target":  func(r *authorityRig) { r.preds.For(authRef, r.action, authPlan, "db01", authSite) },
		"site":    func(r *authorityRig) { r.preds.For(authRef, r.action, authPlan, authHost, "gr") },
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			r := strictRig(t)
			r.risk.Set(authRef, r.action, safety.BandAuto, authPlan)
			seed(r)
			res := r.run(t, autoInput())
			expectRefusal(t, r, res, "prediction-mismatch")
			if !strings.Contains(res.Note, name) {
				t.Fatalf("the refusal must name the mismatched field %q: %q", name, res.Note)
			}
		})
	}
}

// --- the classification record ---

func TestExecuteRefusesWithoutAClassificationRecord(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	expectRefusal(t, r, r.run(t, autoInput()), "no-classification-record")
}

// A single action reads ONLY a row that names it: the session's latest row for some other action cannot lend
// its band (the fallback exists for transaction-plan steps, which are bound to their plan first).
func TestExecuteRefusesTheSessionFallbackRecordForASingleAction(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	r.risk.Set(authRef, "", safety.BandAuto, authPlan)
	expectRefusal(t, r, r.run(t, autoInput()), "no-classification-record")
}

func TestExecuteRefusesAPlanHashTheClassifierDidNotCommit(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	r.risk.Set(authRef, r.action, safety.BandAuto, "plan#somebody-elses")
	expectRefusal(t, r, r.run(t, autoInput()), "plan-hash-mismatch")
}

// THE FORGERY: the input claims AUTO, the classifier recorded POLL_PAUSE, nobody approved. Before TG-660 the
// input band was the band — this executed.
func TestExecuteRefusesABandLooserThanTheRecord(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
	in := autoInput()
	in.Approved = true
	expectRefusal(t, r, r.run(t, in), "no-approval-record")
}

// --- the approval ---

func TestExecuteRefusesAPollPauseActionWithNoApprovalRow(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
	// Approved=true on the INPUT is exactly the claim that must not be trusted.
	expectRefusal(t, r, r.run(t, ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, Approved: true}), "no-approval-record")
}

// An approval authorizes ONLY the action it names. ApprovalActionID may point elsewhere only at a transaction
// plan of which this action is a durably recorded step — and then the prediction must be the plan's.
func TestExecuteRefusesABorrowedApproval(t *testing.T) {
	t.Run("another action's approval row", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		r.appr.Record("act-the-approved-one", authRef, "human:approve")
		in := ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, ApprovalActionID: "act-the-approved-one"}
		expectRefusal(t, r, r.run(t, in), "approval-unbound")
	})
	t.Run("a plan this action is not a step of", func(t *testing.T) {
		r := strictRig(t)
		r.preds.For(authRef, "act-step-1", "plan:p-660", authHost, authSite)
		r.risk.Set(authRef, "", safety.BandPollPause, "")
		r.appr.Record("plan:p-660", authRef, "human:approve")
		r.steps.Add("p-660", "act-step-1")
		in := ExecuteInput{ExternalRef: authRef, PlanHash: "plan:p-660", Band: safety.BandPollPause, ApprovalActionID: "plan:p-660"}
		expectRefusal(t, r, r.run(t, in), "approval-unbound")
	})
	t.Run("a plan step whose input names a prediction other than the plan's", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, "", safety.BandPollPause, "")
		r.appr.Record("plan:p-660", authRef, "human:approve")
		r.steps.Add("p-660", r.action)
		in := ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, ApprovalActionID: "plan:p-660"}
		expectRefusal(t, r, r.run(t, in), "plan-hash-mismatch")
	})
	t.Run("an empty plan id", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		in := ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, ApprovalActionID: "plan:"}
		expectRefusal(t, r, r.run(t, in), "approval-unbound")
	})
}

// A spent authorization: the action already executed in this session (action_execution holds its row). A second
// ExecuteActivity for the same (action, session) is a replay, never a legitimate run.
func TestExecuteRefusesAnAlreadyExecutedAction(t *testing.T) {
	r := strictRig(t)
	r.predicted()
	r.risk.Set(authRef, r.action, safety.BandAuto, authPlan)
	r.execs.Record(r.action, authRef)
	expectRefusal(t, r, r.run(t, autoInput()), "already-executed")
	// the same action in ANOTHER session is a fresh authorization (its own prediction, classification, approval)
	r2 := strictRig(t)
	r2.predicted()
	r2.risk.Set(authRef, r2.action, safety.BandAuto, authPlan)
	r2.execs.Record(r2.action, "TG-an-earlier-session")
	expectExecuted(t, r2, r2.run(t, autoInput()), "an action spent in another session")
	// an unreadable execution store is not evidence of freshness
	r3 := strictRig(t)
	r3.predicted()
	r3.risk.Set(authRef, r3.action, safety.BandAuto, authPlan)
	r3.deps.Executions = erroringExecutions{}
	expectRefusal(t, r3, r3.run(t, autoInput()), "executions-unreadable")
}

func TestExecuteRefusesWhenTheAuthorityReadersAreUnwired(t *testing.T) {
	seeded := func(t *testing.T) *authorityRig {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		r.appr.Record(r.action, authRef, "human:approve")
		return r
	}
	r := seeded(t)
	r.deps.RiskAudits = nil
	expectRefusal(t, r, r.run(t, autoInput()), "authority-records-unwired")
	r = seeded(t)
	r.deps.Approvals = nil
	expectRefusal(t, r, r.run(t, ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause}), "authority-records-unwired")
	r = seeded(t)
	r.deps.Executions = nil
	expectRefusal(t, r, r.run(t, autoInput()), "authority-records-unwired")
	r = seeded(t)
	r.preds.For(authRef, r.action, "plan:p-660", authHost, authSite)
	r.deps.PlanSteps = nil
	r.steps = nil
	in := ExecuteInput{ExternalRef: authRef, PlanHash: "plan:p-660", Band: safety.BandPollPause, ApprovalActionID: "plan:p-660"}
	expectRefusal(t, r, r.run(t, in), "authority-records-unwired")
}

// The positive paths: every record present ⇒ the chain runs. Approved is read from the LEDGER, not the input.
func TestExecuteAdmitsOnDurableRecords(t *testing.T) {
	t.Run("AUTO band with a committed prediction needs no approval", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandAuto, authPlan)
		expectExecuted(t, r, r.run(t, autoInput()), "an AUTO action with every record present")
	})
	t.Run("POLL_PAUSE executes on a human:approve row even when the input says Approved=false", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		r.appr.Record(r.action, authRef, "human:approve")
		expectExecuted(t, r, r.run(t, ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, Approved: false}), "the ledger row, not the input bool,")
	})
	t.Run("POLL_PAUSE executes on the fallback approver's row", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		r.appr.Record(r.action, authRef, "fallback:autonomous-approve:ood-novel-incident")
		expectExecuted(t, r, r.run(t, ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause}), "the autonomous approver's record")
	})
	t.Run("an approval recorded against another session does not count", func(t *testing.T) {
		r := strictRig(t)
		r.predicted()
		r.risk.Set(authRef, r.action, safety.BandPollPause, authPlan)
		r.appr.Record(r.action, "TG-some-other-session", "human:approve")
		expectRefusal(t, r, r.run(t, ExecuteInput{ExternalRef: authRef, PlanHash: authPlan, Band: safety.BandPollPause, Approved: true}), "no-approval-record")
	})
	t.Run("a transaction plan's one approval binds its recorded steps through ApprovalActionID", func(t *testing.T) {
		r := strictRig(t)
		// the plan's one prediction row is the FIRST step's (first-wins under plan:<id>); this action is step 2
		r.preds.For(authRef, "act-step-1", "plan:p-660", "db01", authSite)
		r.risk.Set(authRef, "", safety.BandPollPause, "")
		r.appr.Record("plan:p-660", authRef, "human:approve")
		r.steps.Add("p-660", "act-step-1")
		r.steps.Add("p-660", r.action)
		in := ExecuteInput{ExternalRef: authRef, PlanHash: "plan:p-660", Approved: true, ApprovalActionID: "plan:p-660"}
		expectExecuted(t, r, r.run(t, in), "a recorded plan step on the plan's approval row")
	})
}

// The input may be MORE restrictive than the record, never looser.
func TestMostRestrictiveBand(t *testing.T) {
	cases := []struct{ a, b, want safety.Band }{
		{safety.BandAuto, safety.BandPollPause, safety.BandPollPause},
		{safety.BandPollPause, safety.BandAuto, safety.BandPollPause},
		{safety.BandAuto, safety.BandAutoNotice, safety.BandAutoNotice},
		{safety.BandAutoNotice, safety.BandAuto, safety.BandAutoNotice},
		{safety.BandAuto, safety.BandAuto, safety.BandAuto},
		// an invalid band ranks as POLL_PAUSE (fail closed)
		{safety.Band(99), safety.BandAuto, safety.Band(99)},
	}
	for _, c := range cases {
		if got := mostRestrictiveBand(c.a, c.b); got != c.want {
			t.Errorf("mostRestrictiveBand(%v,%v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
