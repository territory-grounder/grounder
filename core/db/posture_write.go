package db

import (
	"context"
	"fmt"
)

// PosturePublishStore is the pgx-backed WRITE side of the runtime-posture surface: the WORKER publishes its
// live mutation posture — the real gate's Enabled() and the effect leaf's Capability() — so the grounder (a
// SEPARATE process whose own gate is read-only by construction) can report the TRUE posture on /v1/whoami +
// /v1/governance instead of its own always-off gate. It mirrors EstateWriteStore's shape: a best-effort,
// single-writer upsert the worker re-runs on a heartbeat so updated_at stays fresh for the reader's staleness
// check. Parameters are always bound ($1) — no string-built SQL.
type PosturePublishStore struct{ p *Pool }

// NewPosturePublishStore returns the Postgres-backed runtime-posture writer.
func NewPosturePublishStore(p *Pool) *PosturePublishStore { return &PosturePublishStore{p: p} }

// Publish upserts one component's live posture (latest-wins, keyed by component). updated_at is stamped
// server-side to now() so the reader measures staleness against the DB clock, not the worker's — a heartbeat
// gap then reads honestly as stale regardless of clock skew between the two processes.
func (s *PosturePublishStore) Publish(ctx context.Context, component string, mutationEnabled bool, effectCapability string) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO runtime_posture (component, mutation_enabled, effect_capability, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (component) DO UPDATE SET
			mutation_enabled  = EXCLUDED.mutation_enabled,
			effect_capability = EXCLUDED.effect_capability,
			updated_at        = now()`,
		component, mutationEnabled, effectCapability)
	if err != nil {
		return fmt.Errorf("db: runtime posture upsert: %w", err)
	}
	return nil
}
