package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// PredictionStore is the pgx-backed, APPEND-ONLY implementation of predict.PredictionStore over the
// infragraph_prediction table (migration 0002). Commit is idempotent-append: a duplicate (plan_hash, kind)
// is ignored via ON CONFLICT DO NOTHING, so the committed prediction identity is immutable — the runtime
// role holds no UPDATE/DELETE on the immutable columns (the score columns are the sole verify-time write).
// The in-memory predict.MemPredictionStore is the oracle twin of this type; both satisfy the same interface,
// so tests run pure-Go and this runs under compose.
type PredictionStore struct{ p *Pool }

// NewPredictionStore returns a Postgres-backed prediction store.
func NewPredictionStore(p *Pool) *PredictionStore { return &PredictionStore{p: p} }

// compile-time proof it satisfies the interface the gate depends on.
var _ predict.PredictionStore = (*PredictionStore)(nil)

// sortedKeys renders a set as a stable JSON array so the stored jsonb is deterministic (and diff-stable).
func sortedKeys(set map[string]struct{}) ([]byte, error) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return json.Marshal(keys)
}

func keysToSet(raw []byte) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	if len(raw) == 0 {
		return set, nil
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, err
	}
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set, nil
}

// ruleKeysToJSON renders a set of verify.RuleKey values (host+"\x00"+rule) as a NUL-FREE, deterministic jsonb
// array of [host, rule] pairs. Postgres jsonb CANNOT store a NUL (U+0000, unlike the json type), and RuleKey uses a NUL as
// its unambiguous in-band separator — so marshaling the raw keys as strings makes every non-empty prediction
// error on the durable Commit. Splitting each key into a structured pair stores the same information without
// any NUL, and round-trips exactly (host and rule never contain a NUL themselves).
func ruleKeysToJSON(set map[string]struct{}) ([]byte, error) {
	pairs := make([][2]string, 0, len(set))
	for k := range set {
		h, r, _ := strings.Cut(k, "\x00")
		pairs = append(pairs, [2]string{h, r})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return json.Marshal(pairs)
}

// jsonToRuleKeys reverses ruleKeysToJSON, rejoining each [host, rule] pair into its verify.RuleKey.
func jsonToRuleKeys(raw []byte) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	if len(raw) == 0 {
		return set, nil
	}
	var pairs [][2]string
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return nil, err
	}
	for _, p := range pairs {
		set[verify.RuleKey(p[0], p[1])] = struct{}{}
	}
	return set, nil
}

// Commit appends the prediction row (kind='action'). A duplicate plan_hash is ignored (append-only,
// first-wins). The jsonb sets are stored sorted; the score columns are left NULL for the verifier to fill.
func (s *PredictionStore) Commit(ctx context.Context, rec predict.PredictionRecord) error {
	ph, err := sortedKeys(rec.Prediction.PredictedHosts)
	if err != nil {
		return fmt.Errorf("db: marshal predicted_hosts: %w", err)
	}
	pr, err := ruleKeysToJSON(rec.Prediction.PredictedRules)
	if err != nil {
		return fmt.Errorf("db: marshal predicted_rules: %w", err)
	}
	ctrl, err := sortedKeys(rec.ControlHosts)
	if err != nil {
		return fmt.Errorf("db: marshal control_hosts: %w", err)
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO infragraph_prediction
		  (plan_hash, kind, action_id, target_host, site, external_ref, predicted_hosts, predicted_rules, control_hosts,
		   prediction_hash, schema_version)
		VALUES ($1, 'action', $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, $9, $10)
		ON CONFLICT (plan_hash, kind) DO NOTHING`,
		rec.Prediction.PlanHash, rec.Prediction.ActionID, rec.Prediction.TargetHost, rec.Prediction.Site, rec.ExternalRef,
		string(ph), string(pr), string(ctrl), rec.PredictionHash, int(rec.SchemaVersion))
	if err != nil {
		return fmt.Errorf("db: commit prediction %s: %w", rec.Prediction.PlanHash, err)
	}
	return nil
}

// Has reports whether an action prediction is committed for planHash.
func (s *PredictionStore) Has(ctx context.Context, planHash string) (bool, error) {
	var exists bool
	err := s.p.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM infragraph_prediction WHERE plan_hash = $1 AND kind = 'action')", planHash).
		Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("db: has prediction %s: %w", planHash, err)
	}
	return exists, nil
}

// Get returns the committed action prediction for planHash.
func (s *PredictionStore) Get(ctx context.Context, planHash string) (predict.PredictionRecord, bool, error) {
	var (
		actionID, targetHost, site, externalRef, predictionHash string
		ph, pr, ctrl                                            []byte
		schemaVersion                                           int
	)
	err := s.p.QueryRow(ctx, `
		SELECT action_id, target_host, site, external_ref, predicted_hosts, predicted_rules, control_hosts,
		       prediction_hash, schema_version
		FROM infragraph_prediction WHERE plan_hash = $1 AND kind = 'action'`, planHash).
		Scan(&actionID, &targetHost, &site, &externalRef, &ph, &pr, &ctrl, &predictionHash, &schemaVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return predict.PredictionRecord{}, false, nil
		}
		return predict.PredictionRecord{}, false, fmt.Errorf("db: get prediction %s: %w", planHash, err)
	}
	predictedHosts, err := keysToSet(ph)
	if err != nil {
		return predict.PredictionRecord{}, false, err
	}
	predictedRules, err := jsonToRuleKeys(pr)
	if err != nil {
		return predict.PredictionRecord{}, false, err
	}
	controlHosts, err := keysToSet(ctrl)
	if err != nil {
		return predict.PredictionRecord{}, false, err
	}
	return predict.PredictionRecord{
		Prediction: verify.Prediction{
			ActionID: actionID, PlanHash: planHash, TargetHost: targetHost, Site: site,
			PredictedHosts: predictedHosts, PredictedRules: predictedRules,
		},
		ControlHosts:   controlHosts,
		SchemaVersion:  schema.Version(schemaVersion),
		PredictionHash: predictionHash,
		ExternalRef:    externalRef,
	}, true, nil
}
