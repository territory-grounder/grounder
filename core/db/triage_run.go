package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/schema"
)

// RecordRun writes one triage_run row (migration 0121, TG-633): the per-RUN record that session_triage, keyed
// on the alert and first-write-wins, cannot hold. runID is the Temporal workflow RUN id, so an activity retry
// is a no-op (ON CONFLICT DO NOTHING) while a new run of the same alert gets its own row.
//
// The fields are the run-level benchmark inputs as the record activity already carries them. step_count is
// taken as given: session_triage's zero-derivation counts agent_step by external_ref, which spans every run of
// the alert and so is not a per-run number.
func (s *TriageStore) RecordRun(ctx context.Context, runID string, row judge.TriageRow) error {
	if strings.TrimSpace(row.ExternalRef) == "" || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("db: triage run with empty external_ref or run_id refused")
	}
	sv, err := schema.Stamp(schema.TableTriageRun)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO triage_run (external_ref, run_id, host, alert_rule, band, outcome, proposed, op_class, confidence,
		                        step_count, decision_ms, model_tier, decision_model_tier, harness_fp, mutated, schema_version, instrument_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (external_ref, run_id) DO NOTHING`,
		row.ExternalRef, runID, row.Host, row.AlertRule, row.Band, row.Outcome, row.Proposed, row.OpClass, row.Confidence,
		row.StepCount, row.DecisionMillis, row.ModelTier, row.DecisionTier, row.HarnessFP, row.Mutated, sv, row.InstrumentID)
	if err != nil {
		return fmt.Errorf("db: record triage run %s/%s: %w", row.ExternalRef, runID, err)
	}
	return nil
}
