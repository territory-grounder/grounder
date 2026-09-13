-- 0074: append-only occurrence log for the ingest front door (TG-399, spec/006 REQ-510).
--
-- ingest_alert (0033) keeps exactly ONE canonical row per external_ref: Append does ON CONFLICT DO NOTHING and
-- the table is append-only (UPDATE/DELETE revoked, REQ-2016). That is correct for a canonical "what did the
-- front door first admit" record, but it made re-fire traffic UNRECORDABLE BY CONSTRUCTION: an Alertmanager
-- external_ref is stable per (rule, host) (`am-<rule>-<host>`), so an alert re-firing for weeks reached
-- AlertLogStore.Append on every delivery (verified: the front door Appends every accepted, non-recovery
-- envelope) yet added zero rows — so alert volume, collapse ratio and per-window session counts systematically
-- undercounted, and "the recovery window looked empty" was this defect, not quiet infrastructure (TG-399).
--
-- The append-only "DO UPDATE SET occurrence_count" the ticket first proposed cannot work: tg_runtime holds no
-- UPDATE on ingest_alert. This companion table takes the OTHER shape — one append-only row per accepted
-- delivery (first + every re-fire) — so occurrence count, first-seen and last-seen are all derivable by query
-- WITHOUT ever updating the canonical (UPDATE-revoked) row. AlertLogStore.Append writes one row here per
-- accepted envelope, unconditionally (no ON CONFLICT): each delivery IS a distinct occurrence. The write is
-- best-effort (the ingest path must never block on the log) and a delivery carries no dedup key, so the count
-- is a FLOOR on re-fire volume under a transient DB error, not an exact tally — never-block-ingest over
-- exactness; making the write durable-without-blocking is a tracked follow-up.
--
-- NON-SECRET columns only (the same projection discipline as ingest_alert): the correlation key + the
-- normalized identifiers needed to count volume per host/rule/severity over a window. Never the raw payload,
-- never a credential (INV-13).
--
-- APPEND-ONLY evidence (REQ-2016): like ingest_alert (0033), agent_step (0031), interceptor_gate_verdict
-- (0030) — the runtime role may INSERT + SELECT but holds NO UPDATE/DELETE.
CREATE TABLE ingest_alert_occurrence (
  id             bigserial PRIMARY KEY,
  external_ref   text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- the session correlation key (non-secret)
  alert_rule     text NOT NULL DEFAULT '',                               -- the alert rule that fired
  severity       text NOT NULL DEFAULT '',                               -- normalized severity label at this delivery
  host           text NOT NULL DEFAULT '',                               -- the affected host (non-secret identifier)
  site           text NOT NULL DEFAULT '',                               -- the site/deployment
  observed_at    timestamptz,                                            -- provider event time for this delivery (nullable)
  received_at    timestamptz NOT NULL DEFAULT now(),                     -- front-door arrival time of this delivery
  schema_version integer NOT NULL DEFAULT 1
);

-- occurrence count + first/last-seen per correlation key, and windowed volume by arrival time.
CREATE INDEX ingest_alert_occurrence_ref_idx ON ingest_alert_occurrence (external_ref, received_at DESC);
CREATE INDEX ingest_alert_occurrence_received_idx ON ingest_alert_occurrence (received_at DESC);

-- Evidence immutability: revoke UPDATE + DELETE for the DML runtime role. tg_runtime keeps INSERT + SELECT
-- (granted by the blanket ALTER DEFAULT PRIVILEGES in deploy/postgres-init/00-roles.sh), so each recorded
-- delivery can be appended and read but never rewritten (REQ-2016).
REVOKE UPDATE, DELETE ON ingest_alert_occurrence FROM tg_runtime;

-- Credential-plane classification (migration 0060): a triage-tier front-door record, same plane as ingest_alert.
COMMENT ON TABLE ingest_alert_occurrence IS 'plane: triage';
