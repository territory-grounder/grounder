package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

// PendingStore is the pgx-backed durable projection of open POLL_PAUSE decisions (migration 0007). It is
// the CROSS-PROCESS seam behind the console approvals list: the Runner (worker process) writes via
// OpenDecision/ResolveDecision, the console (grounder process) reads via OpenDecisions/CountOpen — an
// in-memory store cannot bridge those two processes. It implements both persist.PendingWriter and
// persist.PendingReader and holds NO authority (see core/persist/pending_decisions.go).
type PendingStore struct{ p *Pool }

// NewPendingStore returns a Postgres-backed pending-decisions projection.
func NewPendingStore(p *Pool) *PendingStore { return &PendingStore{p: p} }

// OpenDecision upserts an open decision keyed by external_ref. Idempotent on Temporal activity retry: a
// re-open of the same ref refreshes the row back to open with the latest sealed poll content.
func (s *PendingStore) OpenDecision(ctx context.Context, d persist.PendingDecision) error {
	if d.ExternalRef == "" || d.ActionID == "" {
		return persist.ErrEmptyDecisionKey
	}
	approaches := d.Approaches
	if approaches == nil {
		approaches = []string{}
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO pending_decision
			(external_ref, action_id, band, approaches, prediction, reversible, site, opened_at, status, outcome, resolved_at)
		VALUES ($1, $2, 'POLL_PAUSE', $3, $4, $5, $6, $7, 'open', '', NULL)
		ON CONFLICT (external_ref) DO UPDATE SET
			action_id   = EXCLUDED.action_id,
			approaches  = EXCLUDED.approaches,
			prediction  = EXCLUDED.prediction,
			reversible  = EXCLUDED.reversible,
			site        = EXCLUDED.site,
			opened_at   = EXCLUDED.opened_at,
			status      = 'open',
			outcome     = '',
			resolved_at = NULL`,
		d.ExternalRef, d.ActionID, approaches, d.Prediction, d.Reversible, d.Site, d.OpenedAt)
	if err != nil {
		return fmt.Errorf("db: open pending decision %s: %w", d.ExternalRef, err)
	}
	return nil
}

// ResolveDecision marks the open row for external_ref resolved, ONLY when action_id matches (a vote or
// timeout for a different action never resolves it, INV-12). Idempotent: 0 rows affected is not an error.
func (s *PendingStore) ResolveDecision(ctx context.Context, externalRef, actionID, outcome string, resolvedAt time.Time) error {
	_, err := s.p.Exec(ctx, `
		UPDATE pending_decision SET status = 'resolved', outcome = $3, resolved_at = $4
		WHERE external_ref = $1 AND action_id = $2 AND status = 'open'`,
		externalRef, actionID, outcome, resolvedAt)
	if err != nil {
		return fmt.Errorf("db: resolve pending decision %s: %w", externalRef, err)
	}
	return nil
}

// OpenDecisions returns the open decisions for review — the console approvals list.
//
// ORDERED IRREVERSIBLE FIRST, THEN OLDEST (TG-173). Oldest-first alone is the right default for a calm
// queue and the wrong one for a flooded one: under an alert storm — or a manufactured flood of low-value
// in-grammar proposals — first-in-first-out means the one decision that cannot be undone is reviewed after
// ninety that can, by a reviewer whom the preceding ninety have trained to click approve.
//
// Ordering is the only prioritisation applied here, deliberately. This is a READ PROJECTION with no
// authority: the Runner workflow waits for a vote regardless of what this table says, so shedding or
// capping rows would not relieve the operator — it would hide a poll that still blocks an action, turning
// a visible backlog into a silently stuck one.
//
// Reversibility is ordered in SQL rather than in Go so it holds for every caller of this store, including
// any future one that does not go through the console handler.
func (s *PendingStore) OpenDecisions(ctx context.Context) ([]persist.PendingDecision, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, action_id, band, approaches, prediction, reversible, site, opened_at
		FROM pending_decision WHERE status = 'open' ORDER BY reversible ASC, opened_at, external_ref`)
	if err != nil {
		return nil, fmt.Errorf("db: list open pending decisions: %w", err)
	}
	defer rows.Close()
	var out []persist.PendingDecision
	for rows.Next() {
		var d persist.PendingDecision
		if err := rows.Scan(&d.ExternalRef, &d.ActionID, &d.Band, &d.Approaches, &d.Prediction, &d.Reversible, &d.Site, &d.OpenedAt); err != nil {
			return nil, err
		}
		d.Status = persist.DecisionOpen
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountOpen returns the number of open decisions (the /v1/stats pending_polls gauge).
func (s *PendingStore) CountOpen(ctx context.Context) (int, error) {
	var n int
	if err := s.p.QueryRow(ctx, "SELECT count(*) FROM pending_decision WHERE status = 'open'").Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count open pending decisions: %w", err)
	}
	return n, nil
}

// ReapAbandoned resolves decisions that have been open longer than `deadline` — the ones whose workflow no
// longer exists, so no vote can ever reach them.
//
// THE DEFECT IT CLOSES. pending_decision is written when a poll opens and cleared by ResolveDecision when the
// workflow records an outcome. If the workflow DIES before resolving — a worker restart mid-deploy is the
// ordinary way — the row stays open forever. Nothing reconciles this table against workflow liveness, so the
// console lists a decision an operator can never act on, with caller_can_act = true.
//
// Measured live 2026-07-29: 13 of 136 open decisions were past the 24h VoteWait deadline, the oldest at
// 84.5h. Voting the three eldest returned HTTP 409 "no waiting decision for that ref" — proof the workflow
// was gone while the row still advertised itself as actionable. The estate had seen many worker restarts
// that day.
//
// WHY A DEADLINE RATHER THAN A TEMPORAL QUERY. The workflow's own bound is VoteWait; a decision open past it
// has either timed out (and failed to record) or lost its workflow. Both are unvotable, and both are proven
// unvotable by the 409. Querying Temporal for liveness would couple this read-side store to the orchestrator
// and still race a workflow that dies between the query and the write. A deadline with margin needs neither.
//
// The outcome is DISTINCT from human:timeout on purpose. A timeout means the poll ran its course and nobody
// answered — a fact about people. This means the poll stopped existing — a fact about the system. Recording
// the second as the first would inflate the human-unresponsiveness signal with an infrastructure failure and
// send someone to fix the wrong thing.
//
// Returns the number reaped so a caller can log a non-zero sweep rather than run silently.
func (s *PendingStore) ReapAbandoned(ctx context.Context, deadline time.Time, resolvedAt time.Time) (int, error) {
	tag, err := s.p.Exec(ctx, `
		UPDATE pending_decision SET status = 'resolved', outcome = 'abandoned:no-workflow', resolved_at = $2
		WHERE status = 'open' AND opened_at < $1`, deadline, resolvedAt)
	if err != nil {
		return 0, fmt.Errorf("db: reap abandoned pending decisions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
