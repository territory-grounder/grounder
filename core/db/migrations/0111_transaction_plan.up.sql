-- 0111 — transaction plans: the durable rows behind the all-or-nothing multi-step repair (spec/030
-- T-030-2, TG-58; governance owner-ruled 2026-08-22: one approval for the whole plan, any step failure
-- auto-compensates).
--
-- transaction_plan is one row per COMPOSED plan, keyed by the content-addressed plan_id the ONE human
-- approval binds (REQ-3002 — the INV-07 argument one level up). Its state machine moves FORWARD ONLY —
-- proposed → approved → executing → committed | reverted | revert-failed — enforced by compare-and-swap
-- in the store (an UPDATE conditioned on the expected prior state), so a replayed or racing activity
-- cannot resurrect a terminal plan; the append-only HISTORY of those transitions is the governance
-- ledger's job (REQ-3006), not a second table's.
--
-- transaction_plan_step binds each step's own sealed action_id (INV-07 unchanged, per step) to its plan
-- and ordinal, with the step's own forward-only state — pending → executed → compensated |
-- compensate-failed — because "which steps remain applied" is exactly what the revert-failed terminal
-- must be able to say (REQ-3005).
--
-- Plane: both — the commit_confirm precedent (0095), deliberately NOT the action_execution one. These
-- rows are a PROJECTION: the workflow history is the sole authority on where a plan is, every step
-- re-enters the full interceptor chain as its own sealed action, and nothing reads a plan row to decide
-- whether to actuate. Their bookkeeping activities therefore run on the triage queue (like the
-- commit-confirm arm/resolve), which under a split-plane deployment could not write an
-- actuation-authority table at all. A forged row here mislabels a console/audit projection — the same
-- accepted exposure as a forged commit_confirm row — and forges no authority.
CREATE TABLE transaction_plan (
  plan_id      text PRIMARY KEY,                    -- content-addressed over the ordered rendered step tuples
  recipe       text NOT NULL,                       -- the declared recipe name (core/plan registry)
  external_ref text NOT NULL,                       -- the session whose proposal selected the recipe
  state        text NOT NULL DEFAULT 'proposed',    -- proposed|approved|executing|committed|reverted|revert-failed
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transaction_plan_step (
  plan_id   text NOT NULL REFERENCES transaction_plan(plan_id),
  ordinal   int  NOT NULL,                          -- 1-based execution order
  action_id text NOT NULL,                          -- the step's own sealed manifest identity (INV-07)
  op_class  text NOT NULL,
  state     text NOT NULL DEFAULT 'pending',        -- pending|executed|compensated|compensate-failed
  PRIMARY KEY (plan_id, ordinal)
);

COMMENT ON TABLE transaction_plan IS 'plane: both';
COMMENT ON TABLE transaction_plan_step IS 'plane: both';
