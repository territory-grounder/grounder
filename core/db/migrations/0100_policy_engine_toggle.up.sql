-- 0100 — durable admin engine-toggle override (TG-506, spec/015 REQ-1519).
--
-- The warn-don't-block admin engine toggle (core/policy/warn.go EngineToggle) held its override only in
-- process memory, so an Override set on the grounder (the authenticated admin plane) never reached the worker
-- (the decision plane) — the "present-not-reaching" gap this closes. This singleton row persists the SINGLE
-- current override so the worker's toggle can Load it (and refresh it), exactly as policy_mode (0019) persists
-- the single active mode. It is latest-wins mutable state, NOT an append-only spine: the immutable audit trail
-- of every toggle change is the governance_ledger (an EngineToggle.Override appends there BEFORE it takes
-- effect). override IS NULL ⇒ follow the per-mode default (no admin override); true/false ⇒ the admin's
-- explicit enable/disable. The never-auto floor (INV-09) is unaffected by the override in either direction.
CREATE TABLE policy_engine_toggle (
  singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),  -- exactly one override row
  override   boolean,                                             -- NULL ⇒ per-mode default; else the admin override
  actor      text NOT NULL DEFAULT '',                            -- who last set it (a non-secret label; no secret lands here)
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Plane: BOTH — the current override is WRITTEN by the grounder (the authenticated admin plane, via
-- EngineToggle.Override) and READ by the worker (the decision plane, via EngineToggle.Load), exactly like
-- policy_mode. It carries no untrusted content and no secret (a boolean + a non-secret actor label).
COMMENT ON TABLE policy_engine_toggle IS 'plane: both';
