DROP INDEX IF EXISTS session_judgment_action;
DROP INDEX IF EXISTS session_judgment_rubric_version;
ALTER TABLE session_judgment DROP COLUMN IF EXISTS action_id;
ALTER TABLE session_judgment DROP COLUMN IF EXISTS rubric_version;
