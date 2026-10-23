-- Reverse 0020: drop the three append-only regime-engine audit tables. Dropping a table removes every
-- grant/revoke scoped to it in one step — the REVOKE on each append-only table is created and destroyed with
-- the table, so no separate GRANT restore is needed. The tamper-evident governance_ledger records these
-- rows chained (INV-19) and is unaffected.
DROP TABLE IF EXISTS deferred_verdict;
DROP TABLE IF EXISTS regime_actuation;
DROP TABLE IF EXISTS regime_resolution;
