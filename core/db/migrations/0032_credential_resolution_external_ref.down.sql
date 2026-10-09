-- 0032 down: drop the additive external_ref correlation column + its index.
DROP INDEX IF EXISTS credential_resolution_ref_idx;
ALTER TABLE credential_resolution
    DROP COLUMN external_ref;
