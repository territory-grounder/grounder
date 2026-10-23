package faultinjector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the durable ledger the engine reasons from. It is the SINGLE source of restore obligations
// (INVARIANT 1) — nothing about what is currently broken may live in process memory.
type Store struct{ pool *pgxpool.Pool }

// NewStore wraps a connected pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Outstanding returns every fault whose restore is still owed, soonest-due first. Called on boot (to inherit
// a dead predecessor's obligations) and on every tick (so a repair missed because a host was briefly
// unreachable is retried rather than forgotten).
// RecentRestores returns, per (host, class), the most recent time a fault of that class on that host was
// RESTORED. The planner uses it to refuse re-faulting a target the monitoring check has not yet observed
// recovered — see Limits.SettleWindow.
//
// Bounded to the lookback the caller needs: anything older than the settle window can never make a target
// ineligible, so fetching it would be work with no effect on any decision.
func (s *Store) RecentRestores(ctx context.Context, within time.Duration) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host, fault_type, MAX(restored_at)
		  FROM injected_fault
		 WHERE restore_state = 'restored' AND restored_at IS NOT NULL AND restored_at >= $1
		 GROUP BY host, fault_type`, time.Now().Add(-within))
	if err != nil {
		return nil, fmt.Errorf("query recent restores: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var host string
		var class Class
		var at time.Time
		if err := rows.Scan(&host, &class, &at); err != nil {
			return nil, fmt.Errorf("scan recent restores: %w", err)
		}
		out[settleKey(host, class)] = at
	}
	return out, rows.Err()
}

func (s *Store) Outstanding(ctx context.Context) ([]Outstanding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, host, fault_type, node, fault_ref, restore_due_at, restore_state
		  FROM injected_fault
		 WHERE restore_state IN ('pending','failed')
		 ORDER BY restore_due_at`)
	if err != nil {
		return nil, fmt.Errorf("query outstanding: %w", err)
	}
	defer rows.Close()

	var out []Outstanding
	for rows.Next() {
		var o Outstanding
		var state string
		var due *time.Time
		if err := rows.Scan(&o.ID, &o.Host, &o.Class, &o.Node, &o.FaultRef, &due, &state); err != nil {
			return nil, fmt.Errorf("scan outstanding: %w", err)
		}
		if due != nil {
			o.RestoreDueAt = *due
		}
		o.Failed = state == "failed"
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecordInjection writes the fault AND its restore obligation in ONE statement, so a crash between "broke it"
// and "wrote down that I broke it" cannot lose the obligation. Callers MUST invoke this BEFORE performing the
// effect: a recorded-but-not-performed fault is harmless (the reconciler's repairs are idempotent), whereas a
// performed-but-unrecorded fault is exactly the stranding this package exists to prevent.
func (s *Store) RecordInjection(ctx context.Context, host string, class Class, node, faultRef string, restoreAfter time.Duration, note string) (int64, error) {
	state, due := "none", (*time.Time)(nil)
	if class.OwesRestore() {
		t := time.Now().UTC().Add(restoreAfter)
		state, due = "pending", &t
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO injected_fault (host, fault_type, note, restore_state, restore_due_at, fault_ref, node)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		host, string(class), note, state, due, faultRef, node).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record injection: %w", err)
	}
	return id, nil
}

// MarkRestored closes an obligation. Only ever called after the repair has been VERIFIED, never merely
// attempted — the whole point of the ledger is that it describes the estate, not our intentions toward it.
func (s *Store) MarkRestored(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE injected_fault SET restore_state='restored', restored_at=now() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("mark restored id=%d: %w", id, err)
	}
	return nil
}

// MarkRestoreFailed records that a repair was attempted and did not succeed. The row stays outstanding, so
// the host remains quarantined from further injection AND the repair is retried next tick.
func (s *Store) MarkRestoreFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE injected_fault SET restore_state='failed', note = note || $2 WHERE id=$1`,
		id, fmt.Sprintf(" | restore failed: %s", reason))
	if err != nil {
		return fmt.Errorf("mark restore-failed id=%d: %w", id, err)
	}
	return nil
}

// BreakerOpen reports whether TG's own mutation breaker has tripped. When TG is already unhappy the engine
// must not add load — the estate is telling us something.
func (s *Store) BreakerOpen(ctx context.Context) (bool, error) {
	var state string
	err := s.pool.QueryRow(ctx,
		`SELECT state FROM mutation_breaker_state WHERE name='mutation'`).Scan(&state)
	if err != nil {
		// Fail CLOSED: if we cannot read the breaker we must assume it is open and hold off. An engine that
		// keeps breaking things because it lost sight of the safety signal is the worst possible behaviour.
		return true, fmt.Errorf("read breaker (treating as OPEN): %w", err)
	}
	return state == "open", nil
}

// KillSwitchEngaged reports the DB-side kill switch. It is checked alongside a file-side switch so an
// operator can stop the engine from either the box or the database, and neither is a single point of failure.
func (s *Store) KillSwitchEngaged(ctx context.Context) bool {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM injected_fault WHERE fault_type='__killswitch__' AND restore_state='pending'`).Scan(&n); err != nil {
		// Unreadable kill switch ⇒ assume ENGAGED. A stop control that fails open is not a stop control.
		return true
	}
	return n > 0
}
