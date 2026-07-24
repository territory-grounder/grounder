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

// OpenDecisions returns the open decisions, oldest first — the console approvals list.
func (s *PendingStore) OpenDecisions(ctx context.Context) ([]persist.PendingDecision, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, action_id, band, approaches, prediction, reversible, site, opened_at
		FROM pending_decision WHERE status = 'open' ORDER BY opened_at, external_ref`)
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
