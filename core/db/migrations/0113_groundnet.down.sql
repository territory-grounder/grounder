-- Reverse 0113: drop the two append-only groundnet federation tables. Dropping a table removes every
-- grant/revoke and the plane comment scoped to it in one step, so no separate restore is needed. The seam is
-- dormant (opt-in, default-off), so these tables are empty on any node that has not armed federation.
DROP TABLE IF EXISTS groundnet_ingest;
DROP TABLE IF EXISTS groundnet_emit;
