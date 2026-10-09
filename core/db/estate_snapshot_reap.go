package db

// RETENTION FOR THE ESTATE-SNAPSHOT PROJECTION (TG-355).
//
// estate_snapshot was 84 MB of a 140 MB database on 2026-08-06 — bigger than the next seven tables
// combined, 6692 rows growing at 334.6/day, with no reaper. Both workers publish a full serialized graph on
// every estate-refresh cycle: ~12 KB a row, ~3.9 MB a day, ~1.4 GB a year on an LXC with a 21.5 GB disk.
//
// It is a PROJECTION, not a ledger. The graph is rebuilt from its sources on every refresh and nothing
// reconstructs history from this table, so deleting an old snapshot loses a re-derivable copy rather than a
// fact. That is what makes retention legitimate here and not on the audit spine.
//
// EVERY FLOOR LIVES IN THE DATABASE, not in this file. reap_estate_snapshot (migration 0065) clamps
// keep_per_plane to a minimum, refuses to touch the last 24 hours, keeps the first snapshot of each UTC day
// per plane, and journals the purge in the same transaction as the DELETE. This Go side cannot widen any of
// them — it has no parameter with which to name a row — which is the same shape reap_agent_step_evidence
// (TG-295) established for the evidence corpus.

import (
	"context"
	"fmt"
)

// MinKeepPerPlane mirrors the clamp inside reap_estate_snapshot. It is declared here so a caller can state
// the effective floor without reading SQL; TestTheGoFloorMatchesTheDatabaseFloor fails if the two drift.
const MinKeepPerPlane = 50

// DefaultKeepPerPlane retains roughly a day of per-plane detail at the measured rate (~167 rows/plane/day),
// with the daily sample carrying everything older. Deliberately not a duration: the reaper's predicate is
// recency RANK, so a plane that stops publishing keeps its last N rather than aging out of existence.
const DefaultKeepPerPlane = 200

// DefaultSnapshotReapBatch bounds one sweep. The first sweep after this ships has ~6000 rows to remove, and
// one unbounded DELETE on the largest table in the database holds locks and bloats WAL for as long as it
// takes. Bounded batches drain across ticks.
const DefaultSnapshotReapBatch = 5000

// EstateSnapshotReapStore is the pgx-backed retention path for estate_snapshot.
type EstateSnapshotReapStore struct{ p *Pool }

// NewEstateSnapshotReapStore returns the store over the shared pool.
func NewEstateSnapshotReapStore(p *Pool) *EstateSnapshotReapStore {
	return &EstateSnapshotReapStore{p: p}
}

// Reap removes one bounded batch of retired snapshots and returns how many rows went.
//
// A non-positive keepPerPlane or batch takes the defaults above; the DATABASE clamps them again, so a caller
// that passes 0 deliberately still cannot empty the table. Returning the count rather than an "it ran" bool
// is deliberate: a sweep that deletes 0 rows and a sweep that never ran must not read alike in a log, which
// is the confusion the wiring register exists to end elsewhere in this tree.
func (s *EstateSnapshotReapStore) Reap(ctx context.Context, keepPerPlane, batch int) (int64, error) {
	if keepPerPlane <= 0 {
		keepPerPlane = DefaultKeepPerPlane
	}
	if batch <= 0 {
		batch = DefaultSnapshotReapBatch
	}
	var deleted int64
	if err := s.p.QueryRow(ctx, `SELECT reap_estate_snapshot($1::int, $2::int)`, keepPerPlane, batch).Scan(&deleted); err != nil {
		return 0, fmt.Errorf("db: reap estate_snapshot (keep %d/plane, batch %d): %w", keepPerPlane, batch, err)
	}
	return deleted, nil
}
