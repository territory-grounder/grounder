ALTER TABLE tracker_entry ALTER COLUMN issue_id DROP DEFAULT;
COMMENT ON COLUMN tracker_entry.issue_id IS NULL;
