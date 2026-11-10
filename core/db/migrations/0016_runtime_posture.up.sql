-- 0016 runtime_posture — the WORKER's live mutation posture, PUBLISHED so the grounder (a SEPARATE
-- process) reports the TRUE, live posture on /v1/whoami + /v1/governance instead of its OWN mutation gate,
-- which is read-only BY CONSTRUCTION (the grounder refuses to boot with mutation on, cmd/grounder preflight).
-- The worker owns the real mutation gate — it alone can flip ON (spec/012 REQ-1107) — and upserts its actual
-- gate.Enabled() + effect-leaf Capability() here on a heartbeat so updated_at stays fresh; the grounder reads
-- the latest row across the process boundary (the same worker->DB->grounder pattern estate_snapshot uses,
-- REQ-516). This is OPERATIONAL projection state, NOT a governed audit row: the governance_ledger remains the
-- authoritative decision record, and a row here can enable nothing (the mutation switch is proof-gated inside
-- the worker's safety core). One row per component (single writer per component) — hence no schema_version
-- (cf. operator_sessions / pending_decision). A stale or absent row the READER treats as UNKNOWN — never a
-- confident false — so a heartbeat gap can never make the console under-report a live-ON worker.
CREATE TABLE runtime_posture (
  component         text PRIMARY KEY CHECK (length(btrim(component)) > 0),
  mutation_enabled  boolean NOT NULL,
  effect_capability text NOT NULL DEFAULT '',
  updated_at        timestamptz NOT NULL DEFAULT now()
);
