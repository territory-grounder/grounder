package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/estate"
)

// LatestSnapshotForPlane returns the newest estate snapshot WRITTEN BY the named plane, with its write
// time — the read half of Publish (TG-346).
//
// THE PLANE FILTER IS THE POINT. Both workers publish into this table and their graphs differ by two
// orders of magnitude (measured 2026-08-06: 410 nodes/1863 edges vs 20/17, seconds apart). A read
// ordered by recency alone answers with whichever worker wrote last — which is the exact defect the
// `plane` column was added to end. The actuation plane's relay asks for 'triage' explicitly; handing it
// its own snapshot back would relay the impoverished graph to itself and call that convergence.
func (s *EstateWriteStore) LatestSnapshotForPlane(ctx context.Context, plane string) (estate.Snapshot, time.Time, error) {
	var raw []byte
	var at time.Time
	err := s.p.Pool.QueryRow(ctx, `
		SELECT graph_json, created_at FROM estate_snapshot
		WHERE plane = $1
		ORDER BY id DESC LIMIT 1`, plane).Scan(&raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return estate.Snapshot{}, time.Time{}, fmt.Errorf("db: no estate snapshot exists for plane %q — the relayed plane has never published", plane)
	}
	if err != nil {
		return estate.Snapshot{}, time.Time{}, fmt.Errorf("db: latest estate snapshot for plane %s: %w", plane, err)
	}
	var snap estate.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return estate.Snapshot{}, time.Time{}, fmt.Errorf("db: estate snapshot for plane %s unmarshal: %w", plane, err)
	}
	return snap, at, nil
}
