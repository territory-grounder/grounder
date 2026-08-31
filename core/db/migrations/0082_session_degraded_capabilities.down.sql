-- 0082 down: drop the self-dependency degraded-capability stamp (TG-394 slice 3). Additive column, so the
-- rollback is a clean DROP — no pre-existing data depends on it (it was NULL on every row before this change).
ALTER TABLE session_triage DROP COLUMN IF EXISTS degraded_capabilities;
