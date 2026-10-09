-- 0042 — split propose-path prediction scores out of action_verdict (roadmap P2-2).
--
-- WHY. action_verdict has carried TWO populations that mean different things:
--
--   1. EXECUTED actions — the interceptor's post-execution check: "TG did X; did the estate change the way
--      the committed prediction said it would?" This is an ACTUATION-ACCURACY signal.
--   2. NEVER-EXECUTED propose-path predictions — the async falsifiability scorer grading a committed
--      blast-radius prediction against what the estate did on its own. This is a WORLD-MODEL-ACCURACY signal.
--
-- Pooling them makes the reported verified-match rate meaningless. Measured on the live box at the time of
-- this migration:
--
--      population                 match  partial  deviation  total  match%
--      EXECUTED action               24        3          1     28   85.7
--      propose-path prediction       22        4         23     49   44.9
--      ------------------------------------------------------------------
--      pooled (what was reported)    46        7         24     77   59.7
--
-- The pooled 59.7% understated actuation accuracy by 26 points AND overstated predictor accuracy by 15.
-- Worse, 23 of the 24 deviations were propose-path — so "TG deviated 23 times" described a world model being
-- wrong about an estate it never touched, not TG doing the wrong thing to a machine. Anyone reading the pooled
-- number (including this project's own status reports) drew the wrong conclusion about which subsystem was at
-- fault.
--
-- After this migration action_verdict has exactly ONE writer (core/actuate's interceptor, executed actions
-- only) and one meaning. prediction_verdict receives propose-path scores.
--
-- HISTORY IS NOT REWRITTEN. The 49 legacy propose-path rows STAY in action_verdict: it is an append-only
-- ledger with UPDATE/DELETE revoked from the runtime role (migration 0015), and quietly relocating audited
-- rows would be exactly the kind of history edit that design exists to prevent. Readers classify legacy rows
-- by the documented anti-join (no interceptor_gate_verdict row with gate='execute' AND verdict='pass' for the
-- same action_id) — see core/db/axis_read.go. Rows written from now on are executed-only by construction.

CREATE TABLE prediction_verdict (
  -- The scored prediction's bound action identity. NOT a foreign key to action_verdict: these actions were
  -- never executed, so they have no execution record to reference.
  action_id      text PRIMARY KEY,
  plan_hash      text NOT NULL,
  verdict        verdict NOT NULL,               -- match | partial | deviation (0001 enum)
  target_host    text NOT NULL DEFAULT '',
  site           text NOT NULL DEFAULT '',
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX prediction_verdict_created ON prediction_verdict (created_at);

-- Same tamper-resistance as every other verdict surface: the runtime role appends and reads, never rewrites.
-- A world-model score that can be edited after the fact is not evidence (INV-19; migration 0015 precedent).
REVOKE UPDATE, DELETE ON prediction_verdict FROM tg_runtime;
