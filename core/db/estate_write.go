package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/schema"
)

// EstateWriteStore is the pgx-backed WRITE side of the estate surface (REQ-516): the worker publishes a
// snapshot of its live causal graph after each build/refresh. INSERT-only (latest-wins is a read
// concern); every row is schema-version stamped from the canonical registry (REQ-505). Parameters are
// always bound ($1) — no string-built SQL.
type EstateWriteStore struct{ p *Pool }

// NewEstateWriteStore returns the Postgres-backed estate snapshot writer.
func NewEstateWriteStore(p *Pool) *EstateWriteStore { return &EstateWriteStore{p: p} }

// Publish writes one snapshot row. sourceCount is the number of live edge sources that seeded the graph.
func (s *EstateWriteStore) Publish(ctx context.Context, snap estate.Snapshot, sourceCount int) error {
	ver, err := schema.Stamp(schema.TableEstateSnapshot)
	if err != nil {
		return fmt.Errorf("db: estate snapshot stamp: %w", err)
	}
	graphJSON, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("db: estate snapshot marshal: %w", err)
	}
	_, err = s.p.Pool.Exec(ctx, `
		INSERT INTO estate_snapshot (node_count, edge_count, source_count, graph_json, schema_version)
		VALUES ($1, $2, $3, $4, $5)`,
		len(snap.Nodes), len(snap.Edges), sourceCount, graphJSON, int(ver))
	if err != nil {
		return fmt.Errorf("db: estate snapshot insert: %w", err)
	}
	return nil
}
