package main

// wireTransactionPlanStore arms the spec/030 plan lane's durable recorder (TG-58) — OUT of main() per
// the TG-501 ratchet. The rows are a plane:both PROJECTION (the commit_confirm precedent): the workflow
// history is the authority, so a nil store fails the lane CLOSED at RecordPlanActivity ("a plan that
// cannot be tracked must not run") rather than running untracked. The boot line states the arming
// honestly either way — the review lens that keeps catching silently-armed (or silently-dark) controls.

import (
	"context"
	"log"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// planStoreAdapter maps the runner's dependency-free step record onto the db store's row type — the
// same thin-seam shape every other Deps store adapter uses (runner never imports core/db).
type planStoreAdapter struct{ s *db.TransactionPlanStore }

func (a planStoreAdapter) Create(ctx context.Context, planID, recipe, externalRef string, steps []runner.PlanStepRecord) error {
	rows := make([]db.PlanStep, len(steps))
	for i, st := range steps {
		rows[i] = db.PlanStep{Ordinal: st.Ordinal, ActionID: st.ActionID, OpClass: st.OpClass}
	}
	return a.s.Create(ctx, planID, recipe, externalRef, rows)
}

func (a planStoreAdapter) Transition(ctx context.Context, planID, from, to string) (bool, error) {
	return a.s.Transition(ctx, planID, from, to)
}

func (a planStoreAdapter) TransitionStep(ctx context.Context, planID string, ordinal int, from, to string) (bool, error) {
	return a.s.TransitionStep(ctx, planID, ordinal, from, to)
}

func wireTransactionPlanStore(deps *runner.Deps, pool *db.Pool) {
	deps.PlanStore = planStoreAdapter{s: db.NewTransactionPlanStore(pool)}
	log.Print("transaction plans: recorder ARMED (transaction_plan rows, spec/030) — the full lane (compose → " +
		"plan poll → saga) is wired and stays inert until a recipe is declared (core/plan ships empty, T-030-6)")
}
