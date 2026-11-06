-- 0028: add the external_ref correlation key to policy_decision (spec/020 T-020-3, REQ-2005).
--
-- policy_decision already carries a non-secret action_id + principal (migration 0019), but both were written
-- EMPTY because the interceptor never threaded the bound action + acting principal into the policy EvalInput
-- (a documented T-015-13 debt). The decision tracer joins the policy audit into the per-incident walk by BOTH
-- correlation keys — action_id AND external_ref — but policy_decision had no external_ref column, so the walk
-- needed a bridge hop. This one additive, defaulted, NON-SECRET column closes it: external_ref is the same
-- normalized trigger id (the session's incident ref) the rest of the spine keys on, so a persisted decision
-- joins straight into ingest→classify→propose→verify with no bridge. Backward-compatible: pre-fix rows read
-- as an empty external_ref (the correlation was simply absent, exactly as before). policy_decision is
-- append-only (0019 REVOKE); this is a DDL add by the migration role, not a runtime write.
ALTER TABLE policy_decision
    ADD COLUMN external_ref text NOT NULL DEFAULT '';
