-- 0001 baseline — single-organization foundation.
-- Provenance: [O] INV-16 (one DB, migrations, integrity by construction), [R] paradigm-rule 1
-- (single-org, multi-user; ADR-0010 — no tenant_id, no cross-tenant RLS). Correlation key is external_ref.
--
-- Role model (see deploy/.env.example): the MIGRATION role owns DDL and runs this file once; the
-- RUNTIME role has DML ONLY and is not a table owner, so a CREATE TABLE at request time fails at the
-- privilege level. That privilege boundary — not tenant RLS — is the isolation model.

-- Integrity-by-construction enums.
CREATE TYPE band AS ENUM ('POLL_PAUSE', 'AUTO_NOTICE', 'AUTO');
CREATE TYPE verdict AS ENUM ('match', 'partial', 'deviation');

-- Authenticated callers. hmac_secret_ref is a REFERENCE (env:/file:), never a literal secret [O] INV-13.
CREATE TABLE sources (
  source_id       text PRIMARY KEY,
  hmac_secret_ref text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);

-- Replay protection for HMAC ingress (P0-2). Keyed by (source_id, nonce); pruned by a Temporal
-- schedule (P1-9).
CREATE TABLE auth_nonce (
  source_id text NOT NULL,
  nonce     text NOT NULL,
  seen_at   timestamptz NOT NULL,
  PRIMARY KEY (source_id, nonce)
);

-- The append-only, content-hashed ActionManifest binding spine (P1-6). Built + threaded in Phase 1;
-- verdict is written only by the verifier in Phase 2. [O] INV-07/INV-10.
CREATE TABLE action_manifest (
  action_id       text PRIMARY KEY,
  action          jsonb NOT NULL,
  band            band NOT NULL DEFAULT 'POLL_PAUSE',   -- default = most restrictive
  plan_hash       text,
  prediction_hash text,
  approval_choice text,
  verdict         verdict,
  sealed_at       timestamptz NOT NULL DEFAULT now()
);
