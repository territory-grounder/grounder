DROP INDEX IF EXISTS action_execution_inverts;
ALTER TABLE action_execution DROP COLUMN IF EXISTS inverts_action_id;
