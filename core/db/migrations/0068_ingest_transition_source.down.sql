DROP INDEX IF EXISTS ingest_transition_source_received_idx;
ALTER TABLE ingest_transition DROP COLUMN IF EXISTS source_id;
