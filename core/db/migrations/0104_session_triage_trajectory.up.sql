-- TG-527 (follow-up to TG-525): persist the session's ordered, digested tool path so the
-- trajectory_grounded axis can cover HISTORICAL sessions — before this column the trajectory lived only
-- in the eval harness's in-process RunnerResult, and any DB-replayed re-judge read the axis N/A forever.
ALTER TABLE session_triage ADD COLUMN trajectory jsonb;

-- The Diagnosis discipline (0056): NULL means "this row predates the column" and the axis scores N/A;
-- an explicit '[]' means "recorded, and the loop took no tool steps" — a gradeable fact. The writer
-- always writes non-NULL from this build on. Steps are {tool, args_key} with ArgsKey already digested —
-- no raw arguments, no untrusted payload (INV-13).
COMMENT ON COLUMN session_triage.trajectory IS
  'TG-527: ordered digested tool path ([]agent.TrajectoryStep as jsonb); NULL = pre-0104 row (axis N/A), [] = recorded-and-empty';
