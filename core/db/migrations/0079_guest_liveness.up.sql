-- 0079: guest_liveness — the queryable guest power-state projection (TG-378 prereq).
--
-- During the 2026-08-06 pve03 cascade TG sealed `start-guest` manifests for guests that were RUNNING the
-- whole time (uptimes 897h and 2,103h) — its estate graph agreed on PLACEMENT (runs_on, 0.95) but the graph
-- is placement-only BY CONSTRUCTION: no power state exists anywhere queryable. The state was in the very
-- /cluster/resources response the pve estate source already fetches, and was discarded at parse.
--
-- This table is that state, projected: one row per guest, upserted by the estate refresh sweep from the
-- same fetch (no extra HTTP, no extra credential). MUTABLE single-writer projection like runtime_posture,
-- NOT an append-only spine table — latest-wins per guest, observed_at stamped server-side so the reader
-- measures staleness against the DB clock. tg_runtime therefore keeps UPDATE/DELETE here (no 0015-style
-- REVOKE): the upsert's ON CONFLICT DO UPDATE is the whole write path, and freezing rows would freeze the
-- projection at its first sweep (the 0077 co_occurrence reasoning).
--
-- Guests that VANISH from the sweep (the pve03 shape: a dead node's guests drop out of /cluster/resources
-- within minutes) are NOT deleted — their rows age past the reader's freshness bound and read as UNKNOWN.
-- Absent is never "stopped": the precondition reader (TG-378 slice 2) refuses on unknown.
CREATE TABLE guest_liveness (
  guest       text PRIMARY KEY CHECK (length(btrim(guest)) > 0),
  node        text NOT NULL DEFAULT '',
  status      text NOT NULL,
  observed_at timestamptz NOT NULL DEFAULT now()
);

-- plane: both — an OBSERVATION projection (competence-plane cache, the 0077 co_occurrence class), not an
-- actuation record: the TRIAGE plane's estate sweep writes it, and the prediction gate (and any later
-- defense-in-depth reader on the actuation plane) reads it. It authorises nothing by itself — the TG-378
-- precondition reader treats absent/stale/other-status as UNKNOWN and refuses.
COMMENT ON TABLE guest_liveness IS 'plane: both';
