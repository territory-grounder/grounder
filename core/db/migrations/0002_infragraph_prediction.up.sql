-- 0002 infragraph prediction — the append-only machine-consequence prediction spine + cascade stats.
-- Provenance: [O] INV-07 (one content-hashed action-bound prediction), INV-19 (append-only audit spine),
-- INV-22 (falsifiable — the degree-preserving shuffled control persists on EVERY prediction row), spec/002
-- REQ-101/REQ-105 · [F] the predecessor infragraph_predictions + infragraph_cascade_stats. DATA-MODEL §4.4.
--
-- Role model (baseline): the MIGRATION role owns this DDL; the RUNTIME role has DML only and is not an
-- owner, so a CREATE TABLE at request time fails at the privilege level. The prediction IDENTITY (action_id,
-- plan_hash, predicted/control host sets, hashes) is written ONCE at commit BEFORE any approval poll and is
-- never changed; the score columns (tp/fp/fn/control_tp/control_fp) are the sole verify-time write, filled
-- once when the deterministic verifier scores the observed cascade. schema_version CHECK > 0 means an
-- unstamped (version 0) row can never pass the reader guard (core/schema).

CREATE TYPE prediction_kind AS ENUM ('action', 'cascade');

CREATE TABLE infragraph_prediction (
  plan_hash       text NOT NULL,                         -- the plan_hash key (spec/002)
  kind            prediction_kind NOT NULL DEFAULT 'action', -- action prediction vs shadow cascade prediction
  action_id       text NOT NULL,                         -- the content-hashed bound action (INV-07)
  target_host     text NOT NULL,
  site            text NOT NULL DEFAULT '',
  predicted_hosts jsonb NOT NULL DEFAULT '[]'::jsonb,     -- the real predicted cascade host set
  predicted_rules jsonb NOT NULL DEFAULT '[]'::jsonb,     -- the predicted (host,rule) pairs
  control_hosts   jsonb NOT NULL DEFAULT '[]'::jsonb,     -- the degree-preserving negative control (INV-22)
  prediction_hash text NOT NULL,                          -- content hash bound into the ActionManifest
  -- verify-time scores (nullable until the verifier writes them exactly once):
  tp              int,
  fp              int,
  fn              int,
  control_tp      int,
  control_fp      int,
  schema_version  int NOT NULL CHECK (schema_version > 0),
  committed_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (plan_hash, kind)                           -- append-only, first-wins per (plan_hash, kind)
);

-- Join to the bound action for audit; the prediction is committed BEFORE the manifest is sealed, so this is
-- an index, not a foreign key (a hard FK would invert the commit-before-seal ordering the gate requires).
CREATE INDEX infragraph_prediction_action_id ON infragraph_prediction (action_id);

-- Cascade over-prediction gating (INV-22): an append-only aggregate of the negative-control ratio across a
-- window of predictions. control_ratio = control_tp / real_tp; a window whose control captures at least half
-- as many true cascades as the real predictions (ratio > 0.5) has no falsifiable predictive value. Not
-- schema-stamped (no Go reader guards it yet); populated by the eval/governance lane.
CREATE TABLE infragraph_cascade_stats (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  window_start  timestamptz NOT NULL,
  window_end    timestamptz NOT NULL,
  real_tp       int NOT NULL DEFAULT 0,
  control_tp    int NOT NULL DEFAULT 0,
  control_ratio double precision NOT NULL DEFAULT 0,      -- control_tp / real_tp; <= 0.5 is falsifiable
  falsifiable   boolean NOT NULL DEFAULT false,
  computed_at   timestamptz NOT NULL DEFAULT now()
);
