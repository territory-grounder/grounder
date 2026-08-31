-- 0085: DURABLE CLUSTER IDENTITY for a correlated cascade (TG-385 / TG-376).
--
-- THE DEFECT THIS CLOSES. On the 2026-08-06 pve03 cascade TG fanned a storm to 1.000 alerts/session — 157
-- alerts became 157 workflows became 157 investigation sessions, with ZERO collapse, even though the
-- correlation stage (TG-169) correctly tagged 159 decisions as correlated (peak 111 members / 54 hosts).
-- The reason was that the "cluster" had no DURABLE identity: every subject recomputed its OWN symmetric
-- +/-span window at its OWN arrival instant against ingest_alert (core/db/correlation.go), so the member
-- set was a function of WHEN a subject arrived, not of what broke — it rose then decayed with arrival time,
-- and 63 of 172 rows did not even contain their own subject's ref. Nothing tied one member of a storm to
-- another, so each opened its own investigation.
--
-- WHAT THIS TABLE IS. One row per detected storm, keyed by (window_bucket, first_seen_ref): the storm's
-- first-seen alert (the earliest ingest_alert in the correlation window) quantized to a coarse time bucket.
-- Every member's correlation stage JOINs this row (INSERT ... ON CONFLICT ... RETURNING) instead of
-- recomputing a per-arrival window, so all members of one storm resolve to the SAME cluster id regardless
-- of arrival order. The elected causal subject investigates once; the rest attach as evidence and open no
-- session (the collapse, TG-376). See core/correlate.ClusterAnchor / ClusterBucket for the key derivation
-- and core/db.AlertClusterStore.Join for the upsert.
--
-- OPERATIONAL IDENTITY, NOT APPEND-ONLY EVIDENCE. Unlike exec_class_decision (0058), ingest_alert (0033)
-- or the accountability spine (0015), this is a JOIN key the correlation stage upserts, not a tamper-
-- resistant record of a decision — so it keeps the runtime role's default INSERT/SELECT/UPDATE (the ON
-- CONFLICT DO UPDATE that makes the upsert concurrency-safe needs UPDATE). The DURABLE EVIDENCE of which
-- cluster a session belonged to, who was elected, and why lives on exec_class_decision below, which stays
-- append-only. DELETE is revoked so a cluster identity cannot be erased by the runtime role — retention is
-- an operator/migration concern, never a request-path one.
--
-- NON-SECRET columns only: an external_ref and a time bucket. Never a payload, never a credential (INV-13).
CREATE TABLE alert_cluster (
  id             bigserial PRIMARY KEY,
  window_bucket  bigint      NOT NULL,                                          -- coarse time bucket of first_seen_at (core/correlate.ClusterBucket)
  first_seen_ref text        NOT NULL CHECK (length(btrim(first_seen_ref)) > 0), -- the storm's earliest admitted alert ref (the anchor)
  first_seen_at  timestamptz NOT NULL,                                          -- that earliest alert's front-door arrival
  span_seconds   integer     NOT NULL DEFAULT 0,                                -- the correlation span the cluster was formed over
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- THE DURABLE CLUSTER KEY. Two members of one storm compute the same anchor (they both see the storm's
-- first-seen alert inside their window), so they collide here and share one id; two independent storms
-- carry different first_seen_refs and stay separate even inside one time bucket. The bucket coarsens the
-- anchor time so the key indexes by locality and a future straddle-tolerant join has a cheap range to probe.
CREATE UNIQUE INDEX alert_cluster_key_uidx ON alert_cluster (window_bucket, first_seen_ref);
-- The review read is "what storms formed over this period".
CREATE INDEX alert_cluster_created_idx ON alert_cluster (created_at DESC);

-- A cluster identity is operational, not evidence: the runtime role may INSERT/SELECT/UPDATE (the upsert),
-- but a request-path DELETE of a storm's identity is never legitimate — revoke it (retention is a migration
-- concern). tg_runtime's INSERT/SELECT/UPDATE come from the blanket ALTER DEFAULT PRIVILEGES in
-- deploy/postgres-init/00-roles.sh.
REVOKE DELETE ON alert_cluster FROM tg_runtime;

-- CREDENTIAL PLANE (migration 0060 discipline): alert_cluster is written by the SAME correlation stage that
-- writes exec_class_decision, in the same worker context, so it declares the same plane. It records no
-- actuation and authorises none — it is a triage-time join key — but it is granted to both planes to match
-- its peer decision table rather than split one stage's two writes across two grant sets.
COMMENT ON TABLE alert_cluster IS 'plane: both';

-- The routing decision (0058) now records WHICH cluster it belonged to, WHO was elected the causal subject,
-- the RUNNER-UP, and the RULE that decided — the audit trail for the collapse. Additive, defaulted,
-- backward-compatible: every existing row stays valid and every pre-field writer keeps working. These ride
-- the same append-only discipline as the rest of exec_class_decision (INSERT + SELECT only; the 0058 REVOKE
-- already holds for the whole table, new columns included).
ALTER TABLE exec_class_decision ADD COLUMN IF NOT EXISTS cluster_id    bigint NOT NULL DEFAULT 0;  -- the alert_cluster this session joined (0 = uncorrelated / no durable identity)
ALTER TABLE exec_class_decision ADD COLUMN IF NOT EXISTS elected_ref   text   NOT NULL DEFAULT ''; -- the external_ref elected to INVESTIGATE the cluster (causal subject)
ALTER TABLE exec_class_decision ADD COLUMN IF NOT EXISTS runner_up_ref text   NOT NULL DEFAULT ''; -- the runner-up the election ranked second (recorded for review)
ALTER TABLE exec_class_decision ADD COLUMN IF NOT EXISTS elect_rule    text   NOT NULL DEFAULT ''; -- which tie-break decided (core/correlate.ElectRule*)

COMMENT ON COLUMN exec_class_decision.cluster_id IS
  'The alert_cluster (0085) this correlated session joined — the DURABLE identity that ties every member of one storm together, replacing the per-arrival window that made the cluster a function of when a subject arrived (TG-385). 0 = an uncorrelated incident or a deployment with no durable cluster store.';
COMMENT ON COLUMN exec_class_decision.elected_ref IS
  'The external_ref elected as the cluster''s CAUSAL SUBJECT — the one member that opens an investigation session (TG-376); the other members attach as evidence and open none. Empty for an uncorrelated incident.';
COMMENT ON COLUMN exec_class_decision.elect_rule IS
  'Which tie-break in the causal election decided the subject (core/correlate.ElectRule*: estate-indegree / cluster-parent-fanout / earliest-ref / ref-order / sole-member) — recorded with the runner-up so a wrong election is reviewable, not silent.';
