-- INGEST FRESHNESS MUST COUNT THE TRAFFIC THAT IS ACTUALLY ARRIVING (TG-393).
--
-- `tg_ingest_source_last_seen_seconds` is computed in core/db/ingest_freshness.go from `ingest_alert`
-- alone. RAISES live there; RECOVERIES live in `ingest_transition`, and there is no union. So a source
-- whose current traffic is entirely recovery notifications reads as SILENT — and that is not an edge
-- case, it is the recovery phase of every incident, i.e. exactly when the estate is getting better.
--
-- MEASURED ON PRODUCTION 2026-08-07:
--
--     source                    last raise (ingest_alert)   last transition (ingest_transition)
--     librenms-dc2           2026-08-06 04:26:06         2026-08-06 13:56:03    (+9.5 h)
--     librenms-dc1           2026-08-06 22:58:04         2026-08-06 23:04:04    (+0.1 h)
--
-- librenms-dc2 delivered recovery traffic NINE AND A HALF HOURS after its last raise, while
-- AlertSourceWentSilent fired against it. A false-positive on a healthy feed trains an operator to
-- ignore the alert, and it buries the one genuine silence there is — pve-liveness, quiet since
-- 2026-07-31 (TG-350).
--
-- WHY A COLUMN AND NOT A STRING MATCH. `ingest_transition` carries no source, and the obvious repairs
-- both fail on measurement:
--
--   * JOIN back to ingest_alert on external_ref — 0 of 2554 transitions join. A recovery's ref does not
--     correspond to a stored raise (the raise may never have been recorded at all: intake is
--     ON CONFLICT DO NOTHING, TG-399).
--   * Derive the source from the external_ref PREFIX — works for librenms (2789/2789 refs start with
--     their source_id) and NOT for prometheus-alertmanager (0 of 168). A fix that silently covers two
--     sources of three is the wrong-denominator defect again, one layer down.
--
-- The envelope already carries SourceID — core/httpapi.RecordFromEnvelope reads `env.SourceID` for the
-- alert log — and the transition projector simply dropped it. So the source is authoritative and free;
-- it was never persisted.
--
-- NULLABLE, AND HISTORICAL ROWS STAY NULL. No backfill is attempted: the 2554 existing transitions
-- cannot be attributed without the string matching rejected above. A NULL contributes nothing to
-- freshness, which is the SAFE direction — the union can only make a source look FRESHER than the
-- raises-only reading, never staler, so an unattributed row degrades to today's behaviour rather than
-- inventing silence.

ALTER TABLE ingest_transition ADD COLUMN IF NOT EXISTS source_id text;

-- Freshness groups by source_id over a recency window, so the index matches the read shape.
CREATE INDEX IF NOT EXISTS ingest_transition_source_received_idx
  ON ingest_transition (source_id, received_at DESC);

COMMENT ON COLUMN ingest_transition.source_id IS
  'TG-393: the ingest source that delivered this recovery, so per-source freshness can union raises and clears. NULL on rows written before migration 0068 — deliberately not backfilled, and a NULL only ever makes freshness read staler (the safe direction), never fresher.';
