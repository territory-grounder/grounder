package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// FalsifiabilityStore is the pgx-backed verify-time writeback over infragraph_prediction (migration 0002):
// it reads committed-but-unscored predictions whose observation window has elapsed and writes the score
// columns (tp/fp/fn/control_tp/control_fp) back onto them. Those columns are the SOLE verify-time write the
// role model permits — the prediction identity (action_id, host sets, hashes) committed before the poll is
// immutable, so this never rewrites it. The in-memory falsify.MemStore is the oracle twin.
type FalsifiabilityStore struct{ p *Pool }

// NewFalsifiabilityStore returns the Postgres-backed verify-time falsifiability writeback store.
func NewFalsifiabilityStore(p *Pool) *FalsifiabilityStore { return &FalsifiabilityStore{p: p} }

// compile-time proof it satisfies the seams the Scorer depends on.
var (
	_ falsify.UnscoredReader = (*FalsifiabilityStore)(nil)
	_ falsify.ScoreWriter    = (*FalsifiabilityStore)(nil)
)

// DueForScoring returns action predictions the verifier has not yet scored (tp IS NULL) whose commit time
// predates olderThan, oldest first, up to limit. The jsonb host/rule sets round-trip through the same
// helpers the prediction store uses (keysToSet / jsonToRuleKeys), so the reconstructed record scores exactly
// as the committed one. olderThan and limit are BOUND parameters ($1/$2) — never string-built.
func (s *FalsifiabilityStore) DueForScoring(ctx context.Context, olderThan time.Time, limit int) ([]falsify.DuePrediction, error) {
	rows, err := s.p.Query(ctx, `
		SELECT plan_hash, action_id, target_host, site, predicted_hosts, predicted_rules, control_hosts,
		       prediction_hash, schema_version, committed_at
		FROM infragraph_prediction
		WHERE kind = 'action' AND tp IS NULL AND committed_at < $1
		ORDER BY committed_at ASC
		LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("db: due predictions: %w", err)
	}
	defer rows.Close()
	var out []falsify.DuePrediction
	for rows.Next() {
		var (
			planHash, actionID, targetHost, site, predictionHash string
			ph, pr, ctrl                                         []byte
			schemaVersion                                        int
			committedAt                                          time.Time
		)
		if err := rows.Scan(&planHash, &actionID, &targetHost, &site, &ph, &pr, &ctrl,
			&predictionHash, &schemaVersion, &committedAt); err != nil {
			return nil, fmt.Errorf("db: due prediction scan: %w", err)
		}
		predictedHosts, err := keysToSet(ph)
		if err != nil {
			return nil, err
		}
		predictedRules, err := jsonToRuleKeys(pr)
		if err != nil {
			return nil, err
		}
		controlHosts, err := keysToSet(ctrl)
		if err != nil {
			return nil, err
		}
		out = append(out, falsify.DuePrediction{
			Record: predict.PredictionRecord{
				Prediction: verify.Prediction{
					ActionID: actionID, PlanHash: planHash, TargetHost: targetHost, Site: site,
					PredictedHosts: predictedHosts, PredictedRules: predictedRules,
				},
				ControlHosts:   controlHosts,
				SchemaVersion:  schema.Version(schemaVersion),
				PredictionHash: predictionHash,
			},
			CommittedAt: committedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: due predictions iterate: %w", err)
	}
	return out, nil
}

// WriteScore writes the verify-time score columns onto the committed prediction row, ONLY while it is still
// unscored (WHERE tp IS NULL) — so a concurrent/repeated pass never double-scores. It returns whether a row
// was updated (RowsAffected > 0). This is the single UPDATE the runtime performs on infragraph_prediction;
// every column it sets is a score column, never a prediction-identity column.
func (s *FalsifiabilityStore) WriteScore(ctx context.Context, planHash string, sc falsify.Score) (bool, error) {
	tag, err := s.p.Exec(ctx, `
		UPDATE infragraph_prediction
		SET tp = $2, fp = $3, fn = $4, control_tp = $5, control_fp = $6
		WHERE plan_hash = $1 AND kind = 'action' AND tp IS NULL`,
		planHash, sc.TP, sc.FP, sc.FN, sc.ControlTP, sc.ControlFP)
	if err != nil {
		return false, fmt.Errorf("db: write score %s: %w", planHash, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CascadeStatsStore is the pgx-backed, APPEND-ONLY writer over infragraph_cascade_stats (migration 0002):
// one windowed real-vs-control aggregate per scoring pass (INV-22 over-prediction gating). It satisfies
// falsify.CascadeStatsWriter.
type CascadeStatsStore struct{ p *Pool }

// NewCascadeStatsStore returns the Postgres-backed cascade-stats window writer.
func NewCascadeStatsStore(p *Pool) *CascadeStatsStore { return &CascadeStatsStore{p: p} }

var _ falsify.CascadeStatsWriter = (*CascadeStatsStore)(nil)

// AppendWindow inserts one cascade-stats window (append-only; all values BOUND, never string-built).
func (s *CascadeStatsStore) AppendWindow(ctx context.Context, w falsify.CascadeWindow) error {
	_, err := s.p.Exec(ctx, `
		INSERT INTO infragraph_cascade_stats
		  (window_start, window_end, real_tp, control_tp, control_ratio, falsifiable)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		w.Start, w.End, w.RealTP, w.ControlTP, w.ControlRatio, w.Falsifiable)
	if err != nil {
		return fmt.Errorf("db: append cascade window: %w", err)
	}
	return nil
}
