-- 0012 sealed_secret — the write-only encrypted secret store (task #27 Phase D, REQ-524).
-- Envelope encryption (core/seal): the value is AES-256-GCM ciphertext under a per-secret DEK; the
-- DEK is wrapped by the master key resolved from TG_SEAL_KEY_REF; both are AEAD-bound to the NAME.
-- This table holds ONLY ciphertext — a dump/backup carries no plaintext (INV-13). Reads happen solely
-- through the store: SecretRef scheme inside the control plane; no HTTP surface ever emits the value.
CREATE TABLE sealed_secret (
  name           text PRIMARY KEY CHECK (name ~ '^[a-z0-9]([a-z0-9._-]{0,62})$'),
  ciphertext     bytea NOT NULL,
  nonce          bytea NOT NULL,
  wrapped_dek    bytea NOT NULL,
  dek_nonce      bytea NOT NULL,
  purpose        text NOT NULL DEFAULT '',
  created_by     text NOT NULL,
  ledger_seq     bigint,
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
