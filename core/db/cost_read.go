package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/cost"
)

// Total reads the durable running spend for a (kind,key) bucket. An unseen bucket returns 0 with a nil
// error (no spend yet) — never an error, so a fresh day/session reads as $0. A query error is returned so
// the caller (core/cost.Accountant) can FAIL OPEN loudly: unlike the safety breaker, a cost-store read
// error must NOT halt work, so the Accountant logs it and treats spend as unknown (no trip).
func (s *CostStore) Total(ctx context.Context, kind, key string) (float64, error) {
	var total float64
	err := s.p.Pool.QueryRow(ctx, `
		SELECT usd_accrued FROM cost_accrual WHERE bucket_kind = $1 AND bucket_key = $2`,
		kind, key).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("db: cost_accrual total (%q/%q): %w", kind, key, err)
	}
	return total, nil
}

// BreakerOpen reads the shared cost breaker state row (name='cost'). An unseen breaker is CLOSED
// (open=false) with a nil error — the correct never-yet-tripped default. A query error is returned so the
// Accountant fails OPEN (treats it as not tripped) and LOGS it — a spend guard we cannot observe must never
// halt legitimate work (the deliberate inverse of the mutation breaker's fail-closed Load).
func (s *CostStore) BreakerOpen(ctx context.Context) (bool, string, error) {
	var (
		state  string
		reason string
	)
	err := s.p.Pool.QueryRow(ctx, `
		SELECT state, reason FROM cost_breaker_state WHERE name = $1`, cost.BreakerName).
		Scan(&state, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("db: cost_breaker_state read: %w", err)
	}
	return state == "open", reason, nil
}
