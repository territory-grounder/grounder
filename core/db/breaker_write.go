package db

import (
	"context"
	"fmt"
	"time"

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
type BreakerStore struct{ p *Pool }

// NewBreakerStore returns the Postgres-backed cross-process breaker store.
func NewBreakerStore(p *Pool) *BreakerStore { return &BreakerStore{p: p} }

// Save upserts one breaker's current record (latest-wins on the name PK). A zero OpenedAt / LastTransitionAt
// is written as SQL NULL so a closed breaker carries no stale open timestamp. last_updated_at is stamped
// server-side to now() so staleness is measured against the DB clock, not a worker's, across the process
// boundary. schema_version defaults to 1 in the DDL and is not overwritten here (forward-compatible reader guard).
func (s *BreakerStore) Save(ctx context.Context, rec breaker.Record) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO mutation_breaker_state
			(name, state, failure_count, opened_at, half_open_successes, last_transition_at, last_updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (name) DO UPDATE SET
			state               = EXCLUDED.state,
			failure_count       = EXCLUDED.failure_count,
			opened_at           = EXCLUDED.opened_at,
			half_open_successes = EXCLUDED.half_open_successes,
			last_transition_at  = EXCLUDED.last_transition_at,
			last_updated_at     = now()`,
		rec.Name, string(rec.State), rec.FailureCount,
		nullableTime(rec.OpenedAt), rec.HalfOpenSuccesses, nullableTime(rec.LastTransitionAt))
	if err != nil {
		return fmt.Errorf("db: mutation_breaker_state upsert (%q): %w", rec.Name, err)
	}
	return nil
}

// nullableTime maps a zero time.Time to a SQL NULL (any, so pgx binds nil) and otherwise to the UTC instant —
// so an unset OpenedAt/LastTransitionAt is stored as NULL, not the Go zero year.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// compile-time proof the pgx store satisfies the cross-process breaker.Store interface (Load/List in breaker_read.go).
var _ breaker.Store = (*BreakerStore)(nil)
