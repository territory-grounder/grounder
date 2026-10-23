-- 0033: durable front-door alert log for the ingest boundary (spec/006 REQ-510, decision-tracer spec/020).
--
-- The alert front door (core/httpapi ingestHandler) records every ACCEPTED, normalized envelope — the ingest
-- tier's own record of what it admitted at acceptance (recordFromEnvelope, INV-15: a rejected payload never
-- became an envelope and is never logged). Until now that record lived ONLY in the in-memory MemAlertLog: it
-- vanished on restart AND was invisible to the decision-tracer (which reads Postgres), so the tracer's ingest
-- stage could never show real front-door data. This table is the durable twin: the /v1/alerts view survives
-- restart, and the tracer reads the ingest boundary for any session by external_ref (the correlation key the
-- whole pipeline joins on).
--
-- NON-SECRET columns only: the normalized envelope's identifiers (source/rule/severity/host/site) + its
-- operational summary + non-secret labels — the SAME projection already served by /v1/alerts. Never the raw
-- payload, never a credential (INV-13).
--
-- APPEND-ONLY evidence (REQ-2016): like agent_step (0031), interceptor_gate_verdict (0030), and the
-- accountability spine (0015), the runtime role may INSERT + SELECT but holds NO UPDATE/DELETE — an admitted
-- alert record is tamper-resistant.
CREATE TABLE ingest_alert (
  id             bigserial PRIMARY KEY,
  external_ref   text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- the session correlation key (non-secret)
  source_type    text NOT NULL DEFAULT '',                               -- the ingest source slug (e.g. 'librenms')
  source_id      text NOT NULL DEFAULT '',                               -- the source's own alert id (non-secret)
  alert_rule     text NOT NULL DEFAULT '',                               -- the alert rule that fired
  severity       text NOT NULL DEFAULT '',                               -- normalized severity label
  host           text NOT NULL DEFAULT '',                               -- the affected host (non-secret identifier)
  site           text NOT NULL DEFAULT '',                               -- the site/deployment
  summary        text NOT NULL DEFAULT '',                               -- the operational alert summary (non-secret)
  labels_json    jsonb NOT NULL DEFAULT '{}',                            -- non-secret normalized labels
  observed_at    timestamptz,                                            -- provider event time (nullable)
  received_at    timestamptz NOT NULL DEFAULT now(),                     -- front-door arrival time
  workflow_id    text NOT NULL DEFAULT '',                               -- the triage session minted for it, if any
  schema_version integer NOT NULL DEFAULT 1
);

-- ONE canonical front-door record per session (external_ref). A source that re-delivers a webhook for an
-- already-admitted alert is idempotent (Append does ON CONFLICT (external_ref) DO NOTHING), so a flapping or
-- retrying transport never accumulates duplicate, unremovable rows on this append-only (DELETE-revoked) table.
CREATE UNIQUE INDEX ingest_alert_ref_uidx ON ingest_alert (external_ref);
CREATE INDEX ingest_alert_received_idx ON ingest_alert (received_at DESC);

-- Evidence immutability: revoke UPDATE + DELETE for the DML runtime role. tg_runtime keeps INSERT + SELECT
-- (granted by the blanket ALTER DEFAULT PRIVILEGES in deploy/postgres-init/00-roles.sh), so the front-door
-- alert record can be appended and read but never rewritten (REQ-2016).
REVOKE UPDATE, DELETE ON ingest_alert FROM tg_runtime;
