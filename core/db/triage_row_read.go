package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/proposal"
)

// Row reads ONE session's triage record by external_ref — the same columns and the same decoding the judge
// cron's batch read uses (UnjudgedSince), so a record rendered from this row is the record the judge adjudicates
// (TG-581: the evidence pack's deliverable must be produced by the grader's own extractor over the grader's own
// facts). ok=false for an unknown ref — a real answer, not an error.
func (s *TriageStore) Row(ctx context.Context, externalRef string) (judge.TriageRow, bool, error) {
	rows, err := s.p.Query(ctx, `SELECT `+triageRowColumns+` FROM session_triage WHERE external_ref = $1 LIMIT 1`, externalRef)
	if err != nil {
		return judge.TriageRow{}, false, fmt.Errorf("db: triage row %s: %w", externalRef, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if rerr := rows.Err(); rerr != nil {
			return judge.TriageRow{}, false, fmt.Errorf("db: triage row %s: %w", externalRef, rerr)
		}
		return judge.TriageRow{}, false, nil
	}
	r, err := scanTriageRow(rows)
	if err != nil {
		return judge.TriageRow{}, false, err
	}
	return r, true, rows.Err()
}

// triageRowColumns is the ONE select list scanTriageRow decodes — shared by UnjudgedSince (the judge cron's batch
// read) and Row (one session), so the two reads can never drift apart.
const triageRowColumns = "external_ref, host, alert_rule, band, outcome, proposed, op, evidence_ids, conclusion, skill_loads, prediction, predicted, confidence, judged, created_at, actor_attribution, actor_evidence, undo_sketch, diagnosis"

// scanTriageRow decodes one session_triage row in the judge cron's column order (UnjudgedSince and Row share the
// SELECT list and this decoder, so the two reads can never drift apart). The honesty rules are the batch read's:
// a NULL or unparseable diagnosis blob reads as "no gradeable claim" (DiagnosisRecorded=false), a malformed
// actor-evidence blob reads as 0 records, and a malformed skill_loads blob is an error (it is our own writer's).
func scanTriageRow(rows pgx.Rows) (judge.TriageRow, error) {
	var r judge.TriageRow
	var loads, actorEv, diagnosis []byte
	if err := rows.Scan(&r.ExternalRef, &r.Host, &r.AlertRule, &r.Band, &r.Outcome, &r.Proposed,
		&r.Op, &r.EvidenceIDs, &r.Conclusion, &loads, &r.Prediction, &r.Predicted, &r.Confidence, &r.Judged, &r.CreatedAt,
		&r.ActorAttribution, &actorEv, &r.UndoSketch, &diagnosis); err != nil {
		return judge.TriageRow{}, fmt.Errorf("db: scan triage row: %w", err)
	}
	if len(diagnosis) > 0 {
		if err := json.Unmarshal(diagnosis, &r.Diagnosis); err == nil {
			r.DiagnosisRecorded = true
		} else {
			r.Diagnosis = proposal.Diagnosis{}
		}
	}
	if len(loads) > 0 {
		if err := json.Unmarshal(loads, &r.SkillLoads); err != nil {
			return judge.TriageRow{}, fmt.Errorf("db: skill_loads for %s: %w", r.ExternalRef, err)
		}
	}
	if len(actorEv) > 0 {
		var recs []struct{}
		if err := json.Unmarshal(actorEv, &recs); err == nil {
			r.ActorEvidenceCount = len(recs)
		}
	}
	return r, nil
}
