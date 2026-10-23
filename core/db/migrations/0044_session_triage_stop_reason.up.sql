-- WHY A SESSION PROPOSED NOTHING.
--
-- Measured 2026-07-28: 220 of 698 sessions in 24h (31.5%) end `no-proposal:stop` after ~4.4 steps of real
-- investigation. Some of those are CORRECT — TG was deliberately taught to stop proposing an inapplicable
-- disk-grow for a loopback disk-fill, and it correctly stands down on stale alerts and self-remediated
-- incidents. Others are genuine diagnosis MISSES. Today they are indistinguishable: both write
-- outcome='no-proposal:stop' with an empty conclusion.
--
-- The orchestrator ALREADY KNOWS which is which. agent.Loop sets a precise Reason on every stop — "model call
-- failed", "unparseable model output", "confidence below stop threshold", "write tool withheld", "trajectory
-- veto — …", "proposal failed the single grammar" — and the field was DROPPED at the activity boundary
-- (InvestigateResult carried Conclusion but not Reason). An infrastructure failure was therefore recorded
-- identically to a considered refusal.
--
-- stop_reason is ORCHESTRATOR-COMPUTED and trusted, deliberately SEPARATE from `conclusion`, which is
-- untrusted agent free-text (INV-08). Do not merge them.
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS stop_reason text NOT NULL DEFAULT '';

COMMENT ON COLUMN session_triage.stop_reason IS
  'Orchestrator-computed reason the agent loop halted without a proposal (trusted; distinct from the untrusted agent conclusion). Empty when the session proposed an action.';
