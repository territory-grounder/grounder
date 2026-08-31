-- Reverse 0015: restore the blanket DML grant (re-enables UPDATE/DELETE for tg_runtime on the spine tables).
-- This re-opens the tamper hole 0015 closed; present only so the migration is cleanly reversible.
GRANT UPDATE, DELETE ON governance_ledger  TO tg_runtime;
GRANT UPDATE, DELETE ON session_risk_audit TO tg_runtime;
GRANT UPDATE, DELETE ON action_verdict     TO tg_runtime;
