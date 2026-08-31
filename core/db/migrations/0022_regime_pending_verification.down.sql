-- Reverse 0022: drop the durable pending-verification store. Dropping the table removes the REVOKE DELETE
-- scoped to it in one step (no separate GRANT restore needed). The async-verify channel then falls back to
-- its in-memory pending store, so a launched job's deferred verify no longer survives a worker restart —
-- safe only while zero launches occur (mutation defaults OFF at Shadow). The append-only deferred_verdict
-- history (0020) and the tamper-evident governance ledger are unaffected.
DROP TABLE IF EXISTS pending_verification;
