// Package skillwrite executes skill-version status transitions in the WORKER — the ledger's single
// writer (spec/014 REQ-1301/1311). The grounder's authenticated write surface never appends to the
// hash chain itself: it starts this workflow and waits for the result, so every transition runs through
// the one skillstore.Transition state machine with the worker's ledger, and a concurrent grounder can
// never fork the chain.
package skillwrite

import (
	"context"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// Request is the typed transition order (operator identity server-derived at the surface, never client-supplied).
type Request struct {
	VersionID int64
	To        skillstore.Status
	Rationale string
	Operator  string
}

// Result is the transitioned version's essentials for the console response.
type Result struct {
	VersionID int64
	SkillName string
	Version   string
	Status    skillstore.Status
	LedgerSeq int64
}

// Deps are the worker-side collaborators.
type Deps struct {
	Store  skillstore.Store
	Ledger skillstore.Ledger
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// TransitionActivity runs the single audited state machine (the operator identity is appended to the
// rationale so the ledger names who ordered the move).
func (a *Activities) TransitionActivity(ctx context.Context, req Request) (Result, error) {
	v, err := skillstore.Transition(ctx, a.D.Store, a.D.Ledger, req.VersionID, req.To,
		req.Rationale+" [by "+req.Operator+"]")
	if err != nil {
		return Result{}, err
	}
	return Result{VersionID: v.ID, SkillName: v.SkillName, Version: v.Version, Status: v.Status, LedgerSeq: v.LedgerSeq}, nil
}

// TransitionWorkflow is the one-activity transition workflow. Named DISTINCTLY — Temporal registers by
// bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow (the
// 2026-07-17 worker boot-loop). No retries on the activity: a refused transition
// (bad state, missing rationale, pinned skill) is a DECISION, not a transient — it surfaces verbatim.
func TransitionWorkflow(ctx workflow.Context, req Request) (Result, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var res Result
	// Temporal dispatches activities by REGISTERED FUNCTION NAME: the zero-Deps receiver here only
	// names the activity; the worker's registered instance (with the real Store+Ledger) executes it.
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), new(Activities).TransitionActivity, req).Get(ctx, &res)
	return res, err
}
