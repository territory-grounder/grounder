package db

import (
	"context"
	"fmt"
)

// category_coverage.go — IS THE HIGH-RISK CATEGORY DRIVER REACHABLE? (TG-405)
//
// temporal/runner/workflow.go reads `env.Labels["category"]` and feeds it to safety.HighRiskCategory,
// which forces a POLL_PAUSE for {maintenance, security-incident, deployment}. The Alertmanager module
// passes EVERY label through raw, so that key carries whatever the estate's Prometheus rules put there.
//
// Measured 2026-08-06 over all 3,165 ingest_alert rows: 39 carried a `category`, and the values were
// agentic-platform, mesh-bgp, mesh-bfd, mesh-ipsec, storage-write-path, iac-hygiene, host-firewall,
// edge-control, real. SUBSYSTEM labels. Zero intersected the high-risk set, so the driver has never been
// reachable on a single production alert — and nothing said so, because "no alert was high-risk" and "the
// driver cannot see a high-risk value" are the same quiet 0.
//
// This returns the RAW (source, category, count) triples and lets the caller classify with
// safety.HighRiskCategory. The vocabulary is deliberately NOT duplicated into SQL: one definition of what
// counts as high-risk, in the package that owns the safety decision. A second copy here would drift, and a
// safety input measured against a different list than the one enforcing it is worse than no measurement.
type CategoryCount struct {
	SourceID string
	Category string // non-empty by construction — rows with no category are counted separately
	Count    int64
}

// CountCategoryValues returns every non-empty category value in use, by source, with its row count, plus
// the total row count per source as the denominator. Counts only; reads no payload.
func (s *Pool) CountCategoryValues(ctx context.Context) (values []CategoryCount, totals map[string]int64, err error) {
	totals = map[string]int64{}
	rows, err := s.Query(ctx, `SELECT source_id, count(*) FROM ingest_alert GROUP BY 1`)
	if err != nil {
		return nil, nil, fmt.Errorf("db: category totals: %w", err)
	}
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("db: category totals scan: %w", err)
		}
		totals[src] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db: category totals: %w", err)
	}

	vrows, err := s.Query(ctx, `
		SELECT source_id, labels_json->>'category' AS category, count(*)
		FROM ingest_alert
		WHERE labels_json ? 'category' AND COALESCE(labels_json->>'category','') <> ''
		GROUP BY 1, 2`)
	if err != nil {
		return nil, nil, fmt.Errorf("db: category values: %w", err)
	}
	defer vrows.Close()
	for vrows.Next() {
		var c CategoryCount
		if err := vrows.Scan(&c.SourceID, &c.Category, &c.Count); err != nil {
			return nil, nil, fmt.Errorf("db: category values scan: %w", err)
		}
		values = append(values, c)
	}
	if err := vrows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db: category values: %w", err)
	}
	return values, totals, nil
}
