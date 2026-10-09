-- 0035_actor_attribution rollback: drop the attribution columns from session_triage.
ALTER TABLE session_triage
    DROP COLUMN IF EXISTS actor_attribution,
    DROP COLUMN IF EXISTS actor_evidence;
