-- Revert 0127: drop the hold projection (TG-667). The governance_ledger rows it mirrors are untouched; after
-- the drop a re-detected incident is judged on fresh input only (the pre-TG-667 behaviour).
DROP TABLE IF EXISTS hold_decision;
