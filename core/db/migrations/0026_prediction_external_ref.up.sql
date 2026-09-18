-- 0026: carry the session's external_ref onto the prediction row (spec/020 T-020-5, REQ-2019).
-- The falsifiability score (tp/fp/fn) that IS the LLM-free verified outcome lives on infragraph_prediction
-- (written by the deterministic verifier / falsify scorer, INV-10), keyed by (plan_hash, action_id) — but the
-- session correlation key (external_ref) was not carried onto it, so the persisted confidence
-- (session_triage.confidence, migration 0024) could not be joined to the verified outcome without a bridge
-- hop. This additive, defaulted, NON-SECRET column (external_ref, INV-13) closes the gap: the confidence
-- calibrator (T-020-15) joins session_triage ⋈ infragraph_prediction by external_ref in ONE hop. The proposal
-- already carries external_ref (proposal.Proposal.ExternalRef), so it is threaded at prediction-commit with no
-- new plumbing. Backward-compatible: pre-fix rows read as an empty external_ref.
ALTER TABLE infragraph_prediction
    ADD COLUMN external_ref text NOT NULL DEFAULT '';
