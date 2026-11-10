DROP INDEX IF EXISTS session_triage_instrument;
ALTER TABLE triage_run    DROP COLUMN IF EXISTS instrument_id;
ALTER TABLE session_triage DROP COLUMN IF EXISTS instrument_id;
DROP TABLE IF EXISTS instrument_identity;
