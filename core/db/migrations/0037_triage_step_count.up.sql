-- 0037: carry the agent loop's read-only investigation step count into the durable triage record
-- (benchmark axis A6 — decision efficiency / MTTR contributor, docs/BENCHMARK-AXES.md).
--
-- The Runner already computes StepCount (the number of ReAct investigation cycles = len(res.Steps),
-- activities.go -> InvestigateResult.StepCount -> RunnerResult.StepCount) but discards it at the DB boundary,
-- so the live-axis scorer (cmd/axisscore) could not measure A6 and listed it as an unmeasurable coverage gap.
-- This persists it so A6 (mean decision steps per triage; lower = more efficient) is measured off real triages.
--
-- OBSERVABILITY ONLY — step_count is a persisted decision RECORD, it re-enters no gate. Additive, defaulted,
-- backward-compatible: pre-migration rows read as 0 (step count unknown), exactly what they were.
ALTER TABLE session_triage
    ADD COLUMN step_count int NOT NULL DEFAULT 0;
