-- 0102 — durable pre-mutation state capture (TG-58, a Phase-2 governed-autonomy prerequisite; spec/013).
--
-- The actuation chokepoint (core/actuate/interceptor.go) captures a target's rollback-relevant state at the LAST
-- pre-effect instant (Request.CaptureState) and hands it to a PreStateSink; this table is the durable sink so a
-- future Phase-2 applied-undo executor can restore to WHAT the world was (the recorded inverse in execution_log,
-- INV-07, says HOW to undo). One row per action_id, LATEST-WINS on re-capture: action_id is content-addressed
-- over the operation shape, so a repeated remediation overwrites — an undo targets the most recent execution of
-- a shape, matching action_execution's LatestExecution semantics (migration 0043).
--
-- Plane: WRITTEN by the worker (the actuation plane, via the interceptor's PreStateSink after a confirmed real
-- mutation) and READ by the applied-undo executor. `data` is an OPAQUE caller-produced snapshot — the op-class
-- CaptureState functions read NON-SECRET target state (a service's active/enabled flags, a guest's power state),
-- so no secret and no untrusted model content lands here.
CREATE TABLE action_prestate (
  action_id   text PRIMARY KEY,                       -- content-addressed over the operation; latest-wins on re-capture
  kind        text NOT NULL,                          -- the op-class / target-kind the snapshot describes
  data        bytea NOT NULL,                         -- the opaque serialized pre-state the caller captured
  captured_at timestamptz NOT NULL DEFAULT now()
);

-- The pre-state is WRITTEN by the actuation chokepoint (after a confirmed mutation) and READ by the applied-undo
-- executor — both actuation plane. A compromised TRIAGE worker must not be able to forge a rollback restore-point,
-- so this table is granted to the actuation plane only (not BOTH-by-default).
COMMENT ON TABLE action_prestate IS 'plane: actuation';
