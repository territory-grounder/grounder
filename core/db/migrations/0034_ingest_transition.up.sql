-- 0034: durable recovery-transition log for the ingest boundary (spec/012 clear-confirm, spec/006 REQ-510).
--
-- LibreNMS pushes a state-0 RECOVERY transition when an alert row goes back UP, carrying the SAME
-- external_ref as the fault that opened it (LibreNMS reuses the alert id across its lifecycle). Until now the
-- front door DISCARDED it twice — StartTriage deduped it into the finished workflow and ingest_alert's
-- ON CONFLICT (external_ref) DO NOTHING dropped it — so TG's confirmed-clear check could only fall back to a
-- LAGGING LibreNMS re-pull that misses recoveries past the 30-min bound (the observed writeback miss on a
-- slow-recovering guest). This table is the durable capture of that provider-asserted recovery: the front
-- door records every recovery transition here and signals the waiting session, so a confirmed-clear can be
-- driven by TG's OWN retained evidence of LibreNMS's own recovery assertion (never the model's word, INV-11)
-- rather than a re-pull that races the poller.
--
-- Unlike ingest_alert there is NO unique external_ref: a fault can recover, re-fire, and recover again, so
-- MANY transitions share one ref — each is an independent, timestamped observation. NON-SECRET columns only
-- (the normalized identifiers), never the raw payload, never a credential (INV-13).
--
-- APPEND-ONLY evidence (REQ-2016): like ingest_alert (0033), agent_step (0031), interceptor_gate_verdict
-- (0030), and the accountability spine (0015), the runtime role may INSERT + SELECT but holds NO
-- UPDATE/DELETE — a recorded recovery observation is tamper-resistant.
CREATE TABLE ingest_transition (
  id             bigserial PRIMARY KEY,
  external_ref   text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- the session correlation key (non-secret)
  kind           text NOT NULL DEFAULT 'recovery',                       -- transition kind ('recovery' today)
  host           text NOT NULL DEFAULT '',                               -- the affected host (non-secret identifier)
  site           text NOT NULL DEFAULT '',                               -- the site/deployment
  alert_rule     text NOT NULL DEFAULT '',                               -- the alert rule that recovered
  observed_at    timestamptz,                                            -- provider event time (nullable)
  received_at    timestamptz NOT NULL DEFAULT now(),                     -- front-door arrival time
  schema_version integer NOT NULL DEFAULT 1
);

-- RecoveredSince(host, since) reads by (host, received_at); external_ref supports the per-session join.
CREATE INDEX ingest_transition_host_idx ON ingest_transition (host, received_at DESC);
CREATE INDEX ingest_transition_ref_idx ON ingest_transition (external_ref);

-- Evidence immutability: revoke UPDATE + DELETE for the DML runtime role. tg_runtime keeps INSERT + SELECT
-- (granted by the blanket ALTER DEFAULT PRIVILEGES in deploy/postgres-init/00-roles.sh), so a recorded
-- recovery observation can never be silently rewritten or removed.
REVOKE UPDATE, DELETE ON ingest_transition FROM tg_runtime;
