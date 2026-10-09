-- 0092: the LEDGER-HEAD ANCHOR — an append-only WITNESS of the governance_ledger HEAD over time (TG-80 P1#1).
--
-- Provenance: [O] INV-19 (append-only, hash-chained audit spine), spec/006 REQ-503 · [F] the h-apache-stack
-- peer's action-log HEAD anchored into a domain its writer cannot rewrite (TG-80 ranked adoption P1 #1).
--
-- WHAT IT CLOSES. VerifyChain (core/audit/ledger.go) proves the rows it is GIVEN form a consistent chain from
-- seq 1 — but it CANNOT see two tampers that leave a self-consistent chain: a TAIL TRUNCATION (delete the rows
-- after seq k; the prefix 1..k still verifies) and a WHOLESALE RE-LINK (rewrite a row and recompute every hash
-- to the HEAD). migration 0015 REVOKEs UPDATE/DELETE from tg_runtime, but that boundary is REVERSIBLE by a
-- privileged role (0015.down, a mis-scoped retention job, a compromised superuser) — the exact actor an audit
-- trail exists to catch. This table records, each interval, the HEAD seq + its chain hash + a digest over the
-- trailing window (core/audit.ComputeAnchor). An anchor at T1 FIXES the HEAD at T1, so a later rollback below
-- it, or a re-linked hash at it, becomes a HEAD that regressed below a witness — detectable by
-- core/audit.VerifyAgainstAnchors.
--
-- WHY THE WITNESS IS MEANINGFUL: the RECORDER HOLDS NO CREDENTIAL TO REWRITE WHAT IT RECORDS. tg_runtime keeps
-- INSERT + SELECT here (append a witness, read the history) but NOT UPDATE/DELETE — revoked below, exactly as
-- 0015 does for the spine — so the same process that appends a HEAD witness cannot later erase or alter it to
-- match a tampered ledger. (The stronger airgap is Temporal event history, a separate credential domain the DB
-- role cannot reach; the periodic recorder returns the anchor so an activity wrapper lands it there too.)
--
-- NON-SECRET by construction: a seq, two content-hash hex digests, a window size — never argv, credential, or
-- payload (INV-13). APPEND-ONLY, same discipline as the accountability spine (0015) and edge_disproof (0075).
CREATE TABLE ledger_anchor (
  id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  anchored_seq   bigint      NOT NULL,                  -- the governance_ledger HEAD seq this row witnesses
  anchored_hash  text        NOT NULL,                  -- the HEAD row's chain hash (commits to rows 1..anchored_seq)
  window_size    integer     NOT NULL DEFAULT 0,        -- trailing rows folded into the digest (a localiser)
  digest         text        NOT NULL,                  -- sha256 HEAD commitment (core/audit.ComputeAnchor)
  created_at     timestamptz NOT NULL DEFAULT now(),    -- when the witness was recorded
  schema_version integer     NOT NULL DEFAULT 1
);

-- The review read is "how has the HEAD been witnessed over time"; the by-seq index supports the verify join.
CREATE INDEX ledger_anchor_seq_idx     ON ledger_anchor (anchored_seq);
CREATE INDEX ledger_anchor_created_idx ON ledger_anchor (created_at DESC);

-- Append-only tamper-resistance (INV-19), same REVOKE as the spine (0015) / edge_disproof (0075): the recorder
-- may INSERT + SELECT (its INSERT/SELECT come from the blanket ALTER DEFAULT PRIVILEGES in
-- deploy/postgres-init/00-roles.sh) but never UPDATE/DELETE, so it cannot rewrite a witness it has recorded.
REVOKE UPDATE, DELETE ON ledger_anchor FROM tg_runtime;

-- Credential-plane classification (migration 0060): an accountability record, like governance_ledger and
-- session_risk_audit — it records no actuation and authorises none — so, like the spine it witnesses, 'both'.
COMMENT ON TABLE ledger_anchor IS 'plane: both';
