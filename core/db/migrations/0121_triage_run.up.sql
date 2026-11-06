-- 0121: triage_run — one row per triage RUN, not per alert (TG-633, TG-624 P2-7).
--
-- session_triage is keyed on external_ref and is first-write-wins, so a recurring alert that is triaged again
-- writes nothing new. Measured on 2026-11-06 over 30 days: 769 alerts had agent runs, about 2,618 runs in all
-- (3.4 per alert), and 186 alerts were triaged more than once. Run-level benchmark axes (A2 diagnosis, A6 steps
-- and decision time, cost) were therefore computed over first runs only.
--
-- Written beside the session_triage insert by the same record activity, keyed (external_ref, run_id) where
-- run_id is the Temporal workflow RUN id: ON CONFLICT DO NOTHING makes an activity retry a no-op, and a new
-- run of the same alert gets its own row. Same plane as session_triage (triage-authored content), INSERT-only.
-- Nothing on the decision path reads it.
-- Idempotent by drop-then-create: renumbered from 0118 before merge (migration-collision gate), and the table holds no
-- data anywhere it could already exist (only a local test fixture ran the old name).
DROP TABLE IF EXISTS triage_run;
CREATE TABLE triage_run (
    id                  bigserial PRIMARY KEY,
    external_ref        text NOT NULL CHECK (external_ref <> ''),
    run_id              text NOT NULL CHECK (run_id <> ''),
    host                text NOT NULL DEFAULT '',
    alert_rule          text NOT NULL DEFAULT '',
    band                text NOT NULL DEFAULT '',
    outcome             text NOT NULL DEFAULT '',
    proposed            boolean NOT NULL DEFAULT false,
    op_class            text NOT NULL DEFAULT '',
    confidence          double precision NOT NULL DEFAULT 0,
    step_count          integer NOT NULL DEFAULT 0,
    decision_ms         bigint NOT NULL DEFAULT 0,
    model_tier          text NOT NULL DEFAULT '',
    decision_model_tier text NOT NULL DEFAULT '',
    harness_fp          text NOT NULL DEFAULT '',
    mutated             boolean NOT NULL DEFAULT false,
    recorded_at         timestamptz NOT NULL DEFAULT now(),
    schema_version      integer NOT NULL DEFAULT 1,
    UNIQUE (external_ref, run_id)
);
CREATE INDEX triage_run_recorded_at ON triage_run (recorded_at);
COMMENT ON TABLE triage_run IS 'plane: triage';
