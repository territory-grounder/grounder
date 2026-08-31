-- TG-346: estate_snapshot conflates two planes' graphs, and Latest() picks by time alone.
--
-- Both the triage and actuation workers publish here. Measured 2026-08-06:
--
--   03:12:32   410 nodes  1863 edges  3 sources   <- triage
--   03:12:30    20 nodes    17 edges  1 source    <- actuation
--
-- 191 of 502 snapshots in 24h are the 0-17-edge actuation-plane graph. EstateReadStore.Latest() is
-- `ORDER BY captured_at DESC LIMIT 1` with nothing to tell them apart, so the console's estate view and
-- every other consumer get whichever plane wrote most recently.
--
-- It has not gone wrong yet only because the triage worker consistently writes ~2 seconds after the
-- actuation worker. Nothing enforces that. A restart, a slower refresh, or a deploy flips the order and the
-- estate silently becomes 17 edges wide.
--
-- DEFAULT 'both' is the honest backfill: existing rows were written before planes were distinguishable and
-- there is no way to recover which wrote them. It is NOT 'triage' — labelling 191 known-actuation rows as
-- triage would make the new discriminator lie about the exact rows that motivated it.
ALTER TABLE estate_snapshot ADD COLUMN IF NOT EXISTS plane text NOT NULL DEFAULT 'both';

-- The read path is "newest row for this plane", so the index must lead with plane.
CREATE INDEX IF NOT EXISTS estate_snapshot_plane_captured_idx
  ON estate_snapshot (plane, captured_at DESC);
