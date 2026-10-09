package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/estate"
)

// EstateReadStore is the pgx-backed READ side of the estate surface (REQ-516): the latest published
// snapshot row (worker-written) decoded back into the graph projection. Read-only — one bound query.
type EstateReadStore struct{ p *Pool }

// NewEstateReadStore returns the Postgres-backed estate snapshot reader.
func NewEstateReadStore(p *Pool) *EstateReadStore { return &EstateReadStore{p: p} }

// EstateRow is the latest snapshot: its capture time, counts, and the decoded graph projection.
type EstateRow struct {
	Found       bool
	CapturedAt  string
	NodeCount   int
	EdgeCount   int
	SourceCount int
	Graph       estate.Snapshot
}

// Latest returns the most recent estate snapshot for the plane whose graph is the ESTATE — Found=false when
// none exists yet.
//
// THE PLANE FILTER IS THE POINT (TG-346). Both workers publish to this table and their graphs differ by two
// orders of magnitude — 1863 edges from the triage plane, 17 from the actuation plane, written two seconds
// apart. This query used to be `ORDER BY captured_at DESC LIMIT 1` with nothing to distinguish them, so the
// console's estate view and every other consumer got whichever worker wrote last. It had not yet gone wrong
// only because the triage worker consistently writes a couple of seconds later; a restart or a slower
// refresh flips that and the estate silently becomes 17 edges wide.
//
// Rows written before the discriminator existed carry 'both' (the pre-split posture) and are accepted, so an
// upgrade does not blank the estate view. The actuation plane's rows are deliberately NOT eligible: that
// plane cannot hold estate read credentials by design, so its graph is a fragment, never the estate.
func (s *EstateReadStore) Latest(ctx context.Context) (EstateRow, error) {
	var row EstateRow
	var graphJSON []byte
	err := s.p.Pool.QueryRow(ctx, `
		SELECT captured_at::text, node_count, edge_count, source_count, graph_json
		FROM estate_snapshot
		WHERE plane IN ('triage', 'both')
		ORDER BY captured_at DESC
		LIMIT 1`).Scan(&row.CapturedAt, &row.NodeCount, &row.EdgeCount, &row.SourceCount, &graphJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return EstateRow{Found: false}, nil
	}
	if err != nil {
		return EstateRow{}, fmt.Errorf("db: estate read: %w", err)
	}
	if len(graphJSON) > 0 {
		if err := json.Unmarshal(graphJSON, &row.Graph); err != nil {
			return EstateRow{}, fmt.Errorf("db: estate graph decode: %w", err)
		}
	}
	row.Found = true
	return row, nil
}
