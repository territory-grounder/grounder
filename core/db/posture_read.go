package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostureRow is the latest published posture for one component. Found=false ⇒ no row yet (the worker has
// not published) — the reader treats that as UNKNOWN, never a confident false. UpdatedAt is the DB-clock
// stamp the caller measures staleness against.
type PostureRow struct {
	Found            bool
	Component        string
	MutationEnabled  bool
	EffectCapability string
	UpdatedAt        time.Time
}

// PostureReadStore is the pgx-backed READ side of the runtime-posture surface: the grounder reads the
// worker's latest published posture across the process boundary. Read-only — one bound query.
type PostureReadStore struct{ p *Pool }

// NewPostureReadStore returns the Postgres-backed runtime-posture reader.
func NewPostureReadStore(p *Pool) *PostureReadStore { return &PostureReadStore{p: p} }

// Latest returns the posture row for a component, Found=false when none exists yet.
func (s *PostureReadStore) Latest(ctx context.Context, component string) (PostureRow, error) {
	var row PostureRow
	err := s.p.Pool.QueryRow(ctx, `
		SELECT component, mutation_enabled, effect_capability, updated_at
		FROM runtime_posture
		WHERE component = $1`, component).
		Scan(&row.Component, &row.MutationEnabled, &row.EffectCapability, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PostureRow{Found: false}, nil
	}
	if err != nil {
		return PostureRow{}, fmt.Errorf("db: runtime posture read: %w", err)
	}
	row.Found = true
	return row, nil
}
