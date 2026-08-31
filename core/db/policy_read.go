package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PolicyReadStore is the pgx-backed READ side of the Policy Engine console surface (spec/015 T-015-12,
// spec/006 read-surface). Four read-only projections over the worker-written policy state (migration 0019):
//   - Decisions:   the recent append-only policy_decision audit tail (newest-first, capped).
//   - Mode:        the single persisted active autonomy mode (absent → the reader reports Shadow, fail closed).
//   - Graduation:  the per-op-class earned-autonomy ladder rows.
//   - Ruleset:     the operator's active rules-as-data document + metadata.
//
// Every query is parameterized ($1) — no string-built SQL — and selects ONLY the non-secret columns the 0019
// tables hold (which are secret-free by construction, INV-13): rule ids, verdicts, bands, op-classes, levels,
// counts, and the rules-as-data document. No column here can carry key material, a secret, or a credential.
type PolicyReadStore struct{ p *Pool }

// NewPolicyReadStore returns the Postgres-backed policy read projections.
func NewPolicyReadStore(p *Pool) *PolicyReadStore { return &PolicyReadStore{p: p} }

// PolicyMatchedRule is one non-secret entry of a decision's deny-overrides provenance (REQ-2004): a rule id
// and its declared verdict, projected from policy_decision.matched_rules.
type PolicyMatchedRule struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
}

// PolicyDecisionRow is one policy_decision audit row (non-secret decision metadata only).
type PolicyDecisionRow struct {
	RuleID        string
	Verdict       string
	BandMode      string
	ComposedBand  string
	MinConfidence float64
	ActionID      string
	PlanHash      string
	Principal     string
	Mode          string
	BundleVersion string              // the in-force ACL bundle version this decision evaluated over (REQ-2004)
	MatchedRules  []PolicyMatchedRule // the FULL deny-overrides provenance (REQ-2004), non-secret {id, verdict}
	Reason        string              // the human-readable packet-tracer explanation of the decision (REQ-2004)
	CreatedAt     time.Time
}

// Decisions returns the recent policy_decision tail newest-first, capped at limit. Only non-secret columns
// are selected. An empty spine yields an empty slice (never a fabricated row).
func (s *PolicyReadStore) Decisions(ctx context.Context, limit int) ([]PolicyDecisionRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT rule_id, verdict, band_mode, composed_band, min_confidence, action_id, plan_hash, principal, mode, bundle_version, matched_rules, reason, created_at
		FROM policy_decision
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: policy decisions: %w", err)
	}
	defer rows.Close()
	var out []PolicyDecisionRow
	for rows.Next() {
		var r PolicyDecisionRow
		var matchedJSON []byte
		if err := rows.Scan(&r.RuleID, &r.Verdict, &r.BandMode, &r.ComposedBand, &r.MinConfidence,
			&r.ActionID, &r.PlanHash, &r.Principal, &r.Mode, &r.BundleVersion, &matchedJSON, &r.Reason, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: policy decision scan: %w", err)
		}
		if len(matchedJSON) > 0 {
			if err := json.Unmarshal(matchedJSON, &r.MatchedRules); err != nil {
				return nil, fmt.Errorf("db: policy decision matched_rules unmarshal: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Mode returns the single persisted active mode spelling and whether a row was present. An absent row (no
// mode ever persisted) returns ("", false, nil) — the reader reports the fail-closed Shadow posture, never a
// fabricated actuating mode.
func (s *PolicyReadStore) Mode(ctx context.Context) (string, bool, error) {
	var name string
	err := s.p.Pool.QueryRow(ctx, `SELECT mode FROM policy_mode WHERE singleton = true`).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("db: policy mode: %w", err)
	}
	return name, true, nil
}

// PolicyGraduationRow is one policy_graduation ladder row (non-secret op-class state).
type PolicyGraduationRow struct {
	OpClass       string
	Level         string
	CleanRunCount int
	LastOutcome   string
	UpdatedAt     time.Time
}

// Graduation returns every per-op-class ladder row, ordered by op-class. An empty projection yields an empty
// slice (never a fabricated row).
func (s *PolicyReadStore) Graduation(ctx context.Context) ([]PolicyGraduationRow, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT op_class, level, clean_run_count, last_outcome, updated_at
		FROM policy_graduation
		ORDER BY op_class`)
	if err != nil {
		return nil, fmt.Errorf("db: policy graduation: %w", err)
	}
	defer rows.Close()
	var out []PolicyGraduationRow
	for rows.Next() {
		var r PolicyGraduationRow
		if err := rows.Scan(&r.OpClass, &r.Level, &r.CleanRunCount, &r.LastOutcome, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: policy graduation scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PolicyRulesetRow is the active rules-as-data document plus its non-secret metadata.
type PolicyRulesetRow struct {
	Document  []byte // the operator policy JSON (rules-as-data, non-secret)
	RuleCount int
	UpdatedBy string
	UpdatedAt time.Time
	Present   bool // false when no ruleset has been persisted
}

// Ruleset returns the single active ruleset document + metadata, or Present=false when none has been
// persisted (the reader reports an honest empty ruleset, never a fabricated rule).
func (s *PolicyReadStore) Ruleset(ctx context.Context) (PolicyRulesetRow, error) {
	var r PolicyRulesetRow
	err := s.p.Pool.QueryRow(ctx, `
		SELECT document, rule_count, updated_by, updated_at FROM policy_ruleset WHERE singleton = true`).
		Scan(&r.Document, &r.RuleCount, &r.UpdatedBy, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyRulesetRow{Present: false}, nil
	}
	if err != nil {
		return PolicyRulesetRow{}, fmt.Errorf("db: policy ruleset: %w", err)
	}
	r.Present = true
	return r, nil
}
