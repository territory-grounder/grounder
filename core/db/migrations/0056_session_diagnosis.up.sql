-- THE TYPED CLAIM ON THE DURABLE RECORD (TG-201 part 1, spec/002 REQ amendment 2026-07-30).
--
-- TG-201 gave the agent a typed, source-bound diagnosis — root cause, mechanism, evidence FOR, evidence
-- AGAINST, alternatives ruled out — with every assertion bound by the ORCHESTRATOR against the ToolResult
-- ids it actually captured (INV-11). Nothing scored it, and nothing could: the judge is ASYNCHRONOUS. It
-- runs hours later, off session_triage, long after the transcript and the captured tool results are gone.
-- A claim that does not reach the record cannot be graded, so `diagnosis_grounded` (core/judge.ScoreDiagnosis)
-- would have been a dimension over an empty struct — decoration, exactly what the ticket exists to end.
--
-- `cited` IS PERSISTED WITH THE CLAIM, DELIBERATELY. It is not recomputable downstream: the only authority
-- for "this id names an observation the orchestrator really captured" is the orchestrator, at bind time.
-- Re-deriving it later would have to trust the model's own citation — the plausible, well-formed, fabricated
-- id this whole mechanism exists to catch.
--
-- NULLABLE ON PURPOSE — this is the backward-compatible default. NULL means "this session ran before the
-- column existed", which is NOT the same fact as "the agent bound no claim" (an explicit '{}'). The judge
-- reads that difference (judge.TriageRow.DiagnosisRecorded) and scores the dimension N/A for pre-feature
-- rows, so ~3,200 historical sessions are not retroactively graded against a rule they were never offered.
-- Flooring them instead is the TG-61 failure: a dimension floored across a whole population fired the
-- flywheel's Regressed trigger for every skill at once.
--
-- OBSERVABILITY + SCORING ONLY. Untrusted DATA (INV-08): a recorded contradiction vetoes nothing and
-- releases nothing; it makes the claim checkable. Additive and defaulted — every existing row and every
-- pre-field writer stays valid.
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS diagnosis jsonb;

COMMENT ON COLUMN session_triage.diagnosis IS
  'Typed, source-bound diagnosis the proposal rested on (core/proposal.Diagnosis, TG-201): root_cause, mechanism, supporting[], contradicting[], ruled_out[], each ref carrying the orchestrator-decided "cited" flag. Scored deterministically as the diagnosis_grounded judge dimension. NULL = the session predates this column (scored N/A, never floored); ''{}'' = the agent bound no claim. Untrusted DATA — it gates nothing (INV-08).';
