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

// Latest returns the most recent estate snapshot, Found=false when none exists yet.
func (s *EstateReadStore) Latest(ctx context.Context) (EstateRow, error) {
	var row EstateRow
	var graphJSON []byte
	err := s.p.Pool.QueryRow(ctx, `
		SELECT captured_at::text, node_count, edge_count, source_count, graph_json
		FROM estate_snapshot
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
