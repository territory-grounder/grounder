-- 0101 down — drop the optimistic-concurrency version token (TG-146 S3/S4). The store's guarded upsert
-- degrades to the prior blind last-writer-wins Save once the column is gone (the pre-TG-146 behavior).
ALTER TABLE policy_graduation DROP COLUMN version;
