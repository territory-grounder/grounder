package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/calibrate"
)

// CalibrationReadStore is the pgx-backed READ side of the confidence calibrator (spec/020 T-020-15,
// REQ-2021): it joins the persisted agent confidence (session_triage.confidence, migration 0024) to the
// LLM-free mechanical verified outcome on infragraph_prediction (the falsify score, joined by external_ref,
// migration 0026 / T-020-5) — producing the (stated confidence, verified-clean) pairs the pure
// calibrate.Compute bins into a reliability curve. Read-only by construction — one bound SELECT, no
// mutation, every parameter bound ($1). OBSERVE-ONLY: it adjudicates nothing and gates nothing; the
// reliability it feeds is EVIDENCE for an operator, never an actuation input (INV-22).
type CalibrationReadStore struct{ p *Pool }

// NewCalibrationReadStore returns the Postgres-backed calibration paired-sample reader.
func NewCalibrationReadStore(p *Pool) *CalibrationReadStore { return &CalibrationReadStore{p: p} }

// compile-time proof it satisfies the sample seam the job depends on.
var _ calibrate.SampleReader = (*CalibrationReadStore)(nil)

// PairedSamples returns up to limit (stated confidence, verified-clean) pairs, newest first. Only SCORED
// predictions (tp IS NOT NULL) contribute — an UNSCORED prediction has no verified outcome yet, so it is
// never counted (never a false "clean"). Clean is the LLM-free falsify outcome: the prediction verified with
// NO over-prediction and NO under-prediction (fp = 0 AND fn = 0) — the agent's consequence model held, so
// its stated confidence was empirically right (the agent NEVER adjudicates its own outcome — INV-10). Only
// rows with a REAL stated confidence (t.confidence > 0) contribute: session_triage.confidence defaults to 0
// and, before the confidence-persist fix (T-020-1), EVERY row persisted 0 — a missing value, not a stated
// 0.0. Counting those would fabricate a garbage 0.0 bin (measured live: 197 such rows joined to scored
// predictions). A confidence of exactly 0 can never be a real proposing sample anyway: a prediction is
// scored only for a PROPOSED action, and the loop proposes only at confidence ≥ 0.7 — so a joined
// confidence=0 is always the unpopulated default. An empty result (the honest state until real proposing
// sessions with scored outcomes accrue) returns an empty slice, which calibrate.Compute renders as "no
// evidence yet", never a fabricated curve.
func (s *CalibrationReadStore) PairedSamples(ctx context.Context, limit int) ([]calibrate.Sample, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.p.Query(ctx, `
		SELECT t.confidence, (p.fp = 0 AND p.fn = 0) AS clean
		FROM session_triage t
		JOIN infragraph_prediction p
		  ON p.external_ref = t.external_ref AND p.kind = 'action'
		WHERE p.tp IS NOT NULL AND t.external_ref <> '' AND t.confidence > 0
		ORDER BY p.committed_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: calibration paired samples: %w", err)
	}
	defer rows.Close()
	var out []calibrate.Sample
	for rows.Next() {
		var sm calibrate.Sample
		if err := rows.Scan(&sm.Confidence, &sm.Clean); err != nil {
			return nil, fmt.Errorf("db: calibration sample scan: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// JudgedDiagnosisSamples is the SECOND reference class, and the reason it exists is that the first one
// alone is not interpretable (TG-335).
//
// The published curve scores `blast_radius_exact` — the prediction verified with fp = 0 AND fn = 0. That is
// a strict and SPECIFIC claim, not "the diagnosis was right", and the confidence alerts say so and tell the
// operator to compare the two before concluding the agent is overconfident. That comparison could not be
// made from the running system: this file computed exactly one outcome, `outcome` was a label with a single
// value in it, and the diagnosis figure the alert text quotes was computed somewhere else entirely. An
// operator following the alert's own guidance hit a dead end.
//
// The judge's `correct_diagnosis` dimension is the other class. It is a 1-5 rubric score, so it needs a
// threshold to become the boolean a reliability curve consumes, and the threshold is a JUDGEMENT stated
// here rather than buried: cleanScore is the lowest score that counts as a correct diagnosis.
//
// Read the two curves together, never apart. Measured 2026-08-06: blast_radius_exact has a base rate near
// 0.20 (hard, 89 paired samples) while correct_diagnosis sits near the top of the rubric across 3,235
// judged rows — a near-constant target. A forecaster looks brilliant against an easy outcome and terrible
// against a hard one WITHOUT changing at all, which is exactly why publishing one of them silently was the
// defect. The alerts' own words: "a negative skill against a near-unpredictable target may indict the
// target, not the forecaster."
//
// THE JUDGE NEVER SCORES ITS OWN CONFIDENCE (INV-10): session_judgment is written by the judge from the
// rubric, session_triage.confidence is stated by the agent before any outcome is known, and this join is
// the only place they meet.
func (s *CalibrationReadStore) JudgedDiagnosisSamples(ctx context.Context, limit int, cleanScore float64) ([]calibrate.Sample, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.p.Query(ctx, `
		SELECT t.confidence, (j.score >= $2) AS clean
		FROM session_triage t
		JOIN session_judgment j
		  ON j.external_ref = t.external_ref AND j.dimension = 'correct_diagnosis'
		WHERE t.external_ref <> '' AND t.confidence > 0
		ORDER BY j.judged_at DESC
		LIMIT $1`, limit, cleanScore)
	if err != nil {
		return nil, fmt.Errorf("db: calibration judged-diagnosis samples: %w", err)
	}
	defer rows.Close()
	var out []calibrate.Sample
	for rows.Next() {
		var sm calibrate.Sample
		if err := rows.Scan(&sm.Confidence, &sm.Clean); err != nil {
			return nil, fmt.Errorf("db: judged-diagnosis sample scan: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// DiagnosisSampleReader adapts JudgedDiagnosisSamples to calibrate.SampleReader, which takes only a limit.
// The threshold is bound here so the job stays a pure consumer of (confidence, clean) pairs and cannot
// silently score one outcome with another's rule.
type DiagnosisSampleReader struct {
	Store      *CalibrationReadStore
	CleanScore float64
}

func (r DiagnosisSampleReader) PairedSamples(ctx context.Context, limit int) ([]calibrate.Sample, error) {
	return r.Store.JudgedDiagnosisSamples(ctx, limit, r.CleanScore)
}
