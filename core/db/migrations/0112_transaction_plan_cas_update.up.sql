-- 0112 — the UPDATE grant on transaction_plan / _step that 0111 never made (TG-551).
--
-- transaction_plan / transaction_plan_step (spec/030 T-030-2, TG-58) run a FORWARD-ONLY compare-and-swap
-- state machine (core/db/transaction_plan.go Transition / TransitionStep do `UPDATE ... SET state=$ WHERE
-- ... state=from`), so the writer legitimately UPDATEs the state column. But 0111 wrote no grants and the
-- append-only default privilege (0105) grants a post-0105 table INSERT+SELECT only — no UPDATE. So
-- tg_runtime held no UPDATE, the CAS would fail at its first Transition, and ApplyPlaneGrants had no UPDATE
-- to mirror to the plane roles.
--
-- This is the append-only-default EXCEPTION a legitimately-mutable projection needs. The table was created
-- AFTER the 0105 inversion, so this grant cannot live in 0105 (the table did not exist then); the TG-80
-- traced-set test (core/db/append_only_default_test.go) now unions the tg_runtime grant-backs across ALL
-- migrations, and both tables are in its golden set. ApplyPlaneGrants mirrors the UPDATE to
-- tg_triage/tg_actuate at boot. tg_runtime is unconditional (it exists in every fixture; 0106 grants it the
-- same way). Idempotent.
GRANT UPDATE ON transaction_plan      TO tg_runtime;
GRANT UPDATE ON transaction_plan_step TO tg_runtime;
