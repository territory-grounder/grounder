-- 0103: generalize the ledger-anchor to witness ANY named chain (TG-515). The anchor store began (0092) as a
-- single witness history of the governance_ledger HEAD; TG-510 needs the SAME tamper-witness for a SECOND
-- chain (the knowledge corpus), whose file-side head a consistent rewrite of every link would otherwise defeat
-- unless the head is anchored OUTSIDE the store. A `domain` column scopes each witness to its chain, so one
-- chain's anchors are never verified against another's rows.
--
-- BACKWARD-COMPATIBLE by construction: the DIGEST is unchanged (it already commits to the chain's own
-- seq/hash/window — content that naturally separates chains), so every anchor recorded before this migration
-- verifies exactly as before once its row is backfilled to 'governance-ledger' (the ADD COLUMN DEFAULT does
-- that in place). Domain is deliberately NOT folded into the digest, precisely to keep that byte-compatibility.
--
-- NON-SECRET, APPEND-ONLY: domain is a short chain NAME (e.g. 'governance-ledger', 'knowledge-corpus'), never
-- argv/credential/payload (INV-13); the table keeps the 0092 REVOKE of UPDATE/DELETE from tg_runtime, so a
-- witness — and now its domain — cannot be rewritten by the process that recorded it.
ALTER TABLE ledger_anchor ADD COLUMN domain text NOT NULL DEFAULT 'governance-ledger';

-- The verify read is per-domain ("this chain's witnesses over time"); index the domain-scoped by-seq lookup
-- the verifier joins on. The plane classification is unchanged (the 0092 COMMENT ON TABLE stands — 'both').
CREATE INDEX ledger_anchor_domain_seq_idx ON ledger_anchor (domain, anchored_seq);
