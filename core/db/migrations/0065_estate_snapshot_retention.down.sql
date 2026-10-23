-- Reverting removes the only bound on estate_snapshot, which grows ~3.9 MB/day with no other reaper
-- (TG-355). The journal is dropped with it: a purge record whose function no longer exists describes a
-- capability the database does not have.
DROP FUNCTION IF EXISTS reap_estate_snapshot(integer, integer);
DROP TABLE IF EXISTS estate_snapshot_reap;
DROP INDEX IF EXISTS estate_snapshot_captured_idx;
