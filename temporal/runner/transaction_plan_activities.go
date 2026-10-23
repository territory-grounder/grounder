package runner

// The transaction plan's four small activities (spec/030 T-030-3): the durable plan rows and the
// ledger events. Each is a thin seam over Deps — the state machines themselves live in the store
// (forward-only CAS, core/db), and the workflow owns the order.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/plan"
)

// ComposePlanInput is the proposed single action's already-screened facts — the ONLY inputs a plan may
// derive from (REQ-3001: the model can at most SELECT a recipe by op-class; params flow only through the
// recipe's declared literals/renamings).
type ComposePlanInput struct {
	OpClass     string
	Target      string
	Params      map[string]string
	ExternalRef string
}

// ComposePlanResult is the composed offer, or Matched=false — and EVERY failure direction (no recipe,
// bad catalog, unrenderable param, unknown step class) is Matched=false: the session falls back to the
// single action exactly as today, never to a half-composed plan.
type ComposePlanResult struct {
	Matched   bool
	PlanID    string
	Recipe    string
	Steps     []TransactionPlanStepInput
	PollLines []string // one line per step: the step AND its compensation, rendered for the vote surface (REQ-3002)
}

// ComposePlanActivity looks up the recipe the proposed op-class triggers (pure, name-ordered, REQ-3001)
// and renders the whole plan — steps, params, poll lines, and the content-addressed plan_id the ONE
// approval will bind. It runs as an activity so the lookup lands in workflow HISTORY: a recipe declared
// in a later deploy can never change what an in-flight session already offered its human.
func (a *Activities) ComposePlanActivity(_ context.Context, in ComposePlanInput) (ComposePlanResult, error) {
	catalog := a.D.PlanRecipes
	if catalog == nil {
		catalog = plan.All
	}
	all, err := catalog()
	if err != nil {
		log.Printf("transaction plan compose %s: recipe catalog invalid (%v) — single action as today", in.ExternalRef, err)
		return ComposePlanResult{}, nil
	}
	r, found := plan.ForTriggerIn(all, in.OpClass)
	if !found {
		return ComposePlanResult{}, nil
	}
	res := ComposePlanResult{Recipe: r.Name, Steps: make([]TransactionPlanStepInput, len(r.Steps)), PollLines: make([]string, len(r.Steps))}
	for i, st := range r.Steps {
		spec, ok := opschema.Lookup(st.OpClass)
		if !ok {
			log.Printf("transaction plan compose %s: recipe %s step %d names unregistered op-class %q — single action as today", in.ExternalRef, r.Name, i+1, st.OpClass)
			return ComposePlanResult{}, nil
		}
		params, perr := st.StepParams(in.Params)
		if perr != nil {
			log.Printf("transaction plan compose %s: recipe %s did not render (%v) — single action as today", in.ExternalRef, r.Name, perr)
			return ComposePlanResult{}, nil
		}
		res.Steps[i] = TransactionPlanStepInput{
			Ordinal: i + 1, OpClass: st.OpClass, Op: spec.Op, Target: in.Target,
			Params: params, Reversible: spec.SafetyTier == opschema.TierLowReversible,
		}
		res.PollLines[i] = fmt.Sprintf("step %d/%d: %s %s on %s (%s) — undo: %s",
			i+1, len(r.Steps), spec.Op, st.OpClass, in.Target, renderPlanParams(params), planUndoSummary(spec))
	}
	id, ierr := plan.PlanID(r, in.Params)
	if ierr != nil {
		log.Printf("transaction plan compose %s: plan id did not derive (%v) — single action as today", in.ExternalRef, ierr)
		return ComposePlanResult{}, nil
	}
	res.Matched, res.PlanID = true, id
	return res, nil
}

// renderPlanParams renders params deterministically (sorted) — this string reaches workflow history and
// the human poll, so its byte order must not depend on map iteration.
func renderPlanParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + params[k]
	}
	return strings.Join(parts, " ")
}

// planUndoSummary states the step's compensation honestly from the ONE static authority (opschema): the
// declared rollback template, or the idempotent re-run that SafelyCompensatable admitted it for.
func planUndoSummary(spec opschema.OpClassSpec) string {
	if len(spec.RollbackTemplate) > 0 {
		return "declared rollback: " + strings.Join(spec.RollbackTemplate, " ")
	}
	return "re-run " + spec.Op + " (idempotent restore)"
}

// PlanStore is the durable plan-row seam (db.TransactionPlanStore satisfies it). nil ⇒ the plan lane
// refuses to record and therefore refuses to poll — a plan that cannot be tracked must not run.
type PlanStore interface {
	Create(ctx context.Context, planID, recipe, externalRef string, steps []PlanStepRecord) error
	Transition(ctx context.Context, planID, from, to string) (bool, error)
	TransitionStep(ctx context.Context, planID string, ordinal int, from, to string) (bool, error)
}

// PlanStepRecord mirrors db.PlanStep's create fields without importing core/db here.
type PlanStepRecord struct {
	Ordinal  int
	ActionID string
	OpClass  string
}

// PlanStepBinding is the workflow-side step binding (ordinal + sealed action id + op-class).
type PlanStepBinding struct {
	Ordinal  int
	ActionID string
	OpClass  string
}

// RecordPlanInput creates the plan + step rows (idempotent on the content-addressed plan_id).
type RecordPlanInput struct {
	PlanID      string
	Recipe      string
	ExternalRef string
	Steps       []PlanStepBinding
}

// RecordPlanActivity records the composed plan durably. FAIL-LOUD: unlike the observability backfills,
// a plan that cannot be recorded must not proceed to a poll (the workflow refuses on error).
func (a *Activities) RecordPlanActivity(ctx context.Context, in RecordPlanInput) error {
	if a.D.PlanStore == nil {
		return fmt.Errorf("record plan %s: no plan store wired — the plan lane cannot run untracked", in.PlanID)
	}
	steps := make([]PlanStepRecord, len(in.Steps))
	for i, s := range in.Steps {
		steps[i] = PlanStepRecord{Ordinal: s.Ordinal, ActionID: s.ActionID, OpClass: s.OpClass}
	}
	return a.D.PlanStore.Create(ctx, in.PlanID, in.Recipe, in.ExternalRef, steps)
}

// PlanTransitionInput moves the plan machine one forward step (CAS in the store).
type PlanTransitionInput struct{ PlanID, From, To string }

// PlanTransitionActivity applies one plan transition. A CAS miss is logged truth ("someone already
// decided"), not an error — the durable rows converge on the workflow's recorded history.
func (a *Activities) PlanTransitionActivity(ctx context.Context, in PlanTransitionInput) error {
	if a.D.PlanStore == nil {
		return nil
	}
	if ok, err := a.D.PlanStore.Transition(ctx, in.PlanID, in.From, in.To); err != nil {
		return err
	} else if !ok {
		log.Printf("transaction plan %s: transition %s→%s missed (already moved) — the workflow history is the authority", in.PlanID, in.From, in.To)
	}
	return nil
}

// PlanStepTransitionInput moves one step's machine.
type PlanStepTransitionInput struct {
	PlanID  string
	Ordinal int
	From    string
	To      string
}

// PlanStepTransitionActivity applies one step transition, same contract as PlanTransitionActivity.
func (a *Activities) PlanStepTransitionActivity(ctx context.Context, in PlanStepTransitionInput) error {
	if a.D.PlanStore == nil {
		return nil
	}
	if ok, err := a.D.PlanStore.TransitionStep(ctx, in.PlanID, in.Ordinal, in.From, in.To); err != nil {
		return err
	} else if !ok {
		log.Printf("transaction plan %s: step %d transition %s→%s missed (already moved)", in.PlanID, in.Ordinal, in.From, in.To)
	}
	return nil
}

// PlanEventInput is one governance-ledger entry for the plan's story (REQ-3006). ActionID may be ""
// for plan-level events; per-step events carry the step's own sealed id so the spine answers both
// "what did this plan do" and "what touched this action".
type PlanEventInput struct {
	PlanID   string
	Decision string
	Reason   string
	ActionID string
	Withheld bool
}

// PlanEventActivity appends one plan event. Best-effort like the triage record: the workflow history
// stays authoritative, and losing one narration line must never fail a plan mid-flight — EXCEPT that a
// nil ledger still errors, because a deployment without the one accountability spine must not run
// plans at all.
//
// REQ-3006: every entry carries BOTH identities — the step's own action_id in the ActionID column
// (plan-level events fall back to plan/<plan_id>), and the FULL plan_id in the reason (never a
// truncated prefix: the spine must answer "what did this plan do" by the same id the approval bound).
func (a *Activities) PlanEventActivity(ctx context.Context, in PlanEventInput) error {
	if a.D.Ledger == nil {
		return fmt.Errorf("plan event %s: no governance ledger wired", in.PlanID)
	}
	actionID := in.ActionID
	if actionID == "" {
		actionID = "plan/" + in.PlanID
	}
	if _, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: in.Decision,
		Reason:   "plan " + in.PlanID + ": " + in.Reason,
		ActionID: actionID,
		Withheld: in.Withheld,
	}); err != nil {
		log.Printf("transaction plan %s: ledger append %s failed (best-effort): %v", in.PlanID, in.Decision, err)
	}
	return nil
}
