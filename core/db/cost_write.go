package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/cost"
)

// CostStore is the pgx-backed, CROSS-PROCESS core/cost.Store: the single durable source of truth for the
// spend guard's day/session accumulators (cost_accrual) and its breaker position (cost_breaker_state),
// migration 0023. It is what makes the cost breaker a SYSTEM-WIDE spend kill — every worker ADDS its spend
// to the same daily/session rows and reads the same breaker-state row, so a budget trip in one worker
// force-Shadows every sibling. It backs the guard in the worker; CI (no Postgres) uses cost.MemStore, the
// in-memory twin that coordinates identically for many Accountant values sharing one store.
//
// Read (Total/BreakerOpen) lives in cost_read.go; this file owns the writes (Accrue/TripBreaker).
// Parameters are always bound ($1) — no string-built SQL (INV-03). Both rows are CURRENT-STATE (additive or
// latest-wins upsert), not append-only: the tamper-evident record of a TRIP is the governance_ledger
// 'cost:breaker-trip' entry, so these coordination rows are legitimately overwritten. NON-SECRET by
// construction — only a bucket key (a UTC date or an external_ref), USD amounts, a state, and a human
// reason ever cross over.
type CostStore struct{ p *Pool }

// NewCostStore returns the Postgres-backed cross-process cost store.
func NewCostStore(p *Pool) *CostStore { return &CostStore{p: p} }

// Accrue ADDS usd to the (kind,key) bucket and returns the new running total. It is an ADDITIVE upsert —
// on conflict the stored total is incremented by the delta (usd_accrued = cost_accrual.usd_accrued +
// EXCLUDED.usd_accrued), so concurrent workers each add their increment to one shared total; RETURNING
// hands back the post-increment value atomically. last_updated_at is stamped server-side to now() so
// staleness is measured against the DB clock across the process boundary.
func (s *CostStore) Accrue(ctx context.Context, kind, key string, usd float64) (float64, error) {
	var total float64
	err := s.p.Pool.QueryRow(ctx, `
		INSERT INTO cost_accrual (bucket_kind, bucket_key, usd_accrued, last_updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (bucket_kind, bucket_key) DO UPDATE SET
			usd_accrued     = cost_accrual.usd_accrued + EXCLUDED.usd_accrued,
			last_updated_at = now()
		RETURNING usd_accrued`,
		kind, key, usd).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: cost_accrual upsert (%q/%q): %w", kind, key, err)
	}
	return total, nil
}

// TripBreaker sets the shared cost breaker state OPEN (latest-wins upsert on the single name='cost' PK),
// stamping the trip reason and the daily total at the trip. opened_at is stamped server-side to now().
func (s *CostStore) TripBreaker(ctx context.Context, reason string, usdAtTrip float64) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO cost_breaker_state (name, state, reason, usd_at_trip, opened_at, last_updated_at)
		VALUES ($1, 'open', $2, $3, now(), now())
		ON CONFLICT (name) DO UPDATE SET
			state           = 'open',
			reason          = EXCLUDED.reason,
			usd_at_trip     = EXCLUDED.usd_at_trip,
			opened_at       = now(),
			last_updated_at = now()`,
		cost.BreakerName, reason, usdAtTrip)
	if err != nil {
		return fmt.Errorf("db: cost_breaker_state trip upsert: %w", err)
	}
	return nil
}

// compile-time proof the pgx store satisfies the cross-process cost.Store interface (reads in cost_read.go).
var _ cost.Store = (*CostStore)(nil)
