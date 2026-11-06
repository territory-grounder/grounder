-- 0020 actuation regime engine — the APPEND-ONLY persistence of the Actuation Regime Engine (spec/017
-- T-017-6, REQ-1715, INV-19/INV-13). Three immutable tables, one per required engine output:
--
--  1. regime_resolution — one immutable, NON-SECRET row per lane selection: the target, the resolved regime,
--                         the selected lane, the matched rule id ('' ⇒ operator default lane), and the
--                         resolved/refused outcome. A refusal (unknown/ambiguous regime, no wired lane)
--                         carries no regime/lane (fail closed) — the integrity CHECK ties outcome to shape.
--
--  2. regime_actuation  — one immutable, NON-SECRET row per job launch: the action_id (INV-07 manifest
--                         lifecycle binding), the launched lane, the AWX job-template id, the authorized
--                         op-class, the launched AWX job id, and the AWX token as a SecretRef REFERENCE only
--                         (token_ref, e.g. 'store:awx.token') — NEVER the token value or a secret extra_var
--                         (INV-13). The token_ref CHECK enforces a "scheme:" reference shape at the database
--                         boundary, so a raw plaintext token cannot land here.
--
--  3. deferred_verdict  — one immutable, NON-SECRET row per completed deferred verify: the action_id, the AWX
--                         job id, the terminal AWX job status, the mechanical verdict against the launch
--                         prediction, and the graduation outcome fed to the ladder.
--
-- Like the accountability spine (0015), credential_resolution (0018), and policy_decision (0019), the runtime
-- DML role tg_runtime is STRIPPED of UPDATE/DELETE on all three tables, so the app can append and read its own
-- regime audit trail but never rewrite one (tamper-resistant, readiness-review G4, INV-19). tg_migration (the
-- table owner) is unaffected. Each table carries the schema_version reader-guard (CHECK schema_version > 0).
--
-- Single-organization (ADR-0010, paradigm-rule 1): NO tenant_id; the DML-only runtime role is the privilege
-- boundary. NON-SECRET by construction: no argv/host/credential/password/private_key column — the only
-- credential material is a SecretRef reference. This migration is PERSISTENCE only; it actuates nothing and
-- gates nothing (mutation stays OFF).

CREATE TABLE regime_resolution (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  target         text NOT NULL CHECK (length(btrim(target)) > 0),   -- non-secret target label (host/device-class/group)
  regime         text NOT NULL DEFAULT '' CHECK (regime IN ('', 'native-ssh', 'awx-job', 'gitops-mr', 'k8s-declarative', 'api')),
  lane           text NOT NULL DEFAULT '' CHECK (lane   IN ('', 'native-ssh', 'awx-job', 'gitops-mr', 'k8s-declarative', 'api')),
  rule_id        text NOT NULL DEFAULT '',                          -- matched rule id; '' ⇒ operator default lane
  outcome        text NOT NULL CHECK (outcome IN ('resolved', 'refused')),
  -- integrity: a resolved row names a regime + lane; a refusal carries neither (fail closed, REQ-1701).
  CHECK ((outcome = 'resolved' AND regime <> '' AND lane <> '') OR (outcome = 'refused' AND regime = '' AND lane = '')),
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0), -- reader-guard invariant
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX regime_resolution_created_idx ON regime_resolution (created_at DESC);
-- Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE (readiness-review G4, INV-19).
REVOKE UPDATE, DELETE ON regime_resolution FROM tg_runtime;

CREATE TABLE regime_actuation (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  action_id       text NOT NULL CHECK (length(btrim(action_id)) > 0),  -- INV-07 manifest lifecycle binding (non-secret)
  lane            text NOT NULL CHECK (lane IN ('native-ssh', 'awx-job', 'gitops-mr', 'k8s-declarative', 'api')),
  job_template_id text NOT NULL DEFAULT '',                            -- the AWX job-template id (non-secret)
  op_class        text NOT NULL DEFAULT '',                            -- the authorized op-class (non-secret)
  job_id          text NOT NULL DEFAULT '',                            -- the launched AWX job id / async handle (non-secret)
  -- INV-13: the AWX token is persisted ONLY as its SecretRef REFERENCE ('scheme:name'), never the value. The
  -- CHECK requires an empty string or a 'scheme:' prefix, so a raw plaintext token fails the constraint.
  token_ref       text NOT NULL DEFAULT '' CHECK (token_ref = '' OR token_ref ~ '^[a-z][a-z0-9]*:.+'),
  schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0),   -- reader-guard invariant
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX regime_actuation_action_idx ON regime_actuation (action_id);
REVOKE UPDATE, DELETE ON regime_actuation FROM tg_runtime;

CREATE TABLE deferred_verdict (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  action_id      text NOT NULL CHECK (length(btrim(action_id)) > 0),  -- the action this verdict closes (INV-07)
  job_id         text NOT NULL DEFAULT '',                            -- the AWX job id that reached terminal (non-secret)
  status         text NOT NULL CHECK (status IN ('successful', 'failed', 'error', 'canceled')),  -- terminal AWX status
  verdict        text NOT NULL CHECK (verdict IN ('match', 'deviation', 'unverified')),          -- mechanical verdict
  graduation     text NOT NULL CHECK (graduation IN ('verified_clean', 'deviated', 'no_credit')), -- ladder outcome
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),   -- reader-guard invariant
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deferred_verdict_action_idx ON deferred_verdict (action_id);
REVOKE UPDATE, DELETE ON deferred_verdict FROM tg_runtime;
