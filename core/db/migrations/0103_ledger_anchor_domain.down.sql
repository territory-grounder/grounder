-- 0103 down (TG-515): revert the domain generalization. Safe only when no non-'governance-ledger' anchors
-- exist (a second consumer like TG-510 would lose its witness history); the column and its index drop cleanly.
DROP INDEX IF EXISTS ledger_anchor_domain_seq_idx;
ALTER TABLE ledger_anchor DROP COLUMN IF EXISTS domain;
