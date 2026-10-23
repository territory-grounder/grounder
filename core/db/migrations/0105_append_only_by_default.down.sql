-- Restores the pre-0105 wildcard posture (every table mutable by tg_runtime), then re-applies the
-- per-table REVOKEs the earlier lattice had already made so the down lands on the exact 0104 state.
GRANT UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tg_runtime;
REVOKE DELETE ON alert_cluster, pending_verification FROM tg_runtime;
REVOKE UPDATE, DELETE ON governance_ledger, session_risk_audit, action_verdict, agent_step,
  agent_step_evidence, ingest_alert, ingest_alert_occurrence, ledger_anchor FROM tg_runtime;
