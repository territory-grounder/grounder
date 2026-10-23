-- 0003 audit spine — the two immutable governance tables the running system writes on every decision.
-- Provenance: [O] INV-19 (append-only, hash-chained audit spine), INV-11/INV-07, spec/006 REQ-503/REQ-505,
-- spec/001 · [F] the predecessor governance_ledger + session_risk_audit. DATA-MODEL §4.
--
-- Role model (baseline): the MIGRATION role owns this DDL; the RUNTIME role has DML only and is NOT an owner.
-- Append-only is enforced by the privilege boundary (no UPDATE/DELETE grant) — never by a trigger — plus the
-- hash chain on governance_ledger, which makes any post-hoc edit detectable by VerifyChain regardless of grant.

-- The tamper-evident governance ledger: one row per governance decision, content-hash-chained to its
-- predecessor so any insertion/edit/reordering breaks the chain (INV-19). seq is monotonic from 1.
CREATE TABLE governance_ledger (
  seq        bigint PRIMARY KEY,                 -- monotonic, gap-free from 1 (the chain position)
  decision   text NOT NULL,                      -- e.g. "classify:AUTO", "gate:deny", "verdict:deviation"
  reason     text NOT NULL DEFAULT '',
  action_id  text NOT NULL,                      -- the content-hashed action this decision binds to (INV-07)
  withheld   boolean NOT NULL DEFAULT false,     -- true when autonomy was withheld (the channel allowed to say no)
  prev_hash  text NOT NULL DEFAULT '',           -- the previous row's hash (the chain link; '' for seq 1)
  hash       text NOT NULL,                      -- sha256 over the length-prefixed fields INCLUDING prev_hash
  created_at timestamptz NOT NULL DEFAULT now()
);

-- The immutable per-classification record: the single source of truth for WHY a session auto-proceeded.
-- De-identified (normalized signals, not raw transcript); part of the audit spine, never TTL-purged.
CREATE TABLE session_risk_audit (
  id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  external_ref            text NOT NULL,
  risk_level             text NOT NULL,
  band                   band NOT NULL DEFAULT 'POLL_PAUSE',   -- reuses the 0001 enum; default = most restrictive
  auto_approved          boolean NOT NULL DEFAULT false,
  -- INV made structural: TG NEVER proceeds on a timed-out poll. The CHECK forbids a true value at the DB
  -- level, so no writer — however buggy — can persist an auto-proceed-on-timeout classification.
  auto_proceed_on_timeout boolean NOT NULL DEFAULT false CHECK (auto_proceed_on_timeout = false),
  notify_required        boolean NOT NULL DEFAULT false,
  operator_override      boolean NOT NULL DEFAULT false,
  signals_json           jsonb NOT NULL DEFAULT '{}'::jsonb,
  plan_hash              text,
  action_id              text NOT NULL,                        -- content-hashed action this binds to (INV-07)
  schema_version         int NOT NULL CHECK (schema_version > 0),
  created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX session_risk_audit_external_ref ON session_risk_audit (external_ref);
CREATE INDEX session_risk_audit_action_id ON session_risk_audit (action_id);
