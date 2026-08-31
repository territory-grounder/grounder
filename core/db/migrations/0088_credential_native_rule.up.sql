-- TG-109: the DB-backed NATIVE per-target credential mapping — operator-authored resolver entries that
-- ride the EXISTING credential sync framework as a registered machine-plane source (spec/016 REQ-1607/
-- REQ-1610, modules/credsource/nativedb). Each row is ONE packed rule in the ParseRules grammar
--   kind:pattern|user|port|scheme[|sshKeyRef|apiTokenRef|become]
-- validated by core/credential.ParseRules AT WRITE TIME in the worker lane (temporal/nativerule) — a row
-- that does not parse to exactly one rule is never stored, so the sync can fail closed on a row id instead
-- of a corrupted set. Rows carry SecretRef REFERENCES only (env:/file:/store:/bao:/…), never secret values
-- (INV-13) — NewBundle enforces the sealed-scheme rule at parse. This is MUTABLE operator config (insert/
-- delete through the single-writer worker lane), NOT the append-only audit spine: the governance ledger
-- records each write, the table holds only the current rule set.
CREATE TABLE credential_native_rule (
  id         bigserial PRIMARY KEY,
  entry      text        NOT NULL CHECK (length(btrim(entry)) > 0),    -- one packed ParseRules rule
  rationale  text        NOT NULL CHECK (length(btrim(rationale)) > 0),-- why the operator added it
  created_by text        NOT NULL,                                     -- the authenticated operator id
  created_at timestamptz NOT NULL DEFAULT now()
);
-- Credential-plane declaration (TG-323 guard): operator CONFIG like policy_ruleset/control_plane_config —
-- written through the single-writer worker lane (runner queue), read by BOTH planes' sync source, so it
-- belongs to neither withheld list.
COMMENT ON TABLE credential_native_rule IS 'plane: both';
