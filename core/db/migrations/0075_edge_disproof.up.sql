-- 0075: the durable per-edge DISPROOF record (TG-206a, spec/018, design-wisdom #11).
--
-- The self-learning estate tier's confidence ratchet only ratchets UP (Upsert MAX-merges). decay-on-disproof
-- (core/estate/decay.go) is the down side: a fresh misprediction (verify's surprise-hosts + rule-mismatches)
-- decays the LEARNED edges on the mispredicted path and ages out any that hit the floor. Until now the
-- DecayReport that named those edges was logged and thrown away — the contradiction lowered a number and
-- vanished. This table ATTACHES THE CONTRADICTION TO THE EDGE: one immutable row per decayed learned edge,
-- naming the misprediction that disproved it (deviation_key + action_id), the confidence it was decayed TO,
-- and whether that aged it out — so a later verdict can vindicate or refute the losing reading and the
-- learned-tier lifecycle (TG-388) has a durable disproof history to consult instead of a lost log line.
--
-- NON-SECRET by construction: host / relation slugs, a reproduction-signature hash, an action-id hash, and a
-- confidence — never argv, credential, or token (INV-13). APPEND-ONLY (REQ-2016): the runtime role holds no
-- UPDATE and no DELETE, so a persisted disproof is tamper-resistant like the rest of the accountability spine.
CREATE TABLE edge_disproof (
  id             bigserial        PRIMARY KEY,
  edge_key       text             NOT NULL CHECK (length(btrim(edge_key)) > 0),  -- the (from|rel|to) learned-edge key
  edge_from      text             NOT NULL DEFAULT '',                           -- the edge source entity (non-secret)
  edge_rel       text             NOT NULL DEFAULT '',                           -- the relation type
  edge_to        text             NOT NULL DEFAULT '',                           -- the edge destination entity (non-secret)
  target_host    text             NOT NULL DEFAULT '',                           -- the target the disproving prediction was made FROM
  deviation_key  text             NOT NULL DEFAULT '',                           -- the misprediction reproduction signature (blank for a flat-host disproof)
  action_id      text             NOT NULL DEFAULT '',                           -- the committed action id of the disproving prediction
  decayed_to     double precision NOT NULL,                                      -- the edge confidence AFTER this pass's decay
  aged_out       boolean          NOT NULL DEFAULT false,                        -- decayed confidence reached the floor and the edge was expired
  observed_at    timestamptz      NOT NULL DEFAULT now(),                        -- the decay pass's observation time
  created_at     timestamptz      NOT NULL DEFAULT now(),
  schema_version integer          NOT NULL DEFAULT 1
);

-- per-edge disproof history (did this edge get disproved before / how far has it decayed) + by-signature lookup.
CREATE INDEX edge_disproof_edge_idx   ON edge_disproof (edge_key, observed_at DESC);
CREATE INDEX edge_disproof_devkey_idx ON edge_disproof (deviation_key);

-- Append-only tamper-resistance (REQ-2016), same pattern as the accountability spine (0015) and ingest_alert
-- (0033) — NOT discovery_deviation (0072), which is deliberately mutable (its reproductions counter is
-- UPDATEd). tg_runtime keeps INSERT + SELECT via the blanket default-privilege grant.
REVOKE UPDATE, DELETE ON edge_disproof FROM tg_runtime;

-- Credential-plane classification (migration 0060): a competence-plane measurement/grounding record — neither
-- an actuation record nor a mutation authority — so, like discovery_deviation (0072), it declares 'both'.
COMMENT ON TABLE edge_disproof IS 'plane: both';
