package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/breaker"
)

// BreakerStore is the pgx-backed, CROSS-PROCESS core/breaker.Store: the single durable source of truth for
// every named breaker's three-state position (migration 0021 mutation_breaker_state). It is what makes the
// safety mutation breaker a SYSTEM-WIDE kill — a deviation trip in one worker upserts the row to 'open', and
// every sibling worker reads that same row (Load) before it actuates, so one worker's trip force-Shadows all
// of them. It backs the armed breaker in the worker; CI (no Postgres) uses breaker.MemStore, the in-memory
// twin that coordinates identically for many breaker values sharing one store.
//
// Read (Load/List) lives in breaker_read.go; this file owns the write (Save). Parameters are always bound
// ($1) — no string-built SQL (INV-03). The row is CURRENT-STATE (latest-wins upsert by name), not append-only:
// the tamper-evident record of a TRIP is the governance_ledger 'safety:breaker-trip' entry, so this coordination
// row is legitimately overwritten as the breaker transitions. NON-SECRET by construction — only a slug name,
// a state, counters, and timestamps ever cross over.
type BreakerStore struct {
	p     *Pool
	reset *Pool // TG-661: the tg_mode_admin pool a CLOSE of the safety breaker is written through; nil ⇒ plane pool
}

// NewBreakerStore returns the Postgres-backed cross-process breaker store.
func NewBreakerStore(p *Pool) *BreakerStore { return &BreakerStore{p: p} }

// MutationBreakerName is the row the safety breaker keeps (core/safety.NewMutationBreaker registers "mutation");
// migration 0135's breaker_reset_guard is scoped to exactly this name.
const MutationBreakerName = "mutation"

// WithResetPool routes the one write that CLOSES the safety breaker (Breaker.Reset ← MutationBreaker.Rearm ← an
// owner-gated escalation into an actuating mode, spec/015 REQ-1523) through a separate pool — the dyndb-leased
// tg_mode_admin login (TG-661): migration 0135's breaker_reset_guard refuses a re-arm from every runtime role. Trips,
// accruals and CompareAndOpen — the kill direction — ALWAYS go through the plane pool, so a trip never depends on
// a second credential being leasable. A nil pool is the documented unarmed state.
func (s *BreakerStore) WithResetPool(p *Pool) *BreakerStore {
	s.reset = p
	return s
}

// Save upserts one breaker's current record (latest-wins on the name PK). A zero OpenedAt / LastTransitionAt
// is written as SQL NULL so a closed breaker carries no stale open timestamp. last_updated_at is stamped
// server-side to now() so staleness is measured against the DB clock, not a worker's, across the process
// boundary. schema_version defaults to 1 in the DDL and is not overwritten here (forward-compatible reader guard).
func (s *BreakerStore) Save(ctx context.Context, rec breaker.Record) error {
	pool := s.p
	if rec.Name == MutationBreakerName && rec.State == breaker.StateClosed && s.reset != nil {
		pool = s.reset // TG-661: closing the safety breaker is the guarded direction
	}
	_, err := pool.Pool.Exec(ctx, `
		INSERT INTO mutation_breaker_state
			(name, state, failure_count, opened_at, half_open_successes, last_transition_at, last_updated_at,
			 tripped_by_action_id, trip_cause, compensation_action_id, compensation_admitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8, $9, $10)
		ON CONFLICT (name) DO UPDATE SET
			state                    = EXCLUDED.state,
			failure_count            = EXCLUDED.failure_count,
			opened_at                = EXCLUDED.opened_at,
			half_open_successes      = EXCLUDED.half_open_successes,
			last_transition_at       = EXCLUDED.last_transition_at,
			last_updated_at          = now(),
			tripped_by_action_id     = EXCLUDED.tripped_by_action_id,
			trip_cause               = EXCLUDED.trip_cause,
			compensation_action_id   = EXCLUDED.compensation_action_id,
			compensation_admitted_at = EXCLUDED.compensation_admitted_at`,
		rec.Name, string(rec.State), rec.FailureCount,
		nullableTime(rec.OpenedAt), rec.HalfOpenSuccesses, nullableTime(rec.LastTransitionAt),
		nullableText(rec.TrippedBy), nullableText(rec.Cause), nullableText(rec.CompensationActionID), nullableTime(rec.CompensationAt))
	if err != nil {
		return fmt.Errorf("db: mutation_breaker_state upsert (%q): %w", rec.Name, err)
	}
	return nil
}

// nullableText maps "" to SQL NULL — the attribution columns are NULL on an unattributed/closed row.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AdmitCompensation is the cross-process CAS behind the ONE admitted compensation (TG-665): it marks the row
// ONLY WHERE the breaker is open, its recorded cause is requiredCause, its tripping action is trippedBy and no
// compensation has been admitted yet. RETURNING yields a row exactly when THIS call won; no row (ErrNoRows)
// means the row is closed, attributed otherwise, or already spent. Parameters bound (INV-03).
func (s *BreakerStore) AdmitCompensation(ctx context.Context, name, trippedBy, requiredCause, compensationID string, now time.Time) (bool, error) {
	var admitted bool
	err := s.p.Pool.QueryRow(ctx, `
		UPDATE mutation_breaker_state
		   SET compensation_action_id = $4, compensation_admitted_at = $5, last_updated_at = now()
		 WHERE name = $1 AND state = 'open' AND trip_cause = $3 AND tripped_by_action_id = $2
		   AND compensation_action_id IS NULL
		RETURNING true`,
		name, trippedBy, requiredCause, compensationID, now.UTC()).Scan(&admitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: mutation_breaker_state admit-compensation (%q): %w", name, err)
	}
	return admitted, nil
}

// nullableTime maps a zero time.Time to a SQL NULL (any, so pgx binds nil) and otherwise to the UTC instant —
// so an unset OpenedAt/LastTransitionAt is stored as NULL, not the Go zero year.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// CompareAndOpen atomically forces one breaker OPEN and reports whether THIS call caused the transition
// (openedNow) — the cross-process compare-and-swap the per-instance mutex cannot provide. It is ONE statement:
// an INSERT that, on a name conflict, updates to 'open' ONLY WHERE the row is not already open. RETURNING
// yields a row on an INSERT (fresh breaker → opened) and on a conditional UPDATE (was closed/half-open →
// opened), and NO row when the ON CONFLICT WHERE is false (already open → did not flip). So "a row came back"
// is exactly "this call opened it": two workers racing the same trip see one INSERT/UPDATE and one no-row.
// Parameters are bound ($1/$2) — no string-built SQL (INV-03). An already-open row keeps its original
// opened_at (the WHERE skips it), so the FIRST trip's timestamp is preserved.
func (s *BreakerStore) CompareAndOpen(ctx context.Context, name string, now time.Time) (bool, error) {
	var opened bool
	err := s.p.Pool.QueryRow(ctx, `
		INSERT INTO mutation_breaker_state
			(name, state, failure_count, opened_at, half_open_successes, last_transition_at, last_updated_at)
		VALUES ($1, 'open', 1, $2, 0, $2, now())
		ON CONFLICT (name) DO UPDATE SET
			state               = 'open',
			failure_count       = mutation_breaker_state.failure_count + 1,
			opened_at           = $2,
			half_open_successes = 0,
			last_transition_at  = $2,
			last_updated_at     = now()
		WHERE mutation_breaker_state.state <> 'open'
		RETURNING true`,
		name, now.UTC()).Scan(&opened)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // ON CONFLICT WHERE was false: the row exists and is ALREADY open — not this call
	}
	if err != nil {
		return false, fmt.Errorf("db: mutation_breaker_state compare-and-open (%q): %w", name, err)
	}
	return opened, nil // a row returned ⇒ inserted or conditionally updated ⇒ THIS call opened it
}

// compile-time proof the pgx store satisfies the cross-process breaker.Store interface (Load/List in
// breaker_read.go) and the optional atomic-open capability (TG-432).
var (
	_ breaker.Store                = (*BreakerStore)(nil)
	_ breaker.AtomicOpener         = (*BreakerStore)(nil)
	_ breaker.CompensationAdmitter = (*BreakerStore)(nil)
)
