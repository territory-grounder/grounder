package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/schema"
)

// TriageStore is the pgx judge spine over session_triage / session_judgment (migration 0010, spec/012
// REQ-1106): the Runner's compact terminal-record writer plus the judge cron's read/mark/write
// surface. Parameterized SQL only; idempotency is STRUCTURAL (the external_ref PK and the
// (external_ref, dimension) UNIQUE with ON CONFLICT), so an activity retry can never duplicate a
// session record or a judgment row. session_judgment's shape satisfies the queries
// skillstore_trial.go already runs against it (armScoresForDim, JudgedSessionRate) — landing this
// table is what turns those from honest empties into real trial data.
type TriageStore struct{ p *Pool }

// NewTriageStore returns the Postgres-backed judge-spine store.
func NewTriageStore(p *Pool) *TriageStore { return &TriageStore{p: p} }

// RecordTriage persists the compact terminal triage record — idempotent on external_ref (the FIRST
// terminal record for a session wins; a workflow-level retry or duplicate delivery is a no-op).
func (s *TriageStore) RecordTriage(ctx context.Context, row judge.TriageRow) error {
	sv, err := schema.Stamp(schema.TableSessionTriage)
	if err != nil {
		return err
	}
	if row.ExternalRef == "" {
		return fmt.Errorf("db: triage record with empty external_ref refused")
	}
	loads := row.SkillLoads
	if loads == nil {
		loads = []string{}
	}
	loadsJSON, err := json.Marshal(loads)
	if err != nil {
		return fmt.Errorf("db: marshal skill_loads: %w", err)
	}
	evidence := row.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO session_triage
			(external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, actor_attribution, actor_evidence, prompt_version, seed_hash, model_tier, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (external_ref) DO NOTHING`,
		row.ExternalRef, row.Host, row.AlertRule, row.Band, row.Outcome, row.Proposed, row.Op,
		evidence, row.Conclusion, loadsJSON, row.Prediction, row.Predicted, row.Confidence,
		attributionOrEmpty(row.Attribution), evidenceOrEmpty(row.ActorEvidence),
		row.PromptVersion, row.SeedHash, row.ModelTier, int(sv))
	if err != nil {
		return fmt.Errorf("db: record triage %s: %w", row.ExternalRef, err)
	}
	return nil
}

// attributionOrEmpty normalizes a pre-feature attribution to the zero/unknown convention ('').
func attributionOrEmpty(s string) string { return s }

// evidenceOrEmpty normalizes a nil actor-evidence blob to an empty JSON array (the '[]' default).
func evidenceOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}

// UnjudgedSince returns the unjudged sessions recorded inside the trailing window, oldest first — the
// judge cron's batch read. Sessions older than the window stay unjudged forever (honest: judging a
// stale record against a moved estate scores noise, not quality).
func (s *TriageStore) UnjudgedSince(ctx context.Context, window time.Duration, limit int) ([]judge.TriageRow, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, judged, created_at
		FROM session_triage
		WHERE NOT judged AND created_at > now() - $1::interval
		ORDER BY created_at ASC
		LIMIT $2`,
		fmt.Sprintf("%d seconds", int(window.Seconds())), limit)
	if err != nil {
		return nil, fmt.Errorf("db: unjudged sessions: %w", err)
	}
	defer rows.Close()
	var out []judge.TriageRow
	for rows.Next() {
		var r judge.TriageRow
		var loads []byte
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Band, &r.Outcome, &r.Proposed,
			&r.Op, &r.EvidenceIDs, &r.Conclusion, &loads, &r.Prediction, &r.Predicted, &r.Confidence, &r.Judged, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan triage row: %w", err)
		}
		if len(loads) > 0 {
			if err := json.Unmarshal(loads, &r.SkillLoads); err != nil {
				return nil, fmt.Errorf("db: skill_loads for %s: %w", r.ExternalRef, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentJudgedByVersion returns the judged sessions that composed a given skill version within the
// trailing window, newest first, capped at limit — the offline admission gate's discovery set
// (spec/014 REQ-1307). Read-only; a session appears only if it was judged (session_triage.judged). The
// composing version is matched by the `#<id>:store` anchor in the skill_load provenance (the bound LIKE
// value is built in Go, never SQL assembled from strings). By construction this yields only the skill's
// OWN judged sessions — never the sealed holdout, which lives outside session_triage.
func (s *TriageStore) RecentJudgedByVersion(ctx context.Context, versionID int64, window time.Duration, limit int) ([]judge.TriageRow, error) {
	loadPattern := fmt.Sprintf("%%#%d:store%%", versionID)
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, judged, created_at
		FROM session_triage t
		WHERE t.judged AND t.created_at > now() - $2::interval
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(t.skill_loads) e WHERE e LIKE $1)
		ORDER BY t.created_at DESC
		LIMIT $3`,
		loadPattern, fmt.Sprintf("%d seconds", int(window.Seconds())), limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: recent judged sessions for version %d: %w", versionID, err)
	}
	defer rows.Close()
	var out []judge.TriageRow
	for rows.Next() {
		var r judge.TriageRow
		var loads []byte
		if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Band, &r.Outcome, &r.Proposed,
			&r.Op, &r.EvidenceIDs, &r.Conclusion, &loads, &r.Prediction, &r.Predicted, &r.Confidence, &r.Judged, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan judged session: %w", err)
		}
		if len(loads) > 0 {
			if err := json.Unmarshal(loads, &r.SkillLoads); err != nil {
				return nil, fmt.Errorf("db: skill_loads for %s: %w", r.ExternalRef, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkJudged flags the session as judged so the next batch read skips it.
func (s *TriageStore) MarkJudged(ctx context.Context, externalRef string) error {
	if _, err := s.p.Exec(ctx,
		`UPDATE session_triage SET judged = true WHERE external_ref = $1`, externalRef); err != nil {
		return fmt.Errorf("db: mark judged %s: %w", externalRef, err)
	}
	return nil
}

// WriteJudgment upserts one (session, dimension) verdict — a re-judge overwrites, never duplicates,
// so armScoresForDim's join sees exactly one score per session per dimension.
func (s *TriageStore) WriteJudgment(ctx context.Context, externalRef, dimension string, score float64, comment string) error {
	sv, err := schema.Stamp(schema.TableSessionJudgment)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO session_judgment (external_ref, dimension, score, comment, schema_version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (external_ref, dimension)
		DO UPDATE SET score = EXCLUDED.score, comment = EXCLUDED.comment, judged_at = now()`,
		externalRef, dimension, score, comment, int(sv))
	if err != nil {
		return fmt.Errorf("db: write judgment %s/%s: %w", externalRef, dimension, err)
	}
	return nil
}
