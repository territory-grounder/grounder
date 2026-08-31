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

	// ROLLING WINDOW (TG-92). The sums above are ALL-TIME and cannot recover from a fixed defect.
	// The TG-61 blast-radius miscalibration left ~26 rows summing tp=1, fp=730 — one leaf guest's local
	// fault predicting ~130 co-hosted siblings — so an all-time ratio keeps reporting the CURRENT model as
	// badly calibrated until 730+ well-calibrated predictions dilute it. That is a metric that can never
	// say "fixed", which makes it useless for the readiness signal it feeds.
	//
	// These mirror the same sums over the most recent RecentWindow scored predictions, so the readiness
	// signal tracks the model that is actually running. The all-time figures stay, unchanged, because the
	// audit trail must not be rewritten by a calibration fix — the answer is to publish BOTH and be clear
	// which is which, not to quietly redefine the old number.
	RecentPredictions  int
	RecentSumTP        int
	RecentSumFP        int
	RecentSumFN        int
	RecentSumControlTP int
	// RecentWindow is the row cap actually applied, reported so a reader can tell "calibrated over 200
	// predictions" from "calibrated over 3" — a rolling ratio computed from a handful of rows is noise
	// wearing the same units as a result.
	RecentWindow int
}

// GroundingRecentWindow is how many of the most recently committed scored predictions the rolling
// calibration view covers. 200 is chosen to be large enough to be stable and small enough that a fixed
// predictor is visible within about a day of normal volume; it is a constant rather than a knob because
// a tunable window is an invitation to tune until the number looks right.
const GroundingRecentWindow = 200

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

	// The same sums over the most recent window (TG-92). Ordered by committed_at DESC with the primary key
	// as a tiebreak so the window is deterministic when several predictions share a timestamp — without it
	// two calls could cover different row sets and the ratio would flicker with no underlying change.
	out.RecentWindow = GroundingRecentWindow
	err = s.p.Pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(tp),0), COALESCE(sum(fp),0), COALESCE(sum(fn),0), COALESCE(sum(control_tp),0)
		FROM (
			SELECT tp, fp, fn, control_tp
			FROM infragraph_prediction
			WHERE tp IS NOT NULL
			ORDER BY committed_at DESC, prediction_hash DESC
			LIMIT $1
		) recent`, GroundingRecentWindow).
		Scan(&out.RecentPredictions, &out.RecentSumTP, &out.RecentSumFP, &out.RecentSumFN, &out.RecentSumControlTP)
	if err != nil {
		return out, fmt.Errorf("db: grounding recent predictions: %w", err)
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
