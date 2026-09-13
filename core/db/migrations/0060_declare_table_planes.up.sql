-- 0060 — DECLARE EACH TABLE'S CREDENTIAL PLANE, so the withheld lists stop narrowing silently (TG-323).
--
-- TG-164 split the worker's Postgres authority: `tg_triage` may not write what RECORDS or AUTHORISES an
-- actuation, `tg_actuate` may not write the untrusted-content corpus a mutation is GROUNDED in. Those two
-- sets are hand-maintained Go slices, and a table absent from both is granted to BOTH planes.
--
-- That fail-OPEN default is deliberate and stays: deny-by-default would make a table added next month
-- unwritable by the plane that needs it, and the failure would surface as a permission error deep inside a
-- Temporal activity rather than at boot — the worst failure mode TG-164's own risk note names.
--
-- What was ungated is the OMISSION. The existing oracle catches a STALE entry (a name no longer in the
-- schema) and an entry in both lists. It cannot catch a MISSING one, so the control narrowed every time the
-- schema grew while the boot report kept printing a count that looked the same.
--
-- The classification now lives in a table COMMENT, next to the CREATE TABLE, where the person adding a
-- table is already thinking about what it holds. A test reads pg_description and fails on any unclassified
-- table — so the omission becomes a hard failure at test time instead of a silent widening in production.
--
-- `schema_migrations` is deliberately ABSENT. It is the migrator's own bookkeeping table, created by the
-- migrator rather than by any .up.sql — and CI applies these files with raw `psql -f`, where it does not
-- exist yet. Commenting on it there fails the whole run. It holds no plane-relevant data either, so the
-- test below skips it for the same reason rather than as a workaround.
--
-- This migration seeds TODAY'S classification exactly as TG-164 derived it by tracing the writers; it
-- changes no grant. Everything not in one of the two withheld lists is `both`, which is what the code
-- already does — written down rather than implied by absence.

COMMENT ON TABLE action_execution IS 'plane: actuation';
COMMENT ON TABLE action_manifest IS 'plane: both';
COMMENT ON TABLE action_verdict IS 'plane: actuation';
COMMENT ON TABLE agent_step IS 'plane: triage';
COMMENT ON TABLE agent_step_evidence IS 'plane: triage';
COMMENT ON TABLE agent_step_evidence_reap IS 'plane: both';
COMMENT ON TABLE auth_nonce IS 'plane: both';
COMMENT ON TABLE chat_cursor IS 'plane: both';
COMMENT ON TABLE chat_events IS 'plane: both';
COMMENT ON TABLE control_plane_config IS 'plane: both';
COMMENT ON TABLE cost_accrual IS 'plane: both';
COMMENT ON TABLE cost_breaker_state IS 'plane: both';
COMMENT ON TABLE credential_binding_projection IS 'plane: both';
COMMENT ON TABLE credential_coverage IS 'plane: both';
COMMENT ON TABLE credential_resolution IS 'plane: both';
COMMENT ON TABLE credential_sync_run IS 'plane: both';
COMMENT ON TABLE deferred_verdict IS 'plane: actuation';
COMMENT ON TABLE discovered_scheduled_reboots IS 'plane: both';
COMMENT ON TABLE escalation_queue IS 'plane: both';
COMMENT ON TABLE estate_snapshot IS 'plane: both';
COMMENT ON TABLE exec_class_decision IS 'plane: both';
COMMENT ON TABLE governance_ledger IS 'plane: both';
COMMENT ON TABLE graduation_credit IS 'plane: both';
COMMENT ON TABLE infragraph_cascade_stats IS 'plane: both';
COMMENT ON TABLE infragraph_prediction IS 'plane: both';
COMMENT ON TABLE ingest_alert IS 'plane: triage';
COMMENT ON TABLE ingest_transition IS 'plane: both';
COMMENT ON TABLE injected_fault IS 'plane: both';
COMMENT ON TABLE interceptor_gate_verdict IS 'plane: actuation';
COMMENT ON TABLE knowledge_embedding IS 'plane: both';
COMMENT ON TABLE manifest_entry IS 'plane: both';
COMMENT ON TABLE module_capability_projection IS 'plane: both';
COMMENT ON TABLE mutation_breaker_state IS 'plane: both';
COMMENT ON TABLE opclass_candidate IS 'plane: both';
COMMENT ON TABLE opclass_candidate_occurrence IS 'plane: both';
COMMENT ON TABLE opclass_ratified IS 'plane: both';
COMMENT ON TABLE operator_sessions IS 'plane: both';
COMMENT ON TABLE pending_decision IS 'plane: both';
COMMENT ON TABLE pending_verification IS 'plane: actuation';
COMMENT ON TABLE policy_decision IS 'plane: actuation';
COMMENT ON TABLE policy_graduation IS 'plane: both';
COMMENT ON TABLE policy_mode IS 'plane: both';
COMMENT ON TABLE policy_ruleset IS 'plane: both';
COMMENT ON TABLE policy_ruleset_version IS 'plane: both';
COMMENT ON TABLE prediction_verdict IS 'plane: both';
COMMENT ON TABLE regime_actuation IS 'plane: actuation';
COMMENT ON TABLE regime_resolution IS 'plane: actuation';
COMMENT ON TABLE runtime_posture IS 'plane: both';
COMMENT ON TABLE sealed_secret IS 'plane: both';
COMMENT ON TABLE session_judgment IS 'plane: both';
COMMENT ON TABLE session_risk_audit IS 'plane: both';
COMMENT ON TABLE session_triage IS 'plane: triage';
COMMENT ON TABLE skill IS 'plane: both';
COMMENT ON TABLE skill_trial IS 'plane: both';
COMMENT ON TABLE skill_trial_assignment IS 'plane: both';
COMMENT ON TABLE skill_version IS 'plane: both';
COMMENT ON TABLE skill_watch IS 'plane: both';
COMMENT ON TABLE sources IS 'plane: both';
