package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/safety"
)

// PredictionVerdictStore is the pgx-backed, APPEND-ONLY writer for prediction_verdict (migration 0042): the
// async falsifiability scorer's grade of a committed blast-radius prediction whose action was NEVER EXECUTED.
//
// WHY THIS EXISTS SEPARATELY FROM VerdictStore. Both stores satisfy the same Commit signature, but they record
// two different claims:
//
//	action_verdict     — "TG did X; did the estate change the way the committed prediction said?"  ACTUATION accuracy.
//	prediction_verdict — "TG predicted Y would happen; did it?"  WORLD-MODEL accuracy, nothing was actuated.
//
// They shared one table until roadmap P2-2, and pooling them produced a verified-match rate that described
// neither. Measured live at the split: executed actions 85.7% match (24/28) against propose-path predictions
// 44.9% (22/49) — a pooled 59.7% that understated actuation accuracy by 26 points and overstated the
// predictor by 15, while 23 of 24 "deviations" were a world model being wrong about an estate TG never
// touched. A single number covering both cannot answer which subsystem is at fault, which is the only
// question it gets asked.
//
// Keeping the signature identical to VerdictStore is deliberate: core/falsify depends on the narrow interface,
// so which table a scorer writes to is a WIRING decision made once at the composition root, not a behaviour
// the scorer can get wrong.
type PredictionVerdictStore struct{ p *Pool }

// NewPredictionVerdictStore returns a Postgres-backed propose-path verdict writer.
func NewPredictionVerdictStore(p *Pool) *PredictionVerdictStore { return &PredictionVerdictStore{p: p} }

// Commit appends the mechanical verdict for a scored, never-executed prediction. A duplicate action_id is
// ignored (append-only, first-wins) — a verdict is never overwritten, matching action_verdict's semantics and
// the ledger doctrine that evidence is added to, never edited.
func (s *PredictionVerdictStore) Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error {
	if !safety.ValidVerdict(v) {
		return ErrInvalidVerdict
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO prediction_verdict (action_id, plan_hash, verdict, target_host, site, schema_version)
		VALUES ($1, $2, $3, $4, $5, 1)
		ON CONFLICT (action_id) DO NOTHING`,
		actionID, planHash, string(v), targetHost, site)
	if err != nil {
		return fmt.Errorf("db: commit prediction verdict %s: %w", actionID, err)
	}
	return nil
}
