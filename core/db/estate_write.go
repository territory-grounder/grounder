package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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

// Publish writes one snapshot row for the CALLING PLANE.
//
// The plane is not decoration (TG-346). Both workers publish here, and their graphs differ by two orders of
// magnitude: measured 2026-08-06 the triage plane wrote 410 nodes / 1863 edges and the actuation plane
// wrote 20 / 17, two seconds apart, into rows nothing could tell apart. Latest() ordered by time alone, so
// which graph a reader got was decided by which worker happened to write last. 191 of 502 snapshots in 24h
// were the impoverished one.
//
// An empty plane records as "both" — the historic, pre-split posture — rather than defaulting to triage.
// Guessing triage would let an actuation-plane graph be served as the estate.
func (s *EstateWriteStore) Publish(ctx context.Context, snap estate.Snapshot, sourceCount int, plane string) error {
	if strings.TrimSpace(plane) == "" {
		plane = "both"
	}
	ver, err := schema.Stamp(schema.TableEstateSnapshot)
	if err != nil {
		return fmt.Errorf("db: estate snapshot stamp: %w", err)
	}
	graphJSON, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("db: estate snapshot marshal: %w", err)
	}
	_, err = s.p.Pool.Exec(ctx, `
		INSERT INTO estate_snapshot (node_count, edge_count, source_count, graph_json, schema_version, plane)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		len(snap.Nodes), len(snap.Edges), sourceCount, graphJSON, int(ver), plane)
	if err != nil {
		return fmt.Errorf("db: estate snapshot insert: %w", err)
	}
	return nil
}
