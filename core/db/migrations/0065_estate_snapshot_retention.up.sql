-- estate_snapshot IS A PROJECTION, NOT A LEDGER, AND IT IS 60% OF THE DATABASE (TG-355).
--
-- Measured 2026-08-06 on dc1tg01:
--
--   estate_snapshot          84 MB    6692 rows   2026-07-17 .. 2026-08-06   334.6 rows/day
--   whole grounder database 140 MB
--
-- Bigger than the next seven tables combined, and ~60% of everything. Both workers publish a FULL serialized
-- graph on the estate-refresh cycle — the triage plane's 410 nodes / 1863 edges and the actuation plane's
-- 20 / 17 — roughly every five minutes each. ~12 KB per row, ~3.9 MB/day, ~1.4 GB/year from this table
-- alone, on an LXC with a 21.5 GB disk.
--
-- Three of the schema's sixty tables have a reaper. This is the one that needed one most.
--
-- WHY RETENTION IS LEGITIMATE HERE. A ledger row is evidence and must not be deleted. A snapshot is a
-- PROJECTION: the graph is rebuilt from its sources on every refresh, and nothing reconstructs history from
-- this table — EstateReadStore.Latest() reads the newest row for a plane. Deleting an old snapshot loses a
-- re-derivable copy, not a fact.
--
-- WHAT IS KEPT, and each clause is a floor rather than a preference:
--
--   1. The newest keep_per_plane rows FOR EACH PLANE. Per-plane matters: the two planes write at different
--      rates and a global "newest N" would let the chattier one evict the other's only recent snapshot —
--      exactly the confusion migration 0061 added the plane column to end.
--   2. The FIRST snapshot of each UTC day, per plane, forever. ~2 rows/day of coarse history instead of 334,
--      so "what did the estate look like last Tuesday" stays answerable at ~24 KB/day.
--   3. Nothing captured in the last 24 hours, whatever the other parameters say. Same floor as
--      reap_agent_step_evidence (migration 0055): a retention reaper legitimately never needs a cutoff of
--      now(), and the recent window is the one that would explain whatever just went wrong.
--
-- A MISCONFIGURED CALL CANNOT EMPTY THE TABLE. keep_per_plane is clamped to a hard minimum below, so
-- reap_estate_snapshot(0, …) keeps the same rows as reap_estate_snapshot(50, …). The floor is in the
-- database, not in the Go caller, for the same reason 0055 put its floor here.
--
-- SECURITY DEFINER, and the cost is the same one 0055 states plainly: the body runs as the function OWNER,
-- so a defect in it is a defect with owner privileges. It is written to keep that surface minimal — no
-- dynamic SQL, no identifier from a parameter, every parameter typed and range-checked before use,
-- search_path pinned, and NO parameter with which a caller can name WHICH row to delete. The shape of the
-- privilege matches the shape of retention.

CREATE INDEX IF NOT EXISTS estate_snapshot_captured_idx ON estate_snapshot (captured_at);

CREATE TABLE IF NOT EXISTS estate_snapshot_reap (
  id           bigserial PRIMARY KEY,
  reaped_at    timestamptz NOT NULL DEFAULT now(),
  keep_per_plane integer   NOT NULL,
  rows_deleted integer     NOT NULL,
  oldest_kept  timestamptz,
  schema_version integer   NOT NULL DEFAULT 1
);

-- plane: both — the sweep runs on BOTH workers (the reaper ranks per plane and keeps the newest N of each,
-- so two sweepers converge rather than fight), and migration 0060 requires every table to say which
-- credential plane may write it. A journal only one plane could write would leave the other's purges
-- unrecorded.
-- Append-only journal of every estate_snapshot purge. Written INSIDE reap_estate_snapshot, in the same
-- transaction as the DELETE, so a purge cannot happen unrecorded. The comment below carries ONLY the plane
-- token: TestEveryTableDeclaresItsPlane parses it as strings.Fields(after "plane:")[0], so "plane: both."
-- reads as the plane "both." and is rejected as unknown. Prose goes here, not there.
COMMENT ON TABLE estate_snapshot_reap IS 'plane: both';

-- MinKeepPerPlane in Go mirrors this literal; core/db has a test that fails if they drift.
CREATE OR REPLACE FUNCTION reap_estate_snapshot(keep_per_plane integer, max_rows integer)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
  deleted integer;
  oldest  timestamptz;
BEGIN
  -- THE FLOORS. Clamped, not rejected: a sweep on a cron must not stop running because someone typed a 0,
  -- and refusing here would leave the table growing while the error is investigated.
  IF keep_per_plane IS NULL OR keep_per_plane < 50 THEN
    keep_per_plane := 50;
  END IF;
  IF max_rows IS NULL OR max_rows <= 0 OR max_rows > 20000 THEN
    max_rows := 5000;
  END IF;

  WITH ranked AS (
    SELECT id,
           captured_at,
           row_number() OVER (PARTITION BY plane ORDER BY captured_at DESC) AS recency,
           row_number() OVER (PARTITION BY plane, date_trunc('day', captured_at)
                              ORDER BY captured_at ASC)                     AS day_rank
    FROM estate_snapshot
  ), deletable AS (
    SELECT id FROM ranked
    WHERE recency > keep_per_plane          -- not among the newest per plane
      AND day_rank > 1                       -- not the first of its UTC day for that plane
      AND captured_at < now() - interval '24 hours'
    ORDER BY captured_at ASC
    LIMIT max_rows
  )
  DELETE FROM estate_snapshot s USING deletable d WHERE s.id = d.id;
  GET DIAGNOSTICS deleted = ROW_COUNT;

  SELECT min(captured_at) INTO oldest FROM estate_snapshot;
  INSERT INTO estate_snapshot_reap (keep_per_plane, rows_deleted, oldest_kept)
  VALUES (keep_per_plane, deleted, oldest);

  RETURN deleted;
END;
$$;

COMMENT ON FUNCTION reap_estate_snapshot(integer, integer) IS
  'TG-355: bounded, journalled retention for the estate_snapshot PROJECTION. Keeps the newest N per plane, the first snapshot of each UTC day per plane, and everything from the last 24h. Cannot be aimed at a specific row.';

REVOKE ALL ON FUNCTION reap_estate_snapshot(integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reap_estate_snapshot(integer, integer) TO tg_runtime;
GRANT SELECT ON estate_snapshot_reap TO tg_runtime;
