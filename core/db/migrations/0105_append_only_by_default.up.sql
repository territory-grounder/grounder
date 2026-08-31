-- TG-80 P1-3: INVERT the runtime role's privilege model from opt-OUT to opt-IN.
--
-- Before: deploy/postgres-init/00-roles.sh granted SELECT, INSERT, UPDATE, DELETE on every table by
-- default, and ~24 migrations then REVOKEd UPDATE/DELETE one audit table at a time (0015, 0018, 0019,
-- 0020 ×3, 0022, 0029, 0030, 0031, 0033, 0034, 0042, 0043, 0048, 0049, 0050, 0053, 0055, 0058, 0074,
-- 0075, 0085, 0092). A privilege model assembled by wildcard-then-subtract: a forgotten REVOKE silently
-- shipped a mutable audit table, and nothing could notice. Measured on prod 2026-08-22: tg_runtime still
-- held UPDATE/DELETE on 53 of the public tables — 7 of them with NO writer anywhere in the Go tree.
--
-- After: 00-roles.sh births every new table SELECT+INSERT only (the opt-in floor), and THIS migration
-- retrofits the existing tables: revoke everything, then grant back EXACTLY the traced mutable working
-- set. The allowlist below is derived from a whole-tree trace of every UPDATE / DELETE / INSERT … ON
-- CONFLICT DO UPDATE the code issues (TG-80 trace, 2026-08-22), not from comments — and it is pinned by
-- core/db/append_only_default_test.go so it can only change deliberately.
--
-- Plane roles need nothing: tg_apply_plane_grants (0059/0066) mirrors tg_runtime privilege-by-privilege
-- on every grounder boot, REVOKEing the complement, so this narrowing propagates to tg_triage/tg_actuate
-- at the next start. Reapers keep using the 0055/0065 SECURITY DEFINER pattern (EXECUTE, not DELETE).

REVOKE UPDATE, DELETE ON ALL TABLES IN SCHEMA public FROM tg_runtime;

-- UPDATE-only (38): upserts, latest-wins projections, guarded state transitions.
GRANT UPDATE ON action_manifest, action_prestate, alert_cluster, chat_cursor, commit_confirm,
  cost_accrual, cost_breaker_state, credential_coverage, credential_sync_run,
  discovered_scheduled_reboots, discovery_deviation, escalation_queue, guest_config_baseline,
  guest_liveness, infragraph_prediction, injected_fault, manifest_entry,
  module_capability_projection, mutation_breaker_state, observation_probe, opclass_candidate,
  pending_decision, pending_verification, policy_engine_toggle, policy_graduation, policy_mode,
  policy_ruleset, runtime_posture, sealed_secret, session_judgment, session_triage, skill,
  skill_chain_head, skill_trial, skill_version, skill_watch, sources, tracker_entry
  TO tg_runtime;

-- DELETE-only (4): replace-snapshot tables and delete-by-id registries.
GRANT DELETE ON co_occurrence, co_occurrence_host, credential_native_rule, estate_object_group
  TO tg_runtime;

-- Both (4): upsert + explicit delete paths.
GRANT UPDATE, DELETE ON control_plane_config, credential_binding_projection, knowledge_embedding,
  operator_sessions
  TO tg_runtime;

-- Deliberately NOT re-granted (no writer in the tree): auth_nonce (the "pruned by a Temporal schedule"
-- comment in core/db/nonce.go describes a pruner that does not exist — when it does, it is a SECURITY
-- DEFINER reaper per 0055, not a DELETE grant), chat_events, estate_snapshot + estate_snapshot_reap
-- (0065 already intended EXECUTE/SELECT only), infragraph_cascade_stats, schema_migrations (written on
-- the migration DSN), skill_trial_assignment.
