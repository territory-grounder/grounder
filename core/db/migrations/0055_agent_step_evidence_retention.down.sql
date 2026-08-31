-- Reverse 0055. Dropping the function removes the ONLY path that can delete an agent_step_evidence row, so
-- the table returns to 0053's posture: append-only and unbounded. The explicit GRANT SELECT, INSERT is left
-- in place deliberately — it restates what the blanket default privilege already gave tg_runtime, and
-- revoking it here would take away the APPEND path and stop evidence being recorded at all.
DROP FUNCTION IF EXISTS reap_agent_step_evidence(timestamptz, integer);
DROP TABLE IF EXISTS agent_step_evidence_reap;
DROP INDEX IF EXISTS agent_step_evidence_created;
