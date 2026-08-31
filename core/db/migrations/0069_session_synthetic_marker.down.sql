DROP INDEX IF EXISTS session_triage_synthetic_leak;
ALTER TABLE session_triage DROP COLUMN IF EXISTS synthetic;
