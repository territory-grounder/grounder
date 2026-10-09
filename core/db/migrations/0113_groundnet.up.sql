-- 0113 groundnet persistence — the APPEND-ONLY durable trail of the DORMANT federation seam (spec/021
-- T-021-8, REQ-2101/2103/2104/2105/2115, INV-19/INV-13). Two immutable, NON-SECRET, DE-IDENTIFIED tables:
-- one row per Emit and one per Ingest.
--
--  1. groundnet_emit   — one immutable row per published wisdom unit: the statement content-address subject
--                        (sub), the payload media type, the producer PSEUDONYM (iss, gnpub:) and its COSE key
--                        id (kid), the Transparency Service Receipt (the provenance anchor), and the declared
--                        retention class. Every field is a KIND or a content-address; NONE is an estate
--                        identifier, a governance field, or a secret (REQ-2101). The producer is a stable
--                        pseudonym, never a real-world or estate identity (REQ-2103).
--
--  2. groundnet_ingest — one immutable row per ingested foreign statement: the sub, the producer pseudonym,
--                        the COSE signature/envelope verify result (REQ-2104), and the re-graduation
--                        disposition (the seam's Disposition* outcome). De-identified; a subordinate hint, so
--                        the row records that a statement entered local re-graduation, never that it earned
--                        authority (REQ-2109/2110).
--
-- The seam is OPT-IN, default-off, and reaches no actuator: nothing writes these tables until an org admin
-- arms federation (far-future). Like the regime audit (0020) and the accountability spine (0015), tg_runtime
-- is STRIPPED of UPDATE/DELETE, so the node appends and reads its federation trail but never rewrites one
-- (append-only, tamper-resistant, INV-19). Single-organization (ADR-0010, paradigm-rule 1): NO tenant_id.
-- This migration is PERSISTENCE only; it actuates nothing and gates nothing (mutation stays OFF).

CREATE TABLE groundnet_emit (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sub            text NOT NULL CHECK (length(btrim(sub)) > 0),          -- statement content-address subject (a hash, de-identified)
  content_type   text NOT NULL CHECK (length(btrim(content_type)) > 0), -- payload media type (a KIND, e.g. application/vnd.groundnet.wisdom+json)
  iss            text NOT NULL CHECK (iss LIKE 'gnpub:%'),              -- producer pseudonym; gnpub:, NEVER a real/estate identity (REQ-2103)
  kid            text NOT NULL DEFAULT '',                              -- COSE key id binding iss (non-secret public-key id)
  receipt        bytea NOT NULL CHECK (octet_length(receipt) > 0),      -- Transparency Service Receipt: the non-secret provenance anchor (REQ-2105)
  retention      text NOT NULL DEFAULT '',                             -- declared retention class of the governed record (REQ-2114)
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),    -- reader-guard invariant
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX groundnet_emit_sub_idx ON groundnet_emit (sub);
COMMENT ON TABLE groundnet_emit IS 'plane: both';
-- Append-only / tamper-resistant: the runtime DML role loses UPDATE+DELETE (readiness-review G4, INV-19).
REVOKE UPDATE, DELETE ON groundnet_emit FROM tg_runtime;

CREATE TABLE groundnet_ingest (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sub            text NOT NULL CHECK (length(btrim(sub)) > 0),          -- ingested statement's content-address subject
  iss            text NOT NULL CHECK (iss LIKE 'gnpub:%'),              -- producer pseudonym (REQ-2103)
  verify_result  text NOT NULL CHECK (verify_result IN ('verified', 'rejected')),  -- COSE signature/envelope verify (REQ-2104)
  disposition    text NOT NULL CHECK (disposition IN ('candidate', 'rejected-malformed', 'rejected-unverified', 'rejected-no-receipt', 'rejected-bad-receipt', 'rejected-replay', 'rejected-unknown-payload')),  -- re-graduation disposition
  -- integrity: only a VERIFIED statement can land as a candidate; every rejection carries a rejected-* disposition.
  CHECK ((verify_result = 'verified' AND disposition = 'candidate') OR (disposition <> 'candidate')),
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),    -- reader-guard invariant
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX groundnet_ingest_sub_idx ON groundnet_ingest (sub);
COMMENT ON TABLE groundnet_ingest IS 'plane: both';
REVOKE UPDATE, DELETE ON groundnet_ingest FROM tg_runtime;
