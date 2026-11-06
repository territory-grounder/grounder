-- 0018 credential resolution — the append-only audit of every per-target credential RESOLUTION (spec/016
-- credential engine, REQ-1617). AFTER policy authorizes and BEFORE execute, the credential engine resolves
-- the target's SSH/API identity through the SyncEngine (native fallback + synced sources) and appends one
-- row here: the target, the identity plane, the outcome (resolved / unresolved / ambiguous), the winning
-- source (or native fallback), the matched rule, and the resolved NON-SECRET identity metadata.
--
-- NEVER A SECRET: the credential.ResolutionEvent projection type is secret-free BY CONSTRUCTION (INV-13). A
-- resolution stores REFERENCES, never values — the most a row holds about the secret is the key reference's
-- SCHEME (env / file / store / vault / bao), never the reference path and never key material. gitleaks CI
-- backs this. No column here can hold a secret.
--
-- APPEND-ONLY / TAMPER-RESISTANT: like the accountability spine (0015), the runtime DML role tg_runtime is
-- granted INSERT + SELECT only — UPDATE and DELETE are revoked below, so the app cannot rewrite its own
-- credential-resolution audit trail. A retention prune, if ever needed, must run as a privileged role.
-- schema_version is stamped on every row (reader-guard forward-compat), single-org.

CREATE TABLE credential_resolution (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  target         text NOT NULL CHECK (length(btrim(target)) > 0),   -- non-secret target label (host/resource/...)
  plane          text NOT NULL CHECK (plane IN ('machine', 'human')),
  outcome        text NOT NULL CHECK (outcome IN ('resolved', 'unresolved', 'ambiguous')),
  source         text NOT NULL DEFAULT '',                          -- winning source id; '' = native fallback / unresolved
  native         boolean NOT NULL DEFAULT false,                    -- resolved from the native fallback store
  rule_id        text NOT NULL DEFAULT '',                          -- matched rule / winning entry id (provenance)
  resolved_user  text NOT NULL DEFAULT '',                          -- resolved login user (non-secret); '' when unresolved
  scheme         text NOT NULL DEFAULT '',                          -- connection scheme (ssh/api/winrm/netconf)
  key_ref_scheme text NOT NULL DEFAULT '',                          -- SCHEME of the key reference only (env/file/...), never its value
  shadowed       text NOT NULL DEFAULT '',                          -- comma-separated lower-precedence sources that also matched
  err            text NOT NULL DEFAULT '',                          -- non-secret refusal text; empty on resolved
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0), -- reader-guard invariant
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credential_resolution_target_created_idx ON credential_resolution (target, created_at DESC);

-- Append-only: the runtime role may INSERT + SELECT but never rewrite/erase its own resolution audit.
REVOKE UPDATE, DELETE ON credential_resolution FROM tg_runtime;
