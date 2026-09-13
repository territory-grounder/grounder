package db

// The worker-published capability projection (TG-251, migration 0051): the channel that lets the API
// process answer "is this worker-resident connector on?" without pretending its own registry is the
// fleet. The worker upserts its Capabilities() view on a heartbeat; the grounder reads it THROUGH a
// staleness cutoff — a row whose publisher stopped refreshing it is an unknown, never an answer.

import (
	"context"
	"fmt"
	"time"
)

// CapabilityProjectionRow is one published (surface, source_type) enablement fact.
type CapabilityProjectionRow struct {
	Surface    string
	SourceType string
	Capability string
	Enabled    bool
	ObservedAt time.Time
}

// CapabilityProjectionStore is the pgx-backed projection table.
type CapabilityProjectionStore struct{ p *Pool }

// NewCapabilityProjectionStore returns the store over the shared pool.
func NewCapabilityProjectionStore(p *Pool) *CapabilityProjectionStore {
	return &CapabilityProjectionStore{p: p}
}

// Publish upserts the worker's full projection in one round trip. Rows the registry no longer declares
// are NOT deleted: their observed_at simply stops advancing, and the reader's staleness cutoff retires
// them — the same mechanism that handles a dead worker handles a removed module, with no second code path.
func (s *CapabilityProjectionStore) Publish(ctx context.Context, rows []CapabilityProjectionRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().UTC()
	batch := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*4+1)
	args = append(args, now)
	for i, r := range rows {
		batch = append(batch, fmt.Sprintf("($%d,$%d,$%d,$%d,$1)", i*4+2, i*4+3, i*4+4, i*4+5))
		args = append(args, r.Surface, r.SourceType, r.Capability, r.Enabled)
	}
	q := `INSERT INTO module_capability_projection (surface, source_type, capability, enabled, observed_at) VALUES `
	for i, v := range batch {
		if i > 0 {
			q += ","
		}
		q += v
	}
	q += ` ON CONFLICT (surface, source_type)
	       DO UPDATE SET capability = EXCLUDED.capability, enabled = EXCLUDED.enabled, observed_at = EXCLUDED.observed_at`
	if _, err := s.p.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("db: publish capability projection (%d rows): %w", len(rows), err)
	}
	return nil
}

// Load returns every projected row with its observed_at. Staleness is judged by the CALLER against its
// own window — the store reports what was published and when, and never launders a stale row into a fresh
// answer by filtering here (a reader that forgot the cutoff should be findable in review, not saved by a
// hidden one).
func (s *CapabilityProjectionStore) Load(ctx context.Context) ([]CapabilityProjectionRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT surface, source_type, capability, enabled, observed_at
		FROM module_capability_projection ORDER BY surface, source_type`)
	if err != nil {
		return nil, fmt.Errorf("db: load capability projection: %w", err)
	}
	defer rows.Close()
	var out []CapabilityProjectionRow
	for rows.Next() {
		var r CapabilityProjectionRow
		if err := rows.Scan(&r.Surface, &r.SourceType, &r.Capability, &r.Enabled, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("db: scan capability projection: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
