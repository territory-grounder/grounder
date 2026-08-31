package runner

// TransactionPlanWorkflow — the all-or-nothing multi-step repair (spec/030 T-030-3, TG-58; governance
// owner-ruled 2026-08-22: ONE approval for the whole plan, any step failure auto-compensates, a failed
// compensation pages and trips instead of pretending).
//
// The workflow adds SEQUENCE and the bank-transfer property; it adds no new way to act. Every step is
// classified with the same ClassifyActivity, sealed with the same GateActivity (its own action_id,
// INV-07 unchanged), executed with the same ExecuteActivity through the unchanged interceptor chain,
// and compensated with the same SealRollbackExecuteActivity the manual rollback and the armed
// commit-confirm revert already use. The ONLY control the plan replaces is the per-step human vote:
// exactly ONE approval — taken at the propose terminal's EXISTING vote surface, bound to
// plan:<plan_id>, presented with every step and its compensation (T-030-4) — decides the whole plan
// (REQ-3002); this workflow starts only carrying that taken approval and refuses to run without it.

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
)

// TransactionPlanStepInput is one composed step: op-class + op + target from the recipe, params
// rendered by core/plan (recipe literals + declared renamings of the trigger's screened params only).
type TransactionPlanStepInput struct {
	Ordinal    int
	OpClass    string
	Op         string
	Target     string
	Params     map[string]string
	Reversible bool
}

// TransactionPlanInput is the composed plan the orchestrator starts this workflow with. EvidenceIDs +
// ToolResults are the TRIGGER session's bound evidence (INV-11) — each step's execution cites the same
// captured grounding the single action would have cited.
type TransactionPlanInput struct {
	PlanID      string
	Recipe      string
	ExternalRef string
	Site        string
	RiskLevel   string
	Host        string // the ingest-validated incident host (the stable subject)
	Steps       []TransactionPlanStepInput
	EvidenceIDs []string
	ToolResults []agent.ToolResult
	// ApprovedVoter is the principal whose ONE well-bound approval — taken at the propose terminal's
	// existing vote surface, bound to plan:<plan_id> (REQ-3002, T-030-4) — authorized this plan. The
	// workflow REFUSES to run without it: there is exactly one vote surface, and it is not here.
	ApprovedVoter string
}

// TransactionPlanResult is the plan's terminal.
type TransactionPlanResult struct {
	PlanID           string
	Terminal         string // committed | reverted | revert-failed | refused:<gate> | denied | timeout
	StepsExecuted    int
	StepsCompensated int
	// AppliedRemaining lists the ordinals still applied at a revert-failed terminal — the exact answer
	// REQ-3005 demands the record carry.
	AppliedRemaining []int
}

// TransactionPlanWorkflow runs one approved-once, all-or-nothing plan.
func TransactionPlanWorkflow(ctx workflow.Context, in TransactionPlanInput) (TransactionPlanResult, error) {
	ctx = workflow.WithActivityOptions(ctx, runnerActivityOptions())
	var a *Activities // nil receiver — activity-name resolution only
	res := TransactionPlanResult{PlanID: in.PlanID, Terminal: "refused:input"}
	if in.PlanID == "" || len(in.Steps) < 2 {
		return res, fmt.Errorf("transaction plan: a plan needs a plan_id and at least two steps")
	}
	if in.ApprovedVoter == "" {
		// The ONE vote lives at the propose terminal (T-030-4); a start without its recorded approval is
		// not a poll-less fast path — it is an unauthorized plan, refused before anything classifies.
		return res, fmt.Errorf("transaction plan %s: no approving voter carried — a plan runs only with the one approval taken at the propose terminal (REQ-3002)", planShortID(in.PlanID))
	}

	event := func(decision, reason, actionID string, withheld bool) {
		_ = workflow.ExecuteActivity(ctx, a.PlanEventActivity, PlanEventInput{
			PlanID: in.PlanID, Decision: decision, Reason: reason, ActionID: actionID, Withheld: withheld,
		}).Get(ctx, nil)
	}

	// 1) CLASSIFY EVERY STEP FIRST (REQ-3003): each step takes the full per-step risk read — canary
	//    pins, never-auto floors, stateful/destructive derivation — before anything seals. Any
	//    classification ERROR refuses the whole plan pre-execution: a plan is never partially
	//    admissible, and an unclassifiable step is an unadmissible one (fail closed).
	for _, st := range in.Steps {
		var out riskDecision
		if err := workflow.ExecuteActivity(ctx, a.ClassifyActivity, ClassifyInput{
			ExternalRef:  in.ExternalRef,
			RiskLevel:    in.RiskLevel,
			AlertRule:    "", // a plan step carries no alert rule of its own; novelty keys on the session
			OpClass:      st.OpClass,
			Op:           st.Op,
			Host:         st.Target,
			IncidentHost: in.Host,
			Reversible:   st.Reversible,
			EvidenceIDs:  in.EvidenceIDs,
			ToolResults:  in.ToolResults,
		}).Get(ctx, &out); err != nil {
			res.Terminal = "refused:classify"
			event("plan:refused", fmt.Sprintf("step %d (%s) failed classification: %v — a plan is never partially admissible", st.Ordinal, st.OpClass, err), "", true)
			return res, nil
		}
	}

	// 2) GATE EVERY STEP: seal each step's own manifest (per-step action_id, committed prediction).
	//    Band POLL_PAUSE for every step — the plan ALWAYS takes the one human vote (REQ-3002), so no
	//    step may carry an auto band into its seal.
	actionIDs := make([]string, len(in.Steps))
	planHashes := make([]string, len(in.Steps))
	for i, st := range in.Steps {
		var gate GateResult
		if err := workflow.ExecuteActivity(ctx, a.GateActivity, GateInput{
			Proposal: proposal.Proposal{
				ExternalRef: in.ExternalRef,
				Action: manifest.Action{
					Target: st.Target, OpClass: st.OpClass, Op: st.Op,
					Reversible: st.Reversible, Params: st.Params,
				},
				EvidenceIDs: in.EvidenceIDs,
				Rationale:   fmt.Sprintf("transaction plan %s (%s) step %d of %d", in.Recipe, planShortID(in.PlanID), st.Ordinal, len(in.Steps)),
			},
			Band:     safety.BandPollPause,
			PlanHash: "plan:" + in.PlanID,
			Site:     in.Site,
		}).Get(ctx, &gate); err != nil || gate.ActionID == "" {
			res.Terminal = "refused:gate"
			event("plan:refused", fmt.Sprintf("step %d (%s) refused at the gate — nothing executed", st.Ordinal, st.OpClass), "", true)
			return res, nil
		}
		actionIDs[i] = gate.ActionID
		planHashes[i] = "plan:" + in.PlanID
	}

	// 3) Record the plan + step bindings durably (idempotent on the content-addressed id).
	if err := workflow.ExecuteActivity(ctx, a.RecordPlanActivity, RecordPlanInput{
		PlanID: in.PlanID, Recipe: in.Recipe, ExternalRef: in.ExternalRef,
		Steps: planStepBindings(in.Steps, actionIDs),
	}).Get(ctx, nil); err != nil {
		res.Terminal = "refused:record"
		event("plan:refused", "the plan rows could not be durably recorded — refusing to poll for a plan that cannot be tracked", "", true)
		return res, nil
	}
	event("plan:proposed", fmt.Sprintf("recipe %s, %d steps, session %s", in.Recipe, len(in.Steps), in.ExternalRef), "", false)

	// 4) THE ONE APPROVAL, already taken: the propose terminal's vote surface parked, bound the vote to
	//    plan:<plan_id> (misbound votes counted THERE; an elapsed window denied THERE — this workflow
	//    never starts for a denied or timed-out plan), and carried the voter here. Record the machine's
	//    walk and the ledger's story.
	_ = workflow.ExecuteActivity(ctx, a.PlanTransitionActivity, PlanTransitionInput{PlanID: in.PlanID, From: "proposed", To: "approved"}).Get(ctx, nil)
	event("plan:approved", "voter="+in.ApprovedVoter+" — one approval binds every presented step and its compensation", "", false)
	_ = workflow.ExecuteActivity(ctx, a.PlanTransitionActivity, PlanTransitionInput{PlanID: in.PlanID, From: "approved", To: "executing"}).Get(ctx, nil)

	// 5) EXECUTE IN ORDER through the unchanged chain; first failure stops the forward march.
	failedAt := -1
	for i, st := range in.Steps {
		var exec ExecuteResult
		err := workflow.ExecuteActivity(ctx, a.ExecuteActivity, ExecuteInput{
			ActionID: actionIDs[i], ExternalRef: in.ExternalRef, PlanHash: planHashes[i],
			Site: in.Site, TargetHost: st.Target, Approved: true,
			EvidenceIDs: in.EvidenceIDs, ToolResults: in.ToolResults,
		}).Get(ctx, &exec)
		if err != nil || !exec.Executed {
			failedAt = i
			event("plan:step-failed", fmt.Sprintf("step %d (%s): executed=%v err=%v — compensating %d applied step(s) in reverse", st.Ordinal, st.OpClass, exec.Executed, err, i), actionIDs[i], true)
			break
		}
		res.StepsExecuted++
		_ = workflow.ExecuteActivity(ctx, a.PlanStepTransitionActivity, PlanStepTransitionInput{PlanID: in.PlanID, Ordinal: st.Ordinal, From: "pending", To: "executed"}).Get(ctx, nil)
		event("plan:step-executed", fmt.Sprintf("step %d (%s) on %s, verdict=%s", st.Ordinal, st.OpClass, st.Target, exec.Verdict), actionIDs[i], false)
	}
	if failedAt < 0 {
		_ = workflow.ExecuteActivity(ctx, a.PlanTransitionActivity, PlanTransitionInput{PlanID: in.PlanID, From: "executing", To: "committed"}).Get(ctx, nil)
		event("plan:committed", fmt.Sprintf("%d/%d steps executed", res.StepsExecuted, len(in.Steps)), "", false)
		res.Terminal = "committed"
		return res, nil
	}

	// 6) COMPENSATE N-1..1 (REQ-3004): the plan's one approval pre-authorized these (AutoFired +
	//    ApprovedBasis — the commit-confirm armed-revert lane), each through the FULL chain.
	for i := failedAt - 1; i >= 0; i-- {
		st := in.Steps[i]
		var comp ExecuteResult
		err := workflow.ExecuteActivity(ctx, a.SealRollbackExecuteActivity, RollbackExecuteInput{
			In: RollbackInput{
				ForwardActionID: actionIDs[i], ForwardOpClass: st.OpClass, ForwardOp: st.Op,
				ForwardTarget: st.Target, ForwardParams: st.Params, ForwardReversible: st.Reversible,
				ForwardSite: in.Site, ForwardExternalRef: in.ExternalRef,
				RollbackExternalRef: in.ExternalRef + "/plan-revert",
				Operator:            "plan:" + planShortID(in.PlanID),
			},
			AutoFired: true, ApprovedBasis: true,
		}).Get(ctx, &comp)
		if err != nil || !comp.Executed {
			// 7) COMPENSATION FAILED: stop, page, trip, and say exactly what remains applied (REQ-3005).
			_ = workflow.ExecuteActivity(ctx, a.PlanStepTransitionActivity, PlanStepTransitionInput{PlanID: in.PlanID, Ordinal: st.Ordinal, From: "executed", To: "compensate-failed"}).Get(ctx, nil)
			_ = workflow.ExecuteActivity(ctx, a.PlanTransitionActivity, PlanTransitionInput{PlanID: in.PlanID, From: "executing", To: "revert-failed"}).Get(ctx, nil)
			for j := 0; j <= i; j++ {
				res.AppliedRemaining = append(res.AppliedRemaining, in.Steps[j].Ordinal)
			}
			reason := fmt.Sprintf("compensation for step %d (%s) failed (executed=%v err=%v) — steps %v REMAIN APPLIED; autonomy tripped, human summoned",
				st.Ordinal, st.OpClass, comp.Executed, err, res.AppliedRemaining)
			event("plan:revert-failed", reason, actionIDs[i], true)
			_ = workflow.ExecuteActivity(ctx, a.TripMutationBreakerActivity,
				"transaction plan "+planShortID(in.PlanID)+" revert-failed — "+reason).Get(ctx, nil)
			_ = workflow.ExecuteActivity(ctx, a.NotifyActivity, NotifyInput{
				DecisionID: in.ExternalRef,
				Body:       "TRANSACTION PLAN REVERT FAILED — " + reason,
				Approval:   false,
			}).Get(ctx, nil)
			res.Terminal = "revert-failed"
			return res, nil
		}
		res.StepsCompensated++
		_ = workflow.ExecuteActivity(ctx, a.PlanStepTransitionActivity, PlanStepTransitionInput{PlanID: in.PlanID, Ordinal: st.Ordinal, From: "executed", To: "compensated"}).Get(ctx, nil)
		event("plan:compensated", fmt.Sprintf("step %d (%s) compensated by inverse %s, verdict=%s", st.Ordinal, st.OpClass, comp.ActionID, comp.Verdict), actionIDs[i], false)
	}
	_ = workflow.ExecuteActivity(ctx, a.PlanTransitionActivity, PlanTransitionInput{PlanID: in.PlanID, From: "executing", To: "reverted"}).Get(ctx, nil)
	event("plan:reverted", fmt.Sprintf("step %d failed; %d applied step(s) compensated in reverse — all-or-nothing held", in.Steps[failedAt].Ordinal, res.StepsCompensated), "", true)
	res.Terminal = "reverted"
	return res, nil
}

// riskDecision is the minimal decode of risk.Decision this workflow needs (band only — every plan
// polls regardless; the per-step signals land on the audit rows the activity writes).
type riskDecision struct {
	Band safety.Band
}

// planShortID is the log-friendly prefix of a content-addressed plan id, safe on any length.
func planShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func planStepBindings(steps []TransactionPlanStepInput, actionIDs []string) []PlanStepBinding {
	out := make([]PlanStepBinding, len(steps))
	for i, st := range steps {
		out[i] = PlanStepBinding{Ordinal: st.Ordinal, ActionID: actionIDs[i], OpClass: st.OpClass}
	}
	return out
}
