DROP INDEX IF EXISTS estate_snapshot_plane_captured_idx;
ALTER TABLE estate_snapshot DROP COLUMN IF EXISTS plane;
