package db

import (
	"context"
	"fmt"
)

// GroundingReadStore is the pgx-backed READ side of the grounding scorecard (REQ-517): live aggregates
// over the verdict, prediction, and audit tables. Read-only — three bound aggregate queries.
type GroundingReadStore struct{ p *Pool }

// NewGroundingReadStore returns the Postgres-backed grounding aggregator.
func NewGroundingReadStore(p *Pool) *GroundingReadStore { return &GroundingReadStore{p: p} }

// GroundingAgg is the raw aggregate the composition maps to the scorecard DTO.
type GroundingAgg struct {
	Verdicts     map[string]int
	Predictions  int
	SumTP        int
	SumFP        int
	SumFN        int
	SumControlTP int
	Bands        map[string]int
}

// Aggregate computes the scorecard aggregates: verdict distribution, prediction scoring sums, and the
// band distribution (latest classification per external_ref).
func (s *GroundingReadStore) Aggregate(ctx context.Context) (GroundingAgg, error) {
	out := GroundingAgg{Verdicts: map[string]int{}, Bands: map[string]int{}}

	vr, err := s.p.Pool.Query(ctx, `SELECT verdict::text, count(*) FROM action_verdict GROUP BY 1`)
	if err != nil {
		return out, fmt.Errorf("db: grounding verdicts: %w", err)
	}
	for vr.Next() {
		var v string
		var n int
		if err := vr.Scan(&v, &n); err != nil {
			vr.Close()
			return out, fmt.Errorf("db: grounding verdict scan: %w", err)
		}
		out.Verdicts[v] = n
	}
	vr.Close()
	if err := vr.Err(); err != nil {
		return out, err
	}

	// prediction scoring sums over predictions the verifier has scored (tp not null).
	err = s.p.Pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(tp),0), COALESCE(sum(fp),0), COALESCE(sum(fn),0), COALESCE(sum(control_tp),0)
		FROM infragraph_prediction WHERE tp IS NOT NULL`).
		Scan(&out.Predictions, &out.SumTP, &out.SumFP, &out.SumFN, &out.SumControlTP)
	if err != nil {
		return out, fmt.Errorf("db: grounding predictions: %w", err)
	}

	br, err := s.p.Pool.Query(ctx, `
		SELECT band::text, count(*) FROM (
			SELECT DISTINCT ON (external_ref) band FROM session_risk_audit ORDER BY external_ref, created_at DESC
		) t GROUP BY 1`)
	if err != nil {
		return out, fmt.Errorf("db: grounding bands: %w", err)
	}
	for br.Next() {
		var b string
		var n int
		if err := br.Scan(&b, &n); err != nil {
			br.Close()
			return out, fmt.Errorf("db: grounding band scan: %w", err)
		}
		out.Bands[b] = n
	}
	br.Close()
	return out, br.Err()
}
