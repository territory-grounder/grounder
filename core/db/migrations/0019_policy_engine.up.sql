-- 0019 policy engine — the operator-managed Policy Engine's DURABLE state (spec/015 T-015-12, REQ-1518,
-- INV-19). Four single-org tables:
--
--  1. policy_decision   — APPEND-ONLY audit: one immutable, NON-SECRET row per Engine.Decide evaluation
--                         (the persistence half of REQ-1518). Like the accountability spine (0015) and the
--                         credential_resolution audit (0018), the runtime DML role is STRIPPED of
--                         UPDATE/DELETE so the app cannot rewrite its own decision trail (tamper-resistant,
--                         readiness-review G4). It carries ONLY non-secret identifiers — the matched rule id,
--                         the resolved verdict, the composition band_mode + composed risk band, the resolved
--                         min_confidence, a non-secret action_id / plan_hash binding (INV-07), the acting
--                         principal, and the active mode. NO argv, host, credential, or secret can land here.
--
--  2. policy_mode       — the SINGLE active global autonomy mode (durable, latest-wins). Exactly one row
--                         (singleton PK). Mode TRANSITIONS are audited as immutable governance_ledger records
--                         by the ModeController (REQ-1502) — this table stores only the current mode, so a
--                         restart resolves the persisted mode (absent/corrupt → fail-closed Shadow, REQ-1519).
--
--  3. policy_graduation — per-op-class earned-autonomy ladder state (durable, latest-wins): the level
--                         (approve/auto) and the consecutive verified-clean run count (REQ-1514). An absent
--                         class resolves fail-closed to approve; graduation promote/demote EVENTS are audited
--                         to the governance ledger by a later leaf.
--
--  4. policy_ruleset    — the operator's ACTIVE rules-as-data policy (durable, latest-wins, one active
--                         document). Rules are DATA, never Rego (REQ-1503) — the document is the validated
--                         operator policy JSON (ParseRuleSet's wire form), NON-SECRET by construction.
--
-- Like runtime_posture (0016) / credential_coverage (0017), the three latest-wins tables are OPERATIONAL
-- state, not governed audit rows, so they carry no schema_version stamp; policy_decision carries the stamp
-- like the credential_resolution audit (0018). The governance_ledger remains the authoritative decision
-- record; nothing here can enable a capability. Mutation stays OFF — the engine is consulted read-only.

CREATE TABLE policy_decision (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  rule_id        text NOT NULL DEFAULT '',                            -- matched rule id; '' = fail-closed default (no rule matched)
  verdict        text NOT NULL CHECK (verdict IN ('auto', 'approve', 'deny')),
  band_mode      text NOT NULL DEFAULT '' CHECK (band_mode IN ('', 'respect', 'force')),  -- '' = no composition (deny short-circuit)
  composed_band  text NOT NULL CHECK (composed_band IN ('POLL_PAUSE', 'AUTO_NOTICE', 'AUTO')),
  min_confidence double precision NOT NULL DEFAULT 0 CHECK (min_confidence >= 0 AND min_confidence <= 1),
  action_id      text NOT NULL DEFAULT '',                            -- non-secret bound action id (INV-07); '' until the interceptor threads a bound action (T-015-13)
  plan_hash      text NOT NULL DEFAULT '',                            -- non-secret plan hash (INV-07); '' until threaded
  principal      text NOT NULL DEFAULT '',                            -- acting principal (non-secret id); '' until threaded
  mode           text NOT NULL CHECK (mode IN ('Shadow', 'HITL', 'Semi-auto', 'Full-auto')),
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX policy_decision_created_idx ON policy_decision (created_at DESC);
-- Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE (readiness-review G4, INV-19),
-- exactly as the accountability spine (0015) and credential_resolution (0018). INSERT+SELECT remain, so the
-- runtime can append and read decisions but never rewrite one. tg_migration (the owner) is unaffected.
REVOKE UPDATE, DELETE ON policy_decision FROM tg_runtime;

CREATE TABLE policy_mode (
  singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),     -- exactly one active-mode row
  mode       text NOT NULL CHECK (mode IN ('Shadow', 'HITL', 'Semi-auto', 'Full-auto')),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE policy_graduation (
  op_class        text PRIMARY KEY CHECK (length(btrim(op_class)) > 0),
  level           text NOT NULL CHECK (level IN ('approve', 'auto')),
  clean_run_count int NOT NULL DEFAULT 0 CHECK (clean_run_count >= 0),
  last_outcome    text NOT NULL DEFAULT 'unverified' CHECK (last_outcome IN ('unverified', 'verified_clean', 'deviated')),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE policy_ruleset (
  singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),      -- one active ruleset
  document   jsonb NOT NULL,                                          -- the validated operator policy JSON (rules-as-data, non-secret)
  rule_count int NOT NULL DEFAULT 0 CHECK (rule_count >= 0),
  updated_by text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);
