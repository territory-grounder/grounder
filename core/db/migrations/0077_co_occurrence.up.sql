-- 0077: durable snapshot of the co-occurrence learner (core/learn, TG-388 face c).
--
-- Before this, the learner lived only in worker memory (learn.NewCoOccurrenceLearner over maps, no load/save),
-- so a redeploy wiped the entire self-learning tier — measured 1,524 learned edges -> 0 on a routine
-- `docker compose up`, its lifetime exactly the worker's uptime. This persists the learner's RAW decay-state
-- floats (the CoOccurrences() view is lossy: it rounds count and collapses delay into a mean), loaded on boot
-- and re-saved periodically, so the tier survives a restart instead of re-learning from zero.
--
-- MUTABLE competence-plane cache, DELIBERATELY NOT an append-only spine table: the learner rewrites it on every
-- save — the tier DECAYS pairs out (TG-388 faces a/b), so a stale row must be DELETED, not kept — and Save is a
-- DELETE-all + COPY-in inside one transaction. tg_runtime therefore keeps UPDATE/DELETE here (no 0015-style
-- REVOKE). It feeds prediction ENRICHMENT only (learned edges are capped at 0.75, below every live source and
-- the suppression cutoff) and never an actuation; losing it costs only re-learning.
CREATE TABLE co_occurrence (
  primary_host   text             NOT NULL CHECK (length(btrim(primary_host)) > 0),
  dependent_host text             NOT NULL CHECK (length(btrim(dependent_host)) > 0),
  -- RAW float weights, not the rounded CoOccurrences view: count is the decayed co-occurrence evidence,
  -- delay_sum its lockstep propagation-gap sum (TG-188). A pair below one whole observation is pruned by the
  -- learner and never reaches here, so count > 0.
  count          double precision NOT NULL CHECK (count > 0),
  delay_sum      double precision NOT NULL DEFAULT 0 CHECK (delay_sum >= 0),
  updated_at     timestamptz      NOT NULL DEFAULT now(),
  PRIMARY KEY (primary_host, dependent_host),
  CONSTRAINT co_occurrence_not_self CHECK (primary_host <> dependent_host)
);
COMMENT ON TABLE co_occurrence IS 'plane: both';

-- The per-host trial denominator (the base-rate denominator LaplaceConfidence uses for every pair the host
-- roots). A separate grain from the pair table; same competence-plane classification.
CREATE TABLE co_occurrence_host (
  host       text             NOT NULL PRIMARY KEY CHECK (length(btrim(host)) > 0),
  trials     double precision NOT NULL CHECK (trials > 0),
  updated_at timestamptz      NOT NULL DEFAULT now()
);
COMMENT ON TABLE co_occurrence_host IS 'plane: both';
