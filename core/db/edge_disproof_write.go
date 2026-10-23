package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// EdgeDisproofs is the pgx-backed, DURABLE estate.EdgeDisproofStore (migration 0075, TG-206a). A
// decay-on-disproof pass over the learned estate tier attaches the contradiction to each decayed edge; this
// persists those records to edge_disproof so a disproof survives a restart and every sibling worker adds to the
// same history. It is the same seam MemEdgeDisproofStore satisfies (the in-memory oracle twin).
//
// Parameters are always bound ($1) — no string-built SQL (INV-03). NON-SECRET by construction: only host /
// relation slugs, hashes, and a confidence ever cross over, exactly as EdgeDisproof guarantees.
type EdgeDisproofs struct{ p *Pool }

// NewEdgeDisproofs returns the Postgres-backed durable disproof store.
func NewEdgeDisproofs(p *Pool) *EdgeDisproofs { return &EdgeDisproofs{p: p} }

// compile-time proof the durable store satisfies the seam the decay pass records through.
var _ estate.EdgeDisproofStore = (*EdgeDisproofs)(nil)

// Record appends the disproofs of one decay pass in a single transaction (all-or-nothing per pass, so a
// half-written pass never reads as a complete disproof history). Empty input is a no-op. Returns rows written.
func (s *EdgeDisproofs) Record(ctx context.Context, at time.Time, rows []estate.EdgeDisproof) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.p.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: edge_disproof begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO edge_disproof
			  (edge_key, edge_from, edge_rel, edge_to, target_host, deviation_key, action_id, decayed_to, aged_out, observed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			r.EdgeKey, r.From, r.Rel, r.To, r.Target, r.DeviationKey, r.ActionID, r.DecayedTo, r.AgedOut, at); err != nil {
			return 0, fmt.Errorf("db: edge_disproof insert %s: %w", r.EdgeKey, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("db: edge_disproof commit: %w", err)
	}
	return len(rows), nil
}

// List returns every recorded disproof, most-recent pass first (observed_at DESC, then id DESC for a stable
// order within a pass) — the durable disproof history the learned-tier lifecycle consults.
func (s *EdgeDisproofs) List(ctx context.Context) ([]estate.RecordedEdgeDisproof, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT edge_key, edge_from, edge_rel, edge_to, target_host, deviation_key, action_id, decayed_to, aged_out, observed_at
		FROM edge_disproof
		ORDER BY observed_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("db: edge_disproof read: %w", err)
	}
	defer rows.Close()
	var out []estate.RecordedEdgeDisproof
	for rows.Next() {
		var (
			r  estate.RecordedEdgeDisproof
			at time.Time
		)
		if err := rows.Scan(&r.EdgeKey, &r.From, &r.Rel, &r.To, &r.Target, &r.DeviationKey, &r.ActionID,
			&r.DecayedTo, &r.AgedOut, &at); err != nil {
			return nil, fmt.Errorf("db: edge_disproof scan: %w", err)
		}
		r.ObservedAt = at
		out = append(out, r)
	}
	return out, rows.Err()
}
