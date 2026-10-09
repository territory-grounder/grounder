-- Reverse 0112: return transaction_plan / _step to the 0111 baseline (INSERT, SELECT from the blanket
-- default privilege, 0105). Revoke only the UPDATE this migration added.
REVOKE UPDATE ON transaction_plan      FROM tg_runtime;
REVOKE UPDATE ON transaction_plan_step FROM tg_runtime;
