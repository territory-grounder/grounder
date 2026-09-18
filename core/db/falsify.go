package db

import (
	"context"
	"encoding/json"
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
//
// SINGLE-WRITER (TG-184): predictions whose action ALREADY has an executed verdict (a row in action_verdict,
// written synchronously by the interceptor WITH the TG-148 baseline) are EXCLUDED via the LEFT JOIN /
// `av.action_id IS NULL` anti-join. Without this the async scorer would re-verdict an executed action with a
// DIFFERENT (weaker-baselined, false-deviation-prone) algorithm and, on an approve-gated-late-execution
// ordering, win the `ON CONFLICT DO NOTHING` race and persist the wrong verdict. The scorer is therefore
// authoritative ONLY for never-executed (read-only / propose-path) predictions, so action_verdict keeps
// exactly one writer per action_id and the append-only guarantee (migration 0015) is preserved untouched.
//
// EXECUTED (Phase C4): the anti-join alone cannot see an execution whose verdict the interceptor WITHHELD
// (an unverifiable post-state writes action_execution with a NULL verdict and no action_verdict row — TG-182)
// or failed to persist. Such a prediction is still an ACTION that ran, not a forecast, so each due row also
// carries `EXISTS(action_execution)`: the scorer falsifiability-scores it but authors no forecast verdict —
// the category split lives in the scorer, this query only reports the fact.
func (s *FalsifiabilityStore) DueForScoring(ctx context.Context, olderThan time.Time, limit int) ([]falsify.DuePrediction, error) {
	rows, err := s.p.Query(ctx, `
		SELECT p.plan_hash, p.action_id, p.target_host, p.site, p.predicted_hosts, p.predicted_rules,
		       p.control_hosts, p.prediction_hash, p.schema_version, p.committed_at,
		       p.predicted_host_confidence,
		       EXISTS (SELECT 1 FROM action_execution ae WHERE ae.action_id = p.action_id) AS executed
		FROM infragraph_prediction p
		LEFT JOIN action_verdict av ON av.action_id = p.action_id
		WHERE p.kind = 'action' AND p.tp IS NULL AND p.committed_at < $1
		  AND av.action_id IS NULL
		ORDER BY p.committed_at ASC
		LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("db: due predictions: %w", err)
	}
	defer rows.Close()
	var out []falsify.DuePrediction
	for rows.Next() {
		var (
			planHash, actionID, targetHost, site, predictionHash string
			ph, pr, ctrl, hc                                     []byte
			schemaVersion                                        int
			committedAt                                          time.Time
			executed                                             bool
		)
		if err := rows.Scan(&planHash, &actionID, &targetHost, &site, &ph, &pr, &ctrl,
			&predictionHash, &schemaVersion, &committedAt, &hc, &executed); err != nil {
			return nil, fmt.Errorf("db: due prediction scan: %w", err)
		}
		if err := schema.CheckRow(schema.TableInfragraphPrediction, schema.Version(schemaVersion)); err != nil {
			return nil, err // fail closed: a future/invalid prediction shape must not be falsifiability-scored (TG-495)
		}
		predictedHosts, err := keysToSet(ph)
		if err != nil {
			return nil, err
		}
		var hostConfidence map[string]float64
		if len(hc) > 0 {
			if err := json.Unmarshal(hc, &hostConfidence); err != nil {
				return nil, fmt.Errorf("db: unmarshal predicted_host_confidence: %w", err)
			}
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
					// TG-189: nil for rows committed before 0070, and for flat-graph models that never
					// carried confidence. Brier() reports UNSCORED on both rather than inventing a 0.0.
					HostConfidence: hostConfidence,
				},
				ControlHosts:   controlHosts,
				SchemaVersion:  schema.Version(schemaVersion),
				PredictionHash: predictionHash,
			},
			CommittedAt: committedAt,
			Executed:    executed,
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

// FalsifyBaseline adapts the durable ingest ledger into the scorer's COMMIT-TIME baseline seam
// (falsify.BaselineReader): the (host,rule) pairs and the open-incident hosts already firing as of a
// prediction's CommittedAt, both arms cut at received_at <= asOf so nothing later can launder in. This is
// the ONE place the error→ok mapping lives for the scoring lane, mirroring OpenIncidentsBaseline: a read
// error is (nil, nil, false) — NEVER (empty, empty, true), because an empty baseline asserts "nothing was
// already wrong", which is exactly the manufactured-deviation defect the baseline exists to close. The
// scorer skips (retries later) on false rather than adjudicating baseline-less.
func FalsifyBaseline(s *AlertHistoryStore, staleAfter time.Duration) falsify.BaselineReader {
	return func(ctx context.Context, asOf time.Time) ([]verify.ObservedAlert, map[string]bool, bool) {
		pairs, err := s.OpenIncidentPairs(ctx, asOf, staleAfter)
		if err != nil {
			return nil, nil, false
		}
		hosts, err := s.OpenIncidentHosts(ctx, asOf, staleAfter)
		if err != nil {
			return nil, nil, false
		}
		return pairs, hosts, true
	}
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
