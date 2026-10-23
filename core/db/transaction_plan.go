package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TransactionPlanStore is the pgx store for transaction plans (spec/030 T-030-2, migration 0111): one
// row per composed plan keyed by the content-addressed plan_id the ONE approval binds, plus the ordered
// step bindings (each step's own sealed action_id, INV-07 unchanged). Both state machines move FORWARD
// ONLY via compare-and-swap — an UPDATE conditioned on the expected prior state — so a replayed or
// racing activity cannot resurrect a terminal plan or re-pend an executed step. The append-only history
// of transitions is the governance ledger's job (REQ-3006); these rows answer "where is the plan NOW"
// and, for the revert-failed terminal, "exactly which steps remain applied" (REQ-3005).
type TransactionPlanStore struct{ p *Pool }

// NewTransactionPlanStore returns the Postgres-backed plan store.
func NewTransactionPlanStore(p *Pool) *TransactionPlanStore { return &TransactionPlanStore{p: p} }

// PlanStep is one step binding as stored.
type PlanStep struct {
	Ordinal  int
	ActionID string
	OpClass  string
	State    string
}

// PlanRow is the plan's current durable state.
type PlanRow struct {
	PlanID      string
	Recipe      string
	ExternalRef string
	State       string
	CreatedAt   time.Time
	Steps       []PlanStep
}

// planTransitions is the closed forward-only machine (REQ-3002/3004/3005 terminals). A transition not
// listed here is refused — including any transition OUT of a terminal.
var planTransitions = map[string]map[string]bool{
	"proposed":  {"approved": true},
	"approved":  {"executing": true},
	"executing": {"committed": true, "reverted": true, "revert-failed": true},
}

// stepTransitions is the per-step machine.
var stepTransitions = map[string]map[string]bool{
	"pending":  {"executed": true},
	"executed": {"compensated": true, "compensate-failed": true},
}

// Create records a composed plan and its ordered step bindings atomically. First-wins on plan_id: the
// id is content-addressed, so a duplicate Create is the same plan re-composed (idempotent), never a
// second plan.
func (s *TransactionPlanStore) Create(ctx context.Context, planID, recipe, externalRef string, steps []PlanStep) error {
	if strings.TrimSpace(planID) == "" || strings.TrimSpace(recipe) == "" || strings.TrimSpace(externalRef) == "" || len(steps) == 0 {
		return fmt.Errorf("db: plan create requires plan_id/recipe/external_ref/steps")
	}
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: plan create begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ct, err := tx.Exec(ctx, `
		INSERT INTO transaction_plan (plan_id, recipe, external_ref)
		VALUES ($1, $2, $3) ON CONFLICT (plan_id) DO NOTHING`, planID, recipe, externalRef)
	if err != nil {
		return fmt.Errorf("db: plan create %s: %w", planID, err)
	}
	if ct.RowsAffected() == 0 {
		return nil // the same content-addressed plan already exists — idempotent re-compose
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO transaction_plan_step (plan_id, ordinal, action_id, op_class)
			VALUES ($1, $2, $3, $4)`, planID, st.Ordinal, st.ActionID, st.OpClass); err != nil {
			return fmt.Errorf("db: plan step %s#%d: %w", planID, st.Ordinal, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: plan create commit %s: %w", planID, err)
	}
	return nil
}

// Transition moves the plan from -> to, forward-only, compare-and-swap. ok=false means the plan was
// NOT in `from` (raced, replayed, or an illegal move) — the caller treats that as "someone else already
// decided", never as success.
func (s *TransactionPlanStore) Transition(ctx context.Context, planID, from, to string) (bool, error) {
	if !planTransitions[from][to] {
		return false, fmt.Errorf("db: plan transition %s: %q -> %q is not a forward move of the machine", planID, from, to)
	}
	ct, err := s.p.Exec(ctx, `
		UPDATE transaction_plan SET state = $3, updated_at = now()
		 WHERE plan_id = $1 AND state = $2`, planID, from, to)
	if err != nil {
		return false, fmt.Errorf("db: plan transition %s: %w", planID, err)
	}
	return ct.RowsAffected() == 1, nil
}

// TransitionStep moves one step's state, forward-only CAS, same contract as Transition.
func (s *TransactionPlanStore) TransitionStep(ctx context.Context, planID string, ordinal int, from, to string) (bool, error) {
	if !stepTransitions[from][to] {
		return false, fmt.Errorf("db: plan step transition %s#%d: %q -> %q is not a forward move", planID, ordinal, from, to)
	}
	ct, err := s.p.Exec(ctx, `
		UPDATE transaction_plan_step SET state = $4
		 WHERE plan_id = $1 AND ordinal = $2 AND state = $3`, planID, ordinal, from, to)
	if err != nil {
		return false, fmt.Errorf("db: plan step transition %s#%d: %w", planID, ordinal, err)
	}
	return ct.RowsAffected() == 1, nil
}

// Get returns the plan and its steps in ordinal order; ok=false when no such plan exists.
func (s *TransactionPlanStore) Get(ctx context.Context, planID string) (PlanRow, bool, error) {
	var r PlanRow
	err := s.p.QueryRow(ctx, `
		SELECT plan_id, recipe, external_ref, state, created_at FROM transaction_plan WHERE plan_id = $1`,
		planID).Scan(&r.PlanID, &r.Recipe, &r.ExternalRef, &r.State, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanRow{}, false, nil
		}
		return PlanRow{}, false, fmt.Errorf("db: plan get %s: %w", planID, err)
	}
	rows, err := s.p.Query(ctx, `
		SELECT ordinal, action_id, op_class, state FROM transaction_plan_step
		 WHERE plan_id = $1 ORDER BY ordinal`, planID)
	if err != nil {
		return PlanRow{}, false, fmt.Errorf("db: plan steps %s: %w", planID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var st PlanStep
		if err := rows.Scan(&st.Ordinal, &st.ActionID, &st.OpClass, &st.State); err != nil {
			return PlanRow{}, false, fmt.Errorf("db: scan plan step: %w", err)
		}
		r.Steps = append(r.Steps, st)
	}
	return r, true, rows.Err()
}
