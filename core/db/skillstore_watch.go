package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pgx skillstore.WatchStore over skill_watch (migration 0010, REQ-1310) — the post-graduation
// regression watch's durable bookkeeping. Open watches are the closed_at IS NULL rows; closing is an
// UPDATE that stamps closed_at + close_reason, so the watch history is append-only readable (a closed
// watch and why it closed remain queryable — the predecessor's rollback procedure had no record at
// all). Implemented on *SkillStore so the one store satisfies Store + TrialStore + WatchStore.

// PutWatch arms a watch idempotently: the FIRST arm for a version wins (a finalizer retry after a
// crash cannot reset an accruing failure streak).
func (s *SkillStore) PutWatch(ctx context.Context, w skillstore.WatchState) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO skill_watch
			(version_id, skill_name, control_mean, min_lift, failures, threshold, dimension, expires_at, prior_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (version_id) DO NOTHING`,
		w.VersionID, w.SkillName, w.ControlMean, w.MinLift, w.Failures, w.Threshold, w.Dimension,
		w.ExpiresAt, w.PriorVersion)
	if err != nil {
		return fmt.Errorf("db: put watch for version %d: %w", w.VersionID, err)
	}
	return nil
}

// OpenWatches implements skillstore.WatchStore.
func (s *SkillStore) OpenWatches(ctx context.Context) ([]skillstore.WatchState, error) {
	rows, err := s.p.Query(ctx, `
		SELECT version_id, skill_name, control_mean, min_lift, failures, threshold, dimension, expires_at, prior_version
		FROM skill_watch WHERE closed_at IS NULL ORDER BY version_id`)
	if err != nil {
		return nil, fmt.Errorf("db: open watches: %w", err)
	}
	defer rows.Close()
	var out []skillstore.WatchState
	for rows.Next() {
		var w skillstore.WatchState
		if err := rows.Scan(&w.VersionID, &w.SkillName, &w.ControlMean, &w.MinLift, &w.Failures,
			&w.Threshold, &w.Dimension, &w.ExpiresAt, &w.PriorVersion); err != nil {
			return nil, fmt.Errorf("db: scan watch: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWatch persists the failure streak for a still-open watch (the only mutable bookkeeping —
// identity, thresholds and window are set at arm time).
func (s *SkillStore) UpdateWatch(ctx context.Context, w skillstore.WatchState) error {
	if _, err := s.p.Exec(ctx, `
		UPDATE skill_watch SET failures = $2 WHERE version_id = $1 AND closed_at IS NULL`,
		w.VersionID, w.Failures); err != nil {
		return fmt.Errorf("db: update watch for version %d: %w", w.VersionID, err)
	}
	return nil
}

// CloseWatch closes an open watch with its reason (tripped or survived); a second close is a no-op —
// the first terminal reason stands.
func (s *SkillStore) CloseWatch(ctx context.Context, versionID int64, reason string) error {
	if _, err := s.p.Exec(ctx, `
		UPDATE skill_watch SET closed_at = now(), close_reason = $2
		WHERE version_id = $1 AND closed_at IS NULL`,
		versionID, reason); err != nil {
		return fmt.Errorf("db: close watch for version %d: %w", versionID, err)
	}
	return nil
}
